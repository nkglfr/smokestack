package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Federated double-check. An instance sees one of its targets fail and
// cannot tell, from one vantage point, whether the target is down or
// whether its own transit is. A paired peer in another AS settles it in
// one pass: if the peer reaches the target and we do not, the fault is
// between us and it; if neither of us reaches it, the target is down.
//
// The design note in docs/design/federated-double-check.md states why this
// is guarded the way it is. The short version: unguarded, the feature is a
// measurement relay — a way to make somebody else's machine probe a third
// party and launder the origin behind their AS number. Three guards, none
// sufficient alone.
//
//  1. Consent granted in advance, per peer, one-directional, revocable.
//     Individual requests then need no human, which is the point: the
//     answer is wanted during the incident, not when a second operator
//     wakes up.
//  2. The address must already be a public target on the requesting
//     instance, verified by reading its public API rather than taken on
//     trust. This is the only guard that constrains what can be probed
//     rather than merely how much.
//  3. Caps on duration, concurrency, daily count and interval, with
//     refusals that name which cap was hit.

const dcSchema = `
CREATE TABLE IF NOT EXISTS fed_dc_grants (
  peer_asn       TEXT PRIMARY KEY,
  granted_at     INTEGER NOT NULL,
  granted_by     TEXT NOT NULL DEFAULT '',
  max_per_day    INTEGER NOT NULL DEFAULT 20,
  max_concurrent INTEGER NOT NULL DEFAULT 3,
  max_minutes    INTEGER NOT NULL DEFAULT 15
);
CREATE TABLE IF NOT EXISTS fed_dc_checks (
  id              TEXT PRIMARY KEY,
  dir             TEXT NOT NULL,
  peer_asn        TEXT NOT NULL,
  host            TEXT NOT NULL,
  proto           TEXT NOT NULL,
  port            INTEGER NOT NULL DEFAULT 0,
  family          INTEGER NOT NULL DEFAULT 0,
  interval_s      INTEGER NOT NULL DEFAULT 60,
  started_at      INTEGER NOT NULL,
  expires_at      INTEGER NOT NULL,
  target_id       INTEGER NOT NULL DEFAULT 0,
  local_target_id INTEGER NOT NULL DEFAULT 0,
  state           TEXT NOT NULL DEFAULT 'running',
  note            TEXT NOT NULL DEFAULT '',
  passes          INTEGER NOT NULL DEFAULT 0,
  down            INTEGER NOT NULL DEFAULT 0,
  med_ms          REAL NOT NULL DEFAULT 0,
  p95_ms          REAL NOT NULL DEFAULT 0,
  reported_at     INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_dc_state ON fed_dc_checks(state, expires_at);
CREATE INDEX IF NOT EXISTS idx_dc_local ON fed_dc_checks(local_target_id, started_at);
`

// dcArchiveCategory holds the temporary targets. They never reach a public
// page — Tree filters them out by column, not by category — but they need
// somewhere to live, and a named category makes them obvious to an
// operator reading the database directly.
const dcCategorySlug = "_double-check"

// Hard ceilings. A grant may lower these, never raise them: an operator
// who grants a peer more than the instance is willing to do has made a
// mistake, and it should be his instance that refuses rather than his
// peer's good manners that protect him.
const (
	dcMaxMinutes    = 60
	dcMaxConcurrent = 10
	dcMaxPerDay     = 50
	dcMinInterval   = 30
)

type DCCaps struct {
	PerDay     int `json:"max_per_day"`
	Concurrent int `json:"max_concurrent"`
	Minutes    int `json:"max_minutes"`
}

func defaultDCCaps() DCCaps { return DCCaps{PerDay: 20, Concurrent: 3, Minutes: 15} }

func (c *DCCaps) clamp() {
	if c.PerDay <= 0 || c.PerDay > dcMaxPerDay {
		c.PerDay = min(dcMaxPerDay, max(1, c.PerDay))
	}
	if c.Concurrent <= 0 || c.Concurrent > dcMaxConcurrent {
		c.Concurrent = min(dcMaxConcurrent, max(1, c.Concurrent))
	}
	if c.Minutes <= 0 || c.Minutes > dcMaxMinutes {
		c.Minutes = min(dcMaxMinutes, max(1, c.Minutes))
	}
}

// DCGrant is the right we have given one peer to ask us for measurements.
// It is ours to withdraw, and withdrawing it takes effect on the next
// request rather than at the end of some window.
type DCGrant struct {
	PeerASN   string `json:"peer_asn"`
	GrantedAt int64  `json:"granted_at"`
	GrantedBy string `json:"granted_by,omitempty"`
	DCCaps
	// Filled for the back-office listing.
	Org        string `json:"org,omitempty"`
	Running    int    `json:"running,omitempty"`
	UsedToday  int    `json:"used_today,omitempty"`
	MutualHint bool   `json:"mutual,omitempty"`
}

