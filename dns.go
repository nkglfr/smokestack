package main

import (
	crand "crypto/rand"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// A DNS target times a resolver answering queries, not a host answering
// pings. The two come apart: a resolver whose recursion is stuck — an
// upstream gone, a wedged cache, an exhausted worker pool — keeps answering
// ICMP from its kernel while every query waits out the client's timeout. That
// is what a production instance saw: ICMP reported a healthy resolver for as
// long as it failed to answer within three seconds.
//
// Each packet of the burst is one query over UDP, recursion desired, for the
// name and type the target states. The queries go out at the target's
// spacing without waiting for each other, as ICMP echoes do, so a silent
// resolver holds a pass for the burst plus the timeout and no longer. An
// answer must come from the address and port queried, which the connected
// socket enforces, repeat the random ID of a query still waiting and its
// question, and arrive within the timeout of that query; anything else is
// dropped, so an answer is only ever timed against its own query. The one
// reply without the question is a bare error header with the right ID, which
// Unbound sends to a client its access control refuses: it counts as the
// error it names, never as an answer. What the server said decides the
// outcome:
//
//	NOERROR, NXDOMAIN  answered: the resolver resolved, and is timed
//	SERVFAIL           lost: the resolver is up and failing to resolve
//	REFUSED            lost, and reported as a configuration to fix
//	silence            lost
//
// A truncated answer (TC) counts as answered: the resolver is what is being
// timed, so there is no retry over TCP. Only the header and the question are
// read, at fixed offsets, so nothing in an answer can make the parser follow
// a pointer or a length anywhere.
//
// ". NS" is the default query because it is the priming query of RFC 8109:
// every recursive resolver holds the root NS set in order to resolve anything
// at all, so the answer comes from its cache and times the resolver rather
// than its upstream, and on the rare miss the question goes to the root
// servers, which exist to answer it. A name of somebody else's would have it
// looked up thousands of times a day on behalf of nobody who asked.

const (
	dnsPort        = 53
	dnsDefaultName = "."
	dnsDefaultType = "NS"
	dnsClassIN     = 1
	dnsHeaderLen   = 12
)

// The types a target may ask for, all answered from an ordinary zone. ANY
// and zone transfers, whose answers are large or refused, are not offered.
var dnsTypes = []struct {
	name string
	code uint16
}{{"A", 1}, {"AAAA", 28}, {"NS", 2}, {"SOA", 6}, {"MX", 15}, {"TXT", 16}}

func dnsTypeCode(name string) (uint16, bool) {
	for _, t := range dnsTypes {
		if t.name == name {
			return t.code, true
		}
	}
	return 0, false
}

func dnsTypeNames() string {
	names := make([]string, len(dnsTypes))
	for i, t := range dnsTypes {
		names[i] = t.name
	}
	return strings.Join(names, ", ")
}

// dnsName cleans the name a target asks for, returns it fully qualified, and
// encodes it the way it travels in a question. Labels follow the rule host
// names follow here.
func dnsName(raw string) (string, []byte, error) {
	name := strings.ToLower(strings.TrimSpace(raw))
	if name == "" {
		name = dnsDefaultName
	}
	if name == "." {
		return name, []byte{0}, nil
	}
	name = strings.TrimSuffix(name, ".")
	wire := make([]byte, 0, len(name)+2)
	for _, label := range strings.Split(name, ".") {
		if !dnsLabel(label) {
			return "", nil, fmt.Errorf("%q is not a DNS name: labels are letters, digits, "+
				"hyphens and underscores, 63 characters at most", strings.TrimSpace(raw))
		}
		wire = append(wire, byte(len(label)))
		wire = append(wire, label...)
	}
	wire = append(wire, 0)
	if len(wire) > 255 {
		return "", nil, fmt.Errorf("the query name is longer than the 255 bytes DNS allows")
	}
	return name + ".", wire, nil
}

func dnsLabel(l string) bool {
	if l == "" || len(l) > 63 {
		return false
	}
	for i, r := range l {
		ok := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') ||
			(r == '-' && i > 0 && i < len(l)-1) || r == '_'
		if !ok {
			return false
		}
	}
	return true
}

// checkDNSQuery validates the query of a dns target, filling the defaults the
// way a UDP echo target gets port 7. On any other protocol the query is
// cleared rather than refused: a request that only changes a target's
// protocol must not be turned down over a field its sender never wrote.
func checkDNSQuery(t *Target) error {
	if t.Proto != "dns" {
		t.DNSName, t.DNSType = "", ""
		return nil
	}
	if t.Port == 0 {
		t.Port = dnsPort
	}
	if t.Port < 1 || t.Port > 65535 {
		return fmt.Errorf("a DNS target needs a port between 1 and 65535 (53 by default)")
	}
	name, _, err := dnsName(t.DNSName)
	if err != nil {
		return err
	}
	typ := strings.ToUpper(strings.TrimSpace(t.DNSType))
	if typ == "" {
		typ = dnsDefaultType
	}
	if _, ok := dnsTypeCode(typ); !ok {
		return fmt.Errorf("dns_type must be one of %s", dnsTypeNames())
	}
	t.DNSName, t.DNSType = name, typ
	return nil
}

// dnsQuery is a standard query with recursion desired and a single question.
// It carries no EDNS record, so each packet is one small datagram out and an
// answer of 512 bytes at most back.
func dnsQuery(id uint16, qname []byte, qtype uint16) []byte {
	b := make([]byte, dnsHeaderLen, dnsHeaderLen+len(qname)+4)
	binary.BigEndian.PutUint16(b[0:2], id)
	binary.BigEndian.PutUint16(b[2:4], 0x0100) // RD
	binary.BigEndian.PutUint16(b[4:6], 1)      // QDCOUNT
	b = append(b, qname...)
	b = binary.BigEndian.AppendUint16(b, qtype)
	return binary.BigEndian.AppendUint16(b, dnsClassIN)
}

// dnsAnswered reports whether an RCODE means the resolver did its job:
// NOERROR, or NXDOMAIN for a name that does not exist.
func dnsAnswered(rcode int) bool { return rcode == 0 || rcode == 3 }

// dnsReply returns the RCODE of b when b answers this exact query: the same
// ID, a response to a standard query, and the same single question. ok is
// false for anything else.
//
// A refusal may come as the bare header, every count zeroed: Unbound answers
// a query without EDNS that way when its access control refuses the client.
// Such a header is accepted for the error it states and never for an answer,
// so nothing that does not repeat the question is ever timed.
//
// The question is the first name of a message, so a compression pointer
// there could only point back into the header. It is compared rather than
// followed, and a pointer byte never matches a label length.
func dnsReply(b []byte, id uint16, qname []byte, qtype uint16) (rcode int, ok bool) {
	if len(b) < dnsHeaderLen || binary.BigEndian.Uint16(b[0:2]) != id {
		return 0, false
	}
	flags := binary.BigEndian.Uint16(b[2:4])
	const qr, opcode = 0x8000, 0x7800
	if flags&qr == 0 || flags&opcode != 0 {
		return 0, false
	}
	rcode = int(flags & 0x000f)
	switch binary.BigEndian.Uint16(b[4:6]) {
	case 0:
		if dnsAnswered(rcode) {
			return 0, false
		}
		return rcode, true
	case 1:
	default:
		return 0, false
	}
	end := dnsHeaderLen + len(qname) + 4
	if len(b) < end {
		return 0, false
	}
	// Names compare without regard to ASCII case (RFC 4343). qname is
	// lower case, and folding never turns a letter into a length byte.
	for i, c := range b[dnsHeaderLen : dnsHeaderLen+len(qname)] {
		if c >= 'A' && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != qname[i] {
			return 0, false
		}
	}
	if binary.BigEndian.Uint16(b[end-4:end-2]) != qtype ||
		binary.BigEndian.Uint16(b[end-2:end]) != dnsClassIN {
		return 0, false
	}
	return rcode, true
}

// dnsRcodeError says what a server's refusal to answer means for whoever has
// to act on it. SERVFAIL is the resolver failing, which is what the target is
// for; REFUSED is the server declining to serve this probe, which no amount
// of looking at the path will fix.
func dnsRcodeError(rcode int, name, typ string) string {
	switch rcode {
	case 2:
		return fmt.Sprintf("the resolver answered SERVFAIL to %s %s: it is reachable "+
			"and failing to resolve", name, typ)
	case 5:
		return fmt.Sprintf("the server answered REFUSED to %s %s: it does not serve this "+
			"probe (an access list, or an authoritative server asked to recurse), a "+
			"configuration to fix rather than a path to investigate", name, typ)
	case 1:
		return fmt.Sprintf("the server answered FORMERR to %s %s", name, typ)
	case 4:
		return fmt.Sprintf("the server answered NOTIMP to %s %s", name, typ)
	}
	return fmt.Sprintf("the server answered RCODE %d to %s %s", rcode, name, typ)
}

// runDNS times one burst of queries, over an ordinary UDP socket that needs no
// privilege of its own. Query i leaves at i × spacing whatever became of the
// others, and the socket is read until the last one's timeout has run out or
// every query has had its reply, so a pass never outlasts the
// packets × spacing + timeout that checkTarget fits into the interval.
func (p *Prober) runDNS(t *Target) ([]float64, string) {
	name, qname, err := dnsName(t.DNSName)
	qtype, known := dnsTypeCode(t.DNSType)
	if err != nil || !known {
		return nil, fmt.Sprintf("this DNS target has no valid query (%q %q): "+
			"set one in the target settings", t.DNSName, t.DNSType)
	}
	port := t.Port
	if port <= 0 {
		port = dnsPort
	}
	ip, err := p.targetIP(t)
	if err != nil {
		return nil, err.Error()
	}
	// Connected, so the kernel delivers only what comes from the address and
	// port queried, and turns an ICMP port-unreachable into an error here.
	c, err := net.DialUDP("udp", nil, &net.UDPAddr{IP: ip, Port: port})
	if err != nil {
		return nil, err.Error()
	}
	defer c.Close()
	enableKernelRxTimestamps(c)

	ids := make([]byte, 2*t.Packets)
	if _, err := crand.Read(ids); err != nil {
		return nil, "could not draw query IDs: " + err.Error()
	}
	used := make(map[uint16]bool, t.Packets)
	// The send time of each query that has had no reply yet. An ID leaves it
	// on its first reply, so a duplicate answers nothing.
	pending := make(map[uint16]time.Time, t.Packets)
	timeout := time.Duration(t.TimeoutMs) * time.Millisecond
	spacing := time.Duration(t.SpacingMs) * time.Millisecond
	buf := make([]byte, 1500)
	oob := make([]byte, 128)

	var out []float64
	var said, lastErr string
	start := time.Now()
	var end time.Time
	for sent := 0; sent < t.Packets || len(pending) > 0; {
		next := start.Add(time.Duration(sent) * spacing)
		if sent < t.Packets && !time.Now().Before(next) {
			// Unique within the pass, so that no answer can belong to two queries.
			id := binary.BigEndian.Uint16(ids[2*sent:])
			for used[id] {
				id++
			}
			used[id] = true
			at := time.Now()
			if _, err := c.Write(dnsQuery(id, qname, qtype)); err != nil {
				lastErr = udpEchoError(err, port)
			} else {
				pending[id] = at
			}
			sent++
			end = at.Add(timeout)
			continue
		}
		if sent < t.Packets {
			c.SetReadDeadline(next)
		} else {
			c.SetReadDeadline(end)
		}
		n, oobn, _, _, err := c.ReadMsgUDP(buf, oob)
		if err != nil {
			if !errors.Is(err, os.ErrDeadlineExceeded) {
				lastErr = udpEchoError(err, port)
			} else if sent == t.Packets {
				break
			}
			continue
		}
		if n < dnsHeaderLen {
			continue
		}
		id := binary.BigEndian.Uint16(buf[0:2])
		at, waiting := pending[id]
		if !waiting {
			continue
		}
		rcode, ok := dnsReply(buf[:n], id, qname, qtype)
		if !ok {
			continue
		}
		delete(pending, id)
		rtt := sinceStamp(at, stampFromOOB(oob[:oobn]))
		if rtt > float64(timeout/time.Microsecond) {
			// Past its own timeout a query is lost, wherever it sat in the burst.
			continue
		}
		if dnsAnswered(rcode) {
			out = append(out, rtt)
		} else {
			said = dnsRcodeError(rcode, name, t.DNSType)
		}
	}
	switch {
	case len(out) > 0:
		return out, ""
	case said != "":
		// The server's own words over silence: it answered, just not usefully.
		return nil, said
	case lastErr != "":
		return nil, lastErr
	}
	return nil, fmt.Sprintf("no answer to %d queries for %s %s sent to %s",
		t.Packets, name, t.DNSType, net.JoinHostPort(ip.String(), strconv.Itoa(port)))
}

// sinceStamp is the round trip from start to a receive time in Unix
// nanoseconds, the kernel's when it gave one. A clock stepped in between
// cannot make it negative: the monotonic reading takes over.
func sinceStamp(start time.Time, rx int64) float64 {
	if d := rx - start.UnixNano(); d > 0 {
		return float64(d) / 1000
	}
	return float64(time.Since(start).Microseconds())
}
