package main

import (
	"fmt"
	"testing"
	"time"
)

// An instance that holds eleven days of fine-grained measurement and empty
// coarse tiers — a stop, an upgrade from a version where the cascade
// leaked, or #68 itself. The long views must come back rather than stay
// empty over data the instance actually has.
func TestBackfillRecoversTheLongViews(t *testing.T) {
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
	// Eleven days of roll_1m, and nothing coarser.
	for b := now - 11*86400; b <= now; b += 60 {
		if _, err := store.mxw.Exec(`INSERT INTO roll_1m(target_id,probe_id,bucket,sent,lost,cnt,
		     min_us,max_us,sum_us,sumsq_us,sketch,passes,down)
		     VALUES(?,1,?,5,0,5,9000,9000,45000,405000000,?,1,0)`, id, b, blob); err != nil {
			t.Fatal(err)
		}
	}
	for _, tbl := range []string{"roll_5m", "roll_1h", "roll_1d"} {
		var n int64
		store.mx.QueryRow("SELECT COUNT(*) FROM " + tbl).Scan(&n)
		if n != 0 {
			t.Fatalf("%s should start empty, has %d", tbl, n)
		}
	}

	// Run the backfill to completion, the way the background loop would.
	for i := 0; i < 400; i++ {
		more, err := store.RollupBackfill(now, 7*86400)
		if err != nil {
			t.Fatal(err)
		}
		if !more {
			break
		}
	}

	for _, g := range cascade[1:] {
		var n, oldest int64
		store.mx.QueryRow(fmt.Sprintf(
			`SELECT COUNT(*),COALESCE(MIN(bucket),0) FROM %s WHERE target_id=?`, g.table), id).Scan(&n, &oldest)
		t.Logf("%-8s rows=%-6d oldest %.1f d back", g.table, n, float64(now-oldest)/86400)
		if n == 0 {
			t.Errorf("%s is still empty after the backfill", g.table)
		}
	}
	// What the views actually read.
	for _, span := range []int64{10 * 86400, 90 * 86400, 365 * 86400} {
		tbl, _ := pickTable(span)
		var n int64
		store.mx.QueryRow(fmt.Sprintf(
			`SELECT COUNT(*) FROM %s WHERE target_id=? AND bucket>=? AND bucket<=?`, tbl),
			id, now-span, now).Scan(&n)
		t.Logf("the %d-day view reads %-8s and finds %d points", span/86400, tbl, n)
		if n == 0 {
			t.Errorf("the %d-day view is still empty", span/86400)
		}
	}
}

// The backfill must stop, and must not reach past what the destination is
// allowed to keep — otherwise it would fight the next purge for ever.
func TestBackfillTerminatesAndRespectsRetention(t *testing.T) {
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
	// Samples reaching far further back than samples are kept, which is the
	// shape an imported or hand-loaded database can have.
	for b := now - 400*86400; b <= now; b += 3600 {
		store.mxw.Exec(`INSERT INTO samples(target_id,probe_id,bucket,sent,lost,cnt,
		     min_us,max_us,sum_us,sumsq_us,sketch,passes,down)
		     VALUES(?,1,?,5,0,5,9000,9000,45000,405000000,?,1,0)`, id, b, blob)
	}
	passes := 0
	for {
		more, err := store.RollupBackfill(now, 30*86400)
		if err != nil {
			t.Fatal(err)
		}
		if !more {
			break
		}
		if passes++; passes > 500 {
			t.Fatal("the backfill does not terminate")
		}
	}
	t.Logf("terminated after %d passes", passes)
	// roll_1m keeps a year: nothing older than that should have been built,
	// since the next purge would delete it and the backfill would rebuild it.
	var oldest int64
	store.mx.QueryRow(`SELECT COALESCE(MIN(bucket),0) FROM roll_1m`).Scan(&oldest)
	if oldest != 0 && oldest < now-366*86400 {
		t.Errorf("roll_1m was backfilled %.0f days back, beyond its retention",
			float64(now-oldest)/86400)
	}
	// And a second run of the loop must find nothing left to do.
	if more, _ := store.RollupBackfill(now, 30*86400); more {
		t.Error("the backfill reports work remaining after it finished")
	}
}

