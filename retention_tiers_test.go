package main

import (
	"testing"
	"time"
)

// Retention per resolution as an instance setting (#115). The per-target
// retention and the interval rules live in retention_test.go, which this file
// deliberately does not touch.

// Purge reads days[i] against cascade[i]. If the two lists ever disagree on
// order, every tier is purged with another tier's retention — silently, and
// destructively. The comment in retention.go claims they cannot drift; this
// is what makes that true.
func TestRetentionTiersMatchTheCascade(t *testing.T) {
	if len(retentionTiers) != len(cascade) {
		t.Fatalf("%d tiers for %d cascade entries", len(retentionTiers), len(cascade))
	}
	for i, g := range cascade {
		if retentionTiers[i] != g.table {
			t.Errorf("position %d: retentionTiers has %q, cascade has %q",
				i, retentionTiers[i], g.table)
		}
	}
	if len(retentionFloors) != len(cascade) {
		t.Errorf("%d floors for %d tiers", len(retentionFloors), len(cascade))
	}
}

// The default has to be today's behaviour to the day, or an upgrade deletes
// history nobody asked it to delete.
func TestTheDefaultIsTodaysCascade(t *testing.T) {
	d := defaultRetention().Days()
	for i, g := range cascade {
		want := int(g.keep / 86400)
		if d[i] != want {
			t.Errorf("%s: default is %d days, the cascade keeps %d days (%ds)",
				g.table, d[i], want, g.keep)
		}
	}
}

// The floors are not taste: RollupTick rebuilds each tier from the one below
// over a fixed window, and a source purged before it is rolled up loses
// measurements rather than history.
func TestEveryFloorClearsTheWindowThatReadsIt(t *testing.T) {
	windows := []int64{2 * 3600, 6 * 3600, 3 * 86400, 3 * 86400}
	for i, w := range windows {
		floor := int64(retentionFloors[i]) * 86400
		if floor < w {
			t.Errorf("%s: floor of %d day(s) is %ds, shorter than the %ds window "+
				"that rolls it up", retentionTiers[i], retentionFloors[i], floor, w)
		}
	}
}

func TestValidateRefusesWhatWouldLoseMeasurements(t *testing.T) {
	cases := []struct {
		name string
		c    RetentionConfig
		ok   bool
	}{
		{"the default", defaultRetention(), true},
		{"all unlimited", RetentionConfig{}, true},
		{"the example from the issue",
			RetentionConfig{Samples: 2, Roll1m: 7, Roll5m: 90}, true},
		{"a long instance",
			RetentionConfig{Samples: 7, Roll1m: 730, Roll5m: 3650}, true},
		{"negative", RetentionConfig{Samples: -1}, false},
		{"samples under its floor is only 0, which is unlimited",
			RetentionConfig{Samples: 0, Roll1m: 365, Roll5m: 1095}, true},
		{"roll_5m under the three days that roll it up",
			RetentionConfig{Samples: 1, Roll1m: 1, Roll5m: 2}, false},
		{"roll_1h under the three days that roll it up",
			RetentionConfig{Samples: 1, Roll1m: 1, Roll5m: 3, Roll1h: 2}, false},
		{"a coarser tier shorter than a finer one",
			RetentionConfig{Samples: 30, Roll1m: 7, Roll5m: 90}, false},
		{"unlimited above a finite tier is fine",
			RetentionConfig{Samples: 2, Roll1m: 7, Roll5m: 0}, true},
		{"a finite tier above an unlimited one is not",
			RetentionConfig{Samples: 2, Roll1m: 0, Roll5m: 90}, false},
		{"equal neighbours are fine",
			RetentionConfig{Samples: 3, Roll1m: 3, Roll5m: 3}, true},
	}
	for _, c := range cases {
		err := c.c.Validate()
		if c.ok && err != nil {
			t.Errorf("%s: refused with %v", c.name, err)
		}
		if !c.ok && err == nil {
			t.Errorf("%s: accepted", c.name)
		}
	}
}

func TestAnUnreadableStoredValueFallsBackToTheDefault(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if got := store.RetentionConfig(); got != defaultRetention() {
		t.Errorf("a fresh instance reads %v, want the default %v", got, defaultRetention())
	}

	// Written by a hand that did not validate: a coarse tier shorter than a
	// fine one. Repairing it field by field would leave a retention nobody
	// chose, so the whole thing is discarded.
	store.SetSetting("retention", `{"samples":30,"roll_1m":7,"roll_5m":90}`)
	if got := store.RetentionConfig(); got != defaultRetention() {
		t.Errorf("an invalid stored value reads %v, want the default", got)
	}

	store.SetSetting("retention", `not json at all`)
	if got := store.RetentionConfig(); got != defaultRetention() {
		t.Errorf("unparseable JSON reads %v, want the default", got)
	}

	want := RetentionConfig{Samples: 2, Roll1m: 7, Roll5m: 90}
	if err := store.SetRetention(want); err != nil {
		t.Fatal(err)
	}
	if got := store.RetentionConfig(); got != want {
		t.Errorf("after SetRetention: %v, want %v", got, want)
	}

	if err := store.SetRetention(RetentionConfig{Samples: 30, Roll1m: 7}); err == nil {
		t.Error("SetRetention stored a configuration Validate refuses")
	}
	if got := store.RetentionConfig(); got != want {
		t.Errorf("a refused write changed the stored value to %v", got)
	}
}

