package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"maps"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// dnsResponder stands in for a resolver on loopback. answer turns each query
// into the datagrams sent back, in order; it runs on the responder's own
// goroutine, one query at a time.
func dnsResponder(t *testing.T, answer func(q []byte) [][]byte) int {
	t.Helper()
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback UDP available here: %v", err)
	}
	t.Cleanup(func() { c.Close() })
	go func() {
		buf := make([]byte, 1500)
		for {
			n, addr, err := c.ReadFrom(buf)
			if err != nil {
				return
			}
			for _, b := range answer(append([]byte(nil), buf[:n]...)) {
				c.WriteTo(b, addr)
			}
		}
	}()
	return c.LocalAddr().(*net.UDPAddr).Port
}

// dnsAnswer is what a resolver sends back: the query, flagged as a response,
// with the given RCODE. Answer records are not needed, since the probe never
// reads them.
func dnsAnswer(q []byte, rcode byte) []byte {
	b := append([]byte(nil), q...)
	b[2] |= 0x80
	b[3] = b[3]&0xf0 | rcode
	return b
}

func dnsTarget(port, timeoutMs int) *Target {
	return &Target{ID: 1, Slug: "resolver", Title: "resolver", Host: "127.0.0.1",
		PinIP: "127.0.0.1", Proto: "dns", Port: port, DNSName: ".", DNSType: "NS",
		Packets: 3, SpacingMs: 5, TimeoutMs: timeoutMs}
}

func TestDNSQueryIsByteExact(t *testing.T) {
	for _, c := range []struct {
		name string
		id   uint16
		typ  uint16
		want []byte
	}{
		{".", 0x1234, 2, []byte{
			0x12, 0x34, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			0x00, 0x00, 0x02, 0x00, 0x01}},
		{"Example.COM", 0xbeef, 1, []byte{
			0xbe, 0xef, 0x01, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
			7, 'e', 'x', 'a', 'm', 'p', 'l', 'e', 3, 'c', 'o', 'm', 0,
			0x00, 0x01, 0x00, 0x01}},
	} {
		_, wire, err := dnsName(c.name)
		if err != nil {
			t.Fatalf("%q: %v", c.name, err)
		}
		if got := dnsQuery(c.id, wire, c.typ); !bytes.Equal(got, c.want) {
			t.Errorf("%q:\n got % x\nwant % x", c.name, got, c.want)
		}
	}
}

func TestDNSNames(t *testing.T) {
	for _, c := range []struct{ in, want string }{
		{"", "."}, {".", "."}, {"  .  ", "."},
		{"example.com", "example.com."}, {"Example.COM.", "example.com."},
		{"\texample.com ", "example.com."}, {"_dmarc.example.com", "_dmarc.example.com."},
		{"xn--bcher-kva.example", "xn--bcher-kva.example."},
	} {
		got, _, err := dnsName(c.in)
		if err != nil || got != c.want {
			t.Errorf("dnsName(%q) = %q, %v; want %q", c.in, got, err, c.want)
		}
	}
	for _, bad := range []string{
		"exa mple.com", "a..b", "..", "-a.example", "a-.example", "bücher.example",
		"*.example.com", "example.com/", strings.Repeat("a", 64) + ".example",
		strings.Repeat(strings.Repeat("a", 63)+".", 4) + "example",
	} {
		if got, _, err := dnsName(bad); err == nil {
			t.Errorf("dnsName(%q) accepted as %q", bad, got)
		}
	}
}

