package main

import (
	"strings"
	"testing"
)

// Corroboration changes what an alert says, never whether it fires. These
// pin the wording rules, because the wording is the whole feature: an
// operator reads "the target itself" or "the path from the deciding probe"
// and goes to a different place.
func TestCorroborationSaysWhatTheOtherProbesSee(t *testing.T) {
	const tid, deciding, other1, other2 = int64(7), int64(1), int64(2), int64(3)
	view := func(status string) probeView {
		return probeView{status: map[int64]string{tid: status}, detail: map[int64]string{}}
	}

	cases := []struct {
		name     string
		status   string
		watchers []int64
		views    map[int64]probeView
		want     string
	}{
		{
			name:   "nobody else is measuring it, so there is nothing to say",
			status: "crit", watchers: []int64{deciding},
			views: map[int64]probeView{deciding: view("crit")},
			want:  "",
		},
		{
			name:   "a healthy target gets no note at all",
			status: "ok", watchers: []int64{deciding, other1},
			views: map[int64]probeView{deciding: view("ok"), other1: view("ok")},
			want:  "",
		},
		{
			name:   "everyone sees it, so it is the target",
			status: "crit", watchers: []int64{deciding, other1, other2},
			views: map[int64]probeView{deciding: view("crit"), other1: view("crit"), other2: view("warn")},
			want:  "the target itself",
		},
		{
			name:   "only the deciding probe sees it, so it is the path from there",
			status: "crit", watchers: []int64{deciding, other1, other2},
			views: map[int64]probeView{deciding: view("crit"), other1: view("ok"), other2: view("ok")},
			want:  "likely the path from the deciding probe",
		},
		{
			name:   "a split opinion is reported as a split, not resolved",
			status: "crit", watchers: []int64{deciding, other1, other2},
			views: map[int64]probeView{deciding: view("crit"), other1: view("crit"), other2: view("ok")},
			want:  "seen by 1 of 2 other probes",
		},
		{
			name:   "a probe with no data has no opinion and is not counted",
			status: "crit", watchers: []int64{deciding, other1, other2},
			views: map[int64]probeView{deciding: view("crit"), other1: view("crit"), other2: view("nodata")},
			want:  "confirmed by 1 other probe(s): the target itself",
		},
		{
			name:   "a probe under maintenance is not counted either",
			status: "crit", watchers: []int64{deciding, other1, other2},
			views: map[int64]probeView{deciding: view("crit"), other1: view("ok"), other2: view("maint")},
			want:  "likely the path from the deciding probe",
		},
		{
			name:   "a target under maintenance asks nobody anything",
			status: "maint", watchers: []int64{deciding, other1},
			views: map[int64]probeView{deciding: view("maint"), other1: view("crit")},
			want:  "",
		},
		{
			name:   "no data on the deciding probe is not a question either",
			status: "nodata", watchers: []int64{deciding, other1},
			views: map[int64]probeView{deciding: view("nodata"), other1: view("crit")},
			want:  "",
		},
		{
			name:   "a probe that produced no view at all is simply absent",
			status: "crit", watchers: []int64{deciding, other1, other2},
			views: map[int64]probeView{deciding: view("crit"), other1: view("crit")},
			want:  "confirmed by 1 other probe(s): the target itself",
		},
	}

	for _, c := range cases {
		got := corroborate(tid, deciding, c.status, c.watchers, c.views)
		if c.want == "" {
			if got != "" {
				t.Errorf("%s: said %q, want nothing", c.name, got)
			}
			continue
		}
		if !strings.Contains(got, c.want) {
			t.Errorf("%s: said %q, want it to contain %q", c.name, got, c.want)
		}
	}
}

// The alerting path must keep working on an instance with one probe and no
// assignments, which is every instance that exists today.
func TestGatherOnASingleProbeInstance(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	mkTarget(t, store, "a")
	mkTarget(t, store, "b")
	local, err := store.ProbeID("local", "Local", "here", true)
	if err != nil {
		t.Fatal(err)
	}

	primary, err := store.TargetPrimaries(local)
	if err != nil {
		t.Fatal(err)
	}
	if len(primary) != 2 {
		t.Fatalf("%d targets have a primary, want 2", len(primary))
	}
	for tid, pid := range primary {
		if pid != local {
			t.Errorf("target %d is decided by probe %d, want the local one", tid, pid)
		}
	}

	// Every target is watched by the local probe even though nothing assigns
	// it: it measures everything and is in no assignment row.
	watchers, err := store.TargetWatchers(local)
	if err != nil {
		t.Fatal(err)
	}
	for tid, ws := range watchers {
		if len(ws) != 1 || ws[0] != local {
			t.Errorf("target %d is watched by %v, want only the local probe", tid, ws)
		}
	}
}

// A remote probe assigned a target joins that target's watchers, and a
// disabled one drops out: an opinion from something that is not running is
// not an opinion.
func TestWatchersFollowTheAssignment(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	a := mkTarget(t, store, "a")
	local, err := store.ProbeID("local", "Local", "here", true)
	if err != nil {
		t.Fatal(err)
	}
	p, err := store.CreateProbe("remote-1", "Remote 1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetProbeTargets(p.ID, []int64{a}); err != nil {
		t.Fatal(err)
	}

	has := func(tid, pid int64) bool {
		w, err := store.TargetWatchers(local)
		if err != nil {
			t.Fatal(err)
		}
		for _, x := range w[tid] {
			if x == pid {
				return true
			}
		}
		return false
	}
	if !has(a, p.ID) {
		t.Error("an assigned remote probe is not among the target's watchers")
	}
	if err := store.SetProbeEnabled(p.ID, false); err != nil {
		t.Fatal(err)
	}
	if has(a, p.ID) {
		t.Error("a disabled probe still counts as watching the target")
	}
	if !has(a, local) {
		t.Error("the local probe stopped watching the target")
	}
}
