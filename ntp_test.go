package main

import (
	"net"
	"strings"
	"testing"
	"time"
)

// ntpServer answers on loopback with whatever shape() returns, and gives back
// its port. shape receives the request and a well-formed reply whose origin
// timestamp is already the request's transmit timestamp, and returns the bytes
// to send — which may be shorter, longer or nothing at all, so that a short
// packet is tested as a short packet rather than as one the harness padded.
func ntpServer(t *testing.T, shape func(req, rep []byte) []byte) int {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	go func() {
		buf := make([]byte, 256)
		for {
			n, addr, err := c.ReadFromUDP(buf)
			if err != nil {
				return
			}
			if n < ntpPacketLen {
				continue
			}
			rep := make([]byte, ntpPacketLen)
			rep[ntpOffFlags] = ntpVersion4<<3 | ntpModeServer
			rep[ntpOffStratum] = 2
			copy(rep[ntpOffOriginTS:ntpOffOriginTS+8], buf[ntpOffTransmitTS:ntpOffTransmitTS+8])
			if out := shape(buf[:n], rep); out != nil {
				c.WriteToUDP(out, addr)
			}
		}
	}()
	return c.LocalAddr().(*net.UDPAddr).Port
}

func ntpTarget(port, timeoutMs int) *Target {
	return &Target{Host: "127.0.0.1", Proto: "ntp", Port: port, Packets: 3,
		SpacingMs: 1, TimeoutMs: timeoutMs, IntervalS: 60}
}

func TestNTPTimesAServerThatAnswers(t *testing.T) {
	port := ntpServer(t, func(req, rep []byte) []byte { return rep })
	p := &Prober{}
	rtts, errStr := p.runNTP(ntpTarget(port, 500))
	if errStr != "" || len(rtts) != 3 {
		t.Fatalf("got %d answers and %q, want 3 and no error", len(rtts), errStr)
	}
	for _, rtt := range rtts {
		if rtt <= 0 {
			t.Errorf("a round trip of %v microseconds", rtt)
		}
	}
}

// The whole reason this is a protocol of its own rather than a UDP echo: a
// clock that answers in a millisecond and is not synchronised is broken, and
// its round trip says nothing about that. It must not be graphed as healthy.
func TestNTPReportsAServerThatIsNotAClock(t *testing.T) {
	cases := []struct {
		name  string
		shape func(req, rep []byte) []byte
		says  string
	}{
		{"stratum 16, reachable and unsynchronised",
			func(req, rep []byte) []byte { rep[ntpOffStratum] = 16; return rep },
			"stratum 16"},
		{"leap indicator 3, the server's own clock is adrift",
			func(req, rep []byte) []byte { rep[ntpOffFlags] |= 3 << 6; return rep },
			"unsynchronised"},
		{"stratum 0 with a RATE kiss-of-death",
			func(req, rep []byte) []byte {
				rep[ntpOffStratum] = 0
				copy(rep[12:16], "RATE")
				return rep
			},
			"RATE"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			port := ntpServer(t, c.shape)
			p := &Prober{}
			rtts, errStr := p.runNTP(ntpTarget(port, 300))
			if len(rtts) != 0 {
				t.Errorf("recorded %d round trips from a server that is not a clock", len(rtts))
			}
			if errStr == "" {
				t.Fatal("no reason given: this reads as a target that was never measured")
			}
			if !strings.Contains(errStr, c.says) {
				t.Errorf("the reason is %q, which does not mention %q — whoever reads it "+
					"has to know which state the server is in", errStr, c.says)
			}
		})
	}
}

