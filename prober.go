package main

import (
	"context"
	"encoding/binary"
	"fmt"
	"hash/fnv"
	"log"
	"math/rand"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The probe keeps one raw ICMP socket per address family for all
// targets and matches replies by sequence number.
//
// Isolation: the probe knows neither the database nor the web interface.
// It reads its targets from a TargetSource (an in-memory cache) and hands
// results to a MeasureSink (a non-blocking queue). The same code runs
// inside the web service or in its own process (`smokestack probe`).
//
// Accuracy: replies are timestamped by the kernel (SO_TIMESTAMPNS) and the
// send time is written into the packet right before sending, so process
// load never adds to a measured round-trip time.
//
// Anomalies: after each pass the probe compares loss and median with a
// per-target moving baseline. When a target leaves it, a traceroute is
// run to show where the path changed (see tracer.go).

const icmpPayloadSize = 56

type TargetSource interface {
	Targets() []*Target
	TraceRequests() []int64
	CheckRequests() []int64
}

type MeasureSink interface {
	Submit(m Measurement, host string)
	SubmitTrace(tr *Traceroute)
}

type TracerouteConfig struct {
	Disabled       bool `json:"disabled"`
	MaxHops        int  `json:"max_hops"`        // default 30
	ReferenceHours int  `json:"reference_hours"` // default 24, negative = off
	PerHour        int  `json:"per_hour"`        // anomaly traces per hour, default 30
}

type inflight struct {
	mu   sync.Mutex
	rtts []float64
	done bool
}

type Prober struct {
	src      TargetSource
	sink     MeasureSink
	probeID  int64
	conn4    net.PacketConn
	conn6    net.PacketConn
	id       uint16
	kernelTS bool

	mu      sync.Mutex
	seq     uint16
	pending map[uint16]*inflight
	// sentAt keeps the send time of each sequence number on our side. The
	// time also travels in the packet, but a reply is not required to echo
	// the payload, and several home routers and CPE reply with a truncated
	// or rewritten one: relying on the echo alone counted those as lost.
	sentAt  map[uint16]int64
	running map[int64]bool
	failing map[int64]string    // dernière erreur journalisée, par cible
	wanted  map[int64]time.Time // immediate measurements waiting for the target

	res    *resolver
	tracer *Tracer
	det    *detector
}

func NewProber(src TargetSource, sink MeasureSink, probeID int64, tc TracerouteConfig) (*Prober, error) {
	p := &Prober{
		src: src, sink: sink, probeID: probeID,
		id:      uint16(time.Now().UnixNano() & 0xffff),
		pending: map[uint16]*inflight{},
		sentAt:  map[uint16]int64{},
		running: map[int64]bool{},
		failing: map[int64]string{},
		wanted:  map[int64]time.Time{},
		res:     newResolver(),
	}
	var err4, err6 error
	p.conn4, err4 = net.ListenPacket("ip4:icmp", "0.0.0.0")
	p.conn6, err6 = net.ListenPacket("ip6:ipv6-icmp", "::")
	if err4 != nil && err6 != nil {
		return nil, err4
	}
	if err4 != nil {
		log.Printf("probe: IPv4 unavailable (%v)", err4)
		p.conn4 = nil
	}
	if err6 != nil {
		log.Printf("probe: IPv6 unavailable (%v), IPv6 targets will report errors", err6)
		p.conn6 = nil
	}
	for _, c := range []net.PacketConn{p.conn4, p.conn6} {
		if c != nil && enableKernelRxTimestamps(c) {
			p.kernelTS = true
		}
	}
	if p.kernelTS {
		log.Printf("probe: kernel receive timestamps enabled")
	} else {
		log.Printf("probe: kernel timestamps unavailable, using application timestamps")
	}
	if p.conn4 != nil {
		go p.readLoop(p.conn4, false)
	}
	if p.conn6 != nil {
		go p.readLoop(p.conn6, true)
	}
	if !tc.Disabled {
		if tc.MaxHops <= 0 || tc.MaxHops > 64 {
			tc.MaxHops = 30
		}
		if tc.ReferenceHours == 0 {
			tc.ReferenceHours = 24
		}
		if tc.PerHour <= 0 {
			tc.PerHour = 30
		}
		t, err := NewTracer(p.id+1, tc.MaxHops)
		if err != nil {
			log.Printf("probe: traceroute unavailable (%v)", err)
		} else {
			p.tracer = t
			p.det = newDetector(tc)
			go p.traceWorker()
			log.Printf("probe: traceroute on anomalies enabled (max %d hops)", tc.MaxHops)
		}
	}
	return p, nil
}

func (p *Prober) Close() {
	for _, c := range []net.PacketConn{p.conn4, p.conn6} {
		if c != nil {
			c.Close()
		}
	}
	if p.tracer != nil {
		p.tracer.Close()
	}
}

// ---------------------------------------------------------------- packets

func checksum(b []byte) uint16 {
	var sum uint32
	for i := 0; i+1 < len(b); i += 2 {
		sum += uint32(b[i])<<8 | uint32(b[i+1])
	}
	if len(b)%2 == 1 {
		sum += uint32(b[len(b)-1]) << 8
	}
	for sum>>16 != 0 {
		sum = (sum & 0xffff) + (sum >> 16)
	}
	return ^uint16(sum)
}

// echoRequest builds an ICMP (v4) or ICMPv6 echo request, without its
// send timestamp. For ICMPv6 the kernel computes the checksum.
func echoRequest(v6 bool, id, seq uint16) []byte {
	b := make([]byte, 8+icmpPayloadSize)
	b[0] = 8
	if v6 {
		b[0] = 128
	}
	binary.BigEndian.PutUint16(b[4:6], id)
	binary.BigEndian.PutUint16(b[6:8], seq)
	for i := 16; i < len(b); i++ {
		b[i] = byte(i)
	}
	return b
}

// stamp writes the send time (and, for IPv4, the checksum).
func stamp(b []byte, v6 bool) {
	b[2], b[3] = 0, 0
	binary.BigEndian.PutUint64(b[8:16], uint64(time.Now().UnixNano()))
	if !v6 {
		binary.BigEndian.PutUint16(b[2:4], checksum(b))
	}
}

// stripIPv4 removes the IPv4 header that raw IPv4 sockets deliver.
func stripIPv4(b []byte) []byte {
	if len(b) >= 20 && b[0]>>4 == 4 {
		if ihl := int(b[0]&0x0f) * 4; ihl >= 20 && len(b) > ihl {
			return b[ihl:]
		}
	}
	return b
}

func (p *Prober) readLoop(conn net.PacketConn, v6 bool) {
	buf := make([]byte, 1500)
	oob := make([]byte, 128)
	reply := byte(0)
	if v6 {
		reply = 129
	}
	for {
		n, now, err := readStamped(conn, buf, oob)
		if err != nil {
			return
		}
		b := buf[:n]
		if !v6 {
			b = stripIPv4(b)
		}
		p.handleReply(b, reply, now)
	}
}

func (p *Prober) nextSeq(fl *inflight) uint16 {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seq++
	p.pending[p.seq] = fl
	p.sentAt[p.seq] = time.Now().UnixNano()
	return p.seq
}

func (p *Prober) release(seqs []uint16) {
	p.mu.Lock()
	for _, s := range seqs {
		delete(p.pending, s)
		delete(p.sentAt, s)
	}
	p.mu.Unlock()
}

// handleReply accounts one ICMP echo reply. It returns false when the
// packet is not one of ours or cannot be timed.
func (p *Prober) handleReply(b []byte, reply byte, now int64) bool {
	// 8 bytes: an echo reply is allowed to come back without our payload.
	if len(b) < 8 || b[0] != reply || binary.BigEndian.Uint16(b[4:6]) != p.id {
		return false
	}
	seq := binary.BigEndian.Uint16(b[6:8])
	p.mu.Lock()
	fl, sent := p.pending[seq], p.sentAt[seq]
	p.mu.Unlock()
	if fl == nil {
		return false
	}
	// The echoed timestamp is used when it is there and plausible; our own
	// record is the fallback, so a truncated or rewritten payload no longer
	// turns an answered probe into a lost one.
	if len(b) >= 16 {
		if ts := int64(binary.BigEndian.Uint64(b[8:16])); ts > 0 && now-ts >= 0 && now-ts < 60e9 {
			sent = ts
		}
	}
	if sent <= 0 {
		return false
	}
	rtt := float64(now-sent) / 1000 // microseconds
	if rtt < 0 || rtt > 60_000_000 {
		return false
	}
	fl.mu.Lock()
	if !fl.done {
		fl.rtts = append(fl.rtts, rtt)
	}
	fl.mu.Unlock()
	return true
}

// targetIP is the address to probe: the pinned one when the operator set
// it, otherwise the result of resolving the host.
func (p *Prober) targetIP(t *Target) (net.IP, error) {
	if t.PinIP != "" {
		if ip := net.ParseIP(t.PinIP); ip != nil {
			return ip, nil
		}
	}
	return p.res.Resolve(t.Host, t.Family)
}

// -------------------------------------------------------------- resolution

type resolved struct {
	ip  net.IP
	exp time.Time
}

type resolver struct {
	mu    sync.Mutex
	cache map[string]resolved
}

func newResolver() *resolver { return &resolver{cache: map[string]resolved{}} }

// hostOnly strips a port ("host:443", "[2001:db8::1]:443").
func hostOnly(h string) string {
	if host, _, err := net.SplitHostPort(h); err == nil {
		return host
	}
	return strings.Trim(h, "[]")
}

// Resolve returns the address to probe for a target: a literal address is
// used as is; a name is resolved in the requested family (auto: IPv4, then
// IPv6). Answers are cached for 5 minutes.
func (r *resolver) Resolve(host string, family int) (net.IP, error) {
	host = hostOnly(host)
	if ip := net.ParseIP(host); ip != nil {
		is4 := ip.To4() != nil
		if (family == 4 && !is4) || (family == 6 && is4) {
			return nil, fmt.Errorf("address %s does not match family IPv%d", host, family)
		}
		return ip, nil
	}
	key := fmt.Sprintf("%s|%d", host, family)
	r.mu.Lock()
	if c, ok := r.cache[key]; ok && time.Now().Before(c.exp) {
		r.mu.Unlock()
		return c.ip, nil
	}
	r.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	var ip net.IP
	switch family {
	case 4, 6:
		ips, err := net.DefaultResolver.LookupIP(ctx, fmt.Sprintf("ip%d", family), host)
		if err != nil || len(ips) == 0 {
			return nil, fmt.Errorf("cannot resolve %s in IPv%d: check the name, "+
				"or set the address family to auto", host, family)
		}
		ip = ips[0]
	default:
		ips, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
		if err != nil || len(ips) == 0 {
			return nil, fmt.Errorf("cannot resolve %s: no address returned by DNS", host)
		}
		ip = ips[0]
		for _, x := range ips {
			if x.To4() != nil {
				ip = x
				break
			}
		}
	}
	r.mu.Lock()
	r.cache[key] = resolved{ip: ip, exp: time.Now().Add(5 * time.Minute)}
	r.mu.Unlock()
	return ip, nil
}

// ------------------------------------------------------------------ passes

// Run executes one measurement pass on a target.
func (p *Prober) Run(t *Target) {
	start := time.Now()
	var m Measurement
	m.TargetID, m.ProbeID = t.ID, p.probeID
	m.TS = start.Unix()
	m.Sent = t.Packets

	switch t.Proto {
	case "tcp":
		m.RTTus, m.Err = p.runTCP(t)
	default:
		m.RTTus, m.Err = p.runICMP(t)
	}
	// The address actually probed: a rotating name (a pool) points at a
	// different machine every few minutes, which the back-office flags.
	if ip, err := p.targetIP(t); err == nil {
		m.IP = ip.String()
	}
	m.Lost = m.Sent - len(m.RTTus)
	// One line when a target starts failing, one when it comes back. The
	// per-target reason is in the back-office, but an operator reading
	// journalctl or docker logs should see it too.
	p.mu.Lock()
	was := p.failing[t.ID]
	switch {
	case m.Err != "" && m.Err != was:
		p.failing[t.ID] = m.Err
		log.Printf("target %q (%s): %s", t.Title, t.Host, m.Err)
	case m.Err == "" && was != "":
		delete(p.failing, t.ID)
		log.Printf("target %q (%s): answering again", t.Title, t.Host)
	}
	p.mu.Unlock()
	if m.Lost < 0 {
		m.Lost = 0
	}
	p.sink.Submit(m, t.Host)
	if p.det != nil {
		if kind, reason := p.det.observeTarget(t.ID, m, t.TraceHours); kind != "" {
			p.queueTrace(t, kind, reason)
		}
	}
}

func (p *Prober) runICMP(t *Target) ([]float64, string) {
	ip, err := p.targetIP(t)
	if err != nil {
		return nil, err.Error()
	}
	v6 := ip.To4() == nil
	conn := p.conn4
	if v6 {
		conn = p.conn6
	}
	if conn == nil {
		return nil, fmt.Sprintf("IPv%d is not available on this probe", map[bool]int{true: 6, false: 4}[v6])
	}
	addr := &net.IPAddr{IP: ip}
	fl := &inflight{}
	seqs := make([]uint16, 0, t.Packets)
	defer func() { p.release(seqs) }()

	spacing := time.Duration(t.SpacingMs) * time.Millisecond
	for i := 0; i < t.Packets; i++ {
		seq := p.nextSeq(fl)
		seqs = append(seqs, seq)
		pkt := echoRequest(v6, p.id, seq)
		stamp(pkt, v6)
		if _, err := conn.WriteTo(pkt, addr); err != nil {
			return nil, "send: " + err.Error()
		}
		if i < t.Packets-1 {
			time.Sleep(spacing)
		}
	}
	time.Sleep(time.Duration(t.TimeoutMs) * time.Millisecond)

	fl.mu.Lock()
	fl.done = true
	out := append([]float64(nil), fl.rtts...)
	fl.mu.Unlock()
	if len(out) == 0 {
		// A target that answers nothing at all deserves a reason, not just
		// 100 % loss: it is almost always filtering or rate limiting.
		return out, fmt.Sprintf("no reply to %d ICMP echo requests sent to %s "+
			"(ICMP filtered on the path, or the target rate-limits it: try fewer packets, "+
			"spaced further apart)", t.Packets, ip)
	}
	return out, ""
}

func (p *Prober) runTCP(t *Target) ([]float64, string) {
	timeout := time.Duration(t.TimeoutMs) * time.Millisecond
	spacing := time.Duration(t.SpacingMs) * time.Millisecond
	network := "tcp"
	if t.Family == 4 || t.Family == 6 {
		network = fmt.Sprintf("tcp%d", t.Family)
	}
	addr := t.Host
	switch {
	case t.Port > 0:
		addr = net.JoinHostPort(t.Host, strconv.Itoa(t.Port))
	case !strings.Contains(t.Host, ":"):
		// Go would say "missing port in address", which tells an operator
		// nothing about what to do.
		return nil, fmt.Sprintf("this TCP target has no port: set one (for example 443 or 80) " +
			"in the target settings")
	}
	var out []float64
	var lastErr string
	for i := 0; i < t.Packets; i++ {
		start := time.Now()
		c, err := net.DialTimeout(network, addr, timeout)
		if err == nil {
			// The SYN -> SYN/ACK time measured by the TCP stack does not
			// depend on process load; application timing is the fallback.
			elapsed := float64(time.Since(start).Microseconds())
			if rtt, ok := kernelTCPRTT(c); ok {
				elapsed = rtt
			}
			out = append(out, elapsed)
			c.Close()
		} else {
			lastErr = err.Error()
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

// passJitter is a small random delay added to each pass, so that targets
// landing on the same second do not all start at once, and so that passes
// never stay in lockstep with another system's timer.
func passJitter(interval int64) time.Duration {
	max := interval * 100 // a tenth of the interval, in milliseconds
	if max > 950 {
		max = 950
	}
	if max <= 0 {
		return 0
	}
	return time.Duration(rand.Int63n(max)) * time.Millisecond
}

// offsetFor spreads pass start times over the interval so that targets do
// not all start on the same second. Derived from the id: stable across
// restarts.
func offsetFor(id, interval int64) int64 {
	h := fnv.New32a()
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], uint64(id))
	h.Write(b[:])
	return int64(h.Sum32()) % interval
}

// claim reserves a target for one pass, so that a slow pass is never run
// twice at the same time. done releases it.
func (p *Prober) claim(id int64) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.running[id] {
		return false
	}
	p.running[id] = true
	return true
}

func (p *Prober) done(id int64) {
	p.mu.Lock()
	delete(p.running, id)
	p.mu.Unlock()
}

// Schedule starts passes until stop is closed.
func (p *Prober) Schedule(stop <-chan struct{}) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case now := <-tick.C:
			targets := p.src.Targets()
			if p.tracer != nil {
				for _, id := range p.src.TraceRequests() {
					for _, t := range targets {
						if t.ID == id {
							p.queueTrace(t, "manual", "requested from the back-office")
						}
					}
				}
			}
			// Targets waiting for an immediate measurement. A target
			// just created may not be in the cache yet, so a request is
			// kept and retried for a minute instead of being lost.
			for _, id := range p.src.CheckRequests() {
				p.wanted[id] = now.Add(time.Minute)
			}
			for id, deadline := range p.wanted {
				if now.After(deadline) {
					delete(p.wanted, id)
					continue
				}
				for _, t := range targets {
					if t.ID == id {
						delete(p.wanted, id)
						if p.claim(t.ID) {
							go func(tt *Target) { defer p.done(tt.ID); p.Run(tt) }(t)
						}
					}
				}
			}
			sec := now.Unix()
			for _, t := range targets {
				if mod(sec, t.IntervalS) != offsetFor(t.ID, t.IntervalS) {
					continue
				}
				if !p.claim(t.ID) {
					continue
				}
				go func(tt *Target) {
					time.Sleep(passJitter(tt.IntervalS))
					defer p.done(tt.ID)
					p.Run(tt)
				}(t)
			}
		}
	}
}

