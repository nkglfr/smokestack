package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ICMP traceroute on dedicated sockets: changing the TTL of the measuring
// socket would disturb the pings of every other target meanwhile.
//
// Three rounds of probes are sent over all TTLs (spaced by 300 ms, which
// eases router ICMP rate limits), then replies are collected for 2 s:
//   - Time Exceeded (v4 type 11, v6 type 3) from intermediate routers,
//     which quote our original header: that is where id and seq are found;
//   - Destination Unreachable (v4 type 3, v6 type 1);
//   - Echo Reply from the destination itself.
//
// Sequence numbers encode the run, the TTL and the round, so a late reply
// from a previous run can never be mistaken for a current one.

const (
	traceRounds  = 3
	traceSpacing = 300 * time.Millisecond
	traceWait    = 2 * time.Second
)

type traceReply struct {
	ttl, round int
	from       net.IP
	rtt        float64 // ms
	kind       byte    // 'r' echo reply, 'x' time exceeded, 'u' unreachable
	code       byte
}

type Tracer struct {
	conn4, conn6 net.PacketConn
	id           uint16
	maxHops      int

	run  sync.Mutex // one traceroute at a time
	mu   sync.Mutex
	seqN uint16
	sent map[uint16]int64
	got  []traceReply
}

func NewTracer(id uint16, maxHops int) (*Tracer, error) {
	t := &Tracer{id: id, maxHops: maxHops, sent: map[uint16]int64{}}
	var err4, err6 error
	t.conn4, err4 = net.ListenPacket("ip4:icmp", "0.0.0.0")
	t.conn6, err6 = net.ListenPacket("ip6:ipv6-icmp", "::")
	if err4 != nil && err6 != nil {
		return nil, err4
	}
	if err4 != nil {
		t.conn4 = nil
	}
	if err6 != nil {
		t.conn6 = nil
	}
	for _, c := range []net.PacketConn{t.conn4, t.conn6} {
		if c != nil {
			enableKernelRxTimestamps(c)
		}
	}
	if t.conn4 != nil {
		go t.readLoop(t.conn4, false)
	}
	if t.conn6 != nil {
		go t.readLoop(t.conn6, true)
	}
	return t, nil
}

func (t *Tracer) Close() {
	for _, c := range []net.PacketConn{t.conn4, t.conn6} {
		if c != nil {
			c.Close()
		}
	}
}

func traceSeq(run uint16, ttl, round int) uint16 {
	return (run&0xff)<<8 | uint16(ttl&0x3f)<<2 | uint16(round&0x3)
}

func decodeTraceSeq(seq uint16) (run uint16, ttl, round int) {
	return seq >> 8, int(seq>>2) & 0x3f, int(seq & 0x3)
}

// parseTraceICMP extracts, from an ICMP message received by the tracer,
// the echo identifier and sequence it refers to, and the kind of message:
// 'r' echo reply, 'x' time exceeded, 'u' destination unreachable (with its
// code). Only the sender address tells whether an unreachable comes from
// the destination or from a router refusing to forward.
func parseTraceICMP(b []byte, v6 bool) (id, seq uint16, kind, code byte, ok bool) {
	if len(b) < 8 {
		return
	}
	typ := b[0]
	echoReply, exceeded, unreach := byte(0), byte(11), byte(3)
	if v6 {
		echoReply, exceeded, unreach = 129, 3, 1
	}
	switch typ {
	case echoReply:
		return binary.BigEndian.Uint16(b[4:6]), binary.BigEndian.Uint16(b[6:8]), 'r', 0, true
	case exceeded, unreach:
		inner := b[8:]
		if v6 {
			if len(inner) < 40+8 || inner[6] != 58 { // next header: ICMPv6
				return
			}
			inner = inner[40:]
		} else {
			if len(inner) < 20 || inner[0]>>4 != 4 || inner[9] != 1 { // protocol: ICMP
				return
			}
			ihl := int(inner[0]&0x0f) * 4
			if len(inner) < ihl+8 {
				return
			}
			inner = inner[ihl:]
		}
		req := byte(8)
		if v6 {
			req = 128
		}
		if inner[0] != req {
			return
		}
		kind = 'x'
		if typ == unreach {
			kind = 'u'
		}
		return binary.BigEndian.Uint16(inner[4:6]), binary.BigEndian.Uint16(inner[6:8]), kind, b[1], true
	}
	return
}

