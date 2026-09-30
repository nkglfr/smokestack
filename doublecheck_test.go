package main

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// dcPeers stands up two federations that trust each other, plus a stub for
// the requester's public API, which is what the measuring side reads to
// check that the address is a target the requester publishes.
func dcPeers(t *testing.T) (measurer *Federation, requesterKey ed25519.PrivateKey,
	tree *[]dcTreeCat, srv *httptest.Server) {
	t.Helper()
	cats := []dcTreeCat{}
	srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/tree" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(cats)
	}))
	t.Cleanup(srv.Close)

	measurer = newFed(t, "AS64500", "Measurer", "https://measurer.example.net")
	pub, priv, _ := ed25519.GenerateKey(nil)
	measurer.store.cfg.Exec(
		`INSERT INTO fed_peers(asn,org,url,pubkey,fingerprint,anchors,state,created_at)
		 VALUES('AS64501','Requester',?,?,?,'[]','trusted',?)`,
		srv.URL, base64.StdEncoding.EncodeToString(pub), fingerprintOf(pub),
		time.Now().Unix())
	measurer.store.cfg.Exec(dcSchema)
	// The stub runs on the loopback, which the safety layer refuses by
	// design; the tests need real instances there.
	fedAllowPrivateForTests = true
	t.Cleanup(func() { fedAllowPrivateForTests = false })
	return measurer, priv, &cats, srv
}

type dcTreeTarget struct {
	Slug   string `json:"slug"`
	Host   string `json:"host"`
	Proto  string `json:"proto"`
	Port   int    `json:"port"`
	Public bool   `json:"public"`
}

type dcTreeCat struct {
	Targets []dcTreeTarget `json:"targets"`
}

func dcAsk(t *testing.T, f *Federation, priv ed25519.PrivateKey, req dcRequest) (int, string) {
	t.Helper()
	api := &API{store: f.store, fed: f, probeID: 1}
	r := signedReq(priv, "AS64501", "AS64500", "POST", "/api/v1/fed/dc/request", req)
	rec := httptest.NewRecorder()
	api.dcRequestIn(rec, r)
	return rec.Code, rec.Body.String()
}

// Guard 1. Without a grant there is no measuring, whatever the signature
// proves: pairing is agreeing to exchange anchor measurements, not
// agreeing to probe on somebody's behalf.
func TestDCRefusedWithoutAGrant(t *testing.T) {
	f, priv, tree, _ := dcPeers(t)
	*tree = []dcTreeCat{{Targets: []dcTreeTarget{
		{Slug: "svc", Host: "198.51.100.10", Proto: "icmp", Public: true}}}}
	code, body := dcAsk(t, f, priv, dcRequest{Host: "198.51.100.10", Proto: "icmp",
		IntervalS: 60, Minutes: 15})
	if code != 403 {
		t.Fatalf("a request without a grant must be refused, got HTTP %d: %s", code, body)
	}
	if !strings.Contains(body, "granted") {
		t.Errorf("the refusal should name the missing grant: %s", body)
	}
	// And nothing was created on the way to refusing.
	if n := len(f.dcChecks("in", 0)); n != 0 {
		t.Errorf("a refused request must leave no check behind, found %d", n)
	}
}

// Guard 1, the other half. A grant is revocable, and revoking it stops
// what is running: a revocation that left running checks alone would be a
// revocation in name only.
func TestDCRevokeStopsRunningChecks(t *testing.T) {
	f, priv, tree, _ := dcPeers(t)
	*tree = []dcTreeCat{{Targets: []dcTreeTarget{
		{Slug: "svc", Host: "198.51.100.10", Proto: "icmp", Public: true}}}}
	if err := f.SetDCGrant("AS64501", defaultDCCaps(), "ops@example.net"); err != nil {
		t.Fatal(err)
	}
	if code, body := dcAsk(t, f, priv, dcRequest{Host: "198.51.100.10", Proto: "icmp",
		IntervalS: 60, Minutes: 15}); code != 200 {
		t.Fatalf("a granted request should be accepted, got HTTP %d: %s", code, body)
	}
	checks := f.dcChecks("in", 0)
	if len(checks) != 1 || checks[0].State != "running" {
		t.Fatalf("one running check expected: %+v", checks)
	}
	tid := checks[0].TargetID
	if tid == 0 {
		t.Fatal("a running check needs its temporary target")
	}
	if _, err := f.store.TargetByID(tid); err != nil {
		t.Fatalf("the temporary target should exist: %v", err)
	}
	if err := f.RevokeDCGrant("AS64501"); err != nil {
		t.Fatal(err)
	}
	after := f.dcChecks("in", 0)
	if len(after) != 1 || after[0].State != "done" {
		t.Errorf("revoking must close the running check: %+v", after)
	}
	if _, err := f.store.TargetByID(tid); err == nil {
		t.Error("revoking must delete the temporary target")
	}
	if _, ok := f.store.dc.get(tid); ok {
		t.Error("the diverted-target set must forget it")
	}
}

