package main

import (
	"bufio"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"
)

// SSH probe: the connection and the server's identification string.
//
// An SSH server announces itself the moment the connection is up, without
// being asked (RFC 4253 §4.2):
//
//	SSH-2.0-OpenSSH_9.6\r\n
//
// So this needs no SSH client and no cryptography. It connects, reads the
// greeting and stops. **Nothing is ever sent**, which is the operational
// point: no key exchange is started and no authentication is attempted, so
// this leaves nothing in the auth log and trips no rate limiter watching for
// failed logins. A probe that got a target banned from its own server every
// thirty seconds would be worse than no probe.
//
// What it sees that a TCP connect cannot: an sshd that accepts a connection
// and is too wedged to greet. The port is open either way, so a tcp target
// reads that as healthy; here it is the timeout it is.
//
// The figure is the connection plus the greeting, which is the exchange a
// client actually waits through before it can do anything.

const sshPort = 22

const (
	// The identification string is at most 255 bytes, CRLF included.
	sshMaxLine = 255
	// A server may send other lines first — a legal banner, most often — and
	// the specification allows it. They are read through, within a budget, so
	// that a banner does not read as a server that never greeted. The budget
	// exists because an endless stream of them would otherwise hold the probe
	// for its whole timeout and be graphed as latency.
	sshMaxPreamble = 8 << 10
)

// runSSH times the connection and the greeting, once per packet of the pass.
func (p *Prober) runSSH(t *Target) ([]float64, string) {
	timeout := time.Duration(t.TimeoutMs) * time.Millisecond
	spacing := time.Duration(t.SpacingMs) * time.Millisecond
	port := t.Port
	if port <= 0 {
		port = sshPort
	}
	// Resolved once per pass, as every other protocol here does.
	ip, err := p.targetIP(t)
	if err != nil {
		return nil, err.Error()
	}
	addr := net.JoinHostPort(ip.String(), strconv.Itoa(port))

	var out []float64
	var lastErr string
	for i := 0; i < t.Packets; i++ {
		rtt, why := sshOnce(addr, timeout)
		if why != "" {
			lastErr = why
		} else {
			out = append(out, rtt)
		}
		if i < t.Packets-1 {
			time.Sleep(spacing)
		}
	}
	if len(out) == 0 {
		return nil, lastErr
	}
	return out, ""
}

// sshOnce connects and waits for the identification string.
func sshOnce(addr string, timeout time.Duration) (float64, string) {
	start := time.Now()
	c, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return 0, sshProbeError(err, addr)
	}
	defer c.Close()
	// One deadline for the whole exchange, set from the start rather than
	// from now: a slow connection must not buy itself a fresh timeout for
	// the greeting, or a target half over budget would read as healthy.
	c.SetDeadline(start.Add(timeout))

	r := bufio.NewReaderSize(c, sshMaxLine+1)
	read := 0
	for {
		line, err := r.ReadString('\n')
		read += len(line)
		if err != nil {
			if line == "" && read == 0 {
				return 0, "the connection was accepted and nothing was sent: " +
					"the port is open and the service did not greet"
			}
			return 0, sshProbeError(err, addr)
		}
		if strings.HasPrefix(line, "SSH-") {
			if len(line) > sshMaxLine {
				return 0, fmt.Sprintf("the identification string is %d bytes, over the 255 "+
					"the protocol allows: this is not an SSH server", len(line))
			}
			return float64(time.Since(start).Microseconds()), ""
		}
		if read > sshMaxPreamble {
			return 0, fmt.Sprintf("%d bytes arrived without an identification string: "+
				"either this is not an SSH server, or its banner is longer than this "+
				"probe will read", read)
		}
	}
}

// sshProbeError says which half failed. "i/o timeout" alone does not
// distinguish a port nobody answers from a service that answered too late.
func sshProbeError(err error, addr string) string {
	s := err.Error()
	switch {
	case strings.Contains(s, "connection refused"):
		return "nothing is listening on " + addr
	case strings.Contains(s, "i/o timeout"), strings.Contains(s, "deadline exceeded"):
		return "no identification string before the timeout: the connection was accepted " +
			"and the service did not greet"
	case strings.Contains(s, "connection reset"):
		return "the connection was accepted and then reset without a greeting, which is " +
			"what a server at its connection limit does"
	case strings.Contains(s, "EOF"):
		return "the connection was closed before any greeting"
	case strings.Contains(s, "no such host"):
		return "the name could not be resolved"
	}
	return s
}