func TestDNSAnswersAreTimed(t *testing.T) {
	for _, c := range []struct {
		name  string
		reply func(q []byte) []byte
	}{
		{"NOERROR", func(q []byte) []byte { return dnsAnswer(q, 0) }},
		// The name does not exist, and the resolver said so: it works.
		{"NXDOMAIN", func(q []byte) []byte { return dnsAnswer(q, 3) }},
		// Timing the resolver, not fetching the records: no retry over TCP.
		{"truncated", func(q []byte) []byte {
			b := dnsAnswer(q, 0)
			b[2] |= 0x02
			return b
		}},
		// RFC 4343: a name is the same name in any case.
		{"case", func(q []byte) []byte {
			b := dnsAnswer(q, 0)
			copy(b[dnsHeaderLen:], bytes.ToUpper(b[dnsHeaderLen:len(b)-4]))
			return b
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			port := dnsResponder(t, func(q []byte) [][]byte { return [][]byte{c.reply(q)} })
			tg := dnsTarget(port, 300)
			tg.DNSName = "example.com."
			p := &Prober{}
			rtts, errStr := p.runDNS(tg)
			if errStr != "" || len(rtts) != 3 {
				t.Fatalf("got %d answers and %q, want 3 and no error", len(rtts), errStr)
			}
			for _, rtt := range rtts {
				if rtt <= 0 {
					t.Errorf("round trip of %v microseconds", rtt)
				}
			}
		})
	}
}

// SERVFAIL is the resolver failing, which is what a DNS target exists to
// see: each one is a lost query, and a pass of nothing else says so.
func TestDNSServfailIsLoss(t *testing.T) {
	n := 0
	port := dnsResponder(t, func(q []byte) [][]byte {
		n++
		if n == 1 {
			return [][]byte{dnsAnswer(q, 2)}
		}
		return [][]byte{dnsAnswer(q, 0)}
	})
	p := &Prober{}
	if rtts, errStr := p.runDNS(dnsTarget(port, 300)); len(rtts) != 2 || errStr != "" {
		t.Errorf("one SERVFAIL out of three: got %d answers and %q, want 2 and no error",
			len(rtts), errStr)
	}

	port = dnsResponder(t, func(q []byte) [][]byte { return [][]byte{dnsAnswer(q, 2)} })
	rtts, errStr := p.runDNS(dnsTarget(port, 300))
	if len(rtts) != 0 || !strings.Contains(errStr, "SERVFAIL") {
		t.Errorf("only SERVFAIL: got %d answers and %q", len(rtts), errStr)
	}
}

// A server refusing to serve the probe is a configuration to fix, and the
// reason has to say that rather than read as an unreachable resolver.
//
// Unbound's access control, which by default refuses every client but
// localhost, answers a query that carries no EDNS record with the bare
// header: QR and RCODE set, every count zeroed, no question. The probe never
// sends EDNS, so that is the form the refusal takes there.
func TestDNSRefusedIsSurfaced(t *testing.T) {
	for _, c := range []struct {
		name  string
		reply func(q []byte) []byte
	}{
		{"with the question", func(q []byte) []byte { return dnsAnswer(q, 5) }},
		{"Unbound's bare header", func(q []byte) []byte {
			return []byte{q[0], q[1], 0x81, 0x05, 0, 0, 0, 0, 0, 0, 0, 0}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			port := dnsResponder(t, func(q []byte) [][]byte { return [][]byte{c.reply(q)} })
			p := &Prober{}
			rtts, errStr := p.runDNS(dnsTarget(port, 300))
			if len(rtts) != 0 {
				t.Fatalf("a REFUSED answer was timed as an answer: %v", rtts)
			}
			if !strings.Contains(errStr, "REFUSED") || !strings.Contains(errStr, "configuration") {
				t.Errorf("the reason should name the refusal as a configuration problem: %q", errStr)
			}
		})
	}
}

// The reason for a random ID per query. With the burst in flight, an answer
// can land just after a later query left; timing it against that one would
// report a latency far below the truth, and nothing about the graph would
// look wrong.
func TestDNSAnswerIsTimedAgainstItsOwnQuery(t *testing.T) {
	// Each query is answered only when the next one arrives.
	var prev []byte
	port := dnsResponder(t, func(q []byte) [][]byte {
		late := prev
		prev = q
		if late == nil {
			return nil
		}
		return [][]byte{dnsAnswer(late, 0)}
	})
	tg := dnsTarget(port, 150)
	tg.SpacingMs = 60
	p := &Prober{}
	rtts, errStr := p.runDNS(tg)
	if len(rtts) != 2 || errStr != "" {
		t.Fatalf("got %d answers and %q, want the first two queries answered", len(rtts), errStr)
	}
	for _, rtt := range rtts {
		if rtt < 30_000 {
			t.Errorf("an answer was timed at %.0f µs: against the query just sent, "+
				"not the one it answers 60 ms earlier", rtt)
		}
	}

	// Answering every query with the first one's answer: the first is
	// answered once, and its copies answer nothing.
	var first []byte
	port = dnsResponder(t, func(q []byte) [][]byte {
		if first == nil {
			first = q
		}
		return [][]byte{dnsAnswer(first, 0)}
	})
	if rtts, _ := p.runDNS(dnsTarget(port, 80)); len(rtts) != 1 {
		t.Errorf("got %d answers, want 1", len(rtts))
	}
}

