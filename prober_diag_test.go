package main

import (
	"strings"
	"testing"
)

// A failing target must say why: an operator should never be left with
// 100 % loss and no explanation (issue #9).
func TestFailureReasons(t *testing.T) {
	p := &Prober{res: newResolver(), id: 4242}
	cases := []struct {
		name   string
		target *Target
		want   []string
	}{
		{"unknown name", &Target{Host: "no-such-host.invalid", Proto: "icmp", Family: 0, Packets: 3},
			[]string{"cannot resolve", "no-such-host.invalid"}},
		{"IPv6 address without an IPv6 socket", &Target{Host: "2001:db8::1", Proto: "icmp", Family: 6, Packets: 3},
			[]string{"IPv6", "not available"}},
		{"IPv4 address forced to IPv6", &Target{Host: "192.0.2.1", Proto: "icmp", Family: 6, Packets: 3},
			[]string{"does not match family"}},
	}
	for _, c := range cases {
		_, msg := p.runICMP(c.target)
		if msg == "" {
			t.Errorf("%s: no reason given", c.name)
			continue
		}
		for _, want := range c.want {
			if !strings.Contains(msg, want) {
				t.Errorf("%s: reason %q lacks %q", c.name, msg, want)
			}
		}
	}
}

// The reason is stored per target, and cleared once it answers again.
func TestTargetErrorStored(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cat, _ := store.CreateCategory("c", "C", "C", true)
	id, err := store.CreateTarget(&Target{CategoryID: cat, Slug: "x", Title: "X", Host: "192.0.2.1",
		Proto: "icmp", IntervalS: 60, Packets: 5, SpacingMs: 100, TimeoutMs: 1000, Public: true, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	fail := queuedMeasure{m: Measurement{TargetID: id, ProbeID: 1, TS: 1000, Sent: 5, Lost: 5,
		Err: "no reply to 5 ICMP echo requests"}}
	if err := store.RecordBatch([]queuedMeasure{fail}); err != nil {
		t.Fatal(err)
	}
	if e := store.TargetErrors()[id]; !strings.Contains(e.Err, "no reply") {
		t.Errorf("the reason was not stored: %+v", e)
	}
	ok := queuedMeasure{m: Measurement{TargetID: id, ProbeID: 1, TS: 1060, Sent: 5, Lost: 0,
		RTTus: []float64{1000, 1100, 1200, 1050, 1080}}}
	if err := store.RecordBatch([]queuedMeasure{ok}); err != nil {
		t.Fatal(err)
	}
	if _, still := store.TargetErrors()[id]; still {
		t.Error("the reason must disappear once the target answers again")
	}
}
