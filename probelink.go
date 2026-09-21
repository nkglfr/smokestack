package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"
)

// Sonde isolee dans son propre processus (probe.mode = "external").
//
//	smokestack          service web + ecrivain, SANS socket ICMP brut
//	smokestack probe    sonde seule, CAP_NET_RAW, prioritaire sur le CPU
//
// Les deux dialoguent par un socket Unix (0660, utilisateur du service) :
//
//	GET  /probe/v1/targets       cibles actives + version du service
//	POST /probe/v1/measurements  lot de mesures -> file de l'ecrivain
//
// La sonde ne partage ni memoire, ni ramasse-miettes, ni ordonnanceur avec
// l'interface web : une page lourde ne peut plus retarder une mesure. Si
// le service redemarre, la sonde conserve ses mesures en memoire et les
// renvoie ensuite. Apres une mise a jour, elle se relance d'elle-meme sur
// la nouvelle version.

const (
	probeBufferMax  = 200000 // ~ 1 h de mesures pour 1 000 cibles a 30 s
	probeFlushEvery = time.Second
	probeFlushMax   = 5000
)

type wireMeasure struct {
	Measurement
	Host string `json:"host"`
}

type probeTargetsResp struct {
	Version       string    `json:"version"`
	ProbeID       int64     `json:"probe_id"`
	Targets       []*Target `json:"targets"`
	TraceRequests []int64   `json:"trace_requests"`
}

func probeSocketPath(cfg Config) string {
	if cfg.Probe.Socket != "" {
		return cfg.Probe.Socket
	}
	return filepath.Join(cfg.DataDir, "probe.sock")
}

// ---------------------------------------------------------------- service

func ServeProbeSocket(path string, cache *TargetCache, writer *Writer, probeID int64, stop <-chan struct{}) error {
	os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return err
	}
	os.Chmod(path, 0o660)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /probe/v1/targets", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, probeTargetsResp{Version: Version, ProbeID: probeID, Targets: cache.Targets(),
			TraceRequests: traceRequests.Pop()})
	})
	mux.HandleFunc("POST /probe/v1/traceroutes", func(w http.ResponseWriter, r *http.Request) {
		var list []*Traceroute
		if err := json.NewDecoder(io.LimitReader(r.Body, 16<<20)).Decode(&list); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		for _, tr := range list {
			tr.ProbeID = probeID
			writer.SubmitTrace(tr)
		}
		writeJSON(w, map[string]int{"accepted": len(list)})
	})
	mux.HandleFunc("POST /probe/v1/measurements", func(w http.ResponseWriter, r *http.Request) {
		var batch []wireMeasure
		if err := json.NewDecoder(io.LimitReader(r.Body, 64<<20)).Decode(&batch); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		for _, b := range batch {
			b.Measurement.ProbeID = probeID
			writer.Submit(b.Measurement, b.Host)
		}
		writeJSON(w, map[string]int{"accepted": len(batch)})
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-stop
		srv.Close()
		os.Remove(path)
	}()
	log.Printf("waiting for the external probe on %s", path)
	go srv.Serve(l)
	return nil
}

// ------------------------------------------------------------------ sonde

type remoteLink struct {
	client *http.Client

	mu      sync.RWMutex
	targets []*Target
	version string
	probeID int64

	bmu     sync.Mutex
	buf     []wireMeasure
	dropped int64
	traces  []*Traceroute
	treqs   []int64
}

func newRemoteLink(sock string) *remoteLink {
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
		MaxIdleConns: 2,
	}
	return &remoteLink{client: &http.Client{Transport: tr, Timeout: 15 * time.Second}}
}

func (l *remoteLink) Targets() []*Target {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.targets
}

// Submit ne bloque jamais : la mesure part dans le tampon memoire.
func (l *remoteLink) Submit(m Measurement, host string) {
	l.bmu.Lock()
	defer l.bmu.Unlock()
	if len(l.buf) >= probeBufferMax {
		l.buf = l.buf[1:]
		l.dropped++
	}
	l.buf = append(l.buf, wireMeasure{Measurement: m, Host: host})
}

func (l *remoteLink) SubmitTrace(tr *Traceroute) {
	l.bmu.Lock()
	defer l.bmu.Unlock()
	if len(l.traces) < 1000 {
		l.traces = append(l.traces, tr)
	}
}

func (l *remoteLink) TraceRequests() []int64 {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.treqs
	l.treqs = nil
	return out
}