// ---------------------------------------------------------- anomaly logic

type targetState struct {
	base      float64 // median baseline, microseconds (EWMA over healthy passes)
	okPasses  int
	bad       bool
	lastTrace time.Time
	lastRef   time.Time
}

type detector struct {
	mu      sync.Mutex
	cfg     TracerouteConfig
	state   map[int64]*targetState
	budget  float64 // anomaly traces left (token bucket, refilled per hour)
	refill  time.Time
	lastRef time.Time
	now     func() time.Time
}

func newDetector(cfg TracerouteConfig) *detector {
	return &detector{cfg: cfg, state: map[int64]*targetState{},
		budget: float64(cfg.PerHour), refill: time.Now(), now: time.Now}
}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	c := append([]float64(nil), v...)
	sort.Float64s(c)
	return c[len(c)/2]
}

// observe updates a target's baseline and returns the kind of traceroute
// to run, if any: "anomaly" when the target leaves its baseline (at most
// every 15 min per target, 60 min while the anomaly lasts, within an hourly
// budget), "reference" now and then while it is healthy.
func (d *detector) observe(id int64, m Measurement) (kind, reason string) {
	return d.observeTarget(id, m, 0)
}

// observeTarget takes the target's own reference interval into account.
func (d *detector) observeTarget(id int64, m Measurement, traceHours int) (kind, reason string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	now := d.now()
	// A clock stepping backwards (NTP correction) must never drain the budget.
	if elapsed := now.Sub(d.refill); elapsed > 0 {
		d.budget += elapsed.Hours() * float64(d.cfg.PerHour)
	}
	if d.budget > float64(d.cfg.PerHour) {
		d.budget = float64(d.cfg.PerHour)
	}
	d.refill = now

	st := d.state[id]
	if st == nil {
		st = &targetState{}
		d.state[id] = st
	}
	loss := 0.0
	if m.Sent > 0 {
		loss = float64(m.Lost) * 100 / float64(m.Sent)
	}
	med := median(m.RTTus)
	switch {
	case loss > 3:
		reason = fmt.Sprintf("packet loss %.0f%%", loss)
	case st.okPasses >= 5 && st.base > 0 && med > st.base*1.4 && med-st.base > 1000:
		reason = fmt.Sprintf("latency x%.1f (%.1f ms, baseline %.1f ms)", med/st.base, med/1000, st.base/1000)
	}

	if reason != "" {
		wasOK := !st.bad
		st.bad = true
		since := now.Sub(st.lastTrace)
		if (wasOK && since >= 15*time.Minute) || since >= time.Hour {
			if d.budget >= 1 {
				d.budget--
				st.lastTrace = now
				return "anomaly", reason
			}
		}
		return "", ""
	}

	st.bad = false
	if st.base == 0 {
		st.base = med
	} else {
		st.base = 0.9*st.base + 0.1*med
	}
	st.okPasses++
	// A target can ask for a denser reference than the instance default:
	// watching a transit path closely is worth four traceroutes a day,
	// while a distant destination is not.
	every := d.cfg.ReferenceHours
	if traceHours > 0 {
		every = traceHours
	}
	if every > 0 && st.okPasses >= 5 &&
		now.Sub(st.lastRef) >= time.Duration(every)*time.Hour &&
		now.Sub(d.lastRef) >= time.Minute {
		st.lastRef, d.lastRef = now, now
		return "reference", "healthy path, for comparison"
	}
	return "", ""
}