// DCCheck is one double-check, in either direction. dir is "in" when we
// measure on a peer's behalf and "out" when we asked a peer to measure on
// ours; the two are kept in one table because an operator wants to see
// both in one place, and because the distinction is exactly one column.
type DCCheck struct {
	ID        string `json:"id"`
	Dir       string `json:"dir"`
	PeerASN   string `json:"peer_asn"`
	Host      string `json:"host"`
	Proto     string `json:"proto"`
	Port      int    `json:"port,omitempty"`
	Family    int    `json:"family,omitempty"`
	IntervalS int    `json:"interval_s"`
	StartedAt int64  `json:"started_at"`
	ExpiresAt int64  `json:"expires_at"`
	// TargetID is our temporary target, for an incoming check.
	TargetID int64 `json:"target_id,omitempty"`
	// LocalTargetID is the target being corroborated, for an outgoing one.
	LocalTargetID int64   `json:"local_target_id,omitempty"`
	State         string  `json:"state"`
	Note          string  `json:"note,omitempty"`
	Passes        int     `json:"passes"`
	Down          int     `json:"down"`
	MedMs         float64 `json:"med_ms"`
	P95Ms         float64 `json:"p95_ms"`
	ReportedAt    int64   `json:"reported_at,omitempty"`
	// Filled for the interface, never stored.
	TargetTitle string `json:"target_title,omitempty"`
	TargetSlug  string `json:"target_slug,omitempty"`
	PeerOrg     string `json:"peer_org,omitempty"`
	Verdict     string `json:"verdict,omitempty"`
}

// Reached reports whether the peer got an answer often enough to call the
// target reachable from there. A single answered pass is not enough — one
// reply during a flap proves little — so the bar is a clear majority.
func (c *DCCheck) Reached() bool {
	return c.Passes > 0 && float64(c.Passes-c.Down)/float64(c.Passes) >= 0.6
}

// Intermittent is the honest third answer: the peer reaches it sometimes.
// Forcing that into "up" or "down" would throw away the finding.
func (c *DCCheck) Intermittent() bool {
	if c.Passes < 3 {
		return false
	}
	r := float64(c.Passes-c.Down) / float64(c.Passes)
	return r > 0.1 && r < 0.6
}

