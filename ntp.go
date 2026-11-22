package main

import (
	"crypto/rand"
	"encoding/binary"
	"fmt"
	"net"
	"strconv"
	"time"
)

// NTP probe (RFC 5905).
//
// Why a protocol of its own rather than a UDP echo at port 123: an NTP server
// does not echo. It answers a 48-byte client packet with a 48-byte server
// packet of its own, and refuses what it does not recognise, so a UDP echo
// target pointed at port 123 measures nothing.
//
// And the answer carries what no latency measurement can: the server's own
// view of itself. A clock that replies in two milliseconds while running at
// stratum 16 is not working, and nothing about its round trip says so. That
// is reported as a failure here rather than graphed as a healthy two
// milliseconds, which is the only reading that is any use to whoever is
// watching.
//
// The round trip is wall-clock, measured the way every other protocol here
// measures it, and NOT NTP's own offset formula. The point of this tool is
// that a target's latency means the same thing whatever protocol it speaks,
// so an NTP target has to stay comparable with an ICMP one. The offset is a
// different quantity and would not belong on the same axis.

const ntpPort = 123

// ntpPacketLen is the fixed length of a packet without authentication
// extensions, which a client does not send and a server does not require.
const ntpPacketLen = 48

// Byte offsets inside the packet, named so that the slicing below reads as
// the specification does.
const (
	ntpOffFlags        = 0  // leap indicator, version, mode
	ntpOffStratum      = 1  //
	ntpOffOriginTS     = 24 // the server copies our transmit timestamp here
	ntpOffTransmitTS   = 40 // where we put it
	ntpModeClient      = 3
	ntpModeServer      = 4
	ntpVersion4        = 4
	ntpLeapUnsynced    = 3  // the server says its own clock is not synchronised
	ntpStratumKoD      = 0  // "kiss of death", or an otherwise invalid reply
	ntpStratumUnsynced = 16 // reachable, answering, and not a usable clock
)

// runNTP times a client request against an NTP server, once per packet of the
// pass.
func (p *Prober) runNTP(t *Target) ([]float64, string) {
	timeout := time.Duration(t.TimeoutMs) * time.Millisecond
	spacing := time.Duration(t.SpacingMs) * time.Millisecond
	port := t.Port
	if port <= 0 {
		port = ntpPort
	}
	// Resolved once per pass, as the other protocols are: a name looked up
	// inside each packet's timeout makes a hesitating resolver read as loss.
	ip, err := p.targetIP(t)
	if err != nil {
		return nil, err.Error()
	}
	// Connected, so the kernel filters replies by peer and an ICMP
	// port-unreachable arrives as an error on this socket instead of being
	// discarded.
	c, err := net.Dial("udp", net.JoinHostPort(ip.String(), strconv.Itoa(port)))
	if err != nil {
		return nil, err.Error()
	}
	defer c.Close()

	req := make([]byte, ntpPacketLen)
	req[ntpOffFlags] = ntpVersion4<<3 | ntpModeClient
	buf := make([]byte, 256)

	var out []float64
	var lastErr string
	for i := 0; i < t.Packets; i++ {
		// The transmit timestamp is the correlation, and it is the protocol's
		// own: a server copies it into the origin field of its reply. Eight
		// random bytes rather than the real clock, because nothing here reads
		// it as a time and a random value cannot collide across a pass. This
		// is what keeps a reply that arrives late — during the wait for the
		// next packet — from being timed against the wrong request, which
		// would understate the latency. Understating is the worst thing a
		// latency tool can do, because nothing about the graph looks wrong.
		var token [8]byte
		if _, err := rand.Read(token[:]); err != nil {
			return nil, "could not generate a probe token: " + err.Error()
		}
		copy(req[ntpOffTransmitTS:ntpOffTransmitTS+8], token[:])

		start := time.Now()
		c.SetDeadline(start.Add(timeout))
		if _, err := c.Write(req); err != nil {
			lastErr = udpEchoError(err, port)
			if i < t.Packets-1 {
				time.Sleep(spacing)
			}
			continue
		}
		for {
			n, err := c.Read(buf)
			if err != nil {
				lastErr = udpEchoError(err, port)
				break
			}
			if n < ntpPacketLen {
				// Too short to be a packet. Keep waiting on the same
				// deadline rather than counting it either way.
				continue
			}
			if !ntpEcho(buf, token) {
				// A straggler from an earlier packet of this pass. Dropped,
				// and the wait continues on the same deadline, so a late
				// reply costs nothing and times nothing.
				continue
			}
			rtt := float64(time.Since(start).Microseconds())
			if why := ntpUsable(buf); why != "" {
				// It answered, and it is not a clock. Recorded as a failure
				// rather than as a healthy round trip.
				lastErr = why
				break
			}
			out = append(out, rtt)
			break
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

// ntpEcho says whether this reply answers the request carrying this token: a
// server packet whose origin timestamp is the transmit timestamp we sent.
func ntpEcho(b []byte, token [8]byte) bool {
	if b[ntpOffFlags]&0x07 != ntpModeServer {
		return false
	}
	return string(b[ntpOffOriginTS:ntpOffOriginTS+8]) == string(token[:])
}

// ntpUsable returns why a reply does not describe a working clock, or "" when
// it does. These are the states a round trip cannot show.
func ntpUsable(b []byte) string {
	if leap := b[ntpOffFlags] >> 6; leap == ntpLeapUnsynced {
		return "the server answers but reports its own clock as unsynchronised " +
			"(leap indicator 3)"
	}
	switch s := b[ntpOffStratum]; s {
	case ntpStratumKoD:
		// Stratum 0 carries a four-character code in the reference id, and
		// the common ones are a refusal rather than a fault: RATE means the
		// client is polling too often, DENY and RSTR that it is not welcome.
		return fmt.Sprintf("the server refused the request (stratum 0, %q): "+
			"it is reachable but will not serve this client", ntpRefID(b))
	case ntpStratumUnsynced:
		return "the server answers but is not synchronised to any clock " +
			"(stratum 16)"
	}
	return ""
}

// ntpRefID reads the reference identifier as the four printable characters a
// stratum-0 or stratum-1 server puts there.
func ntpRefID(b []byte) string {
	id := make([]byte, 0, 4)
	for _, c := range b[12:16] {
		if c >= 0x20 && c < 0x7f {
			id = append(id, c)
		}
	}
	if len(id) == 0 {
		return fmt.Sprintf("%08x", binary.BigEndian.Uint32(b[12:16]))
	}
	return string(id)
}