// --------------------------------------------------------- trace worker

type traceJob struct {
	t      *Target
	kind   string
	reason string
}

var traceJobs = make(chan traceJob, 64)

func (p *Prober) queueTrace(t *Target, kind, reason string) {
	// References are low priority: skipped when the queue is busy.
	if kind == "reference" && len(traceJobs) > 4 {
		return
	}
	select {
	case traceJobs <- traceJob{t, kind, reason}:
	default:
	}
}

// traceWorker runs traceroutes one at a time.
func (p *Prober) traceWorker() {
	for job := range traceJobs {
		ip, err := p.res.Resolve(job.t.Host, job.t.Family)
		if err != nil {
			continue
		}
		start := time.Now()
		hops, reached, err := p.tracer.Trace(ip)
		if err != nil {
			log.Printf("traceroute %s: %v", job.t.Host, err)
			continue
		}
		enrichHops(hops)
		fam := 4
		if ip.To4() == nil {
			fam = 6
		}
		p.sink.SubmitTrace(&Traceroute{TargetID: job.t.ID, ProbeID: p.probeID, TS: start.Unix(),
			Kind: job.kind, Reason: job.reason, Family: fam, Dest: ip.String(), Reached: reached, Hops: hops})
		if job.kind != "reference" {
			log.Printf("traceroute %s (%s): %d hops, destination reached: %v", job.t.Host, job.reason, len(hops), reached)
		}
	}
}