func newDCID() string {
	var b [12]byte
	rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// ------------------------------------------------------------- grants

func (f *Federation) dcGrants() []DCGrant {
	rows, err := f.store.cfg.Query(
		`SELECT peer_asn,granted_at,COALESCE(granted_by,''),
		        max_per_day,max_concurrent,max_minutes FROM fed_dc_grants`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []DCGrant
	for rows.Next() {
		var g DCGrant
		if rows.Scan(&g.PeerASN, &g.GrantedAt, &g.GrantedBy,
			&g.PerDay, &g.Concurrent, &g.Minutes) == nil {
			out = append(out, g)
		}
	}
	return out
}

func (f *Federation) dcGrant(asn string) (*DCGrant, bool) {
	asn, err := normASN(asn)
	if err != nil {
		return nil, false
	}
	var g DCGrant
	err = f.store.cfg.QueryRow(
		`SELECT peer_asn,granted_at,COALESCE(granted_by,''),
		        max_per_day,max_concurrent,max_minutes
		   FROM fed_dc_grants WHERE peer_asn=?`, asn).Scan(
		&g.PeerASN, &g.GrantedAt, &g.GrantedBy, &g.PerDay, &g.Concurrent, &g.Minutes)
	if err != nil {
		return nil, false
	}
	return &g, true
}

// SetDCGrant grants or updates the right for one peer. The peer must
// already be paired and approved: granting a stranger the right to have us
// measure things would be granting it to whoever turns up with that AS
// number, since an AS number is declared and not proved.
func (f *Federation) SetDCGrant(asn string, caps DCCaps, by string) error {
	asn, err := normASN(asn)
	if err != nil {
		return err
	}
	p, err := f.PeerByASN(asn)
	if err != nil || p.State != "trusted" {
		return fmt.Errorf("%s is not an approved peer: pair with it and approve it first", asn)
	}
	caps.clamp()
	_, err = f.store.cfg.Exec(
		`INSERT INTO fed_dc_grants(peer_asn,granted_at,granted_by,max_per_day,max_concurrent,max_minutes)
		 VALUES(?,?,?,?,?,?)
		 ON CONFLICT(peer_asn) DO UPDATE SET
		   max_per_day=excluded.max_per_day, max_concurrent=excluded.max_concurrent,
		   max_minutes=excluded.max_minutes`,
		asn, time.Now().Unix(), oneLine(by, 120), caps.PerDay, caps.Concurrent, caps.Minutes)
	return err
}

// RevokeDCGrant withdraws the right and stops whatever that peer has
// running. Revocation that left running checks in place would be a
// revocation in name only.
func (f *Federation) RevokeDCGrant(asn string) error {
	asn, err := normASN(asn)
	if err != nil {
		return err
	}
	if _, err := f.store.cfg.Exec(`DELETE FROM fed_dc_grants WHERE peer_asn=?`, asn); err != nil {
		return err
	}
	for _, c := range f.dcChecks("in", 0) {
		if c.PeerASN == asn && c.State == "running" {
			f.dcFinish(c, "the grant was withdrawn")
		}
	}
	return nil
}

// ------------------------------------------------------------- checks

func (f *Federation) dcChecks(dir string, since int64) []*DCCheck {
	q := `SELECT id,dir,peer_asn,host,proto,port,family,interval_s,started_at,expires_at,
	             target_id,local_target_id,state,note,passes,down,med_ms,p95_ms,reported_at
	        FROM fed_dc_checks WHERE started_at>=?`
	args := []any{since}
	if dir != "" {
		q += ` AND dir=?`
		args = append(args, dir)
	}
	q += ` ORDER BY started_at DESC LIMIT 500`
	rows, err := f.store.cfg.Query(q, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []*DCCheck
	for rows.Next() {
		c := &DCCheck{}
		if rows.Scan(&c.ID, &c.Dir, &c.PeerASN, &c.Host, &c.Proto, &c.Port, &c.Family,
			&c.IntervalS, &c.StartedAt, &c.ExpiresAt, &c.TargetID, &c.LocalTargetID,
			&c.State, &c.Note, &c.Passes, &c.Down, &c.MedMs, &c.P95Ms, &c.ReportedAt) == nil {
			out = append(out, c)
		}
	}
	return out
}

func (f *Federation) dcCheck(id string) (*DCCheck, bool) {
	for _, c := range f.dcChecks("", 0) {
		if c.ID == id {
			return c, true
		}
	}
	return nil, false
}

func (f *Federation) dcSave(c *DCCheck) error {
	_, err := f.store.cfg.Exec(
		`INSERT INTO fed_dc_checks(id,dir,peer_asn,host,proto,port,family,interval_s,
		            started_at,expires_at,target_id,local_target_id,state,note,
		            passes,down,med_ms,p95_ms,reported_at)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET state=excluded.state, note=excluded.note,
		   passes=excluded.passes, down=excluded.down, med_ms=excluded.med_ms,
		   p95_ms=excluded.p95_ms, reported_at=excluded.reported_at,
		   expires_at=excluded.expires_at, target_id=excluded.target_id`,
		c.ID, c.Dir, c.PeerASN, c.Host, c.Proto, c.Port, c.Family, c.IntervalS,
		c.StartedAt, c.ExpiresAt, c.TargetID, c.LocalTargetID, c.State, c.Note,
		c.Passes, c.Down, c.MedMs, c.P95Ms, c.ReportedAt)
	return err
}

// dcRunning counts what a peer currently has running on us, and what it
// has asked for today. Both caps are checked against this.
func (f *Federation) dcRunning(asn string) (running, today int) {
	day := time.Now().Unix() - 86400
	f.store.cfg.QueryRow(
		`SELECT COUNT(*) FROM fed_dc_checks WHERE dir='in' AND peer_asn=? AND state='running'`,
		asn).Scan(&running)
	f.store.cfg.QueryRow(
		`SELECT COUNT(*) FROM fed_dc_checks WHERE dir='in' AND peer_asn=? AND started_at>=?`,
		asn, day).Scan(&today)
	return
}

// ------------------------------------------------- what may be measured

// dcRequest is what a peer sends us. Everything in it is hostile input:
// the host above all, since we are being asked to connect somewhere on
// somebody else's say-so.
type dcRequest struct {
	Host      string `json:"host"`
	Proto     string `json:"proto"`
	Port      int    `json:"port,omitempty"`
	Family    int    `json:"family,omitempty"`
	IntervalS int    `json:"interval_s"`
	Minutes   int    `json:"minutes"`
	// Slug is the requester's public page for this target. It is not
	// trusted; it only makes the refusal message useful when the
	// verification below fails.
	Slug string `json:"slug,omitempty"`
}

// dcCheckAddress refuses anything that is not a public unicast address.
// A name is resolved and the resolved addresses are checked, not the
// string: a name pointing at 127.0.0.1 would otherwise walk straight
// through. Every answer must pass, not merely the first, because a name
// that resolves to one public and one private address would otherwise be
// a way in.
func dcCheckAddress(host string, family int) error {
	name := hostOnly(host)
	if name == "" {
		return fmt.Errorf("missing host")
	}
	if strings.EqualFold(name, "localhost") ||
		strings.HasSuffix(strings.ToLower(name), ".localhost") {
		return fmt.Errorf("refusing to measure this machine")
	}
	ips := []net.IP{}
	if ip := net.ParseIP(name); ip != nil {
		ips = append(ips, ip)
	} else {
		addrs, err := net.LookupIP(name)
		if err != nil || len(addrs) == 0 {
			return fmt.Errorf("the name %s does not resolve here", oneLine(name, 80))
		}
		ips = addrs
	}
	for _, ip := range ips {
		if family == 4 && ip.To4() == nil {
			continue
		}
		if family == 6 && ip.To4() != nil {
			continue
		}
		if !isPublicIP(ip) || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() ||
			ip.IsLinkLocalMulticast() {
			return fmt.Errorf("refusing to measure a non-public address (%s)", ip)
		}
	}
	return nil
}

// dcVerifyPublicTarget is guard 2, and the one that decides how narrow
// this feature is: the peer may only ask for a second opinion on a target
// it already publishes, under its own name, on its own instance. Read from
// its public API rather than taken on trust.
//
// A private target therefore cannot be double-checked. That is a real
// cost, accepted deliberately — see the design note. Covering private
// targets would need a target list pre-registered between the two peers,
// not a relaxation of this rule.
func (f *Federation) dcVerifyPublicTarget(peer *Peer, req dcRequest) error {
	resp, err := f.client.Get(peer.URL + "/api/v1/tree")
	if err != nil {
		return fmt.Errorf("cannot read the public targets of %s to check the request: %v",
			peer.ASN, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("the public targets of %s answered HTTP %d", peer.ASN, resp.StatusCode)
	}
	var cats []struct {
		Targets []struct {
			Slug   string `json:"slug"`
			Host   string `json:"host"`
			Proto  string `json:"proto"`
			Port   int    `json:"port"`
			Public bool   `json:"public"`
		} `json:"targets"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&cats); err != nil {
		return fmt.Errorf("the public targets of %s are unreadable: %v", peer.ASN, err)
	}
	want := strings.ToLower(hostOnly(req.Host))
	for _, c := range cats {
		for _, t := range c.Targets {
			if strings.ToLower(hostOnly(t.Host)) != want {
				continue
			}
			if !strings.EqualFold(t.Proto, req.Proto) {
				continue
			}
			if req.Proto == "tcp" && t.Port != req.Port {
				continue
			}
			// A target listed by /api/v1/tree to an anonymous caller is
			// public by construction: the endpoint filters the others out.
			return nil
		}
	}
	// The likely cause is an address masked on the peer's public pages,
	// which is the default, so say so rather than only "not found".
	return fmt.Errorf("%s does not publish a target for %s %s: a double-check only covers a "+
		"target the requester publishes itself, and a masked or private target cannot be "+
		"verified this way", peer.ASN, req.Proto, oneLine(req.Host, 80))
}

// dcAccept is the whole decision, in the order that refuses soonest and
// cheapest: consent, then shape, then caps, then the address, then the
// remote verification which costs an HTTP request.
func (f *Federation) dcAccept(peer *Peer, req dcRequest) (*DCCheck, error) {
	g, ok := f.dcGrant(peer.ASN)
	if !ok {
		return nil, fmt.Errorf("%s has not been granted the right to ask this instance for "+
			"measurements; its operator must grant it explicitly", peer.ASN)
	}
	req.Proto = strings.ToLower(strings.TrimSpace(req.Proto))
	if req.Proto != "icmp" && req.Proto != "tcp" {
		return nil, fmt.Errorf("protocol must be icmp or tcp")
	}
	if req.Proto == "tcp" && (req.Port < 1 || req.Port > 65535) {
		return nil, fmt.Errorf("a tcp double-check needs a port between 1 and 65535")
	}
	if req.Proto == "icmp" {
		req.Port = 0
	}
	if req.Family != 0 && req.Family != 4 && req.Family != 6 {
		return nil, fmt.Errorf("address family must be 0, 4 or 6")
	}
	if req.IntervalS < dcMinInterval {
		req.IntervalS = dcMinInterval
	}
	if req.Minutes <= 0 {
		req.Minutes = g.Minutes
	}
	if req.Minutes > g.Minutes {
		return nil, fmt.Errorf("this instance allows %s at most %d minutes per check, %d asked",
			peer.ASN, g.Minutes, req.Minutes)
	}
	running, today := f.dcRunning(peer.ASN)
	if running >= g.Concurrent {
		return nil, fmt.Errorf("%s already has %d checks running here, the limit is %d",
			peer.ASN, running, g.Concurrent)
	}
	if today >= g.PerDay {
		return nil, fmt.Errorf("%s has asked for %d checks in the last twenty-four hours, "+
			"the limit is %d", peer.ASN, today, g.PerDay)
	}
	if err := dcCheckAddress(req.Host, req.Family); err != nil {
		return nil, err
	}
	if err := f.dcVerifyPublicTarget(peer, req); err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	c := &DCCheck{
		ID: newDCID(), Dir: "in", PeerASN: peer.ASN,
		Host: oneLine(req.Host, 200), Proto: req.Proto, Port: req.Port,
		Family: req.Family, IntervalS: req.IntervalS,
		StartedAt: now, ExpiresAt: now + int64(req.Minutes)*60,
		State: "running",
	}
	id, err := f.dcMakeTarget(c)
	if err != nil {
		return nil, err
	}
	c.TargetID = id
	if err := f.dcSave(c); err != nil {
		f.store.dcDropTarget(id)
		return nil, err
	}
	f.store.refreshDCTargets()
	return c, nil
}

// dcMakeTarget creates the temporary target the probe will measure. It is
// private, alerts are off, and it is excluded from the tree, so it reaches
// no public page and no availability figure of ours. Its measurements are
// diverted before they reach the cascade, which is what keeps this from
// being somebody else's data in our history.
func (f *Federation) dcMakeTarget(c *DCCheck) (int64, error) {
	cat, err := f.store.dcCategory()
	if err != nil {
		return 0, err
	}
	t := &Target{
		CategoryID: cat,
		Slug:       "dc-" + c.ID,
		Title:      fmt.Sprintf("double-check for %s: %s", c.PeerASN, oneLine(c.Host, 80)),
		Host:       c.Host, Proto: c.Proto, Port: c.Port, Family: c.Family,
		IntervalS: int64(c.IntervalS), Packets: 5, SpacingMs: 200, TimeoutMs: 2000,
		Public: false, Enabled: true, AlertsOff: true, HideHost: true,
		DCCheck: c.ID,
	}
	return f.store.CreateTarget(t)
}

// dcDropTarget deletes a temporary target outright, with everything it
// produced. It does not go through PurgeTarget, which insists a target be
// archived first: archiving exists so an operator does not lose history he
// might want, and there is no history here to want — the measurements were
// made for somebody else and are not ours to keep.
func (s *Store) dcDropTarget(id int64) {
	if id <= 0 {
		return
	}
	for _, q := range []string{
		`DELETE FROM fed_dc_samples WHERE target_id=?`,
		`DELETE FROM samples WHERE target_id=?`,
		`DELETE FROM live WHERE target_id=?`,
		`DELETE FROM target_errors WHERE target_id=?`,
		`DELETE FROM target_addresses WHERE target_id=?`,
		`DELETE FROM traceroutes WHERE target_id=?`,
	} {
		s.mxw.Exec(q, id)
	}
	s.cfg.Exec(`DELETE FROM targets WHERE id=?`, id)
	s.notifyTargets()
}

func (s *Store) dcCategory() (int64, error) {
	var id int64
	if err := s.cfg.QueryRow(`SELECT id FROM categories WHERE slug=?`,
		dcCategorySlug).Scan(&id); err == nil && id > 0 {
		return id, nil
	}
	return s.CreateCategory(dcCategorySlug, "Double-check (temporaire)",
		"Double-check (temporary)", false)
}

// ------------------------------------------------- diverting the samples

// dcTargets is the set of targets whose measurements must not reach the
// cascade, kept in memory because the write path is on metrics.db and this
// list lives in config.db. Refreshed whenever a check starts or ends, and
// on every turn of the loop, so a restart cannot leave it stale.
type dcSet struct {
	mu sync.RWMutex
	m  map[int64]string
}

func (d *dcSet) get(id int64) (string, bool) {
	d.mu.RLock()
	defer d.mu.RUnlock()
	cid, ok := d.m[id]
	return cid, ok
}

func (d *dcSet) set(m map[int64]string) {
	d.mu.Lock()
	d.m = m
	d.mu.Unlock()
}

func (d *dcSet) len() int {
	d.mu.RLock()
	defer d.mu.RUnlock()
	return len(d.m)
}

func (s *Store) refreshDCTargets() {
	m := map[int64]string{}
	rows, err := s.cfg.Query(
		`SELECT target_id,id FROM fed_dc_checks WHERE dir='in' AND state='running' AND target_id>0`)
	if err == nil {
		for rows.Next() {
			var id int64
			var cid string
			if rows.Scan(&id, &cid) == nil {
				m[id] = cid
			}
		}
		rows.Close()
	}
	s.dc.set(m)
}

// dcReport summarises what our temporary target measured. Computed from
// the diverted samples, never from the cascade, since nothing ever gets
// there.
func (f *Federation) dcReport(c *DCCheck) DCCheck {
	out := *c
	rows, err := f.store.mx.Query(
		`SELECT sent,lost,med_us FROM fed_dc_samples WHERE target_id=? ORDER BY ts`,
		c.TargetID)
	if err != nil {
		return out
	}
	defer rows.Close()
	var meds []float64
	out.Passes, out.Down = 0, 0
	for rows.Next() {
		var sent, lost int
		var med float64
		if rows.Scan(&sent, &lost, &med) != nil {
			continue
		}
		out.Passes++
		if sent > 0 && lost >= sent {
			out.Down++
			continue
		}
		if med > 0 {
			meds = append(meds, med/1000)
		}
	}
	out.MedMs, out.P95Ms = quantile(meds, 0.5), quantile(meds, 0.95)
	return out
}

func quantile(v []float64, q float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := make([]float64, len(v))
	copy(s, v)
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
	i := int(q * float64(len(s)-1))
	return s[i]
}

// dcFinish ends an incoming check: one last report to the peer, then the
// temporary target and every sample it produced are deleted. Nothing of
// somebody else's measurement is kept here after the window closes.
func (f *Federation) dcFinish(c *DCCheck, why string) {
	final := f.dcReport(c)
	final.State, final.Note = "done", oneLine(why, 200)
	final.ReportedAt = time.Now().Unix()
	if p, err := f.PeerByASN(c.PeerASN); err == nil {
		f.dcPush(p, final)
	}
	f.store.dcDropTarget(c.TargetID)
	final.TargetID = 0
	f.dcSave(&final)
	f.store.refreshDCTargets()
}

// dcPush sends the report to the peer that asked. Interim reports matter:
// a fifteen-minute window whose answer arrives at the end would be an
// answer after the incident, which is no answer at all.
func (f *Federation) dcPush(peer *Peer, c DCCheck) error {
	return f.postSigned(peer, "/api/v1/fed/dc/report", map[string]any{
		"id": c.ID, "passes": c.Passes, "down": c.Down,
		"med_ms": c.MedMs, "p95_ms": c.P95Ms,
		"state": c.State, "note": c.Note, "expires_at": c.ExpiresAt,
	})
}

// postSignedJSON is postSigned with the reply decoded. The request flow
// needs the answer — a check id, an expiry, or the reason for a refusal —
// where the report flow only needs to know it arrived.
func (f *Federation) postSignedJSON(peer *Peer, path string, payload, out any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", peer.URL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	f.sign(req, peer.ASN, path, body)
	resp, err := f.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<18))
	if out != nil {
		// A refusal carries its reason in the same shape as a success, so
		// the body is decoded whatever the status: the reason is the most
		// useful thing about a refusal.
		json.Unmarshal(raw, out)
	}
	if resp.StatusCode >= 300 {
		var e struct {
			Error string `json:"error"`
		}
		json.Unmarshal(raw, &e)
		if e.Error != "" {
			return fmt.Errorf("%s refused: %s", peer.ASN, oneLine(e.Error, 300))
		}
		return fmt.Errorf("%s answered HTTP %d", peer.ASN, resp.StatusCode)
	}
	return nil
}

// ------------------------------------------------------ asking a peer

// RequestDoubleCheck asks one peer for a second opinion on one of our
// targets. The target must be public here, which is the mirror of the
// guard the peer will apply: we cannot ask for something we do not
// publish, and finding that out locally gives a better message than the
// peer's refusal would.
func (f *Federation) RequestDoubleCheck(targetID int64, asn string, minutes int) (*DCCheck, error) {
	t, err := f.store.TargetByID(targetID)
	if err != nil {
		return nil, fmt.Errorf("unknown target")
	}
	if !t.Public {
		return nil, fmt.Errorf("%s is a private target: a double-check only covers a target "+
			"this instance publishes, because that is the rule the peer checks on its side", t.Title)
	}
	if t.DCCheck != "" {
		return nil, fmt.Errorf("this target is itself a double-check")
	}
	norm, err := normASN(asn)
	if err != nil {
		return nil, err
	}
	peer, err := f.PeerByASN(norm)
	if err != nil || peer.State != "trusted" {
		return nil, fmt.Errorf("%s is not an approved peer", asn)
	}
	if minutes <= 0 {
		minutes = 15
	}
	if minutes > dcMaxMinutes {
		minutes = dcMaxMinutes
	}
	req := dcRequest{Host: t.Host, Proto: t.Proto, Port: t.Port, Family: t.Family,
		IntervalS: int(t.IntervalS), Minutes: minutes, Slug: t.Slug}
	if req.IntervalS < dcMinInterval {
		req.IntervalS = dcMinInterval
	}
	var reply struct {
		ID        string `json:"id"`
		ExpiresAt int64  `json:"expires_at"`
		Error     string `json:"error"`
	}
	if err := f.postSignedJSON(peer, "/api/v1/fed/dc/request", req, &reply); err != nil {
		return nil, err
	}
	if reply.Error != "" {
		return nil, fmt.Errorf("%s refused: %s", peer.ASN, oneLine(reply.Error, 300))
	}
	if reply.ID == "" {
		return nil, fmt.Errorf("%s answered without a check id", peer.ASN)
	}
	now := time.Now().Unix()
	c := &DCCheck{ID: reply.ID, Dir: "out", PeerASN: peer.ASN,
		Host: t.Host, Proto: t.Proto, Port: t.Port, Family: t.Family,
		IntervalS: req.IntervalS, StartedAt: now, ExpiresAt: reply.ExpiresAt,
		LocalTargetID: targetID, State: "running"}
	if err := f.dcSave(c); err != nil {
		return nil, err
	}
	return c, nil
}

// dcAcceptReport takes a peer's report on a check we asked for. The check
// must be ours, outgoing, and belong to that peer: a peer cannot report on
// another peer's check, and cannot invent one.
func (f *Federation) dcAcceptReport(peer *Peer, in DCCheck) error {
	c, ok := f.dcCheck(in.ID)
	if !ok {
		return fmt.Errorf("unknown check")
	}
	if c.Dir != "out" || c.PeerASN != peer.ASN {
		return fmt.Errorf("this check is not yours to report on")
	}
	if in.Passes < 0 || in.Down < 0 || in.Down > in.Passes {
		return fmt.Errorf("implausible counts")
	}
	c.Passes, c.Down = in.Passes, in.Down
	c.MedMs = clampFloat(in.MedMs, 0, 600000)
	c.P95Ms = clampFloat(in.P95Ms, 0, 600000)
	c.ReportedAt = time.Now().Unix()
	if in.State == "done" {
		c.State = "done"
		c.Note = oneLine(in.Note, 200)
	}
	return f.dcSave(c)
}

// dcVerdict reads a finished or in-flight check against what we see
// ourselves. Three answers, and the third one is kept rather than forced
// into one of the first two.
func (f *Federation) dcVerdict(c *DCCheck, weSeeItDown bool) string {
	if c.Passes == 0 {
		return "pending"
	}
	switch {
	case c.Intermittent():
		return "intermittent"
	case c.Reached() && weSeeItDown:
		return "path"
	case c.Reached():
		return "both_ok"
	default:
		return "target"
	}
}

// ------------------------------------------------------------- the loop

// dcLoop advances every running check: an interim report to the peer so
// the answer arrives during the incident rather than after it, and the
// close-out of anything past its window.
func (f *Federation) dcLoop(stop <-chan struct{}) {
	f.store.cfg.Exec(dcSchema)
	f.store.refreshDCTargets()
	t := time.NewTicker(45 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			f.dcTick(time.Now().Unix())
		}
	}
}

func (f *Federation) dcTick(now int64) {
	f.store.refreshDCTargets()
	for _, c := range f.dcChecks("in", now-2*86400) {
		if c.State != "running" {
			continue
		}
		if now >= c.ExpiresAt {
			f.dcFinish(c, "the window closed")
			continue
		}
		// An interim report, so a fifteen-minute window does not answer
		// only at minute fifteen.
		rep := f.dcReport(c)
		if rep.Passes == 0 {
			continue
		}
		if p, err := f.PeerByASN(c.PeerASN); err == nil {
			f.dcPush(p, rep)
		}
		rep.ReportedAt = now
		f.dcSave(&rep)
	}
	// An outgoing check whose window has passed without a final report is
	// closed here, so it does not sit as "running" for ever.
	for _, c := range f.dcChecks("out", now-2*86400) {
		if c.State == "running" && c.ExpiresAt > 0 && now > c.ExpiresAt+300 {
			c.State = "done"
			if c.Passes == 0 {
				c.Note = "the peer sent no report before the window closed"
			}
			f.dcSave(c)
		}
	}
}

// ------------------------------------------------------------------ API

func (a *API) DoubleCheckRoutes(mux *http.ServeMux) {
	if a.fed == nil {
		return
	}
	// Signed by an approved peer. The grant is checked inside, not here:
	// an unsigned request must not learn whether a grant exists.
	mux.HandleFunc("POST /api/v1/fed/dc/request", a.dcRequestIn)
	mux.HandleFunc("POST /api/v1/fed/dc/report", a.dcReportIn)

	// Public, and empty unless the operator turned publishing on.
	mux.HandleFunc("GET /api/v1/corroboration", a.corroborationGet)

	mux.HandleFunc("GET /api/v1/admin/fed/dc", a.auth(a.dcAdminGet))
	mux.HandleFunc("PUT /api/v1/admin/fed/dc/grants/{asn}", a.need(RoleAdmin, a.dcGrantPut))
	mux.HandleFunc("DELETE /api/v1/admin/fed/dc/grants/{asn}", a.auth(a.dcGrantDelete))
	mux.HandleFunc("POST /api/v1/admin/fed/dc/ask", a.auth(a.dcAsk))
	mux.HandleFunc("PUT /api/v1/admin/fed/dc/settings", a.auth(a.dcSettingsPut))
}

// dcRequestIn is a peer asking us to measure. Every refusal names its
// reason: a peer that is refused should be able to fix it, or know that it
// cannot.
func (a *API) dcRequestIn(w http.ResponseWriter, r *http.Request) {
	peer, body, err := a.fed.verify(r)
	if err != nil {
		writeErr(w, 401, err.Error())
		return
	}
	var req dcRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	c, err := a.fed.dcAccept(peer, req)
	if err != nil {
		writeErr(w, 403, err.Error())
		return
	}
	writeJSON(w, map[string]any{"id": c.ID, "expires_at": c.ExpiresAt,
		"interval_s": c.IntervalS})
}

func (a *API) dcReportIn(w http.ResponseWriter, r *http.Request) {
	peer, body, err := a.fed.verify(r)
	if err != nil {
		writeErr(w, 401, err.Error())
		return
	}
	var in DCCheck
	if err := json.Unmarshal(body, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := a.fed.dcAcceptReport(peer, in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

// dcAdminGet is the whole page in one call: who we allow, what is running
// in each direction, and which peers could be asked.
func (a *API) dcAdminGet(w http.ResponseWriter, r *http.Request) {
	peers, _ := a.fed.Peers()
	byASN := map[string]*Peer{}
	for _, p := range peers {
		byASN[p.ASN] = p
	}
	grants := a.fed.dcGrants()
	for i := range grants {
		if p, ok := byASN[grants[i].PeerASN]; ok {
			grants[i].Org = p.Org
		}
		grants[i].Running, grants[i].UsedToday = a.fed.dcRunning(grants[i].PeerASN)
	}
	since := time.Now().Unix() - 7*86400
	fill := func(list []*DCCheck) []*DCCheck {
		for _, c := range list {
			if p, ok := byASN[c.PeerASN]; ok {
				c.PeerOrg = p.Org
			}
			if c.LocalTargetID > 0 {
				if t, err := a.store.TargetByID(c.LocalTargetID); err == nil {
					c.TargetTitle, c.TargetSlug = t.Title, t.Slug
				}
			}
			if c.Dir == "out" {
				c.Verdict = a.fed.dcVerdict(c, a.fed.weSeeDown(c.LocalTargetID))
			}
			// An incoming check names the address we were asked to reach.
			// It is the requester's target, published by the requester, so
			// showing it to our own operator is right — he is the one
			// answering for what his instance measures.
			c.Host = oneLine(c.Host, 120)
		}
		return list
	}
	var trusted []map[string]string
	for _, p := range peers {
		if p.State == "trusted" {
			trusted = append(trusted, map[string]string{"asn": p.ASN, "org": p.Org})
		}
	}
	writeJSON(w, map[string]any{
		"grants":   grants,
		"incoming": fill(a.fed.dcChecks("in", since)),
		"outgoing": fill(a.fed.dcChecks("out", since)),
		"peers":    trusted,
		"settings": a.fed.dcSettings(),
		"caps": map[string]int{"max_minutes": dcMaxMinutes,
			"max_concurrent": dcMaxConcurrent, "max_per_day": dcMaxPerDay},
	})
}

func (a *API) dcGrantPut(w http.ResponseWriter, r *http.Request, u *User) {
	var caps DCCaps
	json.NewDecoder(io.LimitReader(r.Body, 1<<14)).Decode(&caps)
	by := ""
	if u != nil {
		by = u.Email
	}
	if err := a.fed.SetDCGrant(r.PathValue("asn"), caps, by); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (a *API) dcGrantDelete(w http.ResponseWriter, r *http.Request) {
	if err := a.fed.RevokeDCGrant(r.PathValue("asn")); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	w.WriteHeader(204)
}

func (a *API) dcAsk(w http.ResponseWriter, r *http.Request) {
	var in struct {
		TargetID int64  `json:"target_id"`
		ASN      string `json:"asn"`
		Minutes  int    `json:"minutes"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<14)).Decode(&in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	c, err := a.fed.RequestDoubleCheck(in.TargetID, in.ASN, in.Minutes)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, c)
}

func (a *API) dcSettingsPut(w http.ResponseWriter, r *http.Request) {
	var s DCSettings
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<14)).Decode(&s); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := a.fed.setDCSettings(s); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, s)
}

