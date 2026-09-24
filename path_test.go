package main

import (
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
