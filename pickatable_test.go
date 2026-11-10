package main

import (
	"testing"
	"time"
)

// pickTable answers on the span alone. A window that ends now is right; one
// that reaches further back than its tier's retention is not, and that is the
// whole of #114.
func TestAShortWindowOverAnOldPeriodLeavesSamples(t *testing.T) {
	now := int64(1_800_000_000)

	cases := []struct {
		name      string
		from, to  int64
		wantTable string
		wantStep  int64
	}{
		{
			// The case that reads an empty table today: three hours is a
			// samples-sized span, but samples is kept 48 h.
			name: "three hours, three days ago",
			from: now - 3*86400, to: now - 3*86400 + 3*3600,
			wantTable: "roll_1m", wantStep: 60,
		},
		{
			name: "three hours ending now",
			from: now - 3*3600, to: now,
			wantTable: "samples", wantStep: 0,
		},
		{
			// Still inside the 48 h samples is kept, so nothing moves.
			name: "three hours, one day ago",
			from: now - 86400, to: now - 86400 + 3*3600,
			wantTable: "samples", wantStep: 0,
		},
		{
			// Two days of span is a roll_1m window, and roll_1m is kept a
			// year, so a fortnight back changes nothing.
			name: "two days, a fortnight ago",
			from: now - 14*86400, to: now - 12*86400,
			wantTable: "roll_1m", wantStep: 60,
		},
		{
			// Past the year roll_1m is kept: on to roll_5m.
			name: "two days, two years ago",
			from: now - 730*86400, to: now - 728*86400,
			wantTable: "roll_5m", wantStep: 300,
		},
		{
			// Past the three years roll_5m is kept: on to roll_1h, which is
			// kept for ever.
			name: "two days, four years ago",
			from: now - 1460*86400, to: now - 1458*86400,
			wantTable: "roll_1h", wantStep: 3600,
		},
		{
			// A month ending now already reads roll_5m on the span alone.
			name: "thirty days ending now",
			from: now - 30*86400, to: now,
			wantTable: "roll_5m", wantStep: 300,
		},
		{
			// Nothing is kept longer than roll_1d, so an absurdly old window
			// lands there and the walk terminates.
			name: "a day, fifty years ago",
			from: now - 18250*86400, to: now - 18249*86400,
			wantTable: "roll_1d", wantStep: 86400,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			table, step := pickTableFrom(c.from, c.to, now)
			if table != c.wantTable || step != c.wantStep {
				t.Errorf("pickTableFrom = %s/%d, want %s/%d",
					table, step, c.wantTable, c.wantStep)
			}
		})
	}
}

// A window ending now must read exactly what it reads today, whatever the
// age logic does: the fix is not allowed to change the common case.
func TestWindowsEndingNowAreUnchanged(t *testing.T) {
	now := int64(1_800_000_000)
	for _, span := range []int64{
		60, 3600, 6 * 3600, 6*3600 + 1, 4 * 86400, 45 * 86400,
		500 * 86400, 900 * 86400,
	} {
		wantTable, wantStep := pickTable(span)
		table, step := pickTableFrom(now-span, now, now)
		if table != wantTable || step != wantStep {
			t.Errorf("span %ds ending now: pickTableFrom = %s/%d, pickTable = %s/%d",
				span, table, step, wantTable, wantStep)
		}
	}
}

func TestTierOfNamesEveryTablePickTableCanReturn(t *testing.T) {
	for _, span := range []int64{3600, 2 * 86400, 30 * 86400, 100 * 86400, 900 * 86400} {
		table, _ := pickTable(span)
		if got := cascade[tierOf(table)].table; got != table {
			t.Errorf("tierOf(%q) points at %q", table, got)
		}
	}
}

// The tier chosen from the clock can still be empty when the target carries
// its own keep_days, which pickTableFrom cannot see. Series must walk on
// rather than report a target that has never been measured.
func TestSeriesFallsBackWhenTheNominalTierIsEmpty(t *testing.T) {
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

	// Hourly data for a period three days back, and nothing in samples or
	// roll_1m for it — what a target purged by its own keep_days looks like
	// once the finer tiers have been cut and the coarse ones have not.
	from := now - 3*86400
	to := from + 3*3600
	for b := from; b <= to; b += 3600 {
		if _, err := store.mxw.Exec(`INSERT INTO roll_1h(target_id,probe_id,bucket,sent,lost,cnt,
		     min_us,max_us,sum_us,sumsq_us,sketch,passes,down)
		     VALUES(?,1,?,5,0,5,9000,9000,45000,405000000,?,1,0)`, id, b, blob); err != nil {
			t.Fatal(err)
		}
	}

	// On the clock alone this window is a roll_1m one, and roll_1m is empty.
	if table, _ := pickTableFrom(from, to, now); table != "roll_1m" {
		t.Fatalf("precondition: expected the nominal tier to be roll_1m, got %s", table)
	}

	s, err := store.Series(id, 1, from, to, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.T) == 0 {
		t.Fatal("the series came back empty over a period the instance holds at 1 h")
	}
	if s.Table != "roll_1h" {
		t.Errorf("series read from %q, want roll_1h", s.Table)
	}
	if s.StepS != 3600 {
		t.Errorf("series reports a step of %d, want 3600: the front end draws the "+
			"bucket width from it", s.StepS)
	}
}

// A target with nothing anywhere must still answer, rather than loop or fail.
func TestSeriesOnATargetWithNoMeasurementAtAll(t *testing.T) {
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
	s, err := store.Series(id, 1, now-3*3600, now, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(s.T) != 0 {
		t.Errorf("a target never measured returned %d points", len(s.T))
	}
	// The walk ends on the coarsest tier, having found nothing anywhere.
	if s.Table != "roll_1d" {
		t.Errorf("the empty walk stopped on %q, want roll_1d", s.Table)
	}
}
