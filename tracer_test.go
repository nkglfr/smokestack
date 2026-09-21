package main

import (
	"encoding/binary"
	"net"
	"strings"
	"testing"
	"time"
)

func icmpHdr(typ byte, id, seq uint16) []byte {
	b := make([]byte, 8)
	b[0] = typ
	binary.BigEndian.PutUint16(b[4:6], id)
	binary.BigEndian.PutUint16(b[6:8], seq)
	return b
}

// Time Exceeded (RFC 792): 8-byte header, then the original IP header and
// the first 8 bytes of our echo request.
func TestParseTimeExceededV4(t *testing.T) {
	inner := make([]byte, 20)
	inner[0], inner[9] = 0x45, 1
	pkt := append(append([]byte{11, 0, 0, 0, 0, 0, 0, 0}, inner...), icmpHdr(8, 0x1234, 0x0107)...)
	id, seq, kind, _, ok := parseTraceICMP(pkt, false)
	if !ok || id != 0x1234 || seq != 0x0107 || kind != 'x' {
		t.Fatalf("v4 time exceeded mal lu : id=%x seq=%x kind=%c ok=%v", id, seq, kind, ok)
	}
	pkt[0], pkt[1] = 3, 0 // network unreachable
	if _, _, kind, code, ok := parseTraceICMP(pkt, false); !ok || kind != 'u' || code != 0 {
		t.Error("v4 unreachable mal lu")
	}
}

// ICMPv6 Time Exceeded (RFC 4443, type 3): original IPv6 header (40 bytes,
// next header 58) then our echo request.
func TestParseTimeExceededV6(t *testing.T) {
	inner := make([]byte, 40)
	inner[0], inner[6] = 0x60, 58
	pkt := append(append([]byte{3, 0, 0, 0, 0, 0, 0, 0}, inner...), icmpHdr(128, 0xbeef, 0x0205)...)
	id, seq, kind, _, ok := parseTraceICMP(pkt, true)
	if !ok || id != 0xbeef || seq != 0x0205 || kind != 'x' {
		t.Fatalf("v6 time exceeded mal lu : id=%x seq=%x ok=%v", id, seq, ok)
	}
	if id, _, kind, _, ok := parseTraceICMP(icmpHdr(129, 0xbeef, 9), true); !ok || kind != 'r' || id != 0xbeef {
		t.Error("v6 echo reply de la destination mal lu")
	}
	// A quoted packet that is not ICMPv6 (e.g. UDP) must be ignored.
	inner[6] = 17
	pkt = append(append([]byte{3, 0, 0, 0, 0, 0, 0, 0}, inner...), icmpHdr(128, 0xbeef, 1)...)
	if _, _, _, _, ok := parseTraceICMP(pkt, true); ok {
		t.Error("un paquet cite non ICMPv6 doit etre ignore")
	}
}

func TestEchoRequestV6(t *testing.T) {
	b := echoRequest(true, 7, 9)
	stamp(b, true)
	if b[0] != 128 || b[2] != 0 || b[3] != 0 {
		t.Error("ICMPv6 : type 128 et somme de controle laissee au noyau attendus")
	}
	b4 := echoRequest(false, 7, 9)
	stamp(b4, false)
	if b4[0] != 8 || checksum(b4) != 0 {
		t.Error("ICMPv4 : somme de controle incorrecte")
	}
}

func TestTraceSeq(t *testing.T) {
	for _, c := range []struct{ run, ttl, round int }{{1, 1, 0}, {255, 30, 2}, {300, 63, 3}} {
		r, ttl, round := decodeTraceSeq(traceSeq(uint16(c.run), c.ttl, c.round))
		if r != uint16(c.run)&0xff || ttl != c.ttl || round != c.round {
			t.Errorf("seq %v : relu run=%d ttl=%d round=%d", c, r, ttl, round)
		}
	}
}