// --------------------------------------------------------- our settings

// DCSettings is what this instance does with the feature, as opposed to
// what it allows peers to do, which is the grants.
type DCSettings struct {
	// Auto asks granted peers by itself when one of our targets opens an
	// incident. On by default, and that is deliberate: a request is
	// refused unless a peer granted it in advance, so this switch cannot
	// cause anything a peer has not consented to. It decides whether the
	// answer arrives during the incident or only if somebody thinks to
	// click.
	Auto bool `json:"auto"`
	// Public shows the corroboration on the target's public page. Off by
	// default: the figure is useful to a reader, but it publishes a fact
	// about a third party's reachability as measured by an operator who
	// never agreed to have it published, so it is the local operator's
	// deliberate choice rather than a default.
	Public bool `json:"public"`
	// AskPeers caps how many peers one incident asks at once.
	AskPeers int `json:"ask_peers"`
	Minutes  int `json:"minutes"`
}

func defaultDCSettings() DCSettings {
	return DCSettings{Auto: true, Public: false, AskPeers: 2, Minutes: 15}
}

func (f *Federation) dcSettings() DCSettings {
	s := defaultDCSettings()
	if raw := f.store.Setting("fed_dc", ""); raw != "" {
		var v DCSettings
		if json.Unmarshal([]byte(raw), &v) == nil {
			s = v
		}
	}
	if s.AskPeers < 1 || s.AskPeers > 5 {
		s.AskPeers = 2
	}
	if s.Minutes < 1 || s.Minutes > dcMaxMinutes {
		s.Minutes = 15
	}
	return s
}

