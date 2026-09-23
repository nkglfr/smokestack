package main

import (
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// La reduction de points fusionne les histogrammes : le p95 d'un groupe
// est celui de toutes ses mesures, pas la moyenne des p95.
func TestDownsampleExact(t *testing.T) {
	var rows []seriesRow
	all := NewSketch()
	for i := 0; i < 1000; i++ {
		sk := NewSketch()
		for k := 0; k < 20; k++ {
			v := float64(1000 + (i*37+k*101)%5000)
			sk.Add(v)
			all.Add(v)
		}
		rows = append(rows, seriesRow{bucket: int64(i * 60), sent: 20, cnt: 20, sk: sk})
	}
	out, factor := downsample(rows, 10)
	if len(out) != 10 || factor != 100 {
		t.Fatalf("attendu 10 points (facteur 100), obtenu %d (facteur %d)", len(out), factor)
	}
	merged := NewSketch()
	for _, r := range out {
		merged.Merge(r.sk)
	}
	if a, b := merged.Quantile(.95), all.Quantile(.95); math.Abs(a-b)/b > 1e-9 {
		t.Errorf("p95 apres reduction %f, attendu %f", a, b)
	}
	if out[0].sent != 2000 {
		t.Errorf("paquets envoyes mal cumules : %d", out[0].sent)
	}
}

// La sonde ne doit jamais attendre la base, meme si la file est pleine.
func TestWriterSubmitNeverBlocks(t *testing.T) {
	w := &Writer{ch: make(chan queuedMeasure, 2)}
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			w.Submit(Measurement{TargetID: 1}, "")
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("Submit a bloque")
	}
	if w.dropped.Load() != 998 {
		t.Errorf("mesures ecartees : %d, attendu 998", w.dropped.Load())
	}
}

func TestRateLimiter(t *testing.T) {
	rl := &rateLimiter{buckets: map[string]*bucket{}, rate: 1, burst: 5}
	ok := 0
	for i := 0; i < 20; i++ {
		if rl.allow("198.51.100.7") {
			ok++
		}
	}
	if ok != 5 {
		t.Errorf("rafale autorisee : %d, attendu 5", ok)
	}
	if !rl.allow("203.0.113.9") {
		t.Error("une autre IP ne doit pas etre penalisee")
	}
}

func TestGzipMiddleware(t *testing.T) {
	h := withGzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(strings.Repeat(`{"a":1}`, 500)))
	}))
	req := httptest.NewRequest("GET", "/api/v1/tree", nil)
	req.Header.Set("Accept-Encoding", "gzip")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Header().Get("Content-Encoding") != "gzip" || rec.Body.Len() > 500 {
		t.Errorf("reponse non compressee (%d octets)", rec.Body.Len())
	}
	// Une reponse deja compressee ne l'est pas deux fois.
	h2 := withGzip(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.Write([]byte("deja"))
	}))
	rec2 := httptest.NewRecorder()
	h2.ServeHTTP(rec2, req)
	if rec2.Body.String() != "deja" {
		t.Error("double compression")
	}
}

// Each pass starts with a small random delay, so that targets landing on
// the same second do not all fire at once.
func TestPassJitter(t *testing.T) {
	seen := map[int64]bool{}
	for i := 0; i < 400; i++ {
		d := passJitter(60)
		if d < 0 || d > 950*time.Millisecond {
			t.Fatalf("jitter out of range: %v", d)
		}
		seen[d.Milliseconds()/100] = true
	}
	if len(seen) < 8 {
		t.Errorf("jitter poorly spread: %d buckets out of 10", len(seen))
	}
	if d := passJitter(5); d > 500*time.Millisecond { // a tenth of the interval
		t.Errorf("short interval: %v", d)
	}
}
