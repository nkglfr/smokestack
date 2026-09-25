package main

import (
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
	// The title drawn on the graph stays short; the paths live in the body.
	var title, body string
	store.cfg.QueryRow(`SELECT title,COALESCE(body,'') FROM events WHERE kind='path'
	                    ORDER BY ts_start DESC LIMIT 1`).Scan(&title, &body)
	if len(title) > 60 || !strings.Contains(title, "route changed") {
		t.Errorf("the event title must stay short and readable: %q", title)
	}
	if !strings.Contains(body, "AS174") || !strings.Contains(body, "AS3356") {
		t.Errorf("the paths must be in the body: %q", body)
	}
	if !strings.Contains(detail, "Transit Paris") {
		t.Errorf("the event should name the target: %q", detail)
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
