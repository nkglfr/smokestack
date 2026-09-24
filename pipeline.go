package main

import (
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// Chaine de traitement entre la sonde et la base.
//
//	sonde --Submit (non bloquant)--> file --> Writer --lot, 1 transaction--> metrics.db
//	                                                 \--> archive brute
//
// La sonde ne peut jamais attendre : si la file est pleine (base bloquee
// plusieurs minutes), la mesure est comptee comme perdue et signalee,
// mais la sonde continue a mesurer a l'heure.

const (
	writerQueueSize = 32768
	writerBatchMax  = 1000
	writerFlushWait = 250 * time.Millisecond
)

type queuedMeasure struct {
	m    Measurement
	host string
}

type Writer struct {
	store   *Store
	archive *Archive
	ch      chan queuedMeasure
	traces  chan *Traceroute
	done    chan struct{}

	written atomic.Int64
	dropped atomic.Int64
	lastLag atomic.Int64 // duree de la derniere ecriture de lot, en microsecondes
}

func NewWriter(store *Store, archive *Archive) *Writer {
	return &Writer{store: store, archive: archive,
		ch: make(chan queuedMeasure, writerQueueSize), traces: make(chan *Traceroute, 256),
		done: make(chan struct{})}
}

// Submit ne bloque jamais.
func (w *Writer) Submit(m Measurement, host string) {
	select {
	case w.ch <- queuedMeasure{m, host}:
	default:
		if w.dropped.Add(1)%1000 == 1 {
			log.Printf("write queue full: measurements dropped (%d in total)", w.dropped.Load())
		}
	}
}

// SubmitTrace never blocks either.
func (w *Writer) SubmitTrace(tr *Traceroute) {
	select {
	case w.traces <- tr:
	default:
	}
}

func (w *Writer) saveTrace(tr *Traceroute) {
	if err := w.store.SaveTraceroute(tr); err != nil {
		log.Printf("saving traceroute: %v", err)
	}
}

func (w *Writer) Stats() map[string]any {
	return map[string]any{
		"queued": len(w.ch), "capacity": cap(w.ch),
		"written": w.written.Load(), "dropped": w.dropped.Load(),
		"last_batch_ms": float64(w.lastLag.Load()) / 1000,
	}
}

// Loop ecrit par lots jusqu'a l'arret, puis vide la file.
func (w *Writer) Loop(stop <-chan struct{}) {
	defer close(w.done)
	batch := make([]queuedMeasure, 0, writerBatchMax)
	timer := time.NewTimer(writerFlushWait)
	defer timer.Stop()
	flush := func() {
		if len(batch) == 0 {
			return
		}
		start := time.Now()
		if err := w.store.RecordBatch(batch); err != nil {
			log.Printf("writing measurements: %v", err)
		} else {
			w.written.Add(int64(len(batch)))
		}
		if w.archive != nil {
			for _, q := range batch {
				w.archive.Append(q.m, q.host)
			}
		}
		w.lastLag.Store(time.Since(start).Microseconds())
		batch = batch[:0]
	}
	for {
		select {
		case q := <-w.ch:
			batch = append(batch, q)
			if len(batch) >= writerBatchMax {
				flush()
			}
		case tr := <-w.traces:
			w.saveTrace(tr)
		case <-timer.C:
			flush()
			timer.Reset(writerFlushWait)
		case <-stop:
			for {
				select {
				case q := <-w.ch:
					batch = append(batch, q)
					if len(batch) >= writerBatchMax {
						flush()
					}
				default:
					flush()
					return
				}
			}
		}
	}
}

// Wait attend la fin du vidage apres l'arret.
func (w *Writer) Wait(d time.Duration) {
	select {
	case <-w.done:
	case <-time.After(d):
	}
}

// RecordBatch enregistre un lot de mesures en une seule transaction sur
// la connexion d'ecriture.
func (s *Store) RecordBatch(batch []queuedMeasure) error {
	tx, err := s.mxw.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	ins, err := tx.Prepare(`INSERT INTO samples(target_id,probe_id,bucket,sent,lost,cnt,
	                        min_us,max_us,sum_us,sumsq_us,sketch)
	                        VALUES(?,?,?,?,?,?,?,?,?,?,?)
	                        ON CONFLICT(target_id,probe_id,bucket) DO NOTHING`)
	if err != nil {
		return err
	}
	defer ins.Close()
	live, err := tx.Prepare(`INSERT INTO live(target_id,probe_id,ts,med_us,p95_us,loss_pct)
	                         VALUES(?,?,?,?,?,?)
	                         ON CONFLICT(target_id,probe_id) DO UPDATE SET
	                           ts=excluded.ts, med_us=excluded.med_us,
	                           p95_us=excluded.p95_us, loss_pct=excluded.loss_pct`)
	if err != nil {
		return err
	}
	defer live.Close()
	// The reason a pass failed is kept per target, so that the back-office
	// can show why instead of leaving the operator with 100 % loss.
	setErr, err := tx.Prepare(`INSERT INTO target_errors(target_id,ts,err) VALUES(?,?,?)
	                           ON CONFLICT(target_id) DO UPDATE SET ts=excluded.ts, err=excluded.err`)
	if err != nil {
		return err
	}
	defer setErr.Close()
	clearErr, err := tx.Prepare(`DELETE FROM target_errors WHERE target_id=?`)
	if err != nil {
		return err
	}
	defer clearErr.Close()
	seenIP, err := tx.Prepare(`INSERT INTO target_addresses(target_id,ip,last_seen) VALUES(?,?,?)
	                           ON CONFLICT(target_id,ip) DO UPDATE SET last_seen=excluded.last_seen`)
	if err != nil {
		return err
	}
	defer seenIP.Close()
	probes := map[int64]bool{}
	for _, q := range batch {
		m := q.m
		if m.IP != "" {
			seenIP.Exec(m.TargetID, m.IP, m.TS)
		}
		if m.Err != "" {
			setErr.Exec(m.TargetID, m.TS, m.Err)
		} else if len(m.RTTus) > 0 {
			clearErr.Exec(m.TargetID)
		}
		sk := NewSketch()
		var sum, sumsq float64
		for _, v := range m.RTTus {
			sk.Add(v)
			sum += v
			sumsq += v * v
		}
		if _, err := ins.Exec(m.TargetID, m.ProbeID, m.TS, m.Sent, m.Lost, len(m.RTTus),
			sk.Min(), sk.Max(), sum, sumsq, sk.MarshalBinary()); err != nil {
			return err
		}
		loss := 0.0
		if m.Sent > 0 {
			loss = float64(m.Lost) * 100 / float64(m.Sent)
		}
		if _, err := live.Exec(m.TargetID, m.ProbeID, m.TS, sk.Quantile(0.5), sk.Quantile(0.95), loss); err != nil {
			return err
		}
		probes[m.ProbeID] = true
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	for id := range probes {
		s.TouchProbe(id)
	}
	return nil
}

// ---------------------------------------------------------- cache des cibles

// TargetCache sert a la sonde la liste des cibles actives depuis la
// memoire. Elle est rafraichie en tache de fond toutes les 10 s, et tout
// de suite apres une creation ou une suppression de cible.
type TargetCache struct {
	store *Store
	mu    sync.RWMutex
	list  []*Target
}

func NewTargetCache(store *Store) *TargetCache {
	c := &TargetCache{store: store}
	c.refresh()
	return c
}

func (c *TargetCache) refresh() {
	list, err := c.store.ActiveTargets()
	if err != nil {
		log.Printf("target cache: %v", err)
		return
	}
	c.mu.Lock()
	c.list = list
	c.mu.Unlock()
}

// TraceRequests hands traceroutes requested from the back-office to the
// embedded probe.
func (c *TargetCache) TraceRequests() []int64 { return traceRequests.Pop() }

// CheckRequests hands over targets to measure right away: a target just
// created should not wait a whole interval for its first result.
func (c *TargetCache) CheckRequests() []int64 { return checkRequests.Pop() }

func (c *TargetCache) Targets() []*Target {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.list
}

func (c *TargetCache) Loop(stop <-chan struct{}) {
	t := time.NewTicker(10 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			c.refresh()
		case <-c.store.targetsChanged:
			c.refresh()
		}
	}
}