// An answer that comes after its query's timeout is lost, even while the pass
// is still reading for the queries sent after it: the timeout means the same
// thing for every query of the burst.
func TestDNSLateAnswerIsDropped(t *testing.T) {
	port := dnsResponder(t, func(q []byte) [][]byte {
		time.Sleep(120 * time.Millisecond)
		return [][]byte{dnsAnswer(q, 0)}
	})
	tg := dnsTarget(port, 80)
	tg.SpacingMs = 150
	p := &Prober{}
	rtts, errStr := p.runDNS(tg)
	if len(rtts) != 0 {
		t.Fatalf("got %d answers, each 40 ms past its query's timeout", len(rtts))
	}
	if !strings.Contains(errStr, "no answer to 3 queries") {
		t.Errorf("a pass that heard only stragglers is silence: %q", errStr)
	}
}

// The queries do not wait for each other, so a resolver that says nothing
// holds the pass for the last query's timeout and no longer, inside the
// packets × spacing + timeout that checkTarget fits into the interval. One
// timeout per query, in turn, would have overrun it.
func TestDNSSilenceHoldsThePassForItsBudget(t *testing.T) {
	port := dnsResponder(t, func(q []byte) [][]byte { return nil })
	tg := dnsTarget(port, 150)
	tg.Packets, tg.SpacingMs = 4, 100
	ms := func(n int) time.Duration { return time.Duration(n) * time.Millisecond }
	floor := ms((tg.Packets-1)*tg.SpacingMs + tg.TimeoutMs)
	budget := ms(tg.Packets*tg.SpacingMs + tg.TimeoutMs)
	p := &Prober{}
	begin := time.Now()
	rtts, errStr := p.runDNS(tg)
	took := time.Since(begin)
	if len(rtts) != 0 || !strings.Contains(errStr, "no answer to 4 queries") {
		t.Errorf("got %d answers and %q from a silent resolver", len(rtts), errStr)
	}
	if took < floor || took >= budget {
		t.Errorf("a silent pass took %v: want the last query's timeout, %v, "+
			"and less than the %v budget", took, floor, budget)
	}
}

// A bare header, every count zeroed, is taken for the error it states. Without
// the question it can only ever be an error: one claiming NOERROR or NXDOMAIN
// is not an answer to anything, and one that is not a response to this query
// is not a refusal of it either.
func TestDNSBareErrorHeader(t *testing.T) {
	bare := func(flags uint16) func(q []byte) []byte {
		return func(q []byte) []byte {
			b := make([]byte, dnsHeaderLen)
			copy(b, q[:2])
			binary.BigEndian.PutUint16(b[2:4], flags)
			return b
		}
	}
	for _, c := range []struct {
		name  string
		reply func(q []byte) []byte
		want  string
	}{
		{"SERVFAIL", bare(0x8102), "SERVFAIL"},
		{"FORMERR", bare(0x8101), "FORMERR"},
		{"NOTIMP", bare(0x8104), "NOTIMP"},
		{"NOERROR", bare(0x8100), "no answer to 3 queries"},
		{"NXDOMAIN", bare(0x8103), "no answer to 3 queries"},
		{"REFUSED without QR", bare(0x0105), "no answer to 3 queries"},
		{"REFUSED to another opcode", bare(0x8905), "no answer to 3 queries"},
		{"REFUSED to another ID", func(q []byte) []byte {
			b := bare(0x8105)(q)
			b[1]++
			return b
		}, "no answer to 3 queries"},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			port := dnsResponder(t, func(q []byte) [][]byte { return [][]byte{c.reply(q)} })
			p := &Prober{}
			rtts, errStr := p.runDNS(dnsTarget(port, 80))
			if len(rtts) != 0 {
				t.Fatalf("a header without the question was timed as an answer: %v", rtts)
			}
			if !strings.Contains(errStr, c.want) {
				t.Errorf("the pass said %q, want %q", errStr, c.want)
			}
		})
	}
}

