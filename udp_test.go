package main

import (
	"net"
	"strings"
	"testing"
)

// echoServer answers on loopback the way an RFC 862 service does: the bytes
// back, verbatim. mode changes what it answers with, so that the probe can be
// shown what it does with a reply that is not the one it is waiting for.
func echoServer(t *testing.T, mode string) int {
	t.Helper()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback UDP available here: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	go func() {
		buf := make([]byte, 2048)
		var first []byte
		for {
			n, addr, err := c.ReadFrom(buf)
			if err != nil {
				return
			}
			switch mode {
			case "stuck":
				// Answers every request with the first one it ever saw. A
				// probe that timed replies without checking which packet they
				// belong to would read these as very fast answers.
				if first == nil {
					first = append([]byte(nil), buf[:n]...)
				}
				c.WriteTo(first, addr)
			default:
				c.WriteTo(buf[:n], addr)
			}
		}
	}()
	return c.LocalAddr().(*net.UDPAddr).Port
}

func udpTarget(port int) *Target {
	return &Target{ID: 1, Slug: "echo", Title: "echo", Host: "127.0.0.1", PinIP: "127.0.0.1",
		Proto: "udp", Port: port, Packets: 3, SpacingMs: 5, TimeoutMs: 300}
}

func TestUDPEchoMeasuresEveryPacket(t *testing.T) {
	port := echoServer(t, "echo")
	p := &Prober{}
	rtts, errStr := p.runUDP(udpTarget(port))
	if errStr != "" {
		t.Fatalf("an echo server answered and the pass failed: %s", errStr)
	}
	if len(rtts) != 3 {
		t.Fatalf("got %d round trips, want 3", len(rtts))
	}
	for _, rtt := range rtts {
		if rtt <= 0 {
			t.Errorf("round trip of %v microseconds", rtt)
		}
	}
}

// The reason the payload carries a nonce and a sequence number. A connected
// socket filters by peer but not by which packet, so a reply belonging to an
// earlier request would otherwise be timed against the current one and report
// a latency far below the truth. Understated latency is the worst failure a
// latency tool has, because nothing about the graph looks wrong.
func TestUDPEchoIgnoresAReplyToAnotherPacket(t *testing.T) {
	port := echoServer(t, "stuck")
	p := &Prober{}
	rtts, _ := p.runUDP(udpTarget(port))
	if len(rtts) != 1 {
		t.Fatalf("got %d round trips, want 1: only the first packet was ever answered, "+
			"and the replies to the other two belonged to it", len(rtts))
	}
}

// A host that answers with an ICMP port-unreachable is up and running nothing
// on that port. Reporting that as loss would leave an operator looking at the
// path when the problem is a service that was never started.
func TestUDPEchoSeparatesRefusedFromSilent(t *testing.T) {
	// A port nothing is listening on: taken, then released, so it is almost
	// certainly free and definitely on loopback.
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback UDP available here: %v", err)
	}
	port := c.LocalAddr().(*net.UDPAddr).Port
	c.Close()

	p := &Prober{}
	rtts, errStr := p.runUDP(udpTarget(port))
	if len(rtts) != 0 {
		t.Fatalf("got %d round trips from a port nobody is listening on", len(rtts))
	}
	// Loopback delivers the ICMP error; a path with a firewall in it may not,
	// and then this reads as silence, which is the honest outcome.
	if !strings.Contains(errStr, "nothing is listening") && !strings.Contains(errStr, "timeout") {
		t.Errorf("the pass said %q, want either the refusal or a timeout", errStr)
	}
}

func TestUDPEchoDefaultsToPortSeven(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cat, err := store.CreateCategory("c", "Catégorie", "Category", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateTarget(&Target{CategoryID: cat, Slug: "e", Title: "e",
		Host: "198.51.100.1", Proto: "udp", IntervalS: 60, Packets: 10,
		SpacingMs: 100, TimeoutMs: 1000, Public: true, Enabled: true}); err != nil {
		t.Fatalf("a UDP target with no port was refused: %v", err)
	}
	got, err := store.TargetBySlug("e")
	if err != nil {
		t.Fatal(err)
	}
	if got.Port != udpEchoPort {
		t.Errorf("port %d, want %d", got.Port, udpEchoPort)
	}

	// And it stays settable, which is what the receiver range needs.
	if _, err := store.CreateTarget(&Target{CategoryID: cat, Slug: "e2", Title: "e2",
		Host: "198.51.100.2", Proto: "udp", Port: 49160, IntervalS: 60, Packets: 10,
		SpacingMs: 100, TimeoutMs: 1000, Public: true, Enabled: true}); err != nil {
		t.Fatalf("a UDP target on 49160 was refused: %v", err)
	}
	got2, err := store.TargetBySlug("e2")
	if err != nil {
		t.Fatal(err)
	}
	if got2.Port != 49160 {
		t.Errorf("port %d, want 49160", got2.Port)
	}
}
