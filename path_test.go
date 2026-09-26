package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func hopAS(addr, as string) Hop { return Hop{Addr: addr, ASN: as, Sent: 3, RTTms: []float64{1}} }

// Comparing AS paths, not addresses: two parallel links of the same operator
// give different addresses and the same path, and must not raise anything.
func TestASPathComparison(t *testing.T) {
	a := &Traceroute{Hops: []Hop{hopAS("192.0.2.1", "AS64500"), hopAS("198.51.100.1", "AS174"),
		hopAS("198.51.100.9", "AS174"), hopAS("203.0.113.5", "AS15169")}}
	b := &Traceroute{Hops: []Hop{hopAS("192.0.2.1", "AS64500"), hopAS("198.51.100.77", "AS174"),
		hopAS("203.0.113.5", "AS15169")}}
	if got := strings.Join(asPath(a), " "); got != "AS64500 AS174 AS15169" {
		t.Errorf("repetitions must collapse: %q", got)
	}
	if !samePath(asPath(a), asPath(b)) {
		t.Error("a different address inside the same AS is not a path change")
	}
	c := &Traceroute{Hops: []Hop{hopAS("192.0.2.1", "AS64500"), hopAS("198.51.100.1", "AS3356"),
		hopAS("203.0.113.5", "AS15169")}}
	if samePath(asPath(a), asPath(c)) {
		t.Error("a transit change must be seen")
	}
	// Silent hops carry no AS and must not break the comparison.
	d := &Traceroute{Hops: []Hop{hopAS("192.0.2.1", "AS64500"), hopAS("*", ""),
		hopAS("198.51.100.1", "AS174"), hopAS("203.0.113.5", "AS15169")}}
	if !samePath(asPath(a), asPath(d)) {
		t.Error("a silent hop is not a path change")
	}
}