func (t *Tracer) readLoop(conn net.PacketConn, v6 bool) {
	buf := make([]byte, 1500)
	oob := make([]byte, 128)
	ipc, _ := conn.(*net.IPConn)
	for {
		var n int
		var now int64
		var from net.IP
		var err error
		if ipc != nil {
			var oobn int
			var addr *net.IPAddr
			n, oobn, _, addr, err = ipc.ReadMsgIP(buf, oob)
			now = stampFromOOB(oob[:oobn])
			if addr != nil {
				from = addr.IP
			}
		} else {
			var a net.Addr
			n, a, err = conn.ReadFrom(buf)
			now = time.Now().UnixNano()
			if ia, ok := a.(*net.IPAddr); ok {
				from = ia.IP
			}
		}
		if err != nil {
			return
		}
		b := buf[:n]
		if !v6 {
			b = stripIPv4(b)
		}
		id, seq, kind, code, ok := parseTraceICMP(b, v6)
		if !ok || id != t.id {
			continue
		}
		t.mu.Lock()
		if sent, found := t.sent[seq]; found {
			_, ttl, round := decodeTraceSeq(seq)
			t.got = append(t.got, traceReply{ttl: ttl, round: round, from: from,
				rtt: float64(now-sent) / 1e6, kind: kind, code: code})
			delete(t.sent, seq)
		}
		t.mu.Unlock()
	}
}

// Trace runs one traceroute and returns the hops, and whether the
// destination answered.
func (t *Tracer) Trace(dst net.IP) ([]Hop, bool, error) {
	v6 := dst.To4() == nil
	conn := t.conn4
	if v6 {
		conn = t.conn6
	}
	if conn == nil {
		return nil, false, fmt.Errorf("IPv%d unavailable", map[bool]int{true: 6, false: 4}[v6])
	}
	t.run.Lock()
	defer t.run.Unlock()

	t.mu.Lock()
	t.seqN++
	run := t.seqN
	t.sent = map[uint16]int64{}
	t.got = nil
	t.mu.Unlock()

	addr := &net.IPAddr{IP: dst}
	for round := 0; round < traceRounds; round++ {
		for ttl := 1; ttl <= t.maxHops; ttl++ {
			if err := setTTL(conn, ttl, v6); err != nil {
				return nil, false, err
			}
			seq := traceSeq(run, ttl, round)
			pkt := echoRequest(v6, t.id, seq)
			stamp(pkt, v6)
			t.mu.Lock()
			t.sent[seq] = int64(binary.BigEndian.Uint64(pkt[8:16]))
			t.mu.Unlock()
			conn.WriteTo(pkt, addr)
		}
		time.Sleep(traceSpacing)
	}
	time.Sleep(traceWait)
	setTTL(conn, 64, v6)

	t.mu.Lock()
	replies := t.got
	t.sent = map[uint16]int64{}
	t.got = nil
	t.mu.Unlock()
	return buildHops(replies, dst, t.maxHops, v6)
}

// unreachNote gives the classic traceroute notation for an unreachable
// sent by a router: !N network, !H host, !A administratively prohibited.
func unreachNote(code byte, v6 bool) string {
	if v6 {
		switch code {
		case 0:
			return "!N"
		case 1:
			return "!A"
		case 3:
			return "!H"
		}
		return "!" + strconv.Itoa(int(code))
	}
	switch code {
	case 0, 6:
		return "!N"
	case 1, 7:
		return "!H"
	case 9, 10, 13:
		return "!A"
	}
	return "!" + strconv.Itoa(int(code))
}