func (l *remoteLink) fetchTargets() error {
	resp, err := l.client.Get("http://probe/probe/v1/targets")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var tr probeTargetsResp
	if err := json.NewDecoder(resp.Body).Decode(&tr); err != nil {
		return err
	}
	l.mu.Lock()
	l.targets, l.version, l.probeID = tr.Targets, tr.Version, tr.ProbeID
	l.treqs = append(l.treqs, tr.TraceRequests...)
	l.mu.Unlock()
	return nil
}

func (l *remoteLink) flush() error {
	l.bmu.Lock()
	traces := l.traces
	l.traces = nil
	l.bmu.Unlock()
	if len(traces) > 0 {
		body, _ := json.Marshal(traces)
		resp, err := l.client.Post("http://probe/probe/v1/traceroutes", "application/json", bytes.NewReader(body))
		if err != nil || resp.StatusCode != 200 {
			l.bmu.Lock()
			l.traces = append(traces, l.traces...)
			l.bmu.Unlock()
		}
		if resp != nil {
			resp.Body.Close()
		}
	}
	l.bmu.Lock()
	n := len(l.buf)
	if n > probeFlushMax {
		n = probeFlushMax
	}
	batch := append([]wireMeasure(nil), l.buf[:n]...)
	l.bmu.Unlock()
	if len(batch) == 0 {
		return nil
	}
	body, _ := json.Marshal(batch)
	resp, err := l.client.Post("http://probe/probe/v1/measurements", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	l.bmu.Lock()
	l.buf = l.buf[len(batch):]
	l.bmu.Unlock()
	return nil
}

// currentBinary renvoie le binaire actif (<racine>/current/smokestack)
// quand la sonde tourne depuis une installation geree.
func currentBinary() string {
	exe, err := os.Executable()
	if err != nil {
		return ""
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	releases := filepath.Dir(filepath.Dir(exe))
	if filepath.Base(releases) == "releases" {
		cur := filepath.Join(filepath.Dir(releases), "current", "smokestack")
		if real, err := filepath.EvalSymlinks(cur); err == nil && real != exe {
			return cur
		}
	}
	return ""
}

func runProbe(args []string) error {
	fsn := flag.NewFlagSet("probe", flag.ExitOnError)
	cfgPath := cliConfig(fsn)
	fsn.Parse(args)
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	sock := probeSocketPath(cfg)
	link := newRemoteLink(sock)
	log.Printf("smokestack probe %s, service on %s", Version, sock)

	for i := 0; ; i++ {
		if err := link.fetchTargets(); err == nil {
			break
		} else if i%10 == 0 {
			log.Printf("waiting for the service: %v", err)
		}
		time.Sleep(time.Second)
	}
	prober, err := NewProber(link, link, link.probeID, cfg.Probe.Traceroute)
	if err != nil {
		return fmt.Errorf("ICMP socket: %w (the probe needs CAP_NET_RAW)", err)
	}
	defer prober.Close()

	stop := make(chan struct{})
	go prober.Schedule(stop)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	refresh := time.NewTicker(5 * time.Second)
	flush := time.NewTicker(probeFlushEvery)
	defer refresh.Stop()
	defer flush.Stop()
	var lastWarn time.Time
	for {
		select {
		case <-sig:
			close(stop)
			// Laisse les passes en cours se terminer, puis vide le tampon.
			deadline := time.Now().Add(8 * time.Second)
			for time.Now().Before(deadline) {
				if link.flush() != nil {
					break
				}
				link.bmu.Lock()
				left := len(link.buf)
				link.bmu.Unlock()
				if left == 0 {
					break
				}
			}
			log.Printf("probe stopped")
			return nil
		case <-refresh.C:
			if err := link.fetchTargets(); err != nil {
				continue
			}
			link.mu.RLock()
			v := link.version
			link.mu.RUnlock()
			// Le service a change de version (mise a jour ou retour
			// arriere) : on se relance sur le binaire actif.
			if v != "" && v != Version {
				if bin := currentBinary(); bin != "" {
					link.flush()
					log.Printf("service now runs version %s, restarting the probe", v)
					prober.Close()
					if err := syscall.Exec(bin, os.Args, os.Environ()); err != nil {
						log.Printf("restart failed: %v", err)
					}
				}
			}
		case <-flush.C:
			if err := link.flush(); err != nil && time.Since(lastWarn) > time.Minute {
				link.bmu.Lock()
				n, d := len(link.buf), link.dropped
				link.bmu.Unlock()
				log.Printf("service unreachable (%v): %d measurements pending, %d dropped", err, n, d)
				lastWarn = time.Now()
			}
		}
	}
}
