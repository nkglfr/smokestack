package main

import (
	"encoding/binary"
	"testing"
	"time"
)

// An echo reply is not required to echo our payload: several home routers,
// CPE and ONT answer with a truncated or rewritten one. Counting those as
// lost is what made targets look unreachable while ping saw them (issue #9).
func TestRepliesWithoutOurPayload(t *testing.T) {
	newProber := func() (*Prober, uint16, int64) {
		p := &Prober{id: 4242, pending: map[uint16]*inflight{}, sentAt: map[uint16]int64{}}
		fl := &inflight{}
		seq := p.nextSeq(fl)
		p.pending[seq] = fl
		return p, seq, p.sentAt[seq]
	}
	mk := func(seq uint16, n int) []byte {
		b := make([]byte, n)
		b[0] = 0 // echo reply, IPv4
		binary.BigEndian.PutUint16(b[4:6], 4242)
		binary.BigEndian.PutUint16(b[6:8], seq)
		return b
	}
	cases := []struct {
		name  string
		build func(seq uint16, sent int64) []byte
		want  bool
	}{
		{"full reply, payload echoed", func(seq uint16, sent int64) []byte {
			b := mk(seq, 24)
			binary.BigEndian.PutUint64(b[8:16], uint64(sent))
			return b
		}, true},
		{"reply truncated to the header", func(seq uint16, _ int64) []byte { return mk(seq, 8) }, true},
		{"payload rewritten with zeroes", func(seq uint16, _ int64) []byte { return mk(seq, 24) }, true},
		{"payload rewritten with garbage", func(seq uint16, _ int64) []byte {
			b := mk(seq, 24)
			binary.BigEndian.PutUint64(b[8:16], 0xdeadbeefdeadbeef)
			return b
		}, true},
		{"reply for another prober", func(seq uint16, sent int64) []byte {
			b := mk(seq, 24)
			binary.BigEndian.PutUint16(b[4:6], 1)
			binary.BigEndian.PutUint64(b[8:16], uint64(sent))
			return b
		}, false},
		{"unknown sequence number", func(_ uint16, sent int64) []byte {
			b := mk(60000, 24)
			binary.BigEndian.PutUint64(b[8:16], uint64(sent))
			return b
		}, false},
	}
	for _, c := range cases {
		p, seq, sent := newProber()
		now := time.Now().UnixNano() + int64(2*time.Millisecond)
		if got := p.handleReply(c.build(seq, sent), 0, now); got != c.want {
			t.Errorf("%s: counted=%v, expected %v", c.name, got, c.want)
			continue
		}
		if c.want {
			fl := p.pending[seq]
			fl.mu.Lock()
			n := len(fl.rtts)
			rtt := 0.0
			if n > 0 {
				rtt = fl.rtts[0]
			}
			fl.mu.Unlock()
			if n != 1 {
				t.Errorf("%s: %d round-trip times recorded", c.name, n)
			} else if rtt < 1000 || rtt > 10000 { // about 2 ms, in microseconds
				t.Errorf("%s: round-trip time out of range: %.0f µs", c.name, rtt)
			}
		}
	}
}

// A rotating name (a pool) points at a different machine every few minutes:
// the addresses actually probed are recorded, and pinning one makes the
// measurement comparable over time.
func TestRotatingNameAndPinning(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cat, _ := store.CreateCategory("c", "C", "C", true)
	id, err := store.CreateTarget(&Target{CategoryID: cat, Slug: "pool", Title: "pool.example",
		Host: "pool.example", Proto: "icmp", IntervalS: 300, Packets: 5, SpacingMs: 200,
		TimeoutMs: 1000, Public: true, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	for i, ip := range []string{"192.0.2.1", "192.0.2.2", "192.0.2.1", "198.51.100.7"} {
		m := Measurement{TargetID: id, ProbeID: 1, TS: now - int64(i*300), Sent: 5, IP: ip,
			RTTus: []float64{1000, 1100, 1050, 1080, 1090}}
		if err := store.RecordBatch([]queuedMeasure{{m: m}}); err != nil {
			t.Fatal(err)
		}
	}
	seen := store.TargetAddresses(now - 24*3600)[id]
	if len(seen) != 3 {
		t.Errorf("3 distinct addresses expected, got %v", seen)
	}
	// Pinning one address: the probe must use it instead of resolving.
	tg, _ := store.TargetByID(id)
	tg.PinIP = "192.0.2.2"
	if err := store.UpdateTarget(tg); err != nil {
		t.Fatal(err)
	}
	p := &Prober{res: newResolver()}
	tg, _ = store.TargetByID(id)
	ip, err := p.targetIP(tg)
	if err != nil || ip.String() != "192.0.2.2" {
		t.Errorf("a pinned target must be probed at its address: %v %v", ip, err)
	}
	// Unpinned, the name is resolved again — and fails here, as expected.
	tg.PinIP = ""
	if _, err := p.targetIP(tg); err == nil {
		t.Error("without a pinned address the name is resolved, and pool.example does not exist")
	}
}