// A change between two healthy paths is recorded as an event, once, and is
// available to the alerting as context.
func TestPathChangeRecorded(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cat, _ := store.CreateCategory("c", "C", "C", true)
	id, err := store.CreateTarget(&Target{CategoryID: cat, Slug: "t", Title: "Transit Paris",
		Host: "192.0.2.9", Proto: "icmp", IntervalS: 60, Packets: 10, SpacingMs: 100,
		TimeoutMs: 1000, Public: true, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	ref := func(ts int64, transit string) {
		if err := store.SaveTraceroute(&Traceroute{TargetID: id, ProbeID: 1, TS: ts,
			Kind: "reference", Reason: "healthy path", Family: 4, Dest: "192.0.2.9", Reached: true,
			Hops: []Hop{hopAS("192.0.2.1", "AS64500"), hopAS("198.51.100.1", transit),
				hopAS("203.0.113.5", "AS15169")}}); err != nil {
			t.Fatal(err)
		}
	}
	ref(now-2*86400, "AS174")
	if _, ok := store.RecentPathChange(id, now-7*86400); ok {
		t.Error("a first reference cannot be a change")
	}
	ref(now-86400, "AS174")
	if _, ok := store.RecentPathChange(id, now-7*86400); ok {
		t.Error("an identical path is not a change")
	}
	ref(now-3600, "AS3356") // the transit provider changed
	detail, ok := store.RecentPathChange(id, now-7*86400)
	if !ok || !strings.Contains(detail, "AS174") || !strings.Contains(detail, "AS3356") {
		t.Fatalf("the change should be recorded with both paths: %q", detail)
	}
	// The title drawn on the graph stays short; the body names both ends of
	// the path, because a route is only meaningful between two networks.
	var title, body, scope string
	var scopeID *int64
	store.cfg.QueryRow(`SELECT title,COALESCE(body,''),scope,scope_id FROM events
	                    WHERE kind='path' ORDER BY ts_start DESC LIMIT 1`).
		Scan(&title, &body, &scope, &scopeID)
	if len(title) > 60 || !strings.Contains(strings.ToLower(title), "route changed") {
		t.Errorf("the event title must stay short and readable: %q", title)
	}
	// The event belongs to this target and to nothing else.
	if scope != "target" || scopeID == nil || *scopeID != id {
		t.Errorf("the event must be scoped to its target: scope=%q id=%v", scope, scopeID)
	}
	if !strings.Contains(body, "AS174") || !strings.Contains(body, "AS3356") {
		t.Errorf("the paths must be in the body: %q", body)
	}
	if !strings.Contains(body, "AS15169") || !strings.Contains(body, "Transit Paris") {
		t.Errorf("the body must name the far end of the path: %q", body)
	}
	if !strings.Contains(body, "192.0.2.9") {
		t.Errorf("the body must name the address actually measured: %q", body)
	}
	if !strings.Contains(detail, "Transit Paris") {
		t.Errorf("the context given to alerting should name the target: %q", detail)
	}
	// Out of the window, it is not offered as context any more.
	if _, ok := store.RecentPathChange(id, now-60); ok {
		t.Error("an old change must not be attached to a fresh incident")
	}
}

// The target's own interval wins over the instance default.
func TestReferenceIntervalPerTarget(t *testing.T) {
	d := newDetector(TracerouteConfig{PerHour: 30, ReferenceHours: 24})
	base := time.Now()
	d.now = func() time.Time { return base }
	good := Measurement{Sent: 10, Lost: 0, RTTus: []float64{1000, 1100, 1050, 1080, 1020}}
	for i := 0; i < 6; i++ {
		d.observeTarget(1, good, 4)
		d.observeTarget(2, good, 0)
	}
	// Five hours later: the target asking for four hours is due, the other
	// one, on the instance default of 24 h, is not.
	base = base.Add(5 * time.Hour)
	if kind, _ := d.observeTarget(1, good, 4); kind != "reference" {
		t.Errorf("a target asking for 4 h should be due: %q", kind)
	}
	if kind, _ := d.observeTarget(2, good, 0); kind == "reference" {
		t.Error("a target on the 24 h default should not be due after 5 h")
	}
}

// PeeringDB contacts: the NOC role comes first, contacts without any way to
// reach them are dropped, and a network declaring none is not an error.
// newASNServiceFor points the service at a fake PeeringDB.
func newASNServiceFor(base string) *ASNService {
	svc := NewASNService(nil, nil, "")
	svc.pdbBase = base
	return svc
}

func TestPeeringDBContactOrder(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/net"):
			w.Write([]byte(`{"data":[{"id":42,"name":"Example Transit","asn":174,
			  "policy_general":"Open","website":"https://example.net"}]}`))
		case strings.HasPrefix(r.URL.Path, "/poc"):
			w.Write([]byte(`{"data":[
			  {"role":"Policy","name":"Peering","email":"peering@example.net","status":"ok"},
			  {"role":"NOC","name":"NOC 24/7","email":"noc@example.net","phone":"+33100000000","status":"ok"},
			  {"role":"Technical","name":"Nobody","email":"","phone":"","status":"ok"},
			  {"role":"Abuse","name":"Abuse","email":"abuse@example.net","status":"deleted"}]}`))
		default:
			w.Write([]byte(`{"data":[]}`))
		}
	}))
	defer srv.Close()
	svc := newASNServiceFor(srv.URL + "/")
	info := &ASNInfo{ASN: "AS174"}
	p := svc.fetchPeeringDB("174", info)
	if p == nil {
		t.Fatal("a PeeringDB record was expected")
	}
	if len(p.Contacts) != 2 {
		t.Fatalf("2 reachable contacts expected, got %+v", p.Contacts)
	}
	if p.Contacts[0].Role != "NOC" {
		t.Errorf("the NOC must come first, got %q", p.Contacts[0].Role)
	}
	if p.Contacts[0].Phone == "" || p.Contacts[0].Email == "" {
		t.Errorf("the NOC contact lost its details: %+v", p.Contacts[0])
	}
	for _, c := range p.Contacts {
		if c.Email == "" && c.Phone == "" {
			t.Error("a contact with no way to reach it must be dropped")
		}
	}
}