// Guard 2, the one that decides how narrow the feature is. The address
// must already be a public target on the requesting instance, read from
// its own public API rather than taken on trust.
func TestDCRefusesATargetTheRequesterDoesNotPublish(t *testing.T) {
	f, priv, tree, _ := dcPeers(t)
	if err := f.SetDCGrant("AS64501", defaultDCCaps(), "ops"); err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		tree []dcTreeCat
		req  dcRequest
	}{
		{"publishes nothing", nil,
			dcRequest{Host: "198.51.100.10", Proto: "icmp", IntervalS: 60, Minutes: 5}},
		{"publishes another host",
			[]dcTreeCat{{Targets: []dcTreeTarget{{Host: "203.0.113.7", Proto: "icmp", Public: true}}}},
			dcRequest{Host: "198.51.100.10", Proto: "icmp", IntervalS: 60, Minutes: 5}},
		{"publishes another protocol",
			[]dcTreeCat{{Targets: []dcTreeTarget{{Host: "198.51.100.10", Proto: "icmp", Public: true}}}},
			dcRequest{Host: "198.51.100.10", Proto: "tcp", Port: 443, IntervalS: 60, Minutes: 5}},
		{"publishes another port",
			[]dcTreeCat{{Targets: []dcTreeTarget{{Host: "198.51.100.10", Proto: "tcp", Port: 80, Public: true}}}},
			dcRequest{Host: "198.51.100.10", Proto: "tcp", Port: 443, IntervalS: 60, Minutes: 5}},
	}
	for _, c := range cases {
		*tree = c.tree
		code, body := dcAsk(t, f, priv, c.req)
		if code == 200 {
			t.Errorf("%s: the request should have been refused", c.name)
			continue
		}
		if !strings.Contains(body, "does not publish") {
			t.Errorf("%s: the refusal should name the reason: %s", c.name, body)
		}
	}
	// And the matching case is accepted, so the guard is not simply
	// refusing everything.
	*tree = []dcTreeCat{{Targets: []dcTreeTarget{
		{Host: "198.51.100.10", Proto: "tcp", Port: 443, Public: true}}}}
	if code, body := dcAsk(t, f, priv, dcRequest{Host: "198.51.100.10", Proto: "tcp",
		Port: 443, IntervalS: 60, Minutes: 5}); code != 200 {
		t.Errorf("a published target should be accepted, got HTTP %d: %s", code, body)
	}
}

// Guard 3. Caps are enforced, and each refusal says which one was hit, so
// a peer can tell a limit from a fault.
func TestDCCaps(t *testing.T) {
	f, priv, tree, _ := dcPeers(t)
	*tree = []dcTreeCat{{Targets: []dcTreeTarget{
		{Host: "198.51.100.10", Proto: "icmp", Public: true}}}}
	if err := f.SetDCGrant("AS64501", DCCaps{PerDay: 2, Concurrent: 1, Minutes: 10}, "ops"); err != nil {
		t.Fatal(err)
	}
	ask := func(minutes int) (int, string) {
		return dcAsk(t, f, priv, dcRequest{Host: "198.51.100.10", Proto: "icmp",
			IntervalS: 60, Minutes: minutes})
	}
	// Longer than the grant allows.
	if code, body := ask(30); code == 200 {
		t.Error("a window longer than the grant must be refused")
	} else if !strings.Contains(body, "minutes") {
		t.Errorf("the refusal should name the duration: %s", body)
	}
	// One is allowed.
	if code, body := ask(10); code != 200 {
		t.Fatalf("the first check should be accepted: HTTP %d %s", code, body)
	}
	// A second one exceeds the concurrency cap.
	if code, body := ask(10); code == 200 {
		t.Error("a second concurrent check must be refused with a cap of one")
	} else if !strings.Contains(body, "running") {
		t.Errorf("the refusal should name the concurrency: %s", body)
	}
	// Close the first, then the daily cap bites on the third.
	for _, c := range f.dcChecks("in", 0) {
		f.dcFinish(c, "test")
	}
	if code, body := ask(10); code != 200 {
		t.Fatalf("a second check after the first closed should pass: HTTP %d %s", code, body)
	}
	for _, c := range f.dcChecks("in", 0) {
		if c.State == "running" {
			f.dcFinish(c, "test")
		}
	}
	if code, body := ask(10); code == 200 {
		t.Error("the third check in a day must be refused with a daily cap of two")
	} else if !strings.Contains(body, "twenty-four hours") {
		t.Errorf("the refusal should name the daily cap: %s", body)
	}
}