func (f *Federation) setDCSettings(s DCSettings) error {
	if s.AskPeers < 1 || s.AskPeers > 5 {
		return fmt.Errorf("ask between one and five peers per incident")
	}
	if s.Minutes < 1 || s.Minutes > dcMaxMinutes {
		return fmt.Errorf("a check lasts between one and %d minutes", dcMaxMinutes)
	}
	b, _ := json.Marshal(s)
	return f.store.SetSetting("fed_dc", string(b))
}

// weSeeDown reports whether we currently consider our own target to be in
// incident. It is what turns a peer's report into a verdict: the same
// report means opposite things depending on what we see.
func (f *Federation) weSeeDown(targetID int64) bool {
	if targetID <= 0 {
		return false
	}
	var open int
	f.store.cfg.QueryRow(
		`SELECT COUNT(*) FROM local_incidents WHERE target_id=? AND closed_at IS NULL`,
		targetID).Scan(&open)
	return open > 0
}

// AutoDoubleCheck asks granted peers about a target that has just opened
// an incident. Called from the alerting path, where the need arises, and
// silent about peers that have not granted anything — a refusal there is
// the normal case, not an error worth logging on every incident.
func (f *Federation) AutoDoubleCheck(targetID int64) {
	if f == nil || !f.enabled {
		return
	}
	s := f.dcSettings()
	if !s.Auto {
		return
	}
	// Do not ask twice about the same target in the same hour: an incident
	// that flaps must not spend a peer's daily allowance.
	var recent int
	f.store.cfg.QueryRow(
		`SELECT COUNT(*) FROM fed_dc_checks WHERE dir='out' AND local_target_id=? AND started_at>?`,
		targetID, time.Now().Unix()-3600).Scan(&recent)
	if recent > 0 {
		return
	}
	asked := 0
	for _, g := range f.dcGrants() {
		if asked >= s.AskPeers {
			return
		}
		if _, err := f.RequestDoubleCheck(targetID, g.PeerASN, s.Minutes); err == nil {
			asked++
		}
	}
}

