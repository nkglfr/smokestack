package main

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// The explicit list does not survive a growing instance: a target created
// later is measured by no remote probe until every probe's list is updated by
// hand. This is the option that covers the ordinary case, and the point of
// these tests is that it reaches every place the restriction is enforced —
// six of them — rather than only the one it was written for.
func TestAllTargetsReachesEveryEnforcementPoint(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	a := mkTarget(t, store, "a")
	b := mkTarget(t, store, "b")
	local, err := store.ProbeID("local", "Local", "here", true)
	if err != nil {
		t.Fatal(err)
	}
	p, err := store.CreateProbe("remote-1", "Remote 1", "")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := store.IssueProbeToken(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetProbeTargets(p.ID, []int64{a}); err != nil {
		t.Fatal(err)
	}

	// 1. The rule itself. nil is "everything", never "nothing" — the
	// distinction every caller depends on.
	set, err := store.ProbeAllowedTargets(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if set == nil || !set[a] || set[b] {
		t.Fatalf("with a list, allowed is %v", set)
	}
	if err := store.SetProbeAllTargets(p.ID, true); err != nil {
		t.Fatal(err)
	}
	set, err = store.ProbeAllowedTargets(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if set != nil {
		t.Fatalf("with every target, allowed is %v, want nil", set)
	}

	// 2. The probe protocol: the target list it is served.
	mux := http.NewServeMux()
	ProbeRoutes(mux, store, NewTargetCache(store), NewWriter(store, nil))
	r := httptest.NewRequest("GET", "/probe/v1/targets", nil)
	r.Header.Set("Authorization", "Bearer "+tok)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("fetching targets: %d", w.Code)
	}

	// 3. Writing: a measurement for the target it was never explicitly given.
	post := httptest.NewRequest("POST", "/probe/v1/measurements",
		strings.NewReader(`[{"target_id":`+strconv.FormatInt(b, 10)+`,"ts":1,"sent":1,"lost":0}]`))
	post.Header.Set("Authorization", "Bearer "+tok)
	pw := httptest.NewRecorder()
	mux.ServeHTTP(pw, post)
	if pw.Code != http.StatusOK {
		t.Fatalf("submitting a measurement: %d", pw.Code)
	}
	if got := pw.Body.String(); !strings.Contains(got, `"refused":0`) {
		t.Errorf("a probe measuring everything had a measurement refused: %s", got)
	}

	// 4. Correlation: it watches every target, like the local probe.
	watchers, err := store.TargetWatchers(local)
	if err != nil {
		t.Fatal(err)
	}
	for _, tid := range []int64{a, b} {
		found := false
		for _, x := range watchers[tid] {
			if x == p.ID {
				found = true
			}
		}
		if !found {
			t.Errorf("target %d is not watched by a probe measuring everything", tid)
		}
	}

	// 5. The silence alert: it is expected to report, with no assignment row
	// of its own to prove it.
	if err := store.SetProbeTargets(p.ID, []int64{}); err != nil {
		t.Fatal(err)
	}
	store.cfg.Exec(`UPDATE probes SET last_seen_at=1 WHERE id=?`, p.ID)
	reporting, err := store.ReportingProbes()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, x := range reporting {
		if x.ID == p.ID {
			found = true
		}
	}
	if !found {
		t.Error("a probe measuring everything is not expected to report")
	}

	// 6. Primary probe: it can decide a target it holds in no list.
	if err := store.SetTargetPrimaryProbe(b, p.ID); err != nil {
		t.Errorf("a probe measuring everything was refused as a primary: %v", err)
	}
}

// Turning the option off has to give back what was there. An operator who
// tries "everything" for an afternoon should not come back to an empty list
// they never asked to lose.
func TestTurningAllTargetsOffRestoresTheList(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	a := mkTarget(t, store, "a")
	mkTarget(t, store, "b")
	p, err := store.CreateProbe("remote-1", "Remote 1", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetProbeTargets(p.ID, []int64{a}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetProbeAllTargets(p.ID, true); err != nil {
		t.Fatal(err)
	}
	if err := store.SetProbeAllTargets(p.ID, false); err != nil {
		t.Fatal(err)
	}
	set, err := store.ProbeAllowedTargets(p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if set == nil || !set[a] || len(set) != 1 {
		t.Fatalf("after switching back, allowed is %v, want the list it had", set)
	}
}

// The local probe measures everything by construction and is read from none
// of this. An option on it would be a setting that looks meaningful and
// changes nothing.
func TestTheLocalProbeTakesNoAllTargetsOption(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	id, err := store.ProbeID("local", "Local", "here", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetProbeAllTargets(id, true); err == nil {
		t.Error("the local probe accepted the option")
	}
}
