package main

import (
	"encoding/binary"
	"hash/fnv"
	"log"
	"net"
	"sync"
	"time"
)

// Le prober ouvre un seul socket ICMP brut pour toutes les cibles et
// fait correspondre les reponses par numero de sequence. Un socket par
// cible ne passerait pas l'echelle, et le timestamp d'emission est
// embarque dans le payload pour que le RTT ne depende pas du temps
// passe dans nos propres structures.

const icmpPayloadSize = 56

type inflight struct {
	mu   sync.Mutex
	rtts []float64
	done bool
}

type Prober struct {
	store   *Store
	probeID int64
	conn    net.PacketConn
	id      uint16

	mu      sync.Mutex
	seq     uint16
	pending map[uint16]*inflight
	running map[int64]bool

	archive *Archive
}

func NewProber(store *Store, probeID int64, archive *Archive) (*Prober, error) {
	conn, err := net.ListenPacket("ip4:icmp", "0.0.0.0")
	if err != nil {
		return nil, err
	}
	p := &Prober{
		store:   store,
		probeID: probeID,
		conn:    conn,
		id:      uint16(time.Now().UnixNano() & 0xffff),
		pending: map[uint16]*inflight{},
		running: map[int64]bool{},
		archive: archive,
	}
	go p.readLoop()
	return p, nil
}

func (p *Prober) Close() {
	if p.conn != nil {
		p.conn.Close()
	}
}

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

func (p *Prober) buildEcho(seq uint16) []byte {
	b := make([]byte, 8+icmpPayloadSize)
	b[0] = 8 // echo request
	b[1] = 0
	binary.BigEndian.PutUint16(b[4:6], p.id)
	binary.BigEndian.PutUint16(b[6:8], seq)
	binary.BigEndian.PutUint64(b[8:16], uint64(time.Now().UnixNano()))
	for i := 16; i < len(b); i++ {
		b[i] = byte(i)
	}
	binary.BigEndian.PutUint16(b[2:4], checksum(b))
	return b
}

func (p *Prober) readLoop() {
	buf := make([]byte, 1500)
	for {
		n, _, err := p.conn.ReadFrom(buf)
		if err != nil {
			return
		}
		now := time.Now().UnixNano()
		b := buf[:n]
		// Selon la plateforme, l'en-tete IP peut etre conserve.
		if len(b) >= 20 && b[0]>>4 == 4 {
			ihl := int(b[0]&0x0f) * 4
			if ihl >= 20 && len(b) > ihl {
				b = b[ihl:]
			}
		}
		if len(b) < 16 || b[0] != 0 { // echo reply uniquement
			continue
		}
		if binary.BigEndian.Uint16(b[4:6]) != p.id {
			continue
		}
		seq := binary.BigEndian.Uint16(b[6:8])
		sent := int64(binary.BigEndian.Uint64(b[8:16]))

		p.mu.Lock()
		fl := p.pending[seq]
		p.mu.Unlock()
		if fl == nil {
			continue
		}
		rtt := float64(now-sent) / 1000 // microsecondes
		if rtt < 0 || rtt > 60_000_000 {
			continue
		}
		fl.mu.Lock()
		if !fl.done {
			fl.rtts = append(fl.rtts, rtt)
		}
		fl.mu.Unlock()
	}
}

func (p *Prober) nextSeq(fl *inflight) uint16 {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seq++
	p.pending[p.seq] = fl
	return p.seq
}

func (p *Prober) release(seqs []uint16) {
	p.mu.Lock()
	for _, s := range seqs {
		delete(p.pending, s)
	}
	p.mu.Unlock()
}

// Run exécute une passe complète de mesure sur une cible.
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
	m.Lost = m.Sent - len(m.RTTus)
	if m.Lost < 0 {
		m.Lost = 0
	}

	if err := p.store.Record(m); err != nil {
		log.Printf("record %s: %v", t.Slug, err)
	}
	if p.archive != nil {
		p.archive.Append(m, t.Host)
	}
}

func (p *Prober) runICMP(t *Target) ([]float64, string) {
	addr, err := net.ResolveIPAddr("ip4", t.Host)
	if err != nil {
		return nil, "dns: " + err.Error()
	}
	fl := &inflight{}
	seqs := make([]uint16, 0, t.Packets)
	defer func() { p.release(seqs) }()

	spacing := time.Duration(t.SpacingMs) * time.Millisecond
	for i := 0; i < t.Packets; i++ {
		seq := p.nextSeq(fl)
		seqs = append(seqs, seq)
		if _, err := p.conn.WriteTo(p.buildEcho(seq), addr); err != nil {
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
	return out, ""
}

func (p *Prober) runTCP(t *Target) ([]float64, string) {
	timeout := time.Duration(t.TimeoutMs) * time.Millisecond
	spacing := time.Duration(t.SpacingMs) * time.Millisecond
	var out []float64
	var lastErr string
	for i := 0; i < t.Packets; i++ {
		start := time.Now()
		c, err := net.DialTimeout("tcp", t.Host, timeout)
		if err == nil {
			out = append(out, float64(time.Since(start).Microseconds()))
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

// offsetFor étale les départs de passe sur l'intervalle pour éviter que
// toutes les cibles partent à la seconde ronde. L'offset est dérivé de
// l'identifiant, donc stable entre deux redémarrages.
func offsetFor(id, interval int64) int64 {
	h := fnv.New32a()
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], uint64(id))
	h.Write(b[:])
	return int64(h.Sum32()) % interval
}

// Schedule tourne jusqu'à l'arrêt du contexte et déclenche les passes.
func (p *Prober) Schedule(stop <-chan struct{}) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			return
		case now := <-tick.C:
			targets, err := p.store.ActiveTargets()
			if err != nil {
				log.Printf("targets: %v", err)
				continue
			}
			sec := now.Unix()
			for _, t := range targets {
				if mod(sec, t.IntervalS) != offsetFor(t.ID, t.IntervalS) {
					continue
				}
				p.mu.Lock()
				busy := p.running[t.ID]
				if !busy {
					p.running[t.ID] = true
				}
				p.mu.Unlock()
				if busy {
					continue
				}
				go func(tt *Target) {
					defer func() {
						p.mu.Lock()
						delete(p.running, tt.ID)
						p.mu.Unlock()
					}()
					p.Run(tt)
				}(t)
			}
			p.store.TouchProbe(p.probeID)
		}
	}
}
