package main

import (
	"strings"
	"testing"
)

type sentAlert struct{ subject, body string }

func alertSetup(t *testing.T) (*Alerter, *Store, int64, *[]sentAlert, *[]int64, *int64) {
	t.Helper()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	cat, _ := store.CreateCategory("c", "C", "C", true)
	id, err := store.CreateTarget(&Target{CategoryID: cat, Slug: "t", Title: "Transit",
		Host: "192.0.2.1", Proto: "icmp", IntervalS: 60, Packets: 10, SpacingMs: 100,
		TimeoutMs: 1000, Public: true, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	clock := int64(1_000_000)
	sent := []sentAlert{}
	traced := []int64{}
	a := NewAlerter(store)
	a.now = func() int64 { return clock }
	a.send = func(_ AlertConfig, s, b string) error {
		sent = append(sent, sentAlert{s, b})
		return nil
	}
	a.trace = func(id int64) { traced = append(traced, id) }
	cfg := defaultAlertConfig()
	cfg.Enabled, cfg.AfterMinutes, cfg.RepeatHours = true, 5, 6
	cfg.Recipients = "noc@example.net"
	if err := store.SetAlertConfig(cfg); err != nil {
		t.Fatal(err)
	}
	return a, store, id, &sent, &traced, &clock
}

// Nobody is woken up for a blip, everybody is for a sustained incident, and
// only once — with a traceroute asked for the moment it opened.
func TestAlertOnSustainedIncidentOnly(t *testing.T) {
	a, _, id, sent, traced, clock := alertSetup(t)
	crit := map[int64]string{id: "crit"}
	ok := map[int64]string{id: "ok"}
	det := map[int64]string{id: "42.0 % packet loss"}

	a.Tick(crit, det) // incident opens
	if len(*traced) != 1 || (*traced)[0] != id {
		t.Errorf("a traceroute must be requested when the incident opens: %v", *traced)
	}
	*clock += 2 * 60
	a.Tick(crit, det)
	if len(*sent) != 0 {
		t.Errorf("two minutes is not sustained: %d alert(s) sent", len(*sent))
	}
	*clock += 4 * 60 // six minutes in total
	a.Tick(crit, det)
	if len(*sent) != 1 {
		t.Fatalf("a sustained incident must alert once: %d", len(*sent))
	}
	if !strings.Contains((*sent)[0].subject, "Transit") ||
		!strings.Contains((*sent)[0].body, "42.0 % packet loss") {
		t.Errorf("unhelpful alert: %q / %q", (*sent)[0].subject, (*sent)[0].body)
	}
	*clock += 10 * 60
	a.Tick(crit, det)
	if len(*sent) != 1 {
		t.Errorf("the same incident must not alert twice: %d", len(*sent))
	}
	// Recovery closes the incident and says so.
	*clock += 60
	a.Tick(ok, nil)
	if len(*sent) != 2 || !strings.Contains((*sent)[1].subject, "Recovered") {
		t.Fatalf("a recovery notice was expected: %+v", *sent)
	}
	// A new incident within the silence window stays quiet.
	*clock += 30 * 60
	a.Tick(crit, det)
	*clock += 10 * 60
	a.Tick(crit, det)
	if len(*sent) != 2 {
		t.Errorf("the silence window was not respected: %d alerts", len(*sent))
	}
	// Past that window, it alerts again.
	*clock += 7 * 3600
	a.Tick(crit, det)
	if len(*sent) != 3 {
		t.Errorf("after the silence window a new alert is expected: %d", len(*sent))
	}
}

// Alerting off: incidents are still recorded, nothing is sent.
func TestAlertsDisabled(t *testing.T) {
	a, store, id, sent, _, clock := alertSetup(t)
	cfg := store.AlertConfig()
	cfg.Enabled = false
	store.SetAlertConfig(cfg)
	crit := map[int64]string{id: "crit"}
	a.Tick(crit, nil)
	*clock += 30 * 60
	a.Tick(crit, nil)
	if len(*sent) != 0 {
		t.Errorf("nothing must be sent when alerting is off: %d", len(*sent))
	}
	inc, err := a.Incidents(5)
	if err != nil || len(inc) != 1 || inc[0].Title != "Transit" {
		t.Errorf("the incident must still be recorded: %+v %v", inc, err)
	}
}

func TestSplitList(t *testing.T) {
	got := splitList(" noc@example.net, second@example.net;third@example.net ")
	if len(got) != 3 || got[2] != "third@example.net" {
		t.Errorf("splitList gave %v", got)
	}
	if len(splitList("   ")) != 0 {
		t.Error("an empty list must stay empty")
	}
}