// Anything that is not the answer to this very query is dropped, and the
// wait goes on.
func TestDNSIgnoresWhatIsNotItsAnswer(t *testing.T) {
	for _, c := range []struct {
		name  string
		reply func(q []byte) []byte
	}{
		{"wrong ID", func(q []byte) []byte {
			b := dnsAnswer(q, 0)
			b[1]++
			return b
		}},
		{"wrong name", func(q []byte) []byte {
			b := dnsAnswer(q, 0)
			b[13] = 'f'
			return b
		}},
		{"wrong type", func(q []byte) []byte {
			b := dnsAnswer(q, 0)
			binary.BigEndian.PutUint16(b[len(b)-4:], 28)
			return b
		}},
		{"wrong class", func(q []byte) []byte {
			b := dnsAnswer(q, 0)
			binary.BigEndian.PutUint16(b[len(b)-2:], 3)
			return b
		}},
		{"two questions", func(q []byte) []byte {
			b := dnsAnswer(q, 0)
			b[5] = 2
			return b
		}},
		{"another opcode", func(q []byte) []byte {
			b := dnsAnswer(q, 0)
			b[2] |= 0x10
			return b
		}},
		// An RFC 862 echo service on port 53 sends the query back, which is
		// not an answer.
		{"echoed query", func(q []byte) []byte { return q }},
		{"cut short", func(q []byte) []byte { return dnsAnswer(q, 0)[:len(q)-1] }},
		{"header only", func(q []byte) []byte { return dnsAnswer(q, 0)[:dnsHeaderLen] }},
		{"compression pointer", func(q []byte) []byte {
			b := dnsAnswer(q, 0)[:dnsHeaderLen]
			return append(b, 0xc0, 0x00, 0x00, 0x01, 0x00, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00,
				0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00)
		}},
		{"garbage", func(q []byte) []byte { return []byte("not a DNS message at all") }},
		{"empty", func(q []byte) []byte { return []byte{} }},
	} {
		t.Run(c.name, func(t *testing.T) {
			t.Parallel()
			port := dnsResponder(t, func(q []byte) [][]byte { return [][]byte{c.reply(q)} })
			tg := dnsTarget(port, 80)
			tg.DNSName = "example.com."
			p := &Prober{}
			if rtts, errStr := p.runDNS(tg); len(rtts) != 0 {
				t.Errorf("timed as an answer: %d, %q", len(rtts), errStr)
			}
		})
	}

	// Dropped, not fatal: the real answer behind it is still timed.
	port := dnsResponder(t, func(q []byte) [][]byte {
		return [][]byte{[]byte("noise"), dnsAnswer(q[:4], 0), dnsAnswer(q, 0)}
	})
	p := &Prober{}
	if rtts, errStr := p.runDNS(dnsTarget(port, 300)); len(rtts) != 3 {
		t.Errorf("noise ahead of the answer cost answers: %d, %q", len(rtts), errStr)
	}
}

// The answer must come from where the query went. A resolver answering from
// another port is one a stub resolver would not believe either.
func TestDNSIgnoresAnotherSource(t *testing.T) {
	in, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback UDP available here: %v", err)
	}
	defer in.Close()
	out, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback UDP available here: %v", err)
	}
	defer out.Close()
	go func() {
		buf := make([]byte, 1500)
		for {
			n, addr, err := in.ReadFrom(buf)
			if err != nil {
				return
			}
			out.WriteTo(dnsAnswer(buf[:n], 0), addr)
		}
	}()
	p := &Prober{}
	if rtts, _ := p.runDNS(dnsTarget(in.LocalAddr().(*net.UDPAddr).Port, 80)); len(rtts) != 0 {
		t.Errorf("an answer from another port was timed: %v", rtts)
	}
}

