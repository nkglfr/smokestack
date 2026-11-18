package main

import (
	"compress/gzip"
	"container/list"
	"fmt"
	"io"
	"net"
	"net/http"
	"regexp"
	"runtime"
	"strings"
	"sync"
	"time"
)

// Couche HTTP : ce qui garde l'interface publique et le back-office
// rapides meme quand beaucoup de visiteurs arrivent en meme temps.

// -------------------------------------------------------------- gzip

func acceptsGzip(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept-Encoding"), "gzip")
}

var gzPool = sync.Pool{New: func() any {
	w, _ := gzip.NewWriterLevel(io.Discard, gzip.BestSpeed)
	return w
}}

type gzipWriter struct {
	http.ResponseWriter
	gz      *gzip.Writer
	decided bool
	on      bool
}

func compressible(ct string) bool {
	for _, p := range []string{"application/json", "text/", "application/javascript", "image/svg+xml",
		"application/atom+xml"} {
		if strings.HasPrefix(ct, p) {
			return true
		}
	}
	return false
}

func (g *gzipWriter) decide() {
	if g.decided {
		return
	}
	g.decided = true
	h := g.Header()
	if h.Get("Content-Encoding") != "" || !compressible(h.Get("Content-Type")) {
		return
	}
	g.on = true
	h.Set("Content-Encoding", "gzip")
	h.Add("Vary", "Accept-Encoding")
	h.Del("Content-Length")
	g.gz = gzPool.Get().(*gzip.Writer)
	g.gz.Reset(g.ResponseWriter)
}

func (g *gzipWriter) WriteHeader(code int) {
	if code != http.StatusNotModified && code != http.StatusNoContent {
		g.decide()
	} else {
		g.decided = true
	}
	g.ResponseWriter.WriteHeader(code)
}

func (g *gzipWriter) Write(b []byte) (int, error) {
	if !g.decided {
		if g.Header().Get("Content-Type") == "" {
			g.Header().Set("Content-Type", http.DetectContentType(b))
		}
		g.decide()
	}
	if g.on {
		return g.gz.Write(b)
	}
	return g.ResponseWriter.Write(b)
}

func (g *gzipWriter) close() {
	if g.on {
		g.gz.Close()
		gzPool.Put(g.gz)
	}
}

// Flush garde le flux temps reel (SSE) utilisable derriere la compression.
func (g *gzipWriter) Flush() {
	if g.on {
		g.gz.Flush()
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func withGzip(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Pas de compression pour le flux SSE ni pour les televersements.
		if !acceptsGzip(r) || r.URL.Path == "/api/v1/live" || r.Method == http.MethodPost {
			next.ServeHTTP(w, r)
			return
		}
		g := &gzipWriter{ResponseWriter: w}
		defer g.close()
		next.ServeHTTP(g, r)
	})
}

// ------------------------------------------------------- cache navigateur

// assetETag change a chaque version : les navigateurs gardent CSS, JS et
// pages en cache et ne les retelechargent qu'apres une mise a jour.
func assetETag() string {
	if v := assetVersion(); v != "" {
		return `W/"` + v + `"`
	}
	return ""
}

// assetVersion is what the pages append to the stylesheet and the scripts
// they load. An ETag alone leaves a window: the page HTML is served
// no-cache and is therefore always current, while the assets behind it
// were cached for up to five minutes, so a browser could pair the new
// markup with the previous stylesheet for that long after an update.
//
// Putting the version in the address closes it. A new build is a new
// address, which no cache can confuse with the old one, and the answer
// can then be kept for a year instead of five minutes.
func assetVersion() string {
	if Version == "dev" {
		return ""
	}
	return Version + "-" + BuildDate
}

// localAsset matches a page's reference to a stylesheet or a script it
// serves itself. Spelling them out was fine for four files; the
// back-office alone loads twenty, and a file left off the list is a file
// still cached under a bare address.
var localAsset = regexp.MustCompile(`(src|href)="(/[A-Za-z0-9/._-]+\.(?:js|css))"`)

// versionedAssets rewrites the references of a page on the way out, so the
// files on disk stay plain and openable.
func versionedAssets(b []byte) []byte {
	v := assetVersion()
	if v == "" {
		return b
	}
	return localAsset.ReplaceAll(b, []byte(`$1="$2?v=`+v+`"`))
}