// The middle of the route comes from the traceroute, with the two ends left
// out — they are our AS and the destination's, added by the handler — and a
// gap reported when the path went silent before arriving.
func TestASRouteFrom(t *testing.T) {
	full := &Traceroute{Reached: true, Hops: []Hop{
		hopAS("192.0.2.1", "AS64500"), hopAS("192.0.2.2", "AS64500"),
		hopAS("198.51.100.1", "AS174"), hopAS("198.51.100.9", "AS174"),
		hopAS("203.0.113.5", "AS29222")}}
	mid, gap := asRouteFrom(full, "AS64500", "AS29222")
	if len(mid) != 1 || mid[0].ASN != "AS174" || mid[0].Hops != 2 {
		t.Errorf("only the transit belongs in the middle: %+v", mid)
	}
	if gap {
		t.Error("a traceroute reaching the destination has no gap")
	}
	// Silent last hops: the segment before the destination is unknown.
	silent := &Traceroute{Reached: false, Hops: []Hop{
		hopAS("192.0.2.1", "AS64500"), hopAS("198.51.100.1", "AS174"),
		hopAS("*", ""), hopAS("*", "")}}
	mid, gap = asRouteFrom(silent, "AS64500", "AS29222")
	if len(mid) != 1 || mid[0].ASN != "AS174" {
		t.Errorf("middle: %+v", mid)
	}
	if !gap {
		t.Error("silent hops before the destination must be reported as a gap")
	}
	// A first hop in private space carries no AS: our own AS must not
	// depend on it, which is why the handler adds it.
	priv := &Traceroute{Reached: true, Hops: []Hop{
		hopAS("10.0.0.1", ""), hopAS("198.51.100.1", "AS3356"), hopAS("203.0.113.5", "AS29222")}}
	mid, gap = asRouteFrom(priv, "AS64500", "AS29222")
	if len(mid) != 1 || mid[0].ASN != "AS3356" || gap {
		t.Errorf("private first hop mishandled: %+v gap=%v", mid, gap)
	}
	// A traceroute whose hops carry no AS at all leaves the middle empty
	// and the link unknown, rather than pretending the two ends touch.
	blind := &Traceroute{Reached: false, Hops: []Hop{hopAS("*", ""), hopAS("*", "")}}
	mid, gap = asRouteFrom(blind, "AS64500", "AS29222")
	if len(mid) != 0 || !gap {
		t.Errorf("a blind traceroute must give an explicit gap: %+v gap=%v", mid, gap)
	}
}