func TestDNSNobodyListening(t *testing.T) {
	c, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback UDP available here: %v", err)
	}
	port := c.LocalAddr().(*net.UDPAddr).Port
	c.Close()
	p := &Prober{}
	rtts, errStr := p.runDNS(dnsTarget(port, 80))
	if len(rtts) != 0 {
		t.Fatalf("got %d answers from a port nobody listens on", len(rtts))
	}
	// Loopback delivers the ICMP error; a path with a firewall may not, and
	// then this is silence, which is the honest outcome.
	if !strings.Contains(errStr, "nothing is listening") && !strings.Contains(errStr, "no answer") {
		t.Errorf("the pass said %q, want the refusal or silence", errStr)
	}
}

// The protocol is dispatched by the pass itself, which is what the probe runs.
func TestDNSPassIsMeasured(t *testing.T) {
	port := dnsResponder(t, func(q []byte) [][]byte { return [][]byte{dnsAnswer(q, 0)} })
	w := NewWriter(nil, nil)
	p := &Prober{sink: w, failing: map[int64]string{}}
	p.Run(dnsTarget(port, 300))
	m := (<-w.ch).m
	if m.Err != "" || m.Sent != 3 || m.Lost != 0 || len(m.RTTus) != 3 || m.IP != "127.0.0.1" {
		t.Errorf("unexpected measurement: %+v", m)
	}
}

