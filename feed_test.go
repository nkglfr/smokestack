package main

import (
	"compress/gzip"
	"encoding/xml"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// readFeed is what a feed reader parses, declared here rather than borrowed
// from feed.go so that a mistake in the writer's types cannot hide behind the
// same mistake in the reader's.
type readFeed struct {
	XMLName xml.Name `xml:"http://www.w3.org/2005/Atom feed"`
	Lang    string   `xml:"http://www.w3.org/XML/1998/namespace lang,attr"`
	ID      string   `xml:"http://www.w3.org/2005/Atom id"`
	Updated string   `xml:"http://www.w3.org/2005/Atom updated"`
	Entries []struct {
		ID        string `xml:"http://www.w3.org/2005/Atom id"`
		Title     string `xml:"http://www.w3.org/2005/Atom title"`
		Published string `xml:"http://www.w3.org/2005/Atom published"`
		Updated   string `xml:"http://www.w3.org/2005/Atom updated"`
		Summary   string `xml:"http://www.w3.org/2005/Atom summary"`
		Link      struct {
			Href string `xml:"href,attr"`
		} `xml:"http://www.w3.org/2005/Atom link"`
	} `xml:"http://www.w3.org/2005/Atom entry"`
}

type feedFixture struct {
	store  *Store
	al     *Alerter
	cat    int64
	pub    int64
	hidden int64
	priv   int64
}

func newFeedFixture(t *testing.T) *feedFixture {
	t.Helper()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	cat, err := store.CreateCategory("c", "C", "C", true)
	if err != nil {
		t.Fatal(err)
	}
	f := &feedFixture{store: store, cat: cat}
	f.pub = f.add(t, Target{Slug: "pub", Title: "Public one", Host: "192.0.2.1", Public: true, Enabled: true})
	f.hidden = f.add(t, Target{Slug: "hidden", Title: "Hidden host", Host: "198.51.100.7",
		Public: true, Enabled: true, HideHost: true})
	f.priv = f.add(t, Target{Slug: "priv", Title: "Private one", Host: "203.0.113.9", Enabled: true})
	site := store.Site()
	site.URL, site.PublicIncidents = "https://latency.example.net", true
	if err := store.SetSite(site); err != nil {
		t.Fatal(err)
	}
	f.al = NewAlerter(store)
	f.al.trace = func(int64) {}
	return f
}

// add creates a target dated a day back: the incidents these tests open are
// in the past, and the feed refuses one older than its target.
func (f *feedFixture) add(t *testing.T, tg Target) int64 {
	t.Helper()
	if tg.CategoryID == 0 {
		tg.CategoryID = f.cat
	}
	tg.Proto, tg.IntervalS, tg.Packets, tg.SpacingMs, tg.TimeoutMs = "icmp", 60, 10, 100, 1000
	id, err := f.store.CreateTarget(&tg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.cfg.Exec(`UPDATE targets SET created_at=created_at-86400 WHERE id=?`, id); err != nil {
		t.Fatal(err)
	}
	return id
}

// tick drives the alerter's own write path: "crit" opens an incident on a
// target, anything else closes the one it has.
func (f *feedFixture) tick(target, at int64, status string) {
	f.tickBy(target, 0, at, status)
}

func (f *feedFixture) tickBy(target, probe, at int64, status string) {
	f.al.now = func() int64 { return at }
	f.al.TickFrom(map[int64]string{target: status}, nil, map[int64]int64{target: probe})
}

// incidents counts the incident entries of a feed.
func incidents(feed readFeed) int {
	n := 0
	for _, e := range feed.Entries {
		if strings.Contains(e.ID, ":incident/") {
			n++
		}
	}
	return n
}

func (f *feedFixture) api(t *testing.T) *API {
	t.Helper()
	i, err := NewI18n(os.DirFS("web"), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return &API{store: f.store, probeID: 1, i18n: i}
}

// get asks a fresh API, so that neither the feed cache nor the overview
// behind it remembers an earlier state of the fixture.
func (f *feedFixture) get(t *testing.T, path string) *httptest.ResponseRecorder {
	t.Helper()
	seoOverview.ov, seoOverview.when = nil, time.Time{}
	return serveFeed(f.api(t), path, nil)
}

func serveFeed(api *API, path string, hdr http.Header) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	api.FeedRoutes(mux)
	req := httptest.NewRequest("GET", path, nil)
	for k, v := range hdr {
		req.Header[k] = v
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

func parseFeed(t *testing.T, rec *httptest.ResponseRecorder) readFeed {
	t.Helper()
	if rec.Code != 200 {
		t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/atom+xml") {
		t.Errorf("Content-Type %q", ct)
	}
	var out readFeed
	if err := xml.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("not well-formed Atom: %v\n%s", err, rec.Body.String())
	}
	if _, err := time.Parse(time.RFC3339, out.Updated); err != nil {
		t.Errorf("feed <updated> %q is not RFC 3339", out.Updated)
	}
	for _, e := range out.Entries {
		for _, ts := range []string{e.Published, e.Updated} {
			if _, err := time.Parse(time.RFC3339, ts); err != nil {
				t.Errorf("entry %s: %q is not RFC 3339", e.ID, ts)
			}
		}
	}
	return out
}

// The feed says nothing the public pages do not: a private target is absent
// whatever happens on it, a hidden host stays hidden, and an address is
// masked exactly as the overview masks it.
func TestFeedPublishesOnlyWhatThePagesShow(t *testing.T) {
	f := newFeedFixture(t)
	now := time.Now().Unix()
	// Public targets the overview leaves out: one in a private category,
	// one disabled.
	closedCat, err := f.store.CreateCategory("internal", "Interne", "Internal", false)
	if err != nil {
		t.Fatal(err)
	}
	inPriv := f.add(t, Target{CategoryID: closedCat, Slug: "core", Title: "Core router",
		Host: "203.0.113.20", Public: true, Enabled: true})
	off := f.add(t, Target{Slug: "retired", Title: "Retired link", Host: "203.0.113.30", Public: true})
	for _, id := range []int64{f.pub, f.hidden, f.priv, inPriv, off} {
		f.tick(id, now-1800, "crit")
		f.tick(id, now-600, "ok")
	}
	for _, id := range []int64{f.pub, f.priv, inPriv, off} {
		if _, err := f.store.CreateMaintenance(&Maintenance{TargetID: id, Title: "Router upgrade",
			Note: "Provider window", StartsAt: now + 3600, EndsAt: now + 7200, StopProbe: true}); err != nil {
			t.Fatal(err)
		}
	}
	// A route change names the address measured, and is not an outage.
	f.store.cfg.Exec(`INSERT INTO events(ts_start,kind,title,body,scope,scope_id,public)
		VALUES(?,'path','Route changed','AS path towards 192.0.2.1','target',?,1)`, now-60, f.pub)
	f.store.cfg.Exec(`INSERT INTO events(ts_start,kind,title,public) VALUES(?,'incident','Kept back',0)`, now-60)
	f.store.cfg.Exec(`INSERT INTO events(ts_start,ts_end,kind,title,body) VALUES(?,?,'incident','Upstream outage','Transit down')`,
		now-900, now-300)

	rec := f.get(t, "/feed.atom")
	feed := parseFeed(t, rec)
	body := rec.Body.String()
	for _, leak := range []string{"Private one", "203.0.113", "198.51.100.7", "192.0.2.1", "Kept back",
		"Route changed", "Core router", "Retired link"} {
		if strings.Contains(body, leak) {
			t.Errorf("the feed carries %q", leak)
		}
	}
	for _, want := range []string{"Resolved: incident on Public one", "Public one (192.0.XXX.XXX)",
		"Resolved: incident on Hidden host", "Planned maintenance on Public one: Router upgrade",
		"Provider window", "Upstream outage"} {
		if !strings.Contains(body, want) {
			t.Errorf("the feed lacks %q", want)
		}
	}
	// 2 public incidents, 1 public maintenance, 1 published event.
	if len(feed.Entries) != 4 {
		t.Errorf("%d entries, expected 4", len(feed.Entries))
	}
	if feed.Lang != "en" || feed.ID != "https://latency.example.net/feed.atom" {
		t.Errorf("feed lang %q, id %q", feed.Lang, feed.ID)
	}
	for _, e := range feed.Entries {
		if !strings.HasPrefix(e.Link.Href, "https://latency.example.net/") {
			t.Errorf("entry %s links to %q, not to the published address", e.ID, e.Link.Href)
		}
	}

	// A target without a public page has no feed, and saying so must not
	// differ in any way from a target that does not exist.
	none := f.get(t, "/t/no-such-target/feed.atom")
	if none.Code != 404 {
		t.Fatalf("unknown slug: HTTP %d", none.Code)
	}
	for _, slug := range []string{"priv", "core", "retired"} {
		rec := f.get(t, "/t/"+slug+"/feed.atom")
		if rec.Code != 404 || rec.Body.String() != none.Body.String() {
			t.Errorf("%s: HTTP %d %q, an unknown one %q", slug, rec.Code, rec.Body.String(), none.Body.String())
		}
		for _, h := range []string{"Content-Type", "Cache-Control", "ETag"} {
			if rec.Header().Get(h) != none.Header().Get(h) {
				t.Errorf("%s: %s %q, %q for an unknown one", slug, h, rec.Header().Get(h), none.Header().Get(h))
			}
		}
	}

	hidden := f.get(t, "/t/hidden/feed.atom")
	one := parseFeed(t, hidden)
	if strings.Contains(hidden.Body.String(), "198.51.100") {
		t.Error("the feed of a target whose address is private carries the address")
	}
	// Its incident and the instance's own event; nothing of the others.
	if len(one.Entries) != 2 || strings.Contains(hidden.Body.String(), "Public one") {
		t.Errorf("the target feed carries %d entries:\n%s", len(one.Entries), hidden.Body.String())
	}

	// With masking off, the overview's host is the full one, and so is ours.
	site := f.store.Site()
	site.MaskAddresses = false
	f.store.SetSite(site)
	if body := f.get(t, "/feed.atom").Body.String(); !strings.Contains(body, "Public one (192.0.2.1)") ||
		strings.Contains(body, "198.51.100.7") {
		t.Errorf("unmasked feed:\n%s", body)
	}
}

// An incident is one entry from the moment it is published to the moment it
// closes: same id, published unchanged, updated moved to the close.
func TestFeedIncidentKeepsItsID(t *testing.T) {
	f := newFeedFixture(t)
	now := time.Now().Unix()
	opened := now - 1800
	f.tick(f.pub, opened, "crit")

	before := parseFeed(t, f.get(t, "/t/pub/feed.atom"))
	if len(before.Entries) != 1 {
		t.Fatalf("%d entries for one open incident", len(before.Entries))
	}
	open := before.Entries[0]
	if open.Title != "Incident on Public one" || open.Updated != atomTime(opened) {
		t.Errorf("open incident: %q updated %s", open.Title, open.Updated)
	}
	day := f.store.Setting("feed_tag_date", "")
	if day != time.Now().UTC().Format(time.DateOnly) {
		t.Errorf("the first feed stored %q as the tag date", day)
	}
	want := "tag:latency.example.net," + day + ":incident/"
	if !strings.HasPrefix(open.ID, want) {
		t.Errorf("id %q, expected a tag URI starting with %q", open.ID, want)
	}

	closed := now - 60
	f.tick(f.pub, closed, "ok")
	after := parseFeed(t, f.get(t, "/t/pub/feed.atom"))
	if len(after.Entries) != 1 {
		t.Fatalf("%d entries once the incident closed", len(after.Entries))
	}
	done := after.Entries[0]
	if done.ID != open.ID {
		t.Errorf("the id changed when the incident closed: %q then %q", open.ID, done.ID)
	}
	if done.Published != open.Published || done.Updated != atomTime(closed) {
		t.Errorf("published %s → %s, updated %s → %s", open.Published, done.Published, open.Updated, done.Updated)
	}
	if done.Title != "Resolved: incident on Public one" || !strings.Contains(done.Summary, "(29 min)") {
		t.Errorf("closed incident: %q, %q", done.Title, done.Summary)
	}
	if after.Updated != done.Updated {
		t.Errorf("the feed's <updated> %s does not follow its newest entry %s", after.Updated, done.Updated)
	}

	// The date is the instance's, read back rather than taken from any row:
	// a maintenance recorded long ago does not show when.
	f.store.SetSetting("feed_tag_date", "2020-05-04")
	m, err := f.store.CreateMaintenance(&Maintenance{TargetID: f.pub, Title: "Swap",
		StartsAt: now + 3600, EndsAt: now + 7200, StopAlerts: true})
	if err != nil {
		t.Fatal(err)
	}
	f.store.cfg.Exec(`UPDATE maintenances SET created_at=? WHERE id=?`, now-5*86400, m)
	for _, e := range parseFeed(t, f.get(t, "/feed.atom")).Entries {
		if !strings.HasPrefix(e.ID, "tag:latency.example.net,2020-05-04:") {
			t.Errorf("entry id %q does not carry the instance's date", e.ID)
		}
	}
}

// An incident the alerting kept quiet is not pushed to every subscriber:
// one opened during a window that stops alerts or measurement, one on a
// target left out of alerting, one decided by a probe since disabled.
func TestFeedLeavesOutWhatDidNotAlert(t *testing.T) {
	f := newFeedFixture(t)
	now := time.Now().Unix()
	quiet := f.add(t, Target{Slug: "quiet", Title: "Quiet one", Host: "192.0.2.50",
		Public: true, Enabled: true, AlertsOff: true})
	for _, m := range []Maintenance{
		{TargetID: f.pub, Title: "Reboot", StartsAt: now - 7200, EndsAt: now - 3600, StopAlerts: true},
		{TargetID: f.hidden, Title: "Swap", StartsAt: now - 7200, EndsAt: now - 3600, StopProbe: true},
	} {
		if _, err := f.store.CreateMaintenance(&m); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []int64{f.pub, f.hidden, quiet} {
		f.tick(id, now-6000, "crit")
		f.tick(id, now-4200, "ok")
	}
	if n := incidents(parseFeed(t, f.get(t, "/feed.atom"))); n != 0 {
		t.Errorf("%d incidents published that alerted nobody", n)
	}

	// The same targets, outside any window: published, except the one left
	// out of alerting.
	for _, id := range []int64{f.pub, f.hidden, quiet} {
		f.tick(id, now-3000, "crit")
		f.tick(id, now-1200, "ok")
	}
	if n := incidents(parseFeed(t, f.get(t, "/feed.atom"))); n != 2 {
		t.Errorf("%d incidents published, expected the 2 that alerted", n)
	}

	remote, err := f.store.CreateProbe("remote", "Remote", "")
	if err != nil {
		t.Fatal(err)
	}
	f.tickBy(f.pub, remote.ID, now-900, "crit")
	f.tickBy(f.pub, remote.ID, now-60, "ok")
	if n := incidents(parseFeed(t, f.get(t, "/feed.atom"))); n != 3 {
		t.Errorf("%d incidents with the remote probe enabled, expected 3", n)
	}
	if err := f.store.SetProbeEnabled(remote.ID, false); err != nil {
		t.Fatal(err)
	}
	if n := incidents(parseFeed(t, f.get(t, "/feed.atom"))); n != 2 {
		t.Errorf("%d incidents once the remote probe is disabled, expected 2", n)
	}
}

// Purging a target takes its incidents with it, so that the next target
// handed the same id does not publish them as its own.
func TestFeedPurgedTargetLeavesNoIncident(t *testing.T) {
	f := newFeedFixture(t)
	now := time.Now().Unix()
	count := func(id int64) int {
		var n int
		f.store.cfg.QueryRow(`SELECT COUNT(*) FROM local_incidents WHERE target_id=?`, id).Scan(&n)
		return n
	}
	f.tick(f.pub, now-3600, "crit")
	f.tick(f.pub, now-3000, "ok")
	f.tick(f.priv, now-3600, "crit")
	f.tick(f.priv, now-1800, "ok")
	if err := f.store.ArchiveTarget(f.priv); err != nil {
		t.Fatal(err)
	}
	if err := f.store.PurgeTarget(f.priv); err != nil {
		t.Fatal(err)
	}
	if count(f.priv) != 0 || count(f.pub) != 1 {
		t.Fatalf("after the purge: %d incidents on the purged target, %d on the other", count(f.priv), count(f.pub))
	}
	fresh := f.add(t, Target{Slug: "fresh", Title: "Fresh target", Host: "192.0.2.9", Public: true, Enabled: true})
	if fresh != f.priv {
		t.Fatalf("the new target got id %d, not the purged %d: the test proves nothing", fresh, f.priv)
	}
	feed := parseFeed(t, f.get(t, "/t/fresh/feed.atom"))
	if n := incidents(feed); n != 0 {
		t.Errorf("the new target inherited %d incidents", n)
	}

	// Rows an earlier version left behind on purge still name an id the
	// next target is given: older than that target, they are not its own.
	next := fresh + 1
	f.store.cfg.Exec(`INSERT INTO local_incidents(target_id,opened_at,closed_at) VALUES(?,?,?)`,
		next, now-3600, now-1800)
	late, err := f.store.CreateTarget(&Target{CategoryID: f.cat, Slug: "late", Title: "Late target",
		Host: "192.0.2.10", Proto: "icmp", IntervalS: 60, Packets: 10, SpacingMs: 100, TimeoutMs: 1000,
		Public: true, Enabled: true})
	if err != nil || late != next {
		t.Fatalf("CreateTarget: id %d, %v; expected %d", late, err, next)
	}
	if n := incidents(parseFeed(t, f.get(t, "/t/late/feed.atom"))); n != 0 {
		t.Errorf("the new target published %d incidents older than itself", n)
	}
	f.store.cfg.Exec(`UPDATE targets SET created_at=? WHERE id=?`, now-86400, late)
	if n := incidents(parseFeed(t, f.get(t, "/t/late/feed.atom"))); n != 1 {
		t.Errorf("%d incidents once the row is younger than its target, expected 1", n)
	}

	// The double-check's temporary targets are dropped the same way.
	dc := f.add(t, Target{Slug: "dc-x", Title: "dc", Host: "192.0.2.11", Enabled: true, AlertsOff: true})
	f.tick(dc, now-600, "crit")
	f.store.dcDropTarget(dc)
	if count(dc) != 0 || count(f.pub) != 1 {
		t.Errorf("after dropping a double-check target: %d incidents on it, %d on another", count(dc), count(f.pub))
	}
}

// Incidents are the operator's to publish, and only once they have lasted
// the alert delay; maintenance is announced on the banner's horizon and never
// after the fact.
func TestFeedWhatWaits(t *testing.T) {
	f := newFeedFixture(t)
	now := time.Now().Unix()
	f.tick(f.pub, now-60, "crit") // a blip, still open
	f.tick(f.hidden, now-3600, "crit")
	f.tick(f.hidden, now-3500, "ok") // closed after 100 s
	for _, m := range []Maintenance{
		{TargetID: f.pub, Title: "Next week", StartsAt: now + 2*86400, EndsAt: now + 2*86400 + 3600, StopAlerts: true},
		{TargetID: f.pub, Title: "Next month", StartsAt: now + 20*86400, EndsAt: now + 20*86400 + 3600, StopAlerts: true},
		{TargetID: f.pub, Title: "After the fact", StartsAt: now - 7200, EndsAt: now - 3600, StopAlerts: true},
	} {
		if _, err := f.store.CreateMaintenance(&m); err != nil {
			t.Fatal(err)
		}
	}
	feed := parseFeed(t, f.get(t, "/feed.atom"))
	if len(feed.Entries) != 1 || feed.Entries[0].Title != "Planned maintenance on Public one: Next week" ||
		!strings.HasPrefix(feed.Entries[0].Summary, "Planned from ") {
		t.Fatalf("expected the announced window alone, got %+v", feed.Entries)
	}

	f.tick(f.pub, now, "ok") // closed after a minute: still a blip
	f.tick(f.hidden, now-1200, "crit")
	if feed := parseFeed(t, f.get(t, "/feed.atom")); len(feed.Entries) != 2 {
		t.Errorf("a sustained incident should be published: %d entries", len(feed.Entries))
	}
	site := f.store.Site()
	site.PublicIncidents = false
	f.store.SetSite(site)
	if body := f.get(t, "/feed.atom").Body.String(); strings.Contains(body, "Incident on") {
		t.Errorf("incidents published with the switch off:\n%s", body)
	}
}

// A reader that already has the feed is told so in a few bytes.
func TestFeedRevalidates(t *testing.T) {
	f := newFeedFixture(t)
	seoOverview.ov, seoOverview.when = nil, time.Time{}
	api := f.api(t)
	first := serveFeed(api, "/feed.atom", nil)
	etag := first.Header().Get("ETag")
	if first.Code != 200 || etag == "" || first.Header().Get("Cache-Control") != "public, max-age=60" {
		t.Fatalf("HTTP %d, ETag %q, Cache-Control %q", first.Code, etag, first.Header().Get("Cache-Control"))
	}
	again := serveFeed(api, "/feed.atom", http.Header{"If-None-Match": {etag}})
	if again.Code != http.StatusNotModified || again.Body.Len() != 0 {
		t.Errorf("revalidation: HTTP %d with %d bytes", again.Code, again.Body.Len())
	}
}

// Every public page advertises the instance's feed, a target page its own
// as well, and a page kept out of search engines neither.
func TestFeedAdvertised(t *testing.T) {
	f := newFeedFixture(t)
	api := f.api(t)
	r := httptest.NewRequest("GET", "/t/pub", nil)
	head := api.seoHead(r, pageMeta{Path: "/t/pub", Title: "Public one"})
	for _, want := range []string{
		`<link rel="alternate" type="application/atom+xml" title="Latency monitoring: status updates" href="https://latency.example.net/feed.atom">`,
		`<link rel="alternate" type="application/atom+xml" title="Public one · Latency monitoring: status updates" href="https://latency.example.net/t/pub/feed.atom">`,
	} {
		if !strings.Contains(head, want) {
			t.Errorf("the target page lacks %s", want)
		}
	}
	if head := api.seoHead(r, pageMeta{Path: "/s/token", NoIndex: true}); strings.Contains(head, "atom") {
		t.Error("a page kept out of search engines advertises a feed")
	}
}

// The feed is compressed like the pages, revalidates through the
// compression, and adding its type left every other type as it was.
func TestFeedCompressed(t *testing.T) {
	for ct, want := range map[string]bool{
		"application/atom+xml; charset=utf-8": true,
		"application/json":                    true,
		"text/html; charset=utf-8":            true,
		"application/javascript":              true,
		"image/svg+xml":                       true,
		"image/png":                           false,
		"application/octet-stream":            false,
		"application/xml":                     false,
		"":                                    false,
	} {
		if compressible(ct) != want {
			t.Errorf("compressible(%q) = %v", ct, !want)
		}
	}

	f := newFeedFixture(t)
	seoOverview.ov, seoOverview.when = nil, time.Time{}
	mux := http.NewServeMux()
	f.api(t).FeedRoutes(mux)
	h := withGzip(mux)
	get := func(etag string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", "/feed.atom", nil)
		req.Header.Set("Accept-Encoding", "gzip")
		if etag != "" {
			req.Header.Set("If-None-Match", etag)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	first := get("")
	if first.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("feed served without compression: %v", first.Header())
	}
	zr, err := gzip.NewReader(first.Body)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(zr)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "<feed xmlns=\"http://www.w3.org/2005/Atom\"") {
		t.Errorf("decompressed body is not the feed:\n%s", body)
	}
	again := get(first.Header().Get("ETag"))
	if again.Code != http.StatusNotModified || again.Body.Len() != 0 || again.Header().Get("Content-Encoding") != "" {
		t.Errorf("revalidation through gzip: HTTP %d, %d bytes, Content-Encoding %q",
			again.Code, again.Body.Len(), again.Header().Get("Content-Encoding"))
	}
}
