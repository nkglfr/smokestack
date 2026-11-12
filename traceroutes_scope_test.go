package main

import (
	"testing"
	"time"
)

// Disabling a probe is how an operator says it is no longer part of what the
// instance shows, and /api/v1/probes has always honoured that. The traceroute
// list did not: with no ?probe= it answered from every probe, and every entry
// carries its probe_id, so one endpoint published the existence of a probe
// the other deliberately hid.
func TestATraceroutePageDoesNotPublishADisabledProbe(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	cat, _ := store.CreateCategory("c", "C", "C", true)
	target, err := store.CreateTarget(&Target{CategoryID: cat, Slug: "x", Title: "X",
		Host: "192.0.2.1", Proto: "icmp", IntervalS: 60, Packets: 5,
		SpacingMs: 100, TimeoutMs: 1000, Public: true, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	on, err := store.CreateProbe("on", "On", "")
	if err != nil {
		t.Fatal(err)
	}
	off, err := store.CreateProbe("off", "Off", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetProbeEnabled(off.ID, false); err != nil {
		t.Fatal(err)
	}

	now := time.Now().Unix()
	add := func(probeID, ts int64) {
		if _, err := store.mxw.Exec(
			`INSERT INTO traceroutes(target_id,probe_id,ts,kind,reason,family,dest,reached,hops)
			 VALUES(?,?,?,'reference','',4,'192.0.2.1',1,'[]')`, target, probeID, ts); err != nil {
			t.Fatal(err)
		}
	}
	// The disabled probe's paths are the most recent, which is what made this
	// worth a test: a filter applied after the LIMIT would have returned a
	// short page here rather than a filtered one.
	for i := 0; i < 5; i++ {
		add(on.ID, now-int64(1000+i*60))
	}
	for i := 0; i < 5; i++ {
		add(off.ID, now-int64(i*60))
	}

	hidden, err := store.DisabledProbeIDs()
	if err != nil {
		t.Fatal(err)
	}
	if len(hidden) != 1 || hidden[0] != off.ID {
		t.Fatalf("DisabledProbeIDs = %v, want just the disabled probe %d", hidden, off.ID)
	}

	// An operator reads everything: nil is no restriction.
	all, err := store.TraceroutesOf(target, 0, nil, 40, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 10 {
		t.Errorf("unrestricted: %d paths, want 10", len(all))
	}

	// A visitor reads everything except the disabled probe's, and gets all
	// five of the rest rather than whatever survived a post-filter.
	vis, err := store.TraceroutesOf(target, 0, nil, 40, hidden)
	if err != nil {
		t.Fatal(err)
	}
	if len(vis) != 5 {
		t.Fatalf("visitor: %d paths, want 5", len(vis))
	}
	for _, tr := range vis {
		if tr.ProbeID == off.ID {
			t.Errorf("a path from the disabled probe %d reached a visitor", off.ID)
		}
	}

	// The limit still means what it says. Asking for three of the enabled
	// probe's paths returns three, not three-minus-the-filtered.
	three, err := store.TraceroutesOf(target, 0, nil, 3, hidden)
	if err != nil {
		t.Fatal(err)
	}
	if len(three) != 3 {
		t.Errorf("limit 3 returned %d paths: the filter is being applied after the limit",
			len(three))
	}

	// An empty deny list hides nothing. This is the case my first version got
	// backwards: it allow-listed the enabled probes, so an instance that had
	// never enrolled one — where the probes table is empty and every path
	// carries the local probe — published nothing at all. Two existing tests
	// caught it, and this asserts it directly.
	none, err := store.TraceroutesOf(target, 0, nil, 40, []int64{})
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 10 {
		t.Errorf("an empty deny list returned %d paths, want all 10", len(none))
	}

	// And the old entry point is unchanged for every existing caller.
	same, err := store.TraceroutesFrom(target, 0, nil, 40)
	if err != nil {
		t.Fatal(err)
	}
	if len(same) != 10 {
		t.Errorf("TraceroutesFrom returned %d paths, want the unrestricted 10", len(same))
	}
}

// The case that broke two existing tests: an instance with no row in the
// probes table at all. Every traceroute there belongs to the local probe,
// which is deliberately absent from that table, so a filter built from the
// enabled set hides the lot. A visitor must see them.
func TestAnInstanceWithNoEnrolledProbeStillPublishesItsPaths(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cat, _ := store.CreateCategory("c", "C", "C", true)
	target, err := store.CreateTarget(&Target{CategoryID: cat, Slug: "x", Title: "X",
		Host: "192.0.2.1", Proto: "icmp", IntervalS: 60, Packets: 5,
		SpacingMs: 100, TimeoutMs: 1000, Public: true, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().Unix()
	// probe_id 0 is the instance's own local probe, and what a traceroute
	// recorded before probes existed carries.
	for _, pid := range []int64{0, 1} {
		if _, err := store.mxw.Exec(
			`INSERT INTO traceroutes(target_id,probe_id,ts,kind,reason,family,dest,reached,hops)
			 VALUES(?,?,?,'reference','',4,'192.0.2.1',1,'[]')`, target, pid, now-pid-1); err != nil {
			t.Fatal(err)
		}
	}

	hidden, err := store.DisabledProbeIDs()
	if err != nil {
		t.Fatal(err)
	}
	if len(hidden) != 0 {
		t.Fatalf("a store with no probe row reports %v as disabled", hidden)
	}
	got, err := store.TraceroutesOf(target, 0, nil, 40, hidden)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("a visitor sees %d paths on an instance with no enrolled probe, want 2", len(got))
	}
}
