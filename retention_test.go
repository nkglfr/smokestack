package main

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// Any interval is allowed as long as the burst fits in it: the old list of
// four values was the restriction, not a measurement constraint.
func TestFreeInterval(t *testing.T) {
	ok := []struct {
		interval int64
		packets  int
		spacing  int
		timeout  int
	}{
		{10, 5, 200, 1000},    // ten seconds, tight but valid
		{45, 10, 200, 1000},   // a value the old list refused
		{1800, 10, 300, 2000}, // half an hour
		{86400, 10, 300, 2000},
	}
	for _, c := range ok {
		tg := &Target{Host: "192.0.2.1", Proto: "icmp", IntervalS: c.interval,
			Packets: c.packets, SpacingMs: c.spacing, TimeoutMs: c.timeout}
		if err := checkTarget(tg); err != nil {
			t.Errorf("interval %d s refused: %v", c.interval, err)
		}
	}
	bad := []struct {
		name     string
		interval int64
		packets  int
		spacing  int
		timeout  int
	}{
		{"too short", 5, 5, 100, 500},
		{"longer than a day", 90000, 10, 200, 1000},
		{"burst does not fit", 10, 20, 500, 2000},
	}
	// The retention and the traceroute interval are bounded on creation too,
	// not only through the API.
	for _, tg := range []*Target{
		{Host: "192.0.2.1", Proto: "icmp", IntervalS: 60, Packets: 10, SpacingMs: 100, TimeoutMs: 1000, KeepDays: 5000},
		{Host: "192.0.2.1", Proto: "icmp", IntervalS: 60, Packets: 10, SpacingMs: 100, TimeoutMs: 1000, TraceHours: 5000},
	} {
		if err := checkTarget(tg); err == nil {
			t.Error("an out-of-range retention or traceroute interval should be refused")
		}
	}
	for _, c := range bad {
		tg := &Target{Host: "192.0.2.1", Proto: "icmp", IntervalS: c.interval,
			Packets: c.packets, SpacingMs: c.spacing, TimeoutMs: c.timeout}
		if err := checkTarget(tg); err == nil {
			t.Errorf("%s should have been refused", c.name)
		}
	}
}

// A target can keep its measurements for less time than the instance tiers,
// and that must touch nothing else.
func TestRetentionPerTarget(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cat, _ := store.CreateCategory("c", "C", "C", true)
	mk := func(slug string, keep int) int64 {
		id, err := store.CreateTarget(&Target{CategoryID: cat, Slug: slug, Title: slug,
			Host: "192.0.2.1", Proto: "icmp", IntervalS: 60, Packets: 10, SpacingMs: 100,
			TimeoutMs: 1000, Public: true, Enabled: true, KeepDays: keep})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	short, forever := mk("short", 7), mk("forever", 0)
	now := time.Now().Unix()
	sk := NewSketch()
	sk.Add(1000)
	for _, id := range []int64{short, forever} {
		for _, age := range []int64{2 * 86400, 30 * 86400, 400 * 86400} {
			for _, tbl := range []string{"roll_1h", "roll_1d"} {
				if _, err := store.mxw.Exec(fmt.Sprintf(`INSERT INTO %s(target_id,probe_id,bucket,
				     sent,lost,cnt,min_us,max_us,sum_us,sumsq_us,sketch) VALUES(?,1,?,10,0,1,1,1,1,1,?)`, tbl),
					id, now-age, sk.MarshalBinary()); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	count := func(id int64) int {
		var n int
		store.mx.QueryRow(`SELECT (SELECT COUNT(*) FROM roll_1h WHERE target_id=?) +
		                   (SELECT COUNT(*) FROM roll_1d WHERE target_id=?)`, id, id).Scan(&n)
		return n
	}
	if count(short) != 6 || count(forever) != 6 {
		t.Fatalf("setup: %d and %d rows", count(short), count(forever))
	}
	if err := store.Purge(now); err != nil {
		t.Fatal(err)
	}
	// Seven days: only the two-day-old rows survive.
	if got := count(short); got != 2 {
		t.Errorf("the 7-day target kept %d rows, expected 2", got)
	}
	// The other target is untouched: hourly and daily tiers are unlimited.
	if got := count(forever); got != 6 {
		t.Errorf("a target on the instance tiers lost rows: %d of 6 left", got)
	}
}

// A long interval must not make a target look as if it had no data: the
// "current" window follows the interval.
func TestStatusWindowFollowsInterval(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cat, _ := store.CreateCategory("c", "C", "C", true)
	id, err := store.CreateTarget(&Target{CategoryID: cat, Slug: "slow", Title: "Slow",
		Host: "192.0.2.1", Proto: "icmp", IntervalS: 1800, Packets: 10, SpacingMs: 300,
		TimeoutMs: 2000, Public: true, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	// One pass 20 minutes ago: outside the old 15-minute window.
	now := time.Now().Unix()
	m := Measurement{TargetID: id, ProbeID: 1, TS: now - 20*60, Sent: 10,
		RTTus: []float64{1000, 1100, 1050, 1080, 1020, 1030, 1040, 1060, 1070, 1090}}
	if err := store.RecordBatch([]queuedMeasure{{m: m}}); err != nil {
		t.Fatal(err)
	}
	// The window reads the one-minute rollups, which the background loop
	// builds from the raw passes.
	if err := store.RollupTick(now); err != nil {
		t.Fatal(err)
	}
	ov, err := store.Overview(1, now, false)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range ov.Categories {
		for _, tg := range c.Targets {
			if tg.ID != id {
				continue
			}
			if tg.Status == "nodata" || tg.MedMs == nil {
				t.Errorf("a 30-minute target measured 20 minutes ago reads as %q", tg.Status)
			}
			if !strings.HasPrefix(tg.Status, "ok") && tg.Status != "ok" {
				t.Logf("status: %s", tg.Status)
			}
		}
	}
}
