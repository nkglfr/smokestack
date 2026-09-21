package main

import (
	"encoding/binary"
	"hash/fnv"
	"log"
	"net"
	"sync"
	"time"
)

// La sonde ouvre un seul socket ICMP brut pour toutes les cibles et fait
// correspondre les reponses par numero de sequence.
//
// Isolation : la sonde ne connait ni la base ni l'interface web. Elle lit
// ses cibles dans une TargetSource (un cache en memoire) et depose ses
// mesures dans un MeasureSink (une file non bloquante). Aucune de ces deux
// operations ne peut attendre la base de donnees. Le meme code tourne dans
// le processus principal ou dans un processus dedie (`smokestack probe`).
//
// Exactitude : l'heure de reception est donnee par le noyau
// (SO_TIMESTAMPNS), l'heure d'emission est ecrite dans le paquet juste
// avant l'envoi. La charge du processus ne s'ajoute donc pas au temps de
// reponse mesure.

const icmpPayloadSize = 56

type TargetSource interface {
	Targets() []*Target
}

type MeasureSink interface {
	Submit(m Measurement, host string)
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
	conn     net.PacketConn
	id       uint16
	kernelTS bool

	mu      sync.Mutex
	seq     uint16
	pending map[uint16]*inflight
	running map[int64]bool
}

func NewProber(src TargetSource, sink MeasureSink, probeID int64) (*Prober, error) {
	conn, err := net.ListenPacket("ip4:icmp", "0.0.0.0")
	if err != nil {
		return nil, err
	}
	p := &Prober{
		src: src, sink: sink, probeID: probeID, conn: conn,
		id:      uint16(time.Now().UnixNano() & 0xffff),
		pending: map[uint16]*inflight{},
		running: map[int64]bool{},
	}
	p.kernelTS = enableKernelRxTimestamps(conn)
	if p.kernelTS {
		log.Printf("sonde : horodatage des reponses par le noyau actif")
	} else {
		log.Printf("sonde : horodatage noyau indisponible, horodatage applicatif")
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

// echoTemplate prepare le paquet sans horodatage ; stampAndSend y ecrit
// l'heure d'emission et la somme de controle juste avant l'envoi.
func (p *Prober) echoTemplate(seq uint16) []byte {
	b := make([]byte, 8+icmpPayloadSize)
	b[0] = 8 // echo request
	binary.BigEndian.PutUint16(b[4:6], p.id)
	binary.BigEndian.PutUint16(b[6:8], seq)
	for i := 16; i < len(b); i++ {
		b[i] = byte(i)
	}
	return b
}

func (p *Prober) stampAndSend(b []byte, addr net.Addr) error {
	b[2], b[3] = 0, 0
	binary.BigEndian.PutUint64(b[8:16], uint64(time.Now().UnixNano()))
	binary.BigEndian.PutUint16(b[2:4], checksum(b))
	_, err := p.conn.WriteTo(b, addr)
	return err
}

func (p *Prober) readLoop() {
	buf := make([]byte, 1500)
	oob := make([]byte, 128)
	for {
		n, now, err := readStamped(p.conn, buf, oob)
		if err != nil {
			return
		}
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

	p.sink.Submit(m, t.Host)
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
		if err := p.stampAndSend(p.echoTemplate(seq), addr); err != nil {
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
			// Le temps SYN -> SYN/ACK mesure par la pile TCP ne depend pas
			// de la charge du processus ; a defaut, mesure applicative.
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
			targets := p.src.Targets()
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
		}
	}
}