// The bug behind #68, at the level it happens. RollupRange clears whole
// destination buckets but used to read from the raw `from`, so the bucket
// containing `from` was rebuilt from only the part of its source after that
// instant — and when nothing fell in that part, it was cleared and not
// rebuilt at all.
func TestRollupRebuildsTheEdgeBucketWhole(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cat, _ := store.CreateCategory("c", "C", "C", true)
	id, _ := store.CreateTarget(&Target{CategoryID: cat, Slug: "x", Title: "X",
		Host: "192.0.2.1", Proto: "icmp", IntervalS: 60, Packets: 5,
		SpacingMs: 100, TimeoutMs: 1000, Public: true, Enabled: true})

	sk := NewSketch()
	sk.Add(9000)
	blob := sk.MarshalBinary()
	base := (time.Now().Unix() - 4*3600) / 3600 * 3600 // an hour mark
	// Four hours of five-minute rows: twelve per hour.
	for b := base; b < base+4*3600; b += 300 {
		store.mxw.Exec(`INSERT INTO roll_5m(target_id,probe_id,bucket,sent,lost,cnt,
		   min_us,max_us,sum_us,sumsq_us,sketch,passes,down)
		   VALUES(?,1,?,5,0,5,9000,9000,45000,405000000,?,1,0)`, id, b, blob)
	}
	// Start the range three quarters of the way through the first hour.
	from := base + 2700
	if err := store.RollupRange("roll_5m", "roll_1h", 3600, from, base+4*3600); err != nil {
		t.Fatal(err)
	}
	var sent, passes int64
	if err := store.mx.QueryRow(
		`SELECT sent,passes FROM roll_1h WHERE target_id=? AND bucket=?`, id, base).Scan(&sent, &passes); err != nil {
		t.Fatalf("the hour containing the start of the range must exist: %v", err)
	}
	// Twelve five-minute rows of five packets each: the whole hour, not the
	// three rows that happen to sit after `from`.
	if sent != 60 || passes != 12 {
		t.Errorf("the edge hour was rebuilt from part of its source: sent=%d passes=%d, expected 60 and 12",
			sent, passes)
	}
}

// And the consequence: as the window slides past a bucket, that bucket must
// survive. This is what capped every tier at its own rebuild window, so the
// 3-month and 1-year views showed three days.
func TestSlidingWindowDoesNotEraseWhatItLeaves(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cat, _ := store.CreateCategory("c", "C", "C", true)
	id, _ := store.CreateTarget(&Target{CategoryID: cat, Slug: "x", Title: "X",
		Host: "192.0.2.1", Proto: "icmp", IntervalS: 60, Packets: 5,
		SpacingMs: 100, TimeoutMs: 1000, Public: true, Enabled: true})

	sk := NewSketch()
	sk.Add(9000)
	blob := sk.MarshalBinary()
	now := time.Now().Unix()
	for b := now - 8*3600; b <= now; b += 300 {
		store.mxw.Exec(`INSERT INTO roll_5m(target_id,probe_id,bucket,sent,lost,cnt,
		   min_us,max_us,sum_us,sumsq_us,sketch,passes,down)
		   VALUES(?,1,?,5,0,5,9000,9000,45000,405000000,?,1,0)`, id, b, blob)
	}
	// Sweep a four-hour window forward in thirty-second steps, as the
	// background loop does, and watch the hour that the window leaves.
	const window = 4 * 3600
	watched := (now - 6*3600) / 3600 * 3600
	seen := false
	for t0 := now - 2*3600; t0 <= now; t0 += 30 {
		if err := store.RollupRange("roll_5m", "roll_1h", 3600, t0-window, t0+3600); err != nil {
			t.Fatal(err)
		}
		var n int64
		store.mx.QueryRow(`SELECT COUNT(*) FROM roll_1h WHERE target_id=? AND bucket=?`,
			id, watched).Scan(&n)
		if t0-window > watched+3600 { // the window has left that hour behind
			seen = true
			if n != 1 {
				t.Fatalf("the hour at %d was erased once the window passed it (sweep at %d)",
					watched, t0)
			}
		}
	}
	if !seen {
		t.Fatal("the sweep never passed the watched hour, so nothing was tested")
	}
}