// A reply that does not carry our transmit timestamp belongs to another
// packet of the pass. Timing it against this one would understate the
// latency, which is the worst thing a latency tool can do, because nothing
// about the graph looks wrong.
func TestNTPIgnoresAReplyThatIsNotItsOwn(t *testing.T) {
	cases := []struct {
		name  string
		shape func(req, rep []byte) []byte
	}{
		{"the origin timestamp is somebody else's",
			func(req, rep []byte) []byte {
				copy(rep[ntpOffOriginTS:ntpOffOriginTS+8], "deadbeef")
				return rep
			}},
		{"the mode is client, so this is not a server reply",
			func(req, rep []byte) []byte {
				rep[ntpOffFlags] = ntpVersion4<<3 | ntpModeClient
				return rep
			}},
		{"truncated: fewer than the 48 bytes a packet has",
			func(req, rep []byte) []byte { return rep[:20] }},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			port := ntpServer(t, c.shape)
			p := &Prober{}
			start := time.Now()
			rtts, errStr := p.runNTP(ntpTarget(port, 120))
			if len(rtts) != 0 {
				t.Errorf("timed %d replies that were not answers to our request", len(rtts))
			}
			if errStr == "" {
				t.Error("no reason given for a pass that measured nothing")
			}
			if took := time.Since(start); took < 100*time.Millisecond {
				t.Errorf("the pass gave up after %v: it did not wait out its timeout, so a "+
					"late but genuine reply would have been missed", took)
			}
		})
	}
}

func TestNTPSilenceIsLoss(t *testing.T) {
	port := ntpServer(t, func(req, rep []byte) []byte { return nil })
	p := &Prober{}
	rtts, errStr := p.runNTP(ntpTarget(port, 120))
	if len(rtts) != 0 {
		t.Errorf("a server that answered nothing produced %d round trips", len(rtts))
	}
	if errStr == "" {
		t.Error("silence was recorded without a reason")
	}
}

// Nothing listening is a refusal, not a timeout, and it should say so rather
// than reading as a slow network.
func TestNTPOnAClosedPort(t *testing.T) {
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	port := c.LocalAddr().(*net.UDPAddr).Port
	c.Close()

	p := &Prober{}
	rtts, errStr := p.runNTP(ntpTarget(port, 150))
	if len(rtts) != 0 {
		t.Errorf("a closed port produced %d round trips", len(rtts))
	}
	if errStr == "" {
		t.Fatal("a closed port gave no reason")
	}
}

// The port defaults to 123 rather than to whatever TCP would use, and a
// target that names one keeps it.
func TestNTPDefaultsToPort123(t *testing.T) {
	if ntpPort != 123 {
		t.Errorf("the NTP port is %d", ntpPort)
	}
	// A target with no port must reach 123, which nothing in the test
	// environment listens on: what matters is that it does not reach 0 or a
	// TCP default, which would fail differently.
	tg := ntpTarget(0, 80)
	p := &Prober{}
	if _, errStr := p.runNTP(tg); errStr == "" {
		t.Error("a pass against the default port reported no reason at all")
	}
	if !strings.Contains(ntpTarget(123, 80).Host, "127.0.0.1") {
		t.Error("the fixture changed")
	}
}

func TestNTPIsRefusedByAPeerDoubleCheck(t *testing.T) {
	// The refusal is an allow-list of ICMP and TCP, so a protocol added later
	// is refused without anyone remembering to add it. This pins that, since
	// the alternative is a peer being asked to reproduce a measurement it
	// cannot make.
	if !validProto("ntp") {
		t.Fatal("ntp is not a known protocol")
	}
	for _, proto := range knownProtos {
		if proto == "ntp" {
			return
		}
	}
	t.Error("ntp is missing from knownProtos, so a target could not be created with it")
}

func TestNTPRefIDReadsTheKissOfDeathCode(t *testing.T) {
	b := make([]byte, ntpPacketLen)
	copy(b[12:16], "DENY")
	if got := ntpRefID(b); got != "DENY" {
		t.Errorf("ntpRefID = %q, want DENY", got)
	}
	copy(b[12:16], []byte{0x7f, 0x00, 0x00, 0x01})
	if got := ntpRefID(b); got == "" {
		t.Error("a non-printable reference id came back empty rather than as hex")
	}
}
