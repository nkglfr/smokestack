package main

import (
	"os"
	"path/filepath"
	"testing"
)

// A probe holds what it has measured until the service takes it. That is the
// right place for it while the service is reachable, and exactly the wrong
// place when the probe is restarted during an outage — which is when the
// measurements from the one vantage point that was working matter most.
func TestThePendingBufferSurvivesARestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), probeBufferFile)

	before := &remoteLink{}
	for i := 0; i < 5; i++ {
		before.Submit(Measurement{TargetID: int64(i + 1), TS: int64(100 + i), Sent: 10, Lost: 0}, "host")
	}
	before.SubmitTrace(&Traceroute{TargetID: 1, TS: 100})
	if err := before.persist(path); err != nil {
		t.Fatal(err)
	}

	after := &remoteLink{}
	// Something measured by the new run before the old buffer is read.
	after.Submit(Measurement{TargetID: 99, TS: 999, Sent: 10}, "host")
	if n := after.restore(path); n != 5 {
		t.Fatalf("recovered %d measurements, want 5", n)
	}
	if len(after.buf) != 6 {
		t.Fatalf("buffer holds %d, want the 5 recovered plus the 1 measured", len(after.buf))
	}
	// Oldest first: what was waiting goes out before what was just measured,
	// otherwise a long outage would deliver the backlog out of order.
	if after.buf[0].Measurement.TargetID != 1 || after.buf[5].Measurement.TargetID != 99 {
		t.Errorf("order is %d...%d, want the recovered ones first",
			after.buf[0].Measurement.TargetID, after.buf[5].Measurement.TargetID)
	}
	if len(after.traces) != 1 {
		t.Errorf("recovered %d traceroutes, want 1", len(after.traces))
	}

	// The file is consumed: a buffer that was read must not come back on the
	// next start, or a restart loop would replay the same measurements for
	// as long as it lasted.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("the buffer file survived being read")
	}
}

// Nothing pending means no file. A stale one left behind would be replayed by
// a later start, long after the measurements in it stopped being useful.
func TestAnEmptyBufferLeavesNoFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), probeBufferFile)
	l := &remoteLink{}
	l.Submit(Measurement{TargetID: 1, TS: 1, Sent: 1}, "h")
	if err := l.persist(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("a pending buffer wrote no file: %v", err)
	}

	l.bmu.Lock()
	l.buf = nil
	l.bmu.Unlock()
	if err := l.persist(path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("an empty buffer left its old file behind")
	}
}

// A file written by a version with a larger ceiling must not raise this one's.
// The oldest go, which is the same thing Submit does when the live buffer
// fills up.
func TestRestoreRespectsTheBufferCeiling(t *testing.T) {
	path := filepath.Join(t.TempDir(), probeBufferFile)
	big := &remoteLink{}
	big.bmu.Lock()
	for i := 0; i < probeBufferMax+10; i++ {
		big.buf = append(big.buf, wireMeasure{Measurement: Measurement{TargetID: int64(i), TS: int64(i)}})
	}
	big.bmu.Unlock()
	if err := big.persist(path); err != nil {
		t.Fatal(err)
	}

	l := &remoteLink{}
	l.restore(path)
	if len(l.buf) != probeBufferMax {
		t.Fatalf("buffer holds %d after restore, want the ceiling of %d", len(l.buf), probeBufferMax)
	}
	if l.dropped != 10 {
		t.Errorf("dropped %d, want the 10 that did not fit", l.dropped)
	}
	// The ones kept are the newest, as they are when the live buffer fills.
	if l.buf[len(l.buf)-1].Measurement.TargetID != int64(probeBufferMax+9) {
		t.Error("the wrong end of the buffer was kept")
	}
}

// A file that is not readable must not stop a probe from starting. Measuring
// now matters more than recovering what was measured before.
func TestAnUnreadableBufferDoesNotStopTheProbe(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, probeBufferFile)
	if err := os.WriteFile(path, []byte("{this is not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := &remoteLink{}
	if n := l.restore(path); n != 0 {
		t.Errorf("recovered %d from a corrupt file", n)
	}
	if len(l.buf) != 0 {
		t.Error("a corrupt file put something in the buffer")
	}
	// And it is gone, so the next start does not trip over it again.
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Error("the corrupt file was left in place")
	}
}
