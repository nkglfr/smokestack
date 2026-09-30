package main

import (
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Surveillance des certificats. Deliberement separee de la mesure de
// latence : lire un certificat demande une poignee de main TLS complete,
// et la faire toutes les soixante secondes gaspillerait du reseau tout en
// polluant la serie de latence. Une fois par jour suffit — un certificat
// n'expire pas entre deux passes.
//
// L'expiration n'est pas le seul mode de panne. Un certificat valide mais
// emis pour un autre nom, ou dont la chaine est incomplete, coupe le
// service aussi surement qu'un certificat perime, et se voit moins. Les
// trois sont verifies.

const certSchema = `
CREATE TABLE IF NOT EXISTS certs (
  target_id   INTEGER PRIMARY KEY REFERENCES targets(id) ON DELETE CASCADE,
  checked_at  INTEGER NOT NULL,
  not_after   INTEGER,
  not_before  INTEGER,
  issuer      TEXT,
  subject     TEXT,
  dns_names   TEXT,
  problem     TEXT,
  last_stage  INTEGER NOT NULL DEFAULT 0
);
`

// certStages are the thresholds an alert fires at, in days. Crossing one
// sends a message and records it, so a certificate left alone for a month
// produces four alerts rather than thirty.
var certStages = []int{30, 14, 7, 1}

type CertState struct {
	TargetID  int64    `json:"target_id"`
	CheckedAt int64    `json:"checked_at"`
	NotAfter  int64    `json:"not_after,omitempty"`
	NotBefore int64    `json:"not_before,omitempty"`
	Issuer    string   `json:"issuer,omitempty"`
	Subject   string   `json:"subject,omitempty"`
	DNSNames  []string `json:"dns_names,omitempty"`
	// Problem is empty when the certificate is usable. Otherwise it says
	// what is wrong in a sentence an operator can act on.
	Problem  string `json:"problem,omitempty"`
	DaysLeft int    `json:"days_left"`
}

type CertConfig struct {
	// WarnDays : premier seuil d'alerte, en jours. 0 = 30.
	WarnDays int `json:"warn_days"`
	// Recipients : adresses supplementaires, en plus des canaux de
	// notification. Utile quand le certificat concerne une equipe qui
	// n'est pas celle qui recoit les incidents reseau.
	Recipients string `json:"recipients"`
	Enabled    bool   `json:"enabled"`
}

func defaultCertConfig() CertConfig {
	return CertConfig{Enabled: true, WarnDays: 30}
}

func (s *Store) CertConfig() CertConfig {
	c := defaultCertConfig()
	if raw := s.Setting("certs", ""); raw != "" {
		var v CertConfig
		if json.Unmarshal([]byte(raw), &v) == nil {
			c = v
			if c.WarnDays <= 0 {
				c.WarnDays = 30
			}
		}
	}
	return c
}

func (s *Store) SetCertConfig(c CertConfig) error {
	if c.WarnDays < 1 || c.WarnDays > 365 {
		return fmt.Errorf("warn_days must be between 1 and 365")
	}
	b, _ := json.Marshal(c)
	return s.SetSetting("certs", string(b))
}

// inspectCert opens one TLS connection and reports what the certificate
// says. The verification is the standard one — chain and name — because a
// monitoring tool that accepts a certificate a browser would refuse is
// measuring something nobody experiences.
func inspectCert(host string, port int, family int, timeout time.Duration) CertState {
	var st CertState
	st.CheckedAt = time.Now().Unix()
	name := hostOnly(host)
	network := "tcp"
	if family == 4 || family == 6 {
		network = fmt.Sprintf("tcp%d", family)
	}
	addr := net.JoinHostPort(name, strconv.Itoa(port))
	d := &net.Dialer{Timeout: timeout}

	// First pass: full verification, which is what a client would do.
	conn, err := tls.DialWithDialer(d, network, addr, &tls.Config{ServerName: name})
	if err == nil {
		defer conn.Close()
		fill(&st, conn.ConnectionState().PeerCertificates)
		return st
	}
	verifyErr := err

	// Second pass without verification, only to say what the certificate
	// is. Knowing that the name is wrong, or that it expired last week, is
	// more useful than "handshake failed".
	conn2, err2 := tls.DialWithDialer(d, network, addr,
		&tls.Config{ServerName: name, InsecureSkipVerify: true})
	if err2 != nil {
		st.Problem = oneLine(fmt.Sprintf("no TLS handshake with %s: %v", addr, err2), 200)
		return st
	}
	defer conn2.Close()
	fill(&st, conn2.ConnectionState().PeerCertificates)
	st.Problem = certProblem(verifyErr, name)
	return st
}

// errorsAs is errors.As with the generic noise kept out of the call sites.
func errorsAs(err error, target any) bool { return errors.As(err, target) }

// certProblem turns a verification failure into a sentence that names the
// cause, because "x509: certificate signed by unknown authority" tells an
// operator nothing about which of his three possible mistakes it is.
func certProblem(err error, name string) string {
	var hostErr x509.HostnameError
	var authErr x509.UnknownAuthorityError
	var invErr x509.CertificateInvalidError
	switch {
	case errorsAs(err, &hostErr):
		return fmt.Sprintf("the certificate is not valid for %s: it names %s",
			name, oneLine(strings.Join(hostErr.Certificate.DNSNames, ", "), 120))
	case errorsAs(err, &invErr) && invErr.Reason == x509.Expired:
		return "the certificate has expired"
	case errorsAs(err, &authErr):
		return "the chain cannot be verified: the certificate is self-signed, or the server " +
			"does not send the intermediate certificate, so a client that does not already " +
			"hold it refuses the connection"
	}
	return oneLine(fmt.Sprintf("the certificate is refused: %v", err), 200)
}

func fill(st *CertState, chain []*x509.Certificate) {
	if len(chain) == 0 {
		st.Problem = "the server presented no certificate"
		return
	}
	leaf := chain[0]
	st.NotAfter, st.NotBefore = leaf.NotAfter.Unix(), leaf.NotBefore.Unix()
	st.Issuer = oneLine(leaf.Issuer.CommonName, 120)
	if st.Issuer == "" && len(leaf.Issuer.Organization) > 0 {
		st.Issuer = oneLine(leaf.Issuer.Organization[0], 120)
	}
	st.Subject = oneLine(leaf.Subject.CommonName, 120)
	for _, n := range leaf.DNSNames {
		if len(st.DNSNames) >= 12 {
			break
		}
		st.DNSNames = append(st.DNSNames, oneLine(n, 120))
	}
	st.DaysLeft = daysLeft(leaf.NotAfter, time.Now())
}

// daysLeft counts whole days, rounding towards the operator's interest: a
// certificate expiring in twelve hours has zero days left, not one.
func daysLeft(notAfter, now time.Time) int {
	// Floor, not truncation: an integer conversion rounds towards zero, so a
	// certificate that expired an hour ago would report zero day left and
	// look merely urgent instead of already broken.
	return int(math.Floor(notAfter.Sub(now).Hours() / 24))
}

func (s *Store) saveCert(id int64, st CertState) {
	names, _ := json.Marshal(st.DNSNames)
	s.cfg.Exec(
		`INSERT INTO certs(target_id,checked_at,not_after,not_before,issuer,subject,
		                   dns_names,problem)
		 VALUES(?,?,?,?,?,?,?,?)
		 ON CONFLICT(target_id) DO UPDATE SET
		   checked_at=excluded.checked_at, not_after=excluded.not_after,
		   not_before=excluded.not_before, issuer=excluded.issuer,
		   subject=excluded.subject, dns_names=excluded.dns_names,
		   problem=excluded.problem`,
		id, st.CheckedAt, st.NotAfter, st.NotBefore, st.Issuer, st.Subject,
		string(names), st.Problem)
}

// Certs returns the last known state of every certificate, by target.
func (s *Store) Certs() map[int64]CertState {
	out := map[int64]CertState{}
	rows, err := s.cfg.Query(
		`SELECT target_id,checked_at,COALESCE(not_after,0),COALESCE(not_before,0),
		        COALESCE(issuer,''),COALESCE(subject,''),COALESCE(dns_names,'[]'),
		        COALESCE(problem,'') FROM certs`)
	if err != nil {
		return out
	}
	defer rows.Close()
	now := time.Now()
	for rows.Next() {
		var st CertState
		var names string
		if rows.Scan(&st.TargetID, &st.CheckedAt, &st.NotAfter, &st.NotBefore,
			&st.Issuer, &st.Subject, &names, &st.Problem) != nil {
			continue
		}
		json.Unmarshal([]byte(names), &st.DNSNames)
		if st.NotAfter > 0 {
			st.DaysLeft = daysLeft(time.Unix(st.NotAfter, 0), now)
		}
		out[st.TargetID] = st
	}
	return out
}

// certTargets are the targets worth inspecting: a TCP check with a port,
// and the operator has not turned it off for that target.
func (s *Store) certTargets() []*Target {
	all, err := s.ActiveTargets()
	if err != nil {
		return nil
	}
	var out []*Target
	for _, t := range all {
		if t.Proto == "tcp" && t.Port > 0 && !t.CertOff && t.DCCheck == "" {
			out = append(out, t)
		}
	}
	return out
}

// CertLoop inspects the certificates once a day, and on start so an
// operator who has just added a target does not wait until tomorrow.
type CertWatcher struct {
	store *Store
	send  func(subject, body string) error
	now   func() time.Time
}

func NewCertWatcher(store *Store, send func(string, string) error) *CertWatcher {
	return &CertWatcher{store: store, send: send, now: time.Now}
}

func (w *CertWatcher) Loop(stop <-chan struct{}) {
	w.store.cfg.Exec(certSchema)
	t := time.NewTicker(12 * time.Hour)
	defer t.Stop()
	// A few seconds after start rather than immediately: the probe is
	// settling and a handshake storm at boot is impolite.
	time.Sleep(20 * time.Second)
	for {
		w.Tick()
		select {
		case <-stop:
			return
		case <-t.C:
		}
	}
}

func (w *CertWatcher) Tick() {
	cfg := w.store.CertConfig()
	if !cfg.Enabled {
		return
	}
	for _, t := range w.store.certTargets() {
		st := inspectCert(t.Host, t.Port, t.Family, time.Duration(t.TimeoutMs)*time.Millisecond)
		st.TargetID = t.ID
		w.store.saveCert(t.ID, st)
		w.consider(cfg, t, st)
	}
}

// consider decides whether this state deserves a message. A problem is
// said once per day at most; an expiry is said once per threshold crossed.
func (w *CertWatcher) consider(cfg CertConfig, t *Target, st CertState) {
	if t.AlertsOff {
		return
	}
	var last int
	w.store.cfg.QueryRow(`SELECT last_stage FROM certs WHERE target_id=?`, t.ID).Scan(&last)

	if st.Problem != "" {
		// A problem that is not an expiry: stage -1 marks "already said".
		if last == -1 {
			return
		}
		w.fire(cfg, t, fmt.Sprintf("[smokestack] %s: certificate problem", t.Title),
			fmt.Sprintf("%s (%s)\n\n%s\n\nChecked at %s UTC.",
				t.Title, hostOnly(t.Host), st.Problem,
				w.now().UTC().Format("2006-01-02 15:04")))
		w.store.cfg.Exec(`UPDATE certs SET last_stage=-1 WHERE target_id=?`, t.ID)
		return
	}
	if st.NotAfter == 0 {
		return
	}
	stage := 0
	for _, s := range certStages {
		if s > cfg.WarnDays {
			continue // never warn earlier than the operator asked
		}
		if st.DaysLeft <= s && (stage == 0 || s < stage) {
			stage = s
		}
	}
	if cfg.WarnDays > 0 && st.DaysLeft <= cfg.WarnDays && stage == 0 {
		stage = cfg.WarnDays
	}
	if stage == 0 {
		// Back to healthy — a renewed certificate clears the record, so the
		// next expiry is announced from the top again.
		if last != 0 {
			w.store.cfg.Exec(`UPDATE certs SET last_stage=0 WHERE target_id=?`, t.ID)
		}
		return
	}
	if last > 0 && stage >= last {
		return // this threshold, or an earlier one, was already said
	}
	when := time.Unix(st.NotAfter, 0).UTC().Format("2006-01-02")
	w.fire(cfg, t, fmt.Sprintf("[smokestack] %s: certificate expires in %d day(s)",
		t.Title, st.DaysLeft),
		fmt.Sprintf("%s (%s)\n\nThe certificate expires on %s UTC, in %d day(s).\n"+
			"Issued by %s for %s.\n\nRenewing it is the whole of the fix; this message "+
			"repeats at %s days before expiry.",
			t.Title, hostOnly(t.Host), when, st.DaysLeft, st.Issuer,
			strings.Join(st.DNSNames, ", "), stageList(cfg.WarnDays)))
	w.store.cfg.Exec(`UPDATE certs SET last_stage=? WHERE target_id=?`, stage, t.ID)
}

func stageList(warn int) string {
	var out []string
	if warn > 0 {
		out = append(out, strconv.Itoa(warn))
	}
	for _, s := range certStages {
		if s < warn {
			out = append(out, strconv.Itoa(s))
		}
	}
	return strings.Join(out, ", ")
}

func (w *CertWatcher) fire(cfg CertConfig, t *Target, subject, body string) {
	if w.send == nil {
		return
	}
	if extra := strings.TrimSpace(cfg.Recipients); extra != "" {
		body += "\n\nAlso sent to: " + oneLine(extra, 200)
	}
	if err := w.send(subject, body); err != nil {
		log.Printf("certificate alert for %s: %v", t.Slug, err)
	}
}

// ------------------------------------------------------------- routes

func (a *API) CertRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/admin/certs", a.auth(a.certsGet))
	mux.HandleFunc("PUT /api/v1/admin/certs", a.auth(a.certsPut))
	mux.HandleFunc("POST /api/v1/admin/certs/check", a.auth(a.certsCheck))
}