// A grant cannot raise the instance's own ceilings: an operator who grants
// more than his instance is willing to do has made a mistake, and it
// should be his instance that refuses rather than his peer's manners.
func TestDCGrantCannotExceedTheCeilings(t *testing.T) {
	f, _, _, _ := dcPeers(t)
	if err := f.SetDCGrant("AS64501", DCCaps{PerDay: 10000, Concurrent: 500,
		Minutes: 9999}, "ops"); err != nil {
		t.Fatal(err)
	}
	g, ok := f.dcGrant("AS64501")
	if !ok {
		t.Fatal("the grant should exist")
	}
	if g.Minutes > dcMaxMinutes || g.Concurrent > dcMaxConcurrent || g.PerDay > dcMaxPerDay {
		t.Errorf("a grant must be clamped to the instance ceilings: %+v", g.DCCaps)
	}
}

// A grant can only name an approved peer. An AS number is declared, never
// proved, so granting a stranger would grant whoever turns up with it.
func TestDCGrantNeedsAnApprovedPeer(t *testing.T) {
	f, _, _, _ := dcPeers(t)
	if err := f.SetDCGrant("AS65550", defaultDCCaps(), "ops"); err == nil {
		t.Error("granting an unknown AS must be refused")
	}
	f.store.cfg.Exec(`UPDATE fed_peers SET state='pending' WHERE asn='AS64501'`)
	if err := f.SetDCGrant("AS64501", defaultDCCaps(), "ops"); err == nil {
		t.Error("granting a peer that is not approved yet must be refused")
	}
}

// The address is hostile input: we are being asked to connect somewhere on
// somebody else's say-so. A name is resolved and the resolved addresses
// are checked, not the string.
func TestDCAddressChecks(t *testing.T) {
	for _, host := range []string{
		"127.0.0.1", "10.0.0.5", "192.168.1.1", "169.254.1.1", "::1",
		"fe80::1", "224.0.0.1", "localhost", "anything.localhost", "",
	} {
		if err := dcCheckAddress(host, 0); err == nil {
			t.Errorf("%q should have been refused", host)
		}
	}
	for _, host := range []string{"198.51.100.10", "203.0.113.1", "2001:db8::1"} {
		if err := dcCheckAddress(host, 0); err != nil {
			// 2001:db8:: is documentation space, which is not special-cased
			// as private, so it must pass the unicast test.
			t.Errorf("%q should have been accepted: %v", host, err)
		}
	}
}

