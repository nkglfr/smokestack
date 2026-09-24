package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/smtp"
	"strings"
	"time"
)

// Federation alerting warns a peer's NOC about their network. This file is
// the other half: warning the operator about their own targets. A target has
// to stay in incident for a while before anyone is woken up, and a
// traceroute is taken first, so the message says where the path breaks
// instead of only that something broke.

type AlertConfig struct {
	Enabled      bool   `json:"enabled"`
	AfterMinutes int    `json:"after_minutes"` // sustained incident before alerting
	Recipients   string `json:"recipients"`    // comma separated, never public
	WebhookURL   string `json:"webhook_url"`
	RepeatHours  int    `json:"repeat_hours"` // silence between two alerts for one target
	Recovery     bool   `json:"recovery"`     // also tell when it is over
}

func defaultAlertConfig() AlertConfig {
	return AlertConfig{Enabled: false, AfterMinutes: 5, RepeatHours: 6, Recovery: true}
}

func (s *Store) AlertConfig() AlertConfig {
	c := defaultAlertConfig()
	if raw := s.Setting("local_alerts", ""); raw != "" {
		var v AlertConfig
		if json.Unmarshal([]byte(raw), &v) == nil {
			c = v
			if c.AfterMinutes <= 0 {
				c.AfterMinutes = 5
			}
			if c.RepeatHours <= 0 {
				c.RepeatHours = 6
			}
		}
	}
	return c
}

func (s *Store) SetAlertConfig(c AlertConfig) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return s.SetSetting("local_alerts", string(b))
}

type LocalIncident struct {
	ID         int64  `json:"id"`
	TargetID   int64  `json:"target_id"`
	Title      string `json:"title"`
	Host       string `json:"host"`
	OpenedAt   int64  `json:"opened_at"`
	NotifiedAt int64  `json:"notified_at,omitempty"`
	ClosedAt   int64  `json:"closed_at,omitempty"`
	Detail     string `json:"detail"`
}

func alertSchema(s *Store) {
	s.cfg.Exec(`CREATE TABLE IF NOT EXISTS local_incidents(
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		target_id INTEGER NOT NULL, opened_at INTEGER NOT NULL,
		notified_at INTEGER NOT NULL DEFAULT 0,
		closed_at INTEGER NOT NULL DEFAULT 0,
		detail TEXT NOT NULL DEFAULT '')`)
	s.cfg.Exec(`CREATE INDEX IF NOT EXISTS idx_local_inc ON local_incidents(target_id, closed_at)`)
}

// Alerter holds what the loop needs. The clock and the sender are fields so
// that the state machine can be tested without waiting or sending anything.
type Alerter struct {
	store *Store
	now   func() int64
	send  func(cfg AlertConfig, subject, body string) error
	trace func(targetID int64) // asks the probe for a traceroute
}

func NewAlerter(store *Store) *Alerter {
	alertSchema(store)
	return &Alerter{
		store: store,
		now:   func() int64 { return time.Now().Unix() },
		send:  sendAlert,
		trace: func(id int64) { traceRequests.Push(id) },
	}
}

func (a *Alerter) openIncident(targetID, now int64, detail string) {
	a.store.cfg.Exec(`INSERT INTO local_incidents(target_id,opened_at,detail) VALUES(?,?,?)`,
		targetID, now, detail)
}

func (a *Alerter) openFor(targetID int64) (*LocalIncident, bool) {
	row := a.store.cfg.QueryRow(`SELECT id,target_id,opened_at,notified_at,closed_at,detail
	                             FROM local_incidents WHERE target_id=? AND closed_at=0
	                             ORDER BY opened_at DESC LIMIT 1`, targetID)
	inc := &LocalIncident{}
	if err := row.Scan(&inc.ID, &inc.TargetID, &inc.OpenedAt, &inc.NotifiedAt,
		&inc.ClosedAt, &inc.Detail); err != nil {
		return nil, false
	}
	return inc, true
}

// lastNotified is when this target last woke somebody up, closed incidents
// included: it is what keeps a flapping target from alerting every hour.
func (a *Alerter) lastNotified(targetID int64) int64 {
	var ts int64
	a.store.cfg.QueryRow(`SELECT COALESCE(MAX(notified_at),0) FROM local_incidents
	                      WHERE target_id=?`, targetID).Scan(&ts)
	return ts
}