func TestBuildHops(t *testing.T) {
	dst := net.ParseIP("203.0.113.9")
	a, b := net.ParseIP("192.0.2.1"), net.ParseIP("198.51.100.1")
	replies := []traceReply{
		{ttl: 1, from: a, rtt: 0.5, kind: 'x'}, {ttl: 1, from: a, rtt: 0.6, kind: 'x'},
		{ttl: 2, from: b, rtt: 3.1, kind: 'x'},
		{ttl: 4, from: dst, rtt: 9.0, kind: 'r'},
		{ttl: 5, from: dst, rtt: 9.2, kind: 'r'},
	}
	hops, reached, _ := buildHops(replies, dst, 30, false)
	if !reached || len(hops) != 4 {
		t.Fatalf("attendu 4 sauts jusqu'a la destination, obtenu %d (reached=%v)", len(hops), reached)
	}
	if hops[0].Addr != "192.0.2.1" || len(hops[0].RTTms) != 2 || hops[2].Addr != "" {
		t.Error("sauts mal construits (le saut 3 est muet)")
	}
	// Destination unreachable: silent hops are trimmed to three.
	hops, reached, _ = buildHops(replies[:3], dst, 30, false)
	if reached || len(hops) != 5 {
		t.Errorf("attendu 2 sauts + 3 muets, obtenu %d", len(hops))
	}
	// A router refusing to forward is not the destination: the path stops
	// there, marked !N, and the destination is not reached.
	refused := []traceReply{{ttl: 1, from: a, rtt: 0.4, kind: 'u', code: 0}}
	hops, reached, _ = buildHops(refused, dst, 30, false)
	if reached || len(hops) != 1 || hops[0].Note != "!N" {
		t.Errorf("unreachable d'un routeur : reached=%v sauts=%d note=%q", reached, len(hops), hops[0].Note)
	}
}

func TestDetector(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	d := newDetector(TracerouteConfig{PerHour: 30, ReferenceHours: 24})
	d.now = func() time.Time { return now }
	ok := Measurement{Sent: 10, RTTus: []float64{10000, 10100, 9900, 10050, 10000, 9950, 10000, 10020, 9980, 10010}}
	kinds := map[string]int{}
	for i := 0; i < 10; i++ {
		k, _ := d.observe(1, ok)
		kinds[k]++
		now = now.Add(30 * time.Second)
	}
	if kinds["reference"] != 1 || kinds["anomaly"] != 0 {
		t.Fatalf("phase saine : %v", kinds)
	}
	lossy := Measurement{Sent: 10, Lost: 3, RTTus: ok.RTTus[:7]}
	if k, r := d.observe(1, lossy); k != "anomaly" || !strings.Contains(r, "loss 30%") {
		t.Fatalf("perte : attendu une anomalie, obtenu %q %q", k, r)
	}
	now = now.Add(30 * time.Second)
	if k, _ := d.observe(1, lossy); k != "" {
		t.Error("pas de second traceroute pendant la meme anomalie")
	}
	// Recovery then a latency jump more than 15 minutes later.
	for i := 0; i < 40; i++ {
		now = now.Add(30 * time.Second)
		d.observe(1, ok)
	}
	slow := Measurement{Sent: 10, RTTus: []float64{18000, 18100, 17900, 18000, 18050, 17950, 18000, 18020, 17980, 18010}}
	if k, r := d.observe(1, slow); k != "anomaly" || !strings.Contains(r, "latency x1.8") {
		t.Fatalf("latence : attendu une anomalie, obtenu %q %q", k, r)
	}
	// Hourly budget: 200 targets failing at once give at most 30 traces.
	d2 := newDetector(TracerouteConfig{PerHour: 30})
	n := 0
	for id := int64(0); id < 200; id++ {
		if k, _ := d2.observe(id, lossy); k == "anomaly" {
			n++
		}
	}
	if n != 30 {
		t.Errorf("budget horaire : %d traceroutes, attendu 30", n)
	}
}

func TestCymruAndResolver(t *testing.T) {
	if got := cymruName(net.ParseIP("1.2.3.4")); got != "4.3.2.1.origin.asn.cymru.com" {
		t.Errorf("v4 : %s", got)
	}
	v6 := cymruName(net.ParseIP("2001:db8::1"))
	if !strings.HasPrefix(v6, "1.0.0.0.0.0.0.0.") || !strings.HasSuffix(v6, "8.b.d.0.1.0.0.2.origin6.asn.cymru.com") {
		t.Errorf("v6 : %s", v6)
	}
	if parseCymru("13335 | 1.1.1.0/24 | US | arin | 2010-07-14") != "AS13335" ||
		parseCymru("23456 64512 | 10.0.0.0/8") != "AS23456" || parseCymru("garbage") != "" {
		t.Error("lecture de la reponse Cymru")
	}
	if isPublicIP(net.ParseIP("10.1.2.3")) || isPublicIP(net.ParseIP("100.64.0.1")) || !isPublicIP(net.ParseIP("9.9.9.9")) {
		t.Error("filtrage des adresses privees")
	}
	r := newResolver()
	if _, err := r.Resolve("2001:db8::1", 4); err == nil {
		t.Error("une adresse IPv6 sur une cible IPv4 doit etre refusee")
	}
	if ip, err := r.Resolve("[2001:db8::1]:443", 0); err != nil || ip.To4() != nil {
		t.Error("adresse IPv6 avec port mal lue")
	}
}