// A peer may report only on a check it was asked for, and only in the
// direction that makes sense: a peer cannot report on another peer's
// check, nor invent one.
func TestDCReportBelongsToItsPeer(t *testing.T) {
	f, _, _, _ := dcPeers(t)
	other, _, _ := ed25519.GenerateKey(nil)
	f.store.cfg.Exec(
		`INSERT INTO fed_peers(asn,org,url,pubkey,fingerprint,anchors,state,created_at)
		 VALUES('AS64777','Other','https://other.example.net',?,?,'[]','trusted',?)`,
		base64.StdEncoding.EncodeToString(other), fingerprintOf(other), time.Now().Unix())

	// A check we asked AS64501 for.
	c := &DCCheck{ID: "abc123", Dir: "out", PeerASN: "AS64501", Host: "198.51.100.10",
		Proto: "icmp", IntervalS: 60, StartedAt: time.Now().Unix(),
		ExpiresAt: time.Now().Unix() + 900, LocalTargetID: 1, State: "running"}
	if err := f.dcSave(c); err != nil {
		t.Fatal(err)
	}
	mine, _ := f.PeerByASN("AS64501")
	stranger, _ := f.PeerByASN("AS64777")

	if err := f.dcAcceptReport(stranger, DCCheck{ID: "abc123", Passes: 10}); err == nil {
		t.Error("another peer must not be able to report on this check")
	}
	if err := f.dcAcceptReport(mine, DCCheck{ID: "does-not-exist", Passes: 10}); err == nil {
		t.Error("a report on an unknown check must be refused")
	}
	if err := f.dcAcceptReport(mine, DCCheck{ID: "abc123", Passes: 3, Down: 9}); err == nil {
		t.Error("more silent passes than passes is implausible and must be refused")
	}
	if err := f.dcAcceptReport(mine, DCCheck{ID: "abc123", Passes: 10, Down: 1,
		MedMs: 12.5, State: "done"}); err != nil {
		t.Fatalf("the peer's own report should be accepted: %v", err)
	}
	got, ok := f.dcCheck("abc123")
	if !ok || got.Passes != 10 || got.Down != 1 || got.State != "done" {
		t.Errorf("the report should have been stored: %+v", got)
	}

	// An incoming check cannot be reported on either: it is ours to
	// measure, so a report about it can only be a mistake or a forgery.
	in := &DCCheck{ID: "inbound1", Dir: "in", PeerASN: "AS64501",
		Host: "198.51.100.10", Proto: "icmp", IntervalS: 60,
		StartedAt: time.Now().Unix(), ExpiresAt: time.Now().Unix() + 900, State: "running"}
	f.dcSave(in)
	if err := f.dcAcceptReport(mine, DCCheck{ID: "inbound1", Passes: 5}); err == nil {
		t.Error("a peer must not report on a check we are the ones measuring")
	}
}

