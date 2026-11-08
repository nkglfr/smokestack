package main

import (
	"testing"
)

// A traceroute is only meaningful alongside the vantage point it was run
// from. Showing every probe's on one page, undifferentiated, is how you end
// up comparing a path from Lyon with a path from Paris and concluding
// something about neither.
func TestTraceroutesCanBeNarrowedToOneProbe(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	tid := mkTarget(t, store, "a")

	const fromLyon, fromParis = int64(2), int64(3)
	for _, tr := range []*Traceroute{
		{TargetID: tid, ProbeID: fromLyon, TS: 100, Kind: "reference", Dest: "198.51.100.1"},
		{TargetID: tid, ProbeID: fromLyon, TS: 200, Kind: "reference", Dest: "198.51.100.1"},
		{TargetID: tid, ProbeID: fromParis, TS: 300, Kind: "reference", Dest: "198.51.100.1"},
	} {
		if err := store.SaveTraceroute(tr); err != nil {
			t.Fatal(err)
		}
	}

	// Zero is every probe, which is what every caller that does not offer a
	// choice relies on. That is the part a regression would break silently.
	all, err := store.TraceroutesFrom(tid, 0, nil, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 3 {
		t.Fatalf("asking for any probe gave %d traceroutes, want 3", len(all))
	}
	same, err := store.Traceroutes(tid, nil, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(same) != len(all) {
		t.Errorf("the unfiltered call gave %d, the explicit any-probe call %d: they must agree",
			len(same), len(all))
	}

	one, err := store.TraceroutesFrom(tid, fromLyon, nil, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 2 {
		t.Fatalf("asking for one probe gave %d traceroutes, want 2", len(one))
	}
	for _, tr := range one {
		if tr.ProbeID != fromLyon {
			t.Errorf("a traceroute from probe %d came back for probe %d", tr.ProbeID, fromLyon)
		}
	}

	// A probe that ran none gets none, rather than falling back to everyone
	// else's — a fallback would be the silent wrong answer.
	none, err := store.TraceroutesFrom(tid, 99, nil, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(none) != 0 {
		t.Errorf("a probe that ran no traceroutes was given %d", len(none))
	}

	// The kind filter still applies alongside the probe one.
	kinds, err := store.TraceroutesFrom(tid, fromParis, []string{"reference"}, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(kinds) != 1 {
		t.Errorf("filtering by probe and kind gave %d, want 1", len(kinds))
	}
}