// CertRow is one line of the back-office table: the target, and what the
// last inspection found for it. Targets that have never been inspected
// appear too, with an empty state, because "nothing here" and "not looked
// at yet" are two different situations for an operator.
type CertRow struct {
	TargetID int64      `json:"target_id"`
	Title    string     `json:"title"`
	Host     string     `json:"host"`
	Port     int        `json:"port"`
	Family   int        `json:"family"`
	Off      bool       `json:"off"`
	Cert     *CertState `json:"cert,omitempty"`
}

func (a *API) certsGet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"config": a.store.CertConfig(),
		"rows":   a.certRows(),
		"stages": certStages,
	})
}

// certRows lists every TCP target with a port, whether or not the watcher
// covers it, so the per-target switch is visible where its effect is.
func (a *API) certRows() []CertRow {
	all, err := a.store.ActiveTargets()
	if err != nil {
		return nil
	}
	certs := a.store.Certs()
	var out []CertRow
	for _, t := range all {
		if t.Proto != "tcp" || t.Port <= 0 || t.DCCheck != "" {
			continue
		}
		row := CertRow{TargetID: t.ID, Title: t.Title, Host: maskHost(t.Host),
			Port: t.Port, Family: t.Family, Off: t.CertOff}
		if st, ok := certs[t.ID]; ok {
			c := st
			row.Cert = &c
		}
		out = append(out, row)
	}
	sort.Slice(out, func(i, j int) bool {
		// The ones that need attention first: a problem, then the shortest
		// remaining life, then the rest by name.
		ri, rj := rank(out[i]), rank(out[j])
		if ri != rj {
			return ri < rj
		}
		if out[i].Cert != nil && out[j].Cert != nil && out[i].Cert.DaysLeft != out[j].Cert.DaysLeft {
			return out[i].Cert.DaysLeft < out[j].Cert.DaysLeft
		}
		return out[i].Title < out[j].Title
	})
	return out
}

func rank(r CertRow) int {
	switch {
	case r.Off:
		return 3
	case r.Cert == nil:
		return 2
	case r.Cert.Problem != "":
		return 0
	}
	return 1
}

func (a *API) certsPut(w http.ResponseWriter, r *http.Request) {
	var c CertConfig
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := a.store.SetCertConfig(c); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, c)
}

// certsCheck inspects every certificate now, for an operator who has just
// changed something and does not want to wait twelve hours to see it.
func (a *API) certsCheck(w http.ResponseWriter, r *http.Request) {
	if a.certs == nil {
		writeErr(w, 503, "the certificate watcher is not running")
		return
	}
	a.certs.Tick()
	writeJSON(w, map[string]any{"rows": a.certRows()})
}
