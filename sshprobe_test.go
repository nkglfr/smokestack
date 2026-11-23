package main

import (
	"net"
	"strings"
	"testing"
	"time"
)

// sshServer accepts connections on loopback and hands each one to greet,
// which writes whatever a case needs. Returns the port.
func sshServer(t *testing.T, greet func(c net.Conn)) int {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				greet(c)
			}()
		}
	}()
	return l.Addr().(*net.TCPAddr).Port
}

func sshTarget(port, packets, timeoutMs int) *Target {
	return &Target{Host: "127.0.0.1", Proto: "ssh", Port: port, Packets: packets,
		SpacingMs: 1, TimeoutMs: timeoutMs, IntervalS: 60}
}

func TestSSHTimesTheGreeting(t *testing.T) {
	port := sshServer(t, func(c net.Conn) {
		c.Write([]byte("SSH-2.0-OpenSSH_9.6\r\n"))
		time.Sleep(50 * time.Millisecond) // the client must not wait for this
	})
	p := &Prober{res: newResolver()}
	rtts, errStr := p.runSSH(sshTarget(port, 3, 1000))
	if errStr != "" || len(rtts) != 3 {
		t.Fatalf("got %d measurements and %q, want 3 and no error", len(rtts), errStr)
	}
}

// Nothing may be sent. A probe that started a key exchange or offered a
// credential would fill the auth log and could get itself banned by a rate
// limiter, every pass, for ever.
func TestSSHSendsNothing(t *testing.T) {
	got := make(chan int, 4)
	port := sshServer(t, func(c net.Conn) {
		c.Write([]byte("SSH-2.0-OpenSSH_9.6\r\n"))
		c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		buf := make([]byte, 512)
		n, _ := c.Read(buf)
		got <- n
	})
	p := &Prober{res: newResolver()}
	if _, errStr := p.runSSH(sshTarget(port, 1, 1000)); errStr != "" {
		t.Fatalf("the pass failed: %s", errStr)
	}
	select {
	case n := <-got:
		if n != 0 {
			t.Errorf("the probe sent %d bytes: it must send nothing at all", n)
		}
	case <-time.After(time.Second):
		// The server's read never returned, which also means nothing arrived.
	}
}

// A legal banner before the identification string is allowed, and reading
// through it is the difference between measuring such a server and reporting
// it as one that never greeted.
func TestSSHReadsThroughAPreambleBanner(t *testing.T) {
	port := sshServer(t, func(c net.Conn) {
		c.Write([]byte("Authorised users only.\r\nAll access is logged.\r\n"))
		c.Write([]byte("SSH-2.0-OpenSSH_9.6\r\n"))
	})
	p := &Prober{res: newResolver()}
	rtts, errStr := p.runSSH(sshTarget(port, 1, 1000))
	if errStr != "" || len(rtts) != 1 {
		t.Errorf("a server with a legal banner: got %d measurements and %q", len(rtts), errStr)
	}
}

// The case a TCP connect cannot see: the port is open and the service never
// greets. A tcp target reads that as healthy.
func TestSSHAnOpenPortThatNeverGreets(t *testing.T) {
	port := sshServer(t, func(c net.Conn) {
		time.Sleep(2 * time.Second)
	})
	p := &Prober{res: newResolver()}
	rtts, errStr := p.runSSH(sshTarget(port, 1, 150))
	if len(rtts) != 0 {
		t.Errorf("an open port that never greeted produced %d measurements", len(rtts))
	}
	if !strings.Contains(errStr, "greet") {
		t.Errorf("the reason is %q, which does not say the service did not greet", errStr)
	}
}

func TestSSHAConnectionClosedWithoutAGreeting(t *testing.T) {
	port := sshServer(t, func(c net.Conn) { c.Close() })
	p := &Prober{res: newResolver()}
	rtts, errStr := p.runSSH(sshTarget(port, 1, 300))
	if len(rtts) != 0 {
		t.Errorf("a connection closed at once produced %d measurements", len(rtts))
	}
	if errStr == "" {
		t.Error("no reason given")
	}
}

// Something that is not an SSH server: it talks, and never identifies itself.
// The budget is what keeps that from being graphed as latency.
func TestSSHOnAPortThatIsNotSSH(t *testing.T) {
	port := sshServer(t, func(c net.Conn) {
		line := strings.Repeat("x", 200) + "\r\n"
		for i := 0; i < 200; i++ {
			if _, err := c.Write([]byte(line)); err != nil {
				return
			}
		}
	})
	p := &Prober{res: newResolver()}
	rtts, errStr := p.runSSH(sshTarget(port, 1, 2000))
	if len(rtts) != 0 {
		t.Errorf("a chatty non-SSH service produced %d measurements", len(rtts))
	}
	if !strings.Contains(errStr, "identification string") {
		t.Errorf("the reason is %q, which does not say what was missing", errStr)
	}
}

// The deadline covers the whole exchange, set from the connection's start. A
// slow connection must not buy a fresh timeout for the greeting, or a target
// half over budget reads as healthy.
func TestSSHTheDeadlineCoversConnectAndGreeting(t *testing.T) {
	port := sshServer(t, func(c net.Conn) {
		time.Sleep(300 * time.Millisecond)
		c.Write([]byte("SSH-2.0-OpenSSH_9.6\r\n"))
	})
	p := &Prober{res: newResolver()}
	start := time.Now()
	rtts, _ := p.runSSH(sshTarget(port, 1, 150))
	took := time.Since(start)
	if len(rtts) != 0 {
		t.Errorf("a greeting after the timeout was measured anyway (%d values)", len(rtts))
	}
	if took > 280*time.Millisecond {
		t.Errorf("the pass took %v on a 150 ms timeout: the deadline was reset rather than "+
			"covering the whole exchange", took)
	}
}

func TestSSHOnAClosedPort(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	p := &Prober{res: newResolver()}
	rtts, errStr := p.runSSH(sshTarget(port, 1, 300))
	if len(rtts) != 0 {
		t.Errorf("a closed port produced %d measurements", len(rtts))
	}
	if !strings.Contains(errStr, "listening") {
		t.Errorf("the reason is %q, which does not say nothing is listening", errStr)
	}
}

func TestSSHDefaultsToPort22(t *testing.T) {
	if sshPort != 22 {
		t.Errorf("the SSH port default is %d", sshPort)
	}
	if !validProto("ssh") {
		t.Fatal("ssh is not a known protocol, so no target could be created with it")
	}
}