// The graph is built from several traceroutes: it must show both transits
// when the target was reached through each, mark the current path, and keep
// an unmeasured stretch as an explicit break.
func TestBuildASGraph(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cat, _ := store.CreateCategory("c", "C", "C", true)
	id, err := store.CreateTarget(&Target{CategoryID: cat, Slug: "t", Title: "T", Host: "192.0.2.9",
		Proto: "icmp", IntervalS: 60, Packets: 10, SpacingMs: 100, TimeoutMs: 1000,
		Public: true, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	save := func(ts int64, reached bool, hops ...Hop) {
		if err := store.SaveTraceroute(&Traceroute{TargetID: id, ProbeID: 1, TS: ts,
			Kind: "reference", Family: 4, Dest: "192.0.2.9", Reached: reached, Hops: hops}); err != nil {
			t.Fatal(err)
		}
	}
	// Older: through AS174. Newer: through AS3356, and it is the current one.
	save(now-7200, true, hopAS("198.51.100.1", "AS174"), hopAS("203.0.113.5", "AS29222"))
	save(now-3600, true, hopAS("198.51.100.1", "AS174"), hopAS("203.0.113.5", "AS29222"))
	save(now-60, true, hopAS("198.51.100.9", "AS3356"), hopAS("203.0.113.5", "AS29222"))

	g := store.BuildASGraph(id, "AS64500", "AS29222", 25)
	if g == nil {
		t.Fatal("a graph was expected")
	}
	if g.Traces != 3 {
		t.Errorf("3 traceroutes expected, got %d", g.Traces)
	}
	byASN := map[string]ASGraphNode{}
	for _, n := range g.Nodes {
		byASN[n.ASN] = n
	}
	for _, want := range []string{"AS64500", "AS174", "AS3356", "AS29222"} {
		if _, ok := byASN[want]; !ok {
			t.Errorf("%s missing from the graph", want)
		}
	}
	if !byASN["AS64500"].Origin || !byASN["AS29222"].Dest {
		t.Error("the two ends must be marked as such")
	}
	if byASN["AS64500"].Layer != 0 {
		t.Errorf("our AS starts the graph: layer %d", byASN["AS64500"].Layer)
	}
	if byASN["AS174"].Seen != 2 || byASN["AS3356"].Seen != 1 {
		t.Errorf("how often each transit was seen: %d and %d", byASN["AS174"].Seen, byASN["AS3356"].Seen)
	}
	var currentTransit string
	for _, e := range g.Edges {
		if e.Current && e.From == "AS64500" {
			currentTransit = e.To
		}
	}
	if currentTransit != "AS3356" {
		t.Errorf("the current path should go through AS3356, got %q", currentTransit)
	}
	// A traceroute that stops answering keeps an explicit break.
	save(now-30, false, hopAS("198.51.100.9", "AS3356"), Hop{Addr: "*", Sent: 3})
	g = store.BuildASGraph(id, "AS64500", "AS29222", 25)
	if !g.Incomplete {
		t.Error("an unmeasured stretch must be reported")
	}
	found := false
	for _, n := range g.Nodes {
		if n.Unknown {
			found = true
		}
	}
	if !found {
		t.Error("the break must appear as a node in the graph")
	}
}

// The RIS view is parsed from what RIPEstat returns: the prefix, its origin,
// and the upstreams the collectors see in front of it — ranked by how many
// peers saw each, with prepended AS ignored.
func TestRefreshRIS(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "network-info"):
			w.Write([]byte(`{"data":{"prefix":"185.31.40.0/22","asns":[{"asn":29222}]}}`))
		case strings.Contains(r.URL.Path, "looking-glass"):
			w.Write([]byte(`{"data":{"rrcs":[
			  {"peers":[{"as_path":"1299 3356 29222"},{"as_path":"6939 174 29222"},
			            {"as_path":"20932 3356 29222"},{"as_path":"3333 29222 29222"}]}]}}`))
		default:
			w.Write([]byte(`{"data":{}}`))
		}
	}))
	defer srv.Close()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	svc := NewASNService(store, nil, "")
	oldBase := ripestatBaseForTests
	ripestatBaseForTests = srv.URL + "/"
	defer func() { ripestatBaseForTests = oldBase }()

	svc.RefreshRIS("185.31.40.1")
	v, ok := svc.RISFor("185.31.40.1")
	if !ok {
		t.Fatal("the view should have been cached")
	}
	if v.Prefix != "185.31.40.0/22" || v.OriginASN != "AS29222" {
		t.Errorf("prefix and origin: %+v", v)
	}
	if !v.Announced || v.Peers != 4 {
		t.Errorf("four collector peers expected: %+v", v)
	}
	if len(v.Upstreams) == 0 || v.Upstreams[0] != "AS3356" {
		t.Errorf("the most seen upstream should come first: %v", v.Upstreams)
	}
	// A path ending "29222 29222" is prepending, not an upstream.
	for _, up := range v.Upstreams {
		if up == "AS29222" {
			t.Error("the origin must not be listed as its own upstream")
		}
	}
}