func (a *Alerter) Incidents(limit int) ([]LocalIncident, error) {
	rows, err := a.store.cfg.Query(`SELECT i.id,i.target_id,i.opened_at,i.notified_at,i.closed_at,
	                                i.detail,t.title,t.host FROM local_incidents i
	                                LEFT JOIN targets t ON t.id=i.target_id
	                                ORDER BY i.opened_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []LocalIncident{}
	for rows.Next() {
		var i LocalIncident
		var title, host *string
		if err := rows.Scan(&i.ID, &i.TargetID, &i.OpenedAt, &i.NotifiedAt, &i.ClosedAt,
			&i.Detail, &title, &host); err != nil {
			return nil, err
		}
		if title != nil {
			i.Title = *title
		}
		if host != nil {
			i.Host = *host
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// Tick advances the state machine for one round of statuses. Returns the
// number of alerts sent, which the tests check.
func (a *Alerter) Tick(statuses map[int64]string, details map[int64]string) int {
	cfg := a.store.AlertConfig()
	now, sent := a.now(), 0
	for targetID, status := range statuses {
		inc, open := a.openFor(targetID)
		if status != "crit" {
			if !open {
				continue
			}
			a.store.cfg.Exec(`UPDATE local_incidents SET closed_at=? WHERE id=?`, now, inc.ID)
			if cfg.Enabled && cfg.Recovery && inc.NotifiedAt > 0 {
				// The recovery notice follows the alert: if one was sent,
				// the other is owed, whatever the target setting is now.
				t, err := a.store.TargetByID(targetID)
				if err == nil {
					a.send(cfg, fmt.Sprintf("[smokestack] Recovered: %s", t.Title),
						fmt.Sprintf("%s (%s) is back within its baseline after %s.\n",
							t.Title, t.Host, humanSince(inc.OpenedAt, now)))
					sent++
				}
			}
			continue
		}
		if !open {
			// A traceroute is asked for as soon as the incident opens, so
			// that the path is captured while it is still broken.
			a.openIncident(targetID, now, details[targetID])
			a.trace(targetID)
			continue
		}
		if !cfg.Enabled || inc.NotifiedAt > 0 {
			continue
		}
		// A target can be left out of alerting without being left out of
		// monitoring: the incident is still recorded and still visible.
		if t, err := a.store.TargetByID(targetID); err == nil && t.AlertsOff {
			continue
		}
		if now-inc.OpenedAt < int64(cfg.AfterMinutes)*60 {
			continue // not sustained yet
		}
		if last := a.lastNotified(targetID); last > 0 && now-last < int64(cfg.RepeatHours)*3600 {
			continue // already woke somebody up recently for this target
		}
		t, err := a.store.TargetByID(targetID)
		if err != nil {
			continue
		}
		subject := fmt.Sprintf("[smokestack] %s down for %s", t.Title, humanSince(inc.OpenedAt, now))
		if err := a.send(cfg, subject, a.body(t, inc, details[targetID], now)); err != nil {
			log.Printf("alert for %s: %v", t.Title, err)
			continue
		}
		a.store.cfg.Exec(`UPDATE local_incidents SET notified_at=? WHERE id=?`, now, inc.ID)
		sent++
	}
	return sent
}

func humanSince(from, now int64) string {
	m := (now - from) / 60
	if m < 60 {
		return fmt.Sprintf("%d min", m)
	}
	return fmt.Sprintf("%dh%02d", m/60, m%60)
}

// body describes what is wrong and what the path looks like, so that the
// person reading it at 3 a.m. does not have to open anything to decide.
func (a *Alerter) body(t *Target, inc *LocalIncident, detail string, now int64) string {
	site := a.store.Site()
	var b strings.Builder
	fmt.Fprintf(&b, "%s (%s) has been in incident for %s.\n\n",
		t.Title, t.Host, humanSince(inc.OpenedAt, now))
	if detail != "" {
		fmt.Fprintf(&b, "State: %s\n", detail)
	}
	fmt.Fprintf(&b, "Since: %s UTC\nProbe: %s every %d s, %d packets\n\n",
		time.Unix(inc.OpenedAt, 0).UTC().Format("2006-01-02 15:04"),
		strings.ToUpper(t.Proto), t.IntervalS, t.Packets)

	// The traceroute taken when the incident opened, compared with the last
	// healthy path: this is what says where it breaks.
	if trs, err := a.store.Traceroutes(t.ID, nil, 6); err == nil {
		var cur, ref *Traceroute
		for _, tr := range trs {
			if cur == nil && tr.Kind != "reference" && tr.TS >= inc.OpenedAt-300 {
				cur = tr
			}
			if ref == nil && tr.Kind == "reference" {
				ref = tr
			}
		}
		if cur != nil {
			b.WriteString("Traceroute taken during the incident:\n")
			for i, h := range cur.Hops {
				addr := h.Addr
				if addr == "" {
					addr = "*"
				}
				mark := ""
				if ref != nil && i < len(ref.Hops) && ref.Hops[i].Addr != "" &&
					ref.Hops[i].Addr != h.Addr {
					mark = fmt.Sprintf("   <-- healthy path had %s", ref.Hops[i].Addr)
				}
				fmt.Fprintf(&b, "  %2d  %-40s %s%s\n", i+1, addr, h.Name, mark)
			}
			if !cur.Reached {
				b.WriteString("  destination not reached\n")
			}
			b.WriteString("\n")
		} else {
			b.WriteString("No traceroute was available when this alert was sent.\n\n")
		}
	}
	if url := strings.TrimSuffix(site.URL, "/"); url != "" {
		fmt.Fprintf(&b, "Graph: %s/t/%s\n", url, t.Slug)
	}
	fmt.Fprintf(&b, "\nSent by smokestack on behalf of %s. Turn these alerts off in the "+
		"back-office, Federation > NOC alerting.\n", site.Org)
	return b.String()
}

// sendAlert delivers to the webhook and to the addresses, reusing the SMTP
// settings already configured for the federation NOC alerts.
func sendAlert(cfg AlertConfig, subject, body string) error {
	// Channels first: SMTP with its own settings, chat rooms, SMS. The
	// legacy recipients/webhook of the alert settings still work beside
	// them, so an instance configured before channels existed keeps going.
	if err := SendAll(alertChannels, subject, body); err != nil {
		log.Printf("alert channels: %v", err)
	}
	var firstErr error
	if cfg.WebhookURL != "" {
		payload, _ := json.Marshal(map[string]any{"subject": subject, "body": body})
		resp, err := http.Post(cfg.WebhookURL, "application/json", bytes.NewReader(payload))
		if err != nil {
			firstErr = err
		} else {
			resp.Body.Close()
		}
	}
	to := splitList(cfg.Recipients)
	if len(to) == 0 {
		return firstErr
	}
	n := alertSMTP
	if n.SMTPHost == "" || n.From == "" {
		return firstErr
	}
	msg := []byte(fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\n"+
		"MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s",
		n.From, strings.Join(to, ", "), subject, body))
	var auth smtp.Auth
	if n.SMTPUser != "" {
		auth = smtp.PlainAuth("", n.SMTPUser, n.SMTPPass, n.SMTPHost)
	}
	if err := smtp.SendMail(fmt.Sprintf("%s:%d", n.SMTPHost, n.SMTPPort), auth,
		n.From, to, msg); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

// alertSMTP is filled at start from the NOC alerting settings, so both kinds
// of alert share one mail configuration.
var (
	alertSMTP     NotifyConfig
	alertChannels ChannelSet
)

func splitList(s string) []string {
	var out []string
	for _, p := range strings.FieldsFunc(s, func(r rune) bool {
		return r == ',' || r == ';' || r == ' ' || r == '\n'
	}) {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// Loop checks every minute: often enough for a five-minute threshold, cheap
// enough to run beside everything else.
func (a *Alerter) Loop(stop <-chan struct{}, probeID int64) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			alertSMTP = loadNotifyConfig(a.store)
			alertChannels = a.store.Channels()
			ov, err := a.store.Overview(probeID, a.now(), false)
			if err != nil {
				continue
			}
			statuses, details := map[int64]string{}, map[int64]string{}
			for _, c := range ov.Categories {
				for _, t := range c.Targets {
					statuses[t.ID] = t.Status
					if t.LossPct != nil && *t.LossPct > 0 {
						details[t.ID] = fmt.Sprintf("%.1f %% packet loss", *t.LossPct)
					} else if t.MedMs != nil {
						details[t.ID] = fmt.Sprintf("median %.2f ms", *t.MedMs)
					}
				}
			}
			a.Tick(statuses, details)
		}
	}
}

func loadNotifyConfig(s *Store) NotifyConfig {
	var c NotifyConfig
	if raw := s.Setting("notify", ""); raw != "" {
		json.Unmarshal([]byte(raw), &c)
	}
	return c
}
