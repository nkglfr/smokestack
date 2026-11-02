package main

import (
	"strings"
	"testing"
)

// The defect this pins: a probe that stopped reporting left its targets with
// no data, and no data closed their incidents and announced them as
// recovered. With probes on many machines, one of them dying silently
// declared everything it decided to be fine.
func TestNoDataNeverClosesAnIncident(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	tid := mkTarget(t, store, "a")

	now := int64(1000)
	var subjects []string
	a := &Alerter{store: store, now: func() int64 { return now },
		send:  func(_ AlertConfig, subject, _ string) error { subjects = append(subjects, subject); return nil },
		trace: func(int64) {}}
	alertSchema(store)
	// One minute, stated rather than left at zero: AlertConfig() reads a
	// non-positive value as unset and puts the shipped five minutes back, so
	// a test asking for "alert immediately" would quietly be asking for five.
	store.SetAlertConfig(AlertConfig{Enabled: true, AfterMinutes: 1, RepeatHours: 6, Recovery: true})

	a.Tick(map[int64]string{tid: "crit"}, map[int64]string{tid: "100 % packet loss"})
	if _, open := a.openFor(tid); !open {
		t.Fatal("a critical target opened no incident")
	}
	// The opening tick records the incident; the next one is what alerts. A
	// recovery notice is only owed where an alert was actually sent, so this
	// second tick is what makes the end of the test meaningful.
	now += 120
	a.Tick(map[int64]string{tid: "crit"}, map[int64]string{tid: "100 % packet loss"})
	if inc, _ := a.openFor(tid); inc == nil || inc.NotifiedAt == 0 {
		t.Fatal("a sustained critical target sent no alert")
	}
	before := len(subjects)

	// The probe goes quiet. Nothing is measured, so nothing is known — which
	// is not the same as knowing it is fine.
	now += 600
	a.Tick(map[int64]string{tid: "nodata"}, map[int64]string{})
	if _, open := a.openFor(tid); !open {
		t.Fatal("missing data closed the incident")
	}
	for _, s := range subjects[before:] {
		if strings.Contains(s, "Recovered") {
			t.Fatalf("missing data announced a recovery: %q", s)
		}
	}

	// A planned maintenance is not a recovery either.
	now += 600
	a.Tick(map[int64]string{tid: "maint"}, map[int64]string{})
	if _, open := a.openFor(tid); !open {
		t.Fatal("a maintenance window closed the incident")
	}

	// A real measurement is what closes it, and that still works.
	now += 600
	a.Tick(map[int64]string{tid: "ok"}, map[int64]string{})
	if _, open := a.openFor(tid); open {
		t.Fatal("a target measured healthy did not close its incident")
	}
	found := false
	for _, s := range subjects[before:] {
		if strings.Contains(s, "Recovered") {
			found = true
		}
	}
	if !found {
		t.Errorf("no recovery notice when the target actually recovered, subjects: %v", subjects)
	}
}

// A probe that has stopped reporting has to say so itself, because the
// targets it decides have gone quiet rather than wrong.
func TestASilentProbeAlertsAndComesBack(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	alertSchema(store)
	store.SetAlertConfig(AlertConfig{Enabled: true, AfterMinutes: 0, RepeatHours: 6, Recovery: true})

	tid := mkTarget(t, store, "a")
	p, err := store.CreateProbe("remote-1", "Remote 1", "Lyon, FR")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetProbeTargets(p.ID, []int64{tid}); err != nil {
		t.Fatal(err)
	}

	now := int64(1_000_000)
	var subjects []string
	a := &Alerter{store: store, now: func() int64 { return now },
		send:  func(_ AlertConfig, subject, _ string) error { subjects = append(subjects, subject); return nil },
		trace: func(int64) {}}

	// A probe nobody has ever heard from is not silent, it is new.
	if n := a.checkProbes(now); n != 0 {
		t.Errorf("a probe that never reported raised %d alert(s)", n)
	}

	store.SeenProbeFrom(p.ID, "198.51.100.9")
	// SeenProbeFrom stamps the wall clock, so the test's clock is moved past
	// it rather than the other way round.
	store.cfg.Exec(`UPDATE probes SET last_seen_at=? WHERE id=?`, now, p.ID)

	if n := a.checkProbes(now + probeSilentAfter - 1); n != 0 {
		t.Errorf("a probe raised %d alert(s) one second before the threshold", n)
	}
	if n := a.checkProbes(now + probeSilentAfter); n != 1 {
		t.Fatalf("a silent probe raised %d alerts, want 1", n)
	}
	// And only once, however long it stays quiet.
	if n := a.checkProbes(now + probeSilentAfter*10); n != 0 {
		t.Errorf("a probe that is still silent alerted again (%d)", n)
	}

	store.cfg.Exec(`UPDATE probes SET last_seen_at=? WHERE id=?`, now+probeSilentAfter*10, p.ID)
	if n := a.checkProbes(now + probeSilentAfter*10); n != 1 {
		t.Fatalf("a probe coming back sent %d notices, want 1", n)
	}
	if n := a.checkProbes(now + probeSilentAfter*10); n != 0 {
		t.Errorf("a healthy probe keeps talking about itself (%d)", n)
	}

	if len(subjects) < 2 || !strings.Contains(subjects[0], "Probe silent") ||
		!strings.Contains(subjects[1], "Probe back") {
		t.Errorf("subjects were %v", subjects)
	}
}

// A probe with nothing to measure never writes anything, so its last_seen_at
// never moves. Alerting on that would be a false alarm about a probe that is
// perfectly healthy and simply idle.
func TestAProbeWithNothingToMeasureIsNotCalledSilent(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	alertSchema(store)
	store.SetAlertConfig(AlertConfig{Enabled: true, Recovery: true})

	p, err := store.CreateProbe("remote-1", "Remote 1", "")
	if err != nil {
		t.Fatal(err)
	}
	store.cfg.Exec(`UPDATE probes SET last_seen_at=? WHERE id=?`, 1, p.ID)

	a := &Alerter{store: store, now: func() int64 { return 1_000_000 },
		send: func(AlertConfig, string, string) error { return nil }, trace: func(int64) {}}
	if n := a.checkProbes(1_000_000); n != 0 {
		t.Errorf("a probe assigned nothing was reported silent (%d)", n)
	}

	// Give it something to measure and it becomes answerable for silence.
	tid := mkTarget(t, store, "a")
	if err := store.SetProbeTargets(p.ID, []int64{tid}); err != nil {
		t.Fatal(err)
	}
	if n := a.checkProbes(1_000_000); n != 1 {
		t.Errorf("a probe with an assignment and no news raised %d alerts, want 1", n)
	}
}
