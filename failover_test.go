package main

import (
	"testing"
)

// setup builds an instance with a local probe, two remote ones and a target
// all three may measure, which is the shape every test below needs.
func failoverFixture(t *testing.T) (*Store, int64, int64, Probe, Probe) {
	t.Helper()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	tid := mkTarget(t, store, "a")
	local, err := store.ProbeID("local", "Local", "here", true)
	if err != nil {
		t.Fatal(err)
	}
	first, err := store.CreateProbe("lyon-1", "Lyon 1", "Lyon, FR")
	if err != nil {
		t.Fatal(err)
	}
	second, err := store.CreateProbe("paris-1", "Paris 1", "Paris, FR")
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range []*Probe{first, second} {
		if err := store.SetProbeTargets(p.ID, []int64{tid}); err != nil {
			t.Fatal(err)
		}
	}
	return store, tid, local, *first, *second
}

func seen(t *testing.T, store *Store, id, at int64) {
	t.Helper()
	store.cfg.Exec(`UPDATE probes SET last_seen_at=? WHERE id=?`, at, id)
}

// An instance that never writes a backup list must behave exactly as it did:
// the chosen probe decides, silent or not. This is the case every existing
// installation is in, and the one a regression would hit first.
func TestWithoutBackupsNothingChanges(t *testing.T) {
	store, tid, local, first, _ := failoverFixture(t)
	if err := store.SetTargetPrimaryProbe(tid, first.ID); err != nil {
		t.Fatal(err)
	}
	const now = int64(1_000_000)
	seen(t, store, first.ID, now-probeSilentAfter*10) // long silent

	eff, err := store.EffectivePrimaries(local, now)
	if err != nil {
		t.Fatal(err)
	}
	if eff[tid] != first.ID {
		t.Fatalf("target decided by probe %d, want %d: with no list there is nowhere to fail over to",
			eff[tid], first.ID)
	}
}

func TestASilentPrimaryHandsOverAndTakesBack(t *testing.T) {
	store, tid, local, first, second := failoverFixture(t)
	if err := store.SetTargetPrimaryProbe(tid, first.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.SetTargetBackups(tid, []int64{second.ID}); err != nil {
		t.Fatal(err)
	}
	const now = int64(1_000_000)

	// Both reporting: the chosen one decides.
	seen(t, store, first.ID, now)
	seen(t, store, second.ID, now)
	eff, _ := store.EffectivePrimaries(local, now)
	if eff[tid] != first.ID {
		t.Fatalf("with both reporting, probe %d decides, want the primary %d", eff[tid], first.ID)
	}

	// The primary goes quiet: the backup takes it.
	seen(t, store, first.ID, now-probeSilentAfter-1)
	eff, _ = store.EffectivePrimaries(local, now)
	if eff[tid] != second.ID {
		t.Fatalf("with the primary silent, probe %d decides, want the backup %d", eff[tid], second.ID)
	}

	// It comes back, but a probe that has just returned is not trusted with
	// what it decides: a flapping link would otherwise hand the target back
	// and forth every few minutes, and every handover is a moment where two
	// views of the same target disagree.
	seen(t, store, first.ID, now)
	store.MarkProbeBack(first.ID, now)
	eff, _ = store.EffectivePrimaries(local, now)
	if eff[tid] != second.ID {
		t.Fatalf("a probe back for an instant took its target back from %d", second.ID)
	}
	eff, _ = store.EffectivePrimaries(local, now+probeReclaimAfter-1)
	if eff[tid] != second.ID {
		t.Error("the primary reclaimed one second before it was due to")
	}

	// And once it has been back long enough, it takes them back.
	eff, _ = store.EffectivePrimaries(local, now+probeReclaimAfter)
	if eff[tid] != first.ID {
		t.Fatalf("after the hysteresis, probe %d decides, want the primary %d", eff[tid], first.ID)
	}
}

// Order is the operator's, and it has to be honoured: naming a first and a
// second fallback and getting the second is worse than having no list.
func TestTheFirstReportingBackupWins(t *testing.T) {
	store, tid, local, first, second := failoverFixture(t)
	third, err := store.CreateProbe("lille-1", "Lille 1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetProbeTargets(third.ID, []int64{tid}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetTargetPrimaryProbe(tid, first.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.SetTargetBackups(tid, []int64{second.ID, third.ID}); err != nil {
		t.Fatal(err)
	}
	const now = int64(1_000_000)
	seen(t, store, first.ID, now-probeSilentAfter-1)
	seen(t, store, second.ID, now)
	seen(t, store, third.ID, now)

	eff, _ := store.EffectivePrimaries(local, now)
	if eff[tid] != second.ID {
		t.Fatalf("probe %d decides, want the first backup %d", eff[tid], second.ID)
	}

	// The first backup goes too: the second one stands in.
	seen(t, store, second.ID, now-probeSilentAfter-1)
	eff, _ = store.EffectivePrimaries(local, now)
	if eff[tid] != third.ID {
		t.Fatalf("probe %d decides, want the second backup %d", eff[tid], third.ID)
	}

	// Everything is silent: the target stays with its primary. Somebody has
	// to own it, and leaving it owned by a probe that is not answering is
	// what keeps its incident open rather than quietly closing it.
	seen(t, store, third.ID, now-probeSilentAfter-1)
	eff, _ = store.EffectivePrimaries(local, now)
	if eff[tid] != first.ID {
		t.Fatalf("with everything silent, probe %d decides, want the primary %d", eff[tid], first.ID)
	}
}

// A list that names a probe which could never decide the target would skip
// over a dead entry nobody can see is dead.
func TestBackupsMustBeAbleToMeasureTheTarget(t *testing.T) {
	store, tid, _, first, second := failoverFixture(t)
	other := mkTarget(t, store, "b")

	if err := store.SetTargetBackups(other, []int64{first.ID}); err == nil {
		t.Error("a probe not assigned the target was accepted as its backup")
	}
	if err := store.SetTargetBackups(tid, []int64{first.ID, first.ID}); err == nil {
		t.Error("the same probe was accepted twice in one list")
	}
	if err := store.SetTargetBackups(tid, []int64{999999}); err == nil {
		t.Error("an unknown probe was accepted")
	}
	if err := store.SetProbeEnabled(second.ID, false); err != nil {
		t.Fatal(err)
	}
	if err := store.SetTargetBackups(tid, []int64{second.ID}); err == nil {
		t.Error("a disabled probe was accepted as a backup")
	}

	// A refused list leaves nothing behind, so an operator never believes
	// they saved something they did not.
	got, err := store.TargetBackups(tid)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("a refused list left %d entries", len(got))
	}
}