// Measurements made for a peer never reach samples, the cascade, the
// availability figures or the tree. This is the promise the design note
// makes, and it is the one worth a test of its own.
func TestDCMeasurementsStayOutOfOurData(t *testing.T) {
	f, priv, tree, _ := dcPeers(t)
	*tree = []dcTreeCat{{Targets: []dcTreeTarget{
		{Host: "198.51.100.10", Proto: "icmp", Public: true}}}}
	if err := f.SetDCGrant("AS64501", defaultDCCaps(), "ops"); err != nil {
		t.Fatal(err)
	}
	if code, body := dcAsk(t, f, priv, dcRequest{Host: "198.51.100.10", Proto: "icmp",
		IntervalS: 60, Minutes: 15}); code != 200 {
		t.Fatalf("HTTP %d: %s", code, body)
	}
	c := f.dcChecks("in", 0)[0]
	store := f.store

	// The probe measures it like any other target.
	now := time.Now().Unix()
	var batch []queuedMeasure
	for i := 0; i < 10; i++ {
		lost := 0
		if i%5 == 0 {
			lost = 5
		}
		m := Measurement{TargetID: c.TargetID, ProbeID: 1, TS: now - int64(i)*60,
			Sent: 5, Lost: lost}
		for j := 0; j < 5-lost; j++ {
			m.RTTus = append(m.RTTus, 8000)
		}
		batch = append(batch, queuedMeasure{m: m, host: "198.51.100.10"})
	}
	if err := store.RecordBatch(batch); err != nil {
		t.Fatal(err)
	}

	// Nothing in samples.
	var n int
	if err := store.mx.QueryRow(`SELECT COUNT(*) FROM samples WHERE target_id=?`,
		c.TargetID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("a double-check measurement must not reach samples, found %d rows", n)
	}
	// All of it in the diverted table.
	if err := store.mx.QueryRow(`SELECT COUNT(*) FROM fed_dc_samples WHERE target_id=?`,
		c.TargetID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 10 {
		t.Errorf("the diverted table should hold the ten passes, found %d", n)
	}
	// Nothing reaches the cascade even when it runs.
	for i := 0; i+1 < len(cascade); i++ {
		if err := store.RollupRange(cascade[i].table, cascade[i+1].table,
			cascade[i+1].secs, now-86400, now+86400); err != nil {
			t.Fatal(err)
		}
	}
	for _, tbl := range []string{"roll_1m", "roll_5m", "roll_1h", "roll_1d"} {
		store.mx.QueryRow("SELECT COUNT(*) FROM "+tbl+" WHERE target_id=?",
			c.TargetID).Scan(&n)
		if n != 0 {
			t.Errorf("%s holds %d rows for a double-check target", tbl, n)
		}
	}
	// And the target is on no page of ours.
	cats, err := store.Tree(false)
	if err != nil {
		t.Fatal(err)
	}
	for _, cat := range cats {
		for _, tg := range cat.Targets {
			if tg.ID == c.TargetID {
				t.Error("a double-check target must not appear in the tree")
			}
		}
	}
	// The report reads the diverted samples: eight answered passes of ten.
	rep := f.dcReport(c)
	if rep.Passes != 10 || rep.Down != 2 {
		t.Errorf("the report should count ten passes and two silent, got %d and %d",
			rep.Passes, rep.Down)
	}
	if !rep.Reached() {
		t.Error("eight answers out of ten is reachable")
	}

	// Closing the check removes everything it produced.
	f.dcFinish(c, "test")
	store.mx.QueryRow(`SELECT COUNT(*) FROM fed_dc_samples WHERE target_id=?`,
		c.TargetID).Scan(&n)
	if n != 0 {
		t.Errorf("closing a check must purge its samples, %d left", n)
	}
	if _, err := store.TargetByID(c.TargetID); err == nil {
		t.Error("closing a check must delete its temporary target")
	}
}

// The verdict is the point of the whole exercise: the same report means
// opposite things depending on what we see ourselves, and the third answer
// is kept rather than forced into one of the first two.
func TestDCVerdict(t *testing.T) {
	f, _, _, _ := dcPeers(t)
	cases := []struct {
		name       string
		passes     int
		down       int
		weSeeItBad bool
		want       string
	}{
		{"no report yet", 0, 0, true, "pending"},
		{"peer reaches it, we do not", 10, 0, true, "path"},
		{"neither of us reaches it", 10, 10, true, "target"},
		{"peer reaches it and so do we", 10, 1, false, "both_ok"},
		{"peer reaches it sometimes", 10, 6, true, "intermittent"},
	}
	for _, c := range cases {
		got := f.dcVerdict(&DCCheck{Passes: c.passes, Down: c.down}, c.weSeeItBad)
		if got != c.want {
			t.Errorf("%s: expected %q, got %q", c.name, c.want, got)
		}
	}
}

// Asking is refused for a target this instance does not publish, which
// mirrors the guard the peer applies: better a clear local message than
// the peer's refusal.
func TestDCCannotAskAboutAPrivateTarget(t *testing.T) {
	f, _, _, _ := dcPeers(t)
	cat, _ := f.store.CreateCategory("c", "C", "C", true)
	id, err := f.store.CreateTarget(&Target{CategoryID: cat, Slug: "priv", Title: "Private",
		Host: "198.51.100.10", Proto: "icmp", IntervalS: 60, Packets: 5, SpacingMs: 100,
		TimeoutMs: 1000, Public: false, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.RequestDoubleCheck(id, "AS64501", 15); err == nil {
		t.Error("a private target must not be double-checked")
	} else if !strings.Contains(err.Error(), "private") {
		t.Errorf("the message should say why: %v", err)
	}
	if _, err := f.RequestDoubleCheck(id, "AS65550", 15); err == nil {
		t.Error("asking an unknown peer must be refused")
	}
}

// The corroboration stays in the back-office until an operator decides
// otherwise: it publishes a fact about a third party's reachability
// measured by somebody who never agreed to have it published.
func TestDCCorroborationIsPrivateByDefault(t *testing.T) {
	f, _, _, _ := dcPeers(t)
	c := &DCCheck{ID: "pub1", Dir: "out", PeerASN: "AS64501", Host: "198.51.100.10",
		Proto: "icmp", IntervalS: 60, StartedAt: time.Now().Unix() - 60,
		ExpiresAt: time.Now().Unix() + 600, LocalTargetID: 7, State: "done",
		Passes: 10, Down: 0, MedMs: 9}
	if err := f.dcSave(c); err != nil {
		t.Fatal(err)
	}
	if got := f.PublicCorroboration(7); got != nil {
		t.Error("nothing is published until the operator turns it on")
	}
	s := f.dcSettings()
	s.Public = true
	if err := f.setDCSettings(s); err != nil {
		t.Fatal(err)
	}
	got := f.PublicCorroboration(7)
	if got == nil {
		t.Fatal("with the switch on, the corroboration should be published")
	}
	if got.PeerASN != "AS64501" || got.Passes != 10 {
		t.Errorf("the published figure should carry the AS and the counts: %+v", got)
	}
	// What is published carries no identifier a visitor could replay.
	if got.ID != "" {
		t.Error("the check id must not be published")
	}
	b, _ := json.Marshal(got)
	if strings.Contains(string(b), "local_target_id") {
		t.Errorf("the published shape leaks the local target: %s", b)
	}
}

// The automatic path is the reason the grant exists: it must ask, but it
// must not spend a peer's daily allowance on a flapping target.
func TestDCAutoDoesNotAskTwiceForTheSameTarget(t *testing.T) {
	f, _, _, _ := dcPeers(t)
	now := time.Now().Unix()
	f.dcSave(&DCCheck{ID: "recent", Dir: "out", PeerASN: "AS64501",
		Host: "198.51.100.10", Proto: "icmp", IntervalS: 60,
		StartedAt: now - 60, ExpiresAt: now + 600, LocalTargetID: 42, State: "running"})
	before := len(f.dcChecks("out", 0))
	f.AutoDoubleCheck(42)
	if after := len(f.dcChecks("out", 0)); after != before {
		t.Errorf("a target asked about a minute ago must not be asked again: %d then %d",
			before, after)
	}
}

// The settings refuse what they cannot honour, rather than storing a value
// the rest of the code would have to second-guess.
func TestDCSettingsValidation(t *testing.T) {
	f, _, _, _ := dcPeers(t)
	if err := f.setDCSettings(DCSettings{AskPeers: 50, Minutes: 15}); err == nil {
		t.Error("asking fifty peers per incident must be refused")
	}
	if err := f.setDCSettings(DCSettings{AskPeers: 2, Minutes: 10000}); err == nil {
		t.Error("a window of ten thousand minutes must be refused")
	}
	if err := f.setDCSettings(DCSettings{Auto: true, Public: true, AskPeers: 3,
		Minutes: 20}); err != nil {
		t.Fatalf("a reasonable setting should be accepted: %v", err)
	}
	got := f.dcSettings()
	if !got.Auto || !got.Public || got.AskPeers != 3 || got.Minutes != 20 {
		t.Errorf("the settings should round-trip: %+v", got)
	}
	// The shipped default asks by itself and publishes nothing.
	d := defaultDCSettings()
	if !d.Auto || d.Public {
		t.Errorf("the default should ask automatically and publish nothing: %+v", d)
	}
}

// An unsigned request learns nothing, not even whether a grant exists.
func TestDCRequestNeedsASignature(t *testing.T) {
	f, _, tree, _ := dcPeers(t)
	*tree = []dcTreeCat{{Targets: []dcTreeTarget{
		{Host: "198.51.100.10", Proto: "icmp", Public: true}}}}
	f.SetDCGrant("AS64501", defaultDCCaps(), "ops")
	api := &API{store: f.store, fed: f, probeID: 1}
	body, _ := json.Marshal(dcRequest{Host: "198.51.100.10", Proto: "icmp",
		IntervalS: 60, Minutes: 5})
	req := httptest.NewRequest("POST", "/api/v1/fed/dc/request", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	api.dcRequestIn(rec, req)
	if rec.Code != 401 {
		t.Fatalf("an unsigned request must be refused with 401, got %d", rec.Code)
	}
	if strings.Contains(rec.Body.String(), "granted") {
		t.Errorf("the refusal must not reveal the grant state: %s", rec.Body.String())
	}
	if n := len(f.dcChecks("in", 0)); n != 0 {
		t.Errorf("no check may be created: %d", n)
	}
}

// A signature made for one instance must not work on another, and the
// double-check endpoint is no exception to the rule the rest of the
// federation already follows.
func TestDCSignatureIsBoundToItsAudience(t *testing.T) {
	f, priv, tree, _ := dcPeers(t)
	*tree = []dcTreeCat{{Targets: []dcTreeTarget{
		{Host: "198.51.100.10", Proto: "icmp", Public: true}}}}
	f.SetDCGrant("AS64501", defaultDCCaps(), "ops")
	api := &API{store: f.store, fed: f, probeID: 1}
	// Signed for somebody else's AS.
	r := signedReq(priv, "AS64501", "AS65000", "POST", "/api/v1/fed/dc/request",
		dcRequest{Host: "198.51.100.10", Proto: "icmp", IntervalS: 60, Minutes: 5})
	rec := httptest.NewRecorder()
	api.dcRequestIn(rec, r)
	if rec.Code != 401 {
		t.Errorf("a signature addressed elsewhere must be refused, got %d: %s",
			rec.Code, rec.Body.String())
	}
}

// The interval a peer asks for cannot be faster than the floor: nothing
// measured for somebody else runs faster than we measure for ourselves.
func TestDCIntervalFloor(t *testing.T) {
	f, priv, tree, _ := dcPeers(t)
	*tree = []dcTreeCat{{Targets: []dcTreeTarget{
		{Host: "198.51.100.10", Proto: "icmp", Public: true}}}}
	f.SetDCGrant("AS64501", defaultDCCaps(), "ops")
	code, body := dcAsk(t, f, priv, dcRequest{Host: "198.51.100.10", Proto: "icmp",
		IntervalS: 1, Minutes: 5})
	if code != 200 {
		t.Fatalf("HTTP %d: %s", code, body)
	}
	var reply struct {
		IntervalS int `json:"interval_s"`
	}
	json.Unmarshal([]byte(body), &reply)
	if reply.IntervalS < dcMinInterval {
		t.Errorf("the interval should be floored to %d, got %d", dcMinInterval, reply.IntervalS)
	}
	c := f.dcChecks("in", 0)[0]
	tg, err := f.store.TargetByID(c.TargetID)
	if err != nil {
		t.Fatal(err)
	}
	if tg.IntervalS < dcMinInterval {
		t.Errorf("the temporary target must carry the floored interval, got %d", tg.IntervalS)
	}
	if tg.Public || !tg.AlertsOff || tg.DCCheck == "" {
		t.Errorf("the temporary target must be private, silent and marked: %+v", tg)
	}
}

// A shape that makes no sense is refused before anything is created.
func TestDCRequestShape(t *testing.T) {
	f, priv, tree, _ := dcPeers(t)
	*tree = []dcTreeCat{{Targets: []dcTreeTarget{
		{Host: "198.51.100.10", Proto: "tcp", Port: 443, Public: true},
		{Host: "198.51.100.10", Proto: "icmp", Public: true}}}}
	f.SetDCGrant("AS64501", defaultDCCaps(), "ops")
	bad := []dcRequest{
		{Host: "198.51.100.10", Proto: "http", IntervalS: 60, Minutes: 5},
		{Host: "198.51.100.10", Proto: "tcp", Port: 0, IntervalS: 60, Minutes: 5},
		{Host: "198.51.100.10", Proto: "tcp", Port: 99999, IntervalS: 60, Minutes: 5},
		{Host: "198.51.100.10", Proto: "icmp", Family: 9, IntervalS: 60, Minutes: 5},
		{Host: "", Proto: "icmp", IntervalS: 60, Minutes: 5},
	}
	for i, req := range bad {
		if code, body := dcAsk(t, f, priv, req); code == 200 {
			t.Errorf("request %d should have been refused: %s", i, body)
		}
	}
	if n := len(f.dcChecks("in", 0)); n != 0 {
		t.Errorf("no check may be created by a malformed request, found %d", n)
	}
}

// The loop closes what is past its window and reports it, so a check
// cannot sit running for ever because a report went missing.
func TestDCTickClosesExpiredChecks(t *testing.T) {
	f, _, _, _ := dcPeers(t)
	now := time.Now().Unix()
	f.dcSave(&DCCheck{ID: "stale-out", Dir: "out", PeerASN: "AS64501",
		Host: "198.51.100.10", Proto: "icmp", IntervalS: 60,
		StartedAt: now - 3600, ExpiresAt: now - 1800, LocalTargetID: 5, State: "running"})
	f.dcTick(now)
	c, ok := f.dcCheck("stale-out")
	if !ok {
		t.Fatal("the check should still be there")
	}
	if c.State != "done" {
		t.Errorf("an outgoing check past its window should be closed, got %q", c.State)
	}
	if !strings.Contains(c.Note, "no report") {
		t.Errorf("the note should say the peer never reported: %q", c.Note)
	}
}