// Shortening deletes at the next purge and cannot be undone, so the figure
// shown before saving has to be the real one.
func TestThePreviewCountsWhatShorteningWouldDelete(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cat, _ := store.CreateCategory("c", "C", "C", true)
	id, err := store.CreateTarget(&Target{CategoryID: cat, Slug: "x", Title: "X",
		Host: "192.0.2.1", Proto: "icmp", IntervalS: 60, Packets: 5,
		SpacingMs: 100, TimeoutMs: 1000, Public: true, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}

	now := time.Now().Unix()
	sk := NewSketch()
	sk.Add(9000)
	blob := sk.MarshalBinary()
	// Thirty days of 1-minute buckets, one per hour to keep the test quick.
	// What matters is how many fall either side of a cut, not the spacing.
	var total int64
	for b := now - 30*86400; b <= now; b += 3600 {
		if _, err := store.mxw.Exec(`INSERT INTO roll_1m(target_id,probe_id,bucket,sent,lost,cnt,
		     min_us,max_us,sum_us,sumsq_us,sketch,passes,down)
		     VALUES(?,1,?,5,0,5,9000,9000,45000,405000000,?,1,0)`, id, b, blob); err != nil {
			t.Fatal(err)
		}
		total++
	}

	// Default keeps roll_1m 365 days, so nothing here is currently purged.
	next := RetentionConfig{Samples: 2, Roll1m: 7, Roll5m: 1095}
	changes, err := store.RetentionPreview(next, now)
	if err != nil {
		t.Fatal(err)
	}
	var seen *RetentionChange
	for i := range changes {
		if changes[i].Table == "roll_1m" {
			seen = &changes[i]
		}
	}
	if seen == nil {
		t.Fatal("the preview says nothing about roll_1m, which is the tier being shortened")
	}

	var want int64
	store.mx.QueryRow(`SELECT COUNT(*) FROM roll_1m WHERE bucket < ?`,
		now-7*86400).Scan(&want)
	if want == 0 || want == total {
		t.Fatalf("the fixture is wrong: %d of %d rows fall before the cut", want, total)
	}
	if seen.Rows != want {
		t.Errorf("the preview says %d rows, the cut matches %d", seen.Rows, want)
	}
	// The second loss, which an operator will not have thought of.
	if seen.BreaksBackfillFor != "roll_5m" {
		t.Errorf("the preview does not say roll_5m can no longer be rebuilt, it says %q",
			seen.BreaksBackfillFor)
	}

	// Loosening deletes nothing, so it has nothing to report.
	changes, err = store.RetentionPreview(RetentionConfig{Samples: 2, Roll1m: 0, Roll5m: 0}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 0 {
		t.Errorf("loosening every tier reported %d deletions", len(changes))
	}

	// And the preview refuses what SetRetention would refuse, rather than
	// showing a cost for a configuration that cannot be saved.
	if _, err := store.RetentionPreview(RetentionConfig{Samples: 30, Roll1m: 7}, now); err == nil {
		t.Error("the preview accepted a configuration Validate refuses")
	}
}

// The purge has to follow the setting, not the compiled cascade, or the
// setting is decoration.
func TestPurgeFollowsTheSetting(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cat, _ := store.CreateCategory("c", "C", "C", true)
	id, _ := store.CreateTarget(&Target{CategoryID: cat, Slug: "x", Title: "X",
		Host: "192.0.2.1", Proto: "icmp", IntervalS: 60, Packets: 5,
		SpacingMs: 100, TimeoutMs: 1000, Public: true, Enabled: true})

	now := time.Now().Unix()
	sk := NewSketch()
	sk.Add(9000)
	blob := sk.MarshalBinary()
	for _, b := range []int64{now - 40*86400, now - 20*86400, now - 86400} {
		if _, err := store.mxw.Exec(`INSERT INTO roll_1m(target_id,probe_id,bucket,sent,lost,cnt,
		     min_us,max_us,sum_us,sumsq_us,sketch,passes,down)
		     VALUES(?,1,?,5,0,5,9000,9000,45000,405000000,?,1,0)`, id, b, blob); err != nil {
			t.Fatal(err)
		}
	}

	count := func() int64 {
		var n int64
		store.mx.QueryRow(`SELECT COUNT(*) FROM roll_1m`).Scan(&n)
		return n
	}
	if count() != 3 {
		t.Fatalf("fixture: %d rows", count())
	}

	// Under the default, roll_1m is kept a year: the purge takes nothing.
	if err := store.Purge(now); err != nil {
		t.Fatal(err)
	}
	if count() != 3 {
		t.Errorf("the default purge removed rows inside a year: %d left", count())
	}

	// Thirty days: the oldest goes, the other two stay.
	if err := store.SetRetention(RetentionConfig{Samples: 2, Roll1m: 30, Roll5m: 1095}); err != nil {
		t.Fatal(err)
	}
	if err := store.Purge(now); err != nil {
		t.Fatal(err)
	}
	if got := count(); got != 2 {
		t.Errorf("after a 30-day retention: %d rows left, want 2", got)
	}
}
