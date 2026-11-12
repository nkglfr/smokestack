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

	enabled, err := store.EnabledProbeIDs()
	if err != nil {
		t.Fatal(err)
	}
	if len(enabled) != 1 || enabled[0] != on.ID {
		t.Fatalf("EnabledProbeIDs = %v, want just the enabled probe %d", enabled, on.ID)
	}

	// An operator reads everything: nil is no restriction.
	all, err := store.TraceroutesOf(target, 0, nil, 40, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 10 {
		t.Errorf("unrestricted: %d paths, want 10", len(all))
	}

	// A visitor reads only the enabled probe's, and gets all five of them
	// rather than whatever survived a post-filter.
	vis, err := store.TraceroutesOf(target, 0, nil, 40, enabled)
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
	three, err := store.TraceroutesOf(target, 0, nil, 3, enabled)
	if err != nil {
		t.Fatal(err)
	}
	if len(three) != 3 {
		t.Errorf("limit 3 returned %d paths: the filter is being applied after the limit",
			len(three))
	}

	// An instance with no enabled probe publishes no path. That is not the
	// same question as "any probe", which is what an empty list used to mean
	// if it were handled like nil.
	none, err := store.TraceroutesOf(target, 0, nil, 40, []int64{})
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Errorf("an empty allow list returned %d paths, want none", len(none))
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