func withAssetCache(next http.Handler, maxAge int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if et := assetETag(); et != "" {
			w.Header().Set("ETag", et)
			switch {
			case r.URL.Query().Get("v") == assetVersion():
				// The address names the build, so this answer cannot go
				// stale: the next build asks for a different address.
				w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
			case maxAge > 0:
				w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", maxAge))
			default:
				w.Header().Set("Cache-Control", "no-cache")
			}
			if r.Header.Get("If-None-Match") == et {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}
		next.ServeHTTP(w, r)
	})
}

// ------------------------------------------------------- limite de debit

// Seau a jetons par adresse IP sur l'API : 20 requetes/s en regime
// etabli, rafales de 80. Un robot ne peut plus monopoliser le serveur.
// Les connexions locales sans en-tete de proxy (supervision locale,
// outils d'administration) ne sont pas limitees.
type bucket struct {
	tokens float64
	last   time.Time
}

type rateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
	rate    float64
	burst   float64
}

func newRateLimiter(rate, burst float64) *rateLimiter {
	rl := &rateLimiter{buckets: map[string]*bucket{}, rate: rate, burst: burst}
	go func() {
		for range time.Tick(time.Minute) {
			rl.mu.Lock()
			for k, b := range rl.buckets {
				if time.Since(b.last) > 5*time.Minute {
					delete(rl.buckets, k)
				}
			}
			rl.mu.Unlock()
		}
	}()
	return rl
}

func (rl *rateLimiter) allow(key string) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := time.Now()
	b := rl.buckets[key]
	if b == nil {
		b = &bucket{tokens: rl.burst, last: now}
		rl.buckets[key] = b
	}
	b.tokens += now.Sub(b.last).Seconds() * rl.rate
	if b.tokens > rl.burst {
		b.tokens = rl.burst
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

func isLocalDirect(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback() && r.Header.Get("X-Forwarded-For") == ""
}

func withRateLimit(rl *rateLimiter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		limited := strings.HasPrefix(r.URL.Path, "/api/") || r.URL.Path == "/metrics"
		if limited && !isLocalDirect(r) && !rl.allow(clientIP(r)) {
			w.Header().Set("Retry-After", "1")
			writeErr(w, http.StatusTooManyRequests, "too many requests, slow down")
			return
		}
		next.ServeHTTP(w, r)
	})
}

// ------------------------------------------------------ series : cache

type seriesEntry struct {
	key  string
	data *Series
	exp  time.Time
}

type seriesLRU struct {
	mu    sync.Mutex
	max   int
	ll    *list.List
	items map[string]*list.Element
}

func newSeriesLRU(max int) *seriesLRU {
	return &seriesLRU{max: max, ll: list.New(), items: map[string]*list.Element{}}
}

func (c *seriesLRU) get(k string) *Series {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[k]; ok {
		en := e.Value.(*seriesEntry)
		if time.Now().Before(en.exp) {
			c.ll.MoveToFront(e)
			return en.data
		}
		c.ll.Remove(e)
		delete(c.items, k)
	}
	return nil
}

func (c *seriesLRU) put(k string, s *Series, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[k]; ok {
		c.ll.Remove(e)
	}
	c.items[k] = c.ll.PushFront(&seriesEntry{key: k, data: s, exp: time.Now().Add(ttl)})
	for c.ll.Len() > c.max {
		last := c.ll.Back()
		c.ll.Remove(last)
		delete(c.items, last.Value.(*seriesEntry).key)
	}
}

var (
	seriesCache = newSeriesLRU(512)
	// Nombre de lectures lourdes simultanees : au-dela, les requetes
	// attendent leur tour au lieu d'ecrouler le serveur.
	seriesSlots = make(chan struct{}, max(2, runtime.NumCPU()))
)

// Downsample regroupe les points consecutifs par paquets pour ne pas
// depasser maxPoints. Les percentiles sont recalcules a partir des
// histogrammes fusionnes : ils restent exacts, contrairement a une
// moyenne de percentiles.
func downsample(rows []seriesRow, maxPoints int) ([]seriesRow, int) {
	if maxPoints <= 0 || len(rows) <= maxPoints {
		return rows, 1
	}
	factor := (len(rows) + maxPoints - 1) / maxPoints
	out := make([]seriesRow, 0, maxPoints)
	for i := 0; i < len(rows); i += factor {
		g := seriesRow{bucket: rows[i].bucket, sk: NewSketch()}
		for j := i; j < i+factor && j < len(rows); j++ {
			g.sent += rows[j].sent
			g.lost += rows[j].lost
			g.cnt += rows[j].cnt
			g.sk.Merge(rows[j].sk)
		}
		out = append(out, g)
	}
	return out, factor
}