// PublicCorroboration is what a visitor may see, when the operator has
// turned that on: the latest finished check for a target, without the
// requesting or measuring detail beyond the AS that answered.
func (f *Federation) PublicCorroboration(targetID int64) *DCCheck {
	if f == nil || !f.enabled || !f.dcSettings().Public {
		return nil
	}
	for _, c := range f.dcChecks("out", time.Now().Unix()-24*3600) {
		if c.LocalTargetID != targetID || c.Passes == 0 {
			continue
		}
		out := &DCCheck{ID: "", PeerASN: c.PeerASN, Passes: c.Passes, Down: c.Down,
			MedMs: c.MedMs, StartedAt: c.StartedAt, State: c.State,
			Verdict: f.dcVerdict(c, f.weSeeDown(targetID))}
		return out
	}
	return nil
}

// corroborationGet serves the public corroboration line. It answers
// nothing at all unless the operator turned publishing on, and it applies
// the same visibility rule as the series: a private target tells a visitor
// nothing, including what a peer saw of it.
func (a *API) corroborationGet(w http.ResponseWriter, r *http.Request) {
	targetID, err := strconv.ParseInt(r.URL.Query().Get("target"), 10, 64)
	if err != nil {
		writeErr(w, 400, "missing target parameter")
		return
	}
	shared, hasShare := a.shareGrant(r)
	t, err := a.store.TargetByID(targetID)
	if err != nil || (!t.Public && !a.authenticated(r) && !(hasShare && shared == targetID)) {
		writeErr(w, 404, "target not found")
		return
	}
	if a.fed == nil {
		writeJSON(w, map[string]any{})
		return
	}
	c := a.fed.PublicCorroboration(targetID)
	if c == nil {
		writeJSON(w, map[string]any{})
		return
	}
	writeJSON(w, map[string]any{
		"peer_asn": c.PeerASN, "verdict": c.Verdict,
		"passes": c.Passes, "answered": c.Passes - c.Down,
		"med_ms": c.MedMs, "at": c.StartedAt,
	})
}