func TestDNSTargetValidation(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cat, err := store.CreateCategory("c", "Catégorie", "Category", true)
	if err != nil {
		t.Fatal(err)
	}
	api := &API{store: store, token: "secret", probeID: 1}
	call := func(method, path string, id int64, fields map[string]any) *httptest.ResponseRecorder {
		b, _ := json.Marshal(fields)
		req := httptest.NewRequest(method, path, bytes.NewReader(b))
		req.SetPathValue("id", strconv.FormatInt(id, 10))
		rec := httptest.NewRecorder()
		if method == "POST" {
			api.targetsPost(rec, req)
		} else {
			api.targetsPatch(rec, req)
		}
		return rec
	}
	create := func(fields map[string]any) (*Target, string) {
		body := map[string]any{"category_id": cat, "title": "Resolver", "host": "192.0.2.53",
			"interval_s": 60, "packets": 10, "spacing_ms": 100, "timeout_ms": 1000}
		maps.Copy(body, fields)
		rec := call("POST", "/api/v1/admin/targets", 0, body)
		if rec.Code != 200 {
			return nil, rec.Body.String()
		}
		var made Target
		json.Unmarshal(rec.Body.Bytes(), &made)
		got, err := store.TargetByID(made.ID)
		if err != nil {
			t.Fatal(err)
		}
		return got, ""
	}

	for _, c := range []struct {
		in         map[string]any
		port       int
		name, typ  string
		proto      string
		refusedFor string
	}{
		{in: map[string]any{"proto": "dns"}, port: 53, name: ".", typ: "NS"},
		{in: map[string]any{"proto": " DNS "}, port: 53, name: ".", typ: "NS"},
		{in: map[string]any{"proto": "dns", "dns_name": " Example.COM ", "dns_type": "aaaa"},
			port: 53, name: "example.com.", typ: "AAAA"},
		{in: map[string]any{"proto": "dns", "dns_name": "_dmarc.example.com.", "dns_type": "TXT",
			"port": 5353}, port: 5353, name: "_dmarc.example.com.", typ: "TXT"},
		{in: map[string]any{"proto": "dns", "host": "192.0.2.53:5353"}, port: 5353, name: ".", typ: "NS"},
		{in: map[string]any{"proto": "dns", "dns_type": "ANY"}, refusedFor: "dns_type"},
		{in: map[string]any{"proto": "dns", "dns_type": "AXFR"}, refusedFor: "dns_type"},
		{in: map[string]any{"proto": "dns", "dns_type": "PTR"}, refusedFor: "dns_type"},
		{in: map[string]any{"proto": "dns", "dns_name": "exa mple.com"}, refusedFor: "not a DNS name"},
		{in: map[string]any{"proto": "dns", "dns_name": "a..example"}, refusedFor: "not a DNS name"},
		{in: map[string]any{"proto": "dns", "dns_name": strings.Repeat("a", 64)}, refusedFor: "not a DNS name"},
		{in: map[string]any{"proto": "dns", "dns_name": strings.Repeat("abcdefghi.", 26)},
			refusedFor: "255 bytes"},
		{in: map[string]any{"proto": "dns", "port": 70000}, refusedFor: "port"},
		{in: map[string]any{"proto": "dns", "port": -1}, refusedFor: "port"},
		// A query on a target that makes none is cleared, not kept for show.
		{in: map[string]any{"proto": "icmp", "dns_name": "example.com", "dns_type": "A"},
			proto: "icmp"},
		{in: map[string]any{"proto": "udp", "dns_name": "example.com"}, proto: "udp", port: 7},
	} {
		got, refusal := create(c.in)
		if c.refusedFor != "" {
			if got != nil {
				t.Errorf("%v was accepted", c.in)
			} else if !strings.Contains(refusal, c.refusedFor) {
				t.Errorf("%v: the refusal %q does not mention %q", c.in, refusal, c.refusedFor)
			}
			continue
		}
		if got == nil {
			t.Errorf("%v was refused: %s", c.in, refusal)
			continue
		}
		proto := c.proto
		if proto == "" {
			proto = "dns"
		}
		if got.Proto != proto || got.Port != c.port || got.DNSName != c.name || got.DNSType != c.typ {
			t.Errorf("%v was stored as %s port %d %q %q, want %s port %d %q %q", c.in,
				got.Proto, got.Port, got.DNSName, got.DNSType, proto, c.port, c.name, c.typ)
		}
	}

	// Editing goes through the same gate, and the PATCH body reads both
	// fields by their JSON names.
	tg, _ := create(map[string]any{"proto": "dns"})
	path := "/api/v1/admin/targets/" + strconv.FormatInt(tg.ID, 10)
	if rec := call("PATCH", path, tg.ID, map[string]any{"dns_name": "example.net", "dns_type": "soa"}); rec.Code != 200 {
		t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
	}
	if got, _ := store.TargetByID(tg.ID); got.DNSName != "example.net." || got.DNSType != "SOA" {
		t.Errorf("the query was not saved: %q %q", got.DNSName, got.DNSType)
	}
	if rec := call("PATCH", path, tg.ID, map[string]any{"dns_type": "ANY"}); rec.Code != 400 {
		t.Errorf("an edit to a refused type answered HTTP %d", rec.Code)
	}
	if rec := call("PATCH", path, tg.ID, map[string]any{"proto": "icmp"}); rec.Code != 200 {
		t.Fatalf("switching to ICMP: HTTP %d: %s", rec.Code, rec.Body.String())
	}
	if got, _ := store.TargetByID(tg.ID); got.DNSName != "" || got.DNSType != "" {
		t.Errorf("an ICMP target kept a query: %q %q", got.DNSName, got.DNSType)
	}
	if rec := call("PATCH", path, tg.ID, map[string]any{"proto": "dns"}); rec.Code != 200 {
		t.Fatalf("switching back to DNS: HTTP %d: %s", rec.Code, rec.Body.String())
	}
	if got, _ := store.TargetByID(tg.ID); got.DNSName != "." || got.DNSType != "NS" || got.Port != 53 {
		t.Errorf("switching back did not restore the default query: %+v", got)
	}
}

