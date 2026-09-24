package main

import (
	"strings"
	"testing"
)

// The three causes of loss look different in the data, and the advice must
// tell them apart — above all it must not suggest hiding a real outage.
func TestAdviseTarget(t *testing.T) {
	heavy := &Target{Packets: 20, SpacingMs: 500, TimeoutMs: 2000, IntervalS: 60}
	gentle := &Target{Packets: 10, SpacingMs: 300, TimeoutMs: 1000, IntervalS: 60}

	cases := []struct {
		name       string
		target     *Target
		st         passStats
		addrs      int
		verdict    string
		wantPatch  bool
		mustSay    string
		mustNotSay string
	}{
		{"no loss at all", gentle,
			passStats{Passes: 1440, Sent: 14400}, 1, "clean", false, "Nothing to change", ""},
		{"one packet per burst, often", heavy,
			passStats{Passes: 1440, Sent: 28800, Lost: 200, WithLoss: 200, MaxLostOne: 1},
			1, "rate limiting", true, "answers only so many", "path"},
		{"already gentle, limit remains", gentle,
			passStats{Passes: 1440, Sent: 14400, Lost: 150, WithLoss: 150, MaxLostOne: 1},
			1, "rate limiting", false, "already gentle", ""},
		{"whole passes lost", heavy,
			passStats{Passes: 1440, Sent: 28800, Lost: 400, WithLoss: 20, Complete: 20, MaxLostOne: 20},
			1, "real outages", false, "would only hide it", ""},
		{"scattered loss on the path", gentle,
			passStats{Passes: 1440, Sent: 14400, Lost: 300, WithLoss: 90, Complete: 2, MaxLostOne: 6},
			1, "scattered loss", false, "looks like the path", ""},
		{"too little data", gentle, passStats{Passes: 3, Sent: 30}, 1, "not enough data", false, "", ""},
	}
	for _, c := range cases {
		got := adviseTarget(c.target, c.st, c.addrs)
		if got.Verdict != c.verdict {
			t.Errorf("%s: verdict %q, expected %q", c.name, got.Verdict, c.verdict)
		}
		hasPatch := false
		for _, s := range got.Suggestions {
			if s.Patch["packets"] != nil {
				hasPatch = true
			}
		}
		if hasPatch != c.wantPatch {
			t.Errorf("%s: suggestion offered=%v, expected %v (%+v)", c.name, hasPatch, c.wantPatch, got.Suggestions)
		}
		if c.mustSay != "" && !strings.Contains(got.Detail, c.mustSay) {
			t.Errorf("%s: %q lacks %q", c.name, got.Detail, c.mustSay)
		}
		if c.mustNotSay != "" && strings.Contains(got.Detail, c.mustNotSay) {
			t.Errorf("%s: %q should not mention %q", c.name, got.Detail, c.mustNotSay)
		}
	}
	// A rotating name is mentioned when whole passes are lost: some went to
	// a server that ignores ICMP.
	rot := adviseTarget(heavy, passStats{Passes: 300, Sent: 6000, Lost: 2000,
		WithLoss: 100, Complete: 100, MaxLostOne: 20}, 6)
	if !strings.Contains(rot.Detail, "6 addresses") {
		t.Errorf("the rotating name should be mentioned: %q", rot.Detail)
	}
	// A burst crowding its interval is flagged even with no loss.
	crowd := adviseTarget(&Target{Packets: 50, SpacingMs: 800, TimeoutMs: 2000, IntervalS: 60},
		passStats{Passes: 100, Sent: 5000}, 1)
	if len(crowd.Suggestions) == 0 || !strings.Contains(crowd.Suggestions[0].Why, "little room") {
		t.Errorf("a crowded burst should be flagged: %+v", crowd.Suggestions)
	}
}

// The measured share of loss must be reported as it is.
func TestAdviceLossPercentage(t *testing.T) {
	a := adviseTarget(&Target{Packets: 20, SpacingMs: 200, TimeoutMs: 1000, IntervalS: 60},
		passStats{Passes: 15, Sent: 300, Lost: 2, WithLoss: 2, MaxLostOne: 1}, 1)
	if a.LossPct < 0.66 || a.LossPct > 0.67 {
		t.Errorf("loss share: %.3f", a.LossPct)
	}
}