// A route event belongs to one target. It must appear on that target's page
// and on no other, which is what the scoped events endpoint decides.
func TestRouteEventsAreServedPerTarget(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cat, _ := store.CreateCategory("c", "C", "C", true)
	mk := func(slug, title string) int64 {
		id, err := store.CreateTarget(&Target{CategoryID: cat, Slug: slug, Title: title,
			Host: "192.0.2.9", Proto: "icmp", IntervalS: 60, Packets: 10, SpacingMs: 100,
			TimeoutMs: 1000, Public: true, Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	netflix, tv := mk("netflix", "Netflix"), mk("france-tv", "France TV")
	now := time.Now().Unix()
	store.cfg.Exec(`INSERT INTO events(ts_start,kind,title,body,scope,scope_id,public)
	                VALUES(?,'path','Route changed','to Netflix','target',?,1)`, now-60, netflix)
	store.cfg.Exec(`INSERT INTO events(ts_start,kind,title,body,scope,scope_id,public)
	                VALUES(?,'path','Route changed','to France TV','target',?,1)`, now-60, tv)
	store.cfg.Exec(`INSERT INTO events(ts_start,kind,title,scope,public)
	                VALUES(?,'maintenance','Instance maintenance','global',1)`, now-60)

	api := &API{store: store}
	get := func(q string) []Event {
		w := httptest.NewRecorder()
		api.events(w, httptest.NewRequest("GET", "/api/v1/events?"+q, nil))
		if w.Code != 200 {
			t.Fatalf("HTTP %d for %q: %s", w.Code, q, w.Body)
		}
		var out []Event
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	bodies := func(evs []Event) string {
		var b []string
		for _, e := range evs {
			b = append(b, e.Title+"/"+e.Body)
		}
		return strings.Join(b, "|")
	}

	// Netflix's page: its own route change, plus what concerns the instance.
	got := bodies(get("target=netflix"))
	if !strings.Contains(got, "to Netflix") {
		t.Errorf("the target's own route change is missing: %q", got)
	}
	if strings.Contains(got, "to France TV") {
		t.Errorf("another target's route change must not appear here: %q", got)
	}
	if !strings.Contains(got, "Instance maintenance") {
		t.Errorf("an instance-wide event still belongs on every page: %q", got)
	}
	// By numeric identifier too, which is what the page actually sends.
	if g := bodies(get(fmt.Sprintf("target=%d", tv))); !strings.Contains(g, "to France TV") ||
		strings.Contains(g, "to Netflix") {
		t.Errorf("lookup by id: %q", g)
	}
	// Without a target — the home page — no route event at all.
	if g := bodies(get("")); strings.Contains(g, "Route changed") {
		t.Errorf("a route event concerns one path, not the instance: %q", g)
	}
	// An unknown target is an error, not an empty list that could be mistaken
	// for "this target has no events".
	w := httptest.NewRecorder()
	api.events(w, httptest.NewRequest("GET", "/api/v1/events?target=nope", nil))
	if w.Code != 404 {
		t.Errorf("an unknown target should answer 404, got %d", w.Code)
	}
}

// A rotating name answers from a different machine at each pass. The two
// reference traceroutes then went to different places, so the AS path differs
// without anything having been rerouted: that is not a route change, and the
// route map must not mix the paths either.
func TestRotatingTargetIsNotARouteChange(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cat, _ := store.CreateCategory("c", "C", "C", true)
	id, err := store.CreateTarget(&Target{CategoryID: cat, Slug: "pool", Title: "NTP pool",
		Host: "fr.pool.ntp.org", Proto: "icmp", IntervalS: 60, Packets: 10, SpacingMs: 100,
		TimeoutMs: 1000, Public: true, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	save := func(ts int64, dest, transit, last string) {
		if err := store.SaveTraceroute(&Traceroute{TargetID: id, ProbeID: 1, TS: ts,
			Kind: "reference", Family: 4, Dest: dest, Reached: true,
			Hops: []Hop{hopAS("192.0.2.1", "AS64500"), hopAS("198.51.100.1", transit),
				hopAS(dest, last)}}); err != nil {
			t.Fatal(err)
		}
	}
	// Two references, two different servers of the pool, two different paths.
	save(now-7200, "203.0.113.10", "AS174", "AS2200")
	save(now-3600, "203.0.113.77", "AS3356", "AS1234")
	if detail, ok := store.RecentPathChange(id, now-86400); ok {
		t.Errorf("a different server is not a route change: %q", detail)
	}
	// The same server, a real transit change: that one is recorded.
	save(now-1800, "203.0.113.77", "AS174", "AS1234")
	if _, ok := store.RecentPathChange(id, now-86400); !ok {
		t.Error("a genuine change on the same address must still be seen")
	}
	// The map keeps one address and says how many traceroutes it left out.
	g := store.BuildASGraph(id, "AS64500", "AS1234", 25)
	if g == nil {
		t.Fatal("a graph was expected")
	}
	if g.OtherAddrs != 1 {
		t.Errorf("one traceroute went to another address: OtherAddrs=%d", g.OtherAddrs)
	}
	for _, n := range g.Nodes {
		if n.ASN == "AS2200" {
			t.Error("the path to another server must not appear in this map")
		}
	}
}