// The query travels with the target to every probe, local or remote, since
// both read the same endpoint.
func TestDNSQueryTravelsToTheProbe(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cat, _ := store.CreateCategory("c", "C", "C", true)
	if _, err := store.CreateTarget(&Target{CategoryID: cat, Slug: "r", Title: "r",
		Host: "192.0.2.53", Proto: "dns", DNSName: "example.net", DNSType: "A",
		IntervalS: 60, Packets: 10, SpacingMs: 100, TimeoutMs: 1000, Public: true, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	id, err := store.ProbeID("local", "Local", "here", true)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	probeEndpoints(mux, NewTargetCache(store), NewWriter(store, nil), func(*http.Request) (probeCaller, bool) {
		return probeCaller{id: id}, true
	})
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest("GET", "/probe/v1/targets", nil))
	var resp probeTargetsResp
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if len(resp.Targets) != 1 {
		t.Fatalf("%d targets, want 1", len(resp.Targets))
	}
	if got := resp.Targets[0]; got.Proto != "dns" || got.Port != 53 ||
		got.DNSName != "example.net." || got.DNSType != "A" {
		t.Errorf("the probe was sent %s port %d %q %q", got.Proto, got.Port, got.DNSName, got.DNSType)
	}
}

// A target whose address is private keeps its query name private too: it can
// name an internal zone as plainly as the host names an internal machine.
func TestDNSQueryNameFollowsTheHiddenHost(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cat, _ := store.CreateCategory("c", "C", "C", true)
	for _, c := range []struct {
		slug, name string
		hide       bool
	}{{"hidden", "intranet.example.net", true}, {"shown", "www.example.org", false}} {
		if _, err := store.CreateTarget(&Target{CategoryID: cat, Slug: c.slug, Title: c.slug,
			Host: "192.0.2.53", Proto: "dns", DNSName: c.name, HideHost: c.hide,
			IntervalS: 60, Packets: 10, SpacingMs: 100, TimeoutMs: 1000, Public: true, Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	// The store rather than the handler: the handler's public cache is shared
	// by every store in the process, and keyed by a counter each one restarts.
	tree := func(publicOnly bool) string {
		cats, err := store.Tree(publicOnly)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(cats)
		return string(b)
	}
	pub := tree(true)
	if strings.Contains(pub, "intranet") {
		t.Error("the public tree names the zone of a target whose address is private")
	}
	if !strings.Contains(pub, "www.example.org.") {
		t.Error("the public tree lost the query of a target that publishes its address")
	}
	if !strings.Contains(tree(false), "intranet.example.net.") {
		t.Error("the operator must still see the query")
	}
}

// Neither a breakdown nor a peer's double-check can reproduce a query: the
// first times a TCP handshake, and a double-check request carries no DNS
// question. Both refuse rather than measure something else under its name.
func TestDNSHasNoBreakdownAndNoDoubleCheck(t *testing.T) {
	store := bdStore(t)
	id := bdTarget(t, store, "resolver", "192.0.2.53", "dns", 0)
	api := &API{store: store, token: "secret", probeID: 1}
	req := httptest.NewRequest("POST", "/api/v1/admin/breakdown/"+strconv.FormatInt(id, 10), nil)
	req.SetPathValue("id", strconv.FormatInt(id, 10))
	rec := httptest.NewRecorder()
	api.breakdownRun(rec, req)
	if rec.Code != 400 {
		t.Errorf("a DNS target was broken down: HTTP %d", rec.Code)
	}
	for _, b := range store.Breakdowns() {
		if b.TargetID == id {
			t.Error("a DNS target is listed for breakdown")
		}
	}

	f := &Federation{store: store}
	if _, err := f.RequestDoubleCheck(id, "AS64501", 5); err == nil ||
		!strings.Contains(err.Error(), "ICMP and TCP") {
		t.Errorf("asking a peer about a DNS target: %v", err)
	}

	peer, priv, tree, _ := dcPeers(t)
	*tree = []dcTreeCat{{Targets: []dcTreeTarget{
		{Host: "198.51.100.53", Proto: "dns", Port: 53, Public: true}}}}
	peer.SetDCGrant("AS64501", defaultDCCaps(), "ops")
	if code, body := dcAsk(t, peer, priv, dcRequest{Host: "198.51.100.53", Proto: "dns",
		Port: 53, IntervalS: 60, Minutes: 5}); code == 200 {
		t.Errorf("a peer accepted to double-check a DNS target: %s", body)
	}
}