// buildHops turns replies into one line per TTL. The path ends at the
// first TTL where the destination answered, or where a router sent an
// unreachable. Otherwise silent hops at the end are trimmed (at most three
// "* * *" lines are kept, as a hint of where the path stops).
func buildHops(replies []traceReply, dst net.IP, maxHops int, v6 bool) ([]Hop, bool, error) {
	hops := make([]Hop, maxHops)
	for i := range hops {
		hops[i] = Hop{TTL: i + 1, Sent: traceRounds, RTTms: []float64{}}
	}
	reachedAt, stopAt := 0, 0
	for _, r := range replies {
		if r.ttl < 1 || r.ttl > maxHops {
			continue
		}
		h := &hops[r.ttl-1]
		if h.Addr == "" && r.from != nil {
			h.Addr = r.from.String()
		}
		h.RTTms = append(h.RTTms, r.rtt)
		fromDst := r.from != nil && r.from.Equal(dst)
		switch {
		case fromDst && (r.kind == 'r' || r.kind == 'u'):
			if reachedAt == 0 || r.ttl < reachedAt {
				reachedAt = r.ttl
			}
		case r.kind == 'u':
			h.Note = unreachNote(r.code, v6)
			if stopAt == 0 || r.ttl < stopAt {
				stopAt = r.ttl
			}
		}
	}
	var end int
	switch {
	case reachedAt > 0 && (stopAt == 0 || reachedAt <= stopAt):
		end = reachedAt
	case stopAt > 0:
		end = stopAt
	default:
		last := 0
		for i, h := range hops {
			if len(h.RTTms) > 0 {
				last = i + 1
			}
		}
		end = last + 3
		if end > maxHops {
			end = maxHops
		}
	}
	return hops[:end], reachedAt > 0 && reachedAt == end, nil
}

// ----------------------------------------------------------- enrichment

type hopInfo struct {
	name, asn string
	exp       time.Time
}

var (
	hopCacheMu sync.Mutex
	hopCache   = map[string]hopInfo{}
	cgnat      = mustCIDR("100.64.0.0/10")
)

func mustCIDR(s string) *net.IPNet {
	_, n, _ := net.ParseCIDR(s)
	return n
}

func isPublicIP(ip net.IP) bool {
	return ip != nil && !ip.IsPrivate() && !ip.IsLoopback() && !ip.IsLinkLocalUnicast() &&
		!ip.IsUnspecified() && !cgnat.Contains(ip)
}

// cymruName builds the Team Cymru DNS name that returns the origin AS of
// an address: 4.3.2.1.origin.asn.cymru.com, or reversed nibbles under
// origin6.asn.cymru.com for IPv6.
func cymruName(ip net.IP) string {
	if v4 := ip.To4(); v4 != nil {
		return fmt.Sprintf("%d.%d.%d.%d.origin.asn.cymru.com", v4[3], v4[2], v4[1], v4[0])
	}
	v6 := ip.To16()
	var sb strings.Builder
	for i := len(v6) - 1; i >= 0; i-- {
		sb.WriteString(strconv.FormatUint(uint64(v6[i]&0x0f), 16))
		sb.WriteByte('.')
		sb.WriteString(strconv.FormatUint(uint64(v6[i]>>4), 16))
		sb.WriteByte('.')
	}
	sb.WriteString("origin6.asn.cymru.com")
	return sb.String()
}

// parseCymru reads "13335 | 1.1.1.0/24 | US | arin | 2010-07-14".
func parseCymru(txt string) string {
	f := strings.Fields(strings.SplitN(txt, "|", 2)[0])
	if len(f) == 0 {
		return ""
	}
	if _, err := strconv.ParseUint(f[0], 10, 32); err != nil {
		return ""
	}
	return "AS" + f[0]
}

// enrichHops adds reverse DNS names and origin ASes, in parallel, within
// about three seconds. Answers are cached for six hours.
func enrichHops(hops []Hop) {
	var wg sync.WaitGroup
	for i := range hops {
		ip := net.ParseIP(hops[i].Addr)
		if ip == nil {
			continue
		}
		hopCacheMu.Lock()
		c, ok := hopCache[hops[i].Addr]
		hopCacheMu.Unlock()
		if ok && time.Now().Before(c.exp) {
			hops[i].Name, hops[i].ASN = c.name, c.asn
			continue
		}
		wg.Add(1)
		go func(h *Hop, ip net.IP) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			var info hopInfo
			if names, err := net.DefaultResolver.LookupAddr(ctx, h.Addr); err == nil && len(names) > 0 {
				info.name = strings.TrimSuffix(names[0], ".")
			}
			if isPublicIP(ip) {
				if txts, err := net.DefaultResolver.LookupTXT(ctx, cymruName(ip)); err == nil && len(txts) > 0 {
					info.asn = parseCymru(txts[0])
				}
			}
			info.exp = time.Now().Add(6 * time.Hour)
			hopCacheMu.Lock()
			hopCache[h.Addr] = info
			hopCacheMu.Unlock()
			h.Name, h.ASN = info.name, info.asn
		}(&hops[i], ip)
	}
	wg.Wait()
}
