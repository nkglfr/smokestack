package main

import (
	"context"
	"crypto/rand"
	"embed"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"syscall"
	"time"
)

//go:embed web
var webFS embed.FS

type ProbeConfig struct {
	Enabled bool `json:"enabled"`
	// Mode "embedded" : la sonde tourne dans le service (petites
	// installations). Mode "external" : elle tourne dans son propre
	// processus (`smokestack probe`), isolee de l'interface web.
	Mode   string `json:"mode"`
	Socket string `json:"socket"`
	// Traceroute on anomalies (on by default).
	Traceroute TracerouteConfig `json:"traceroute"`
	Slug       string           `json:"slug"`
	Name       string           `json:"name"`
	Location   string           `json:"location"`
}

type FedConfig struct {
	Enabled bool     `json:"enabled"`
	ASN     string   `json:"asn"`
	Org     string   `json:"org"`
	BaseURL string   `json:"base_url"`
	Anchors []string `json:"anchors"`
}

type Config struct {
	PeeringDBKey string        `json:"peeringdb_api_key"`
	Listen       string        `json:"listen,omitempty"` // older combined form, see listen.go
	ListenIP     string        `json:"listen_ip,omitempty"`
	ListenPort   int           `json:"listen_port,omitempty"`
	DataDir      string        `json:"data_dir"`
	AdminToken   string        `json:"admin_token"`
	Probe        ProbeConfig   `json:"probe"`
	Storage      StorageConfig `json:"storage"`
	Federation   FedConfig     `json:"federation"`
	Update       UpdateConfig  `json:"update"`
}

func defaultConfig() Config {
	return Config{

		DataDir: "/var/lib/smokestack",
		Probe: ProbeConfig{
			Enabled: true, Slug: "local-01",
			Name: "Sonde locale", Location: "on site",
		},
		Federation: FedConfig{Enabled: false, Anchors: []string{}},
		Update:     defaultUpdateConfig(),
		Storage: StorageConfig{
			Mode: "local",
			Local: LocalConfig{
				QuotaBytes:     5 << 30,
				HighWatermark:  0.85,
				LowWatermark:   0.70,
				KeepLocalHours: 48,
			},
		},
	}
}

func loadConfig(path string) (Config, error) {
	c := defaultConfig()
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		c.AdminToken = randomToken()
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			return c, err
		}
		out, _ := json.MarshalIndent(c, "", "  ")
		if err := os.WriteFile(path, out, 0o640); err != nil {
			return c, err
		}
		log.Printf("configuration written to %s", path)
		return c, nil
	}
	if err != nil {
		return c, err
	}
	err = json.Unmarshal(b, &c)
	return c, err
}

func randomToken() string {
	b := make([]byte, 24)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func seed(store *Store) error {
	var n int
	if err := store.cfg.QueryRow(`SELECT COUNT(*) FROM categories`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	catID, err := store.CreateCategory("dns-publics", "DNS publics", "Public DNS", true)
	if err != nil {
		return err
	}
	demo := []struct{ title, host string }{
		{"Cloudflare", "1.1.1.1"},
		{"Quad9", "9.9.9.9"},
		{"Google", "8.8.8.8"},
	}
	for _, d := range demo {
		t := &Target{
			CategoryID: catID, Slug: d.host, Title: d.title, Host: d.host,
			Proto: "icmp", IntervalS: 60, Packets: 20, SpacingMs: 500,
			TimeoutMs: 2000, Public: true, Enabled: true,
		}
		if _, err := store.CreateTarget(t); err != nil {
			return err
		}
	}
	log.Printf("demo set created: %d targets", len(demo))
	return nil
}

func main() {
	// Keep the last log lines in memory as well, so the back-office can
	// show them: the operator who needs them does not always have a shell.
	captureLog(os.Stderr)
	if runCLI(os.Args[1:]) {
		return
	}
	args := os.Args[1:]
	if len(args) > 0 && args[0] == "serve" {
		args = args[1:]
	}
	fl := flag.NewFlagSet("smokestack", flag.ExitOnError)
	cfgPath := fl.String("config", "/etc/smokestack/config.json", "configuration file")
	listen := fl.String("listen", "", "listen address, ip:port")
	flagIP := fl.String("ip", "", "listen IP (* for all interfaces)")
	flagPort := fl.String("port", "", "listen port")
	fl.Usage = func() { fmt.Fprintf(os.Stderr, usage, Version) }
	fl.Parse(args)
	log.Printf("smokestack %s (%s-%s)", Version, runtime.GOOS, runtime.GOARCH)

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		log.Fatalf("configuration: %v", err)
	}
	addr, err := resolveListen(cfg, listenSource{flagListen: *listen, flagIP: *flagIP,
		flagPort: *flagPort, getenv: os.Getenv})
	if err != nil {
		log.Fatalf("listen address: %v", err)
	}
	cfg.Listen = addr
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		log.Fatalf("data directory: %v", err)
	}

	// Avant tout : si la version precedente a ete installee et echoue a
	// demarrer en boucle, on revient a l'ancienne.
	if early := NewUpdater(cfg.Update, cfg.DataDir, nil); early.CheckPending() {
		execBinary(early.CurrentBinary())
	}

	store, err := OpenStore(cfg.DataDir)
	if err != nil {
		log.Fatalf("base: %v", err)
	}
	defer store.Close()
	if err := seed(store); err != nil {
		log.Fatalf("amorcage: %v", err)
	}

	// La configuration de stockage stockee en base fait foi : c'est
	// elle que le back-office modifie a chaud.
	storage := cfg.Storage
	if raw := store.Setting("storage", ""); raw != "" {
		var s StorageConfig
		if err := json.Unmarshal([]byte(raw), &s); err == nil {
			storage = s
		}
	} else {
		b, _ := json.Marshal(storage)
		store.SetSetting("storage", string(b))
	}

	probeID, err := store.ProbeID(cfg.Probe.Slug, cfg.Probe.Name,
		cfg.Probe.Location, cfg.Probe.Enabled)
	if err != nil {
		log.Fatalf("probe: %v", err)
	}

	archive, err := NewArchive(store, cfg.DataDir, cfg.Probe.Slug, storage)
	if err != nil {
		log.Fatalf("archive: %v", err)
	}

	stop := make(chan struct{})
	go archive.Loop(stop)
	go backgroundLoop(store, stop)

	// La sonde ne parle jamais a la base : elle lit ses cibles dans un
	// cache memoire et depose ses mesures dans la file de l'ecrivain.
	writer := NewWriter(store, archive)
	go writer.Loop(stop)
	targets := NewTargetCache(store)
	go targets.Loop(stop)

	if cfg.Probe.Enabled {
		if cfg.Probe.Mode == "external" {
			if err := ServeProbeSocket(probeSocketPath(cfg), targets, writer, probeID, stop); err != nil {
				log.Fatalf("probe socket: %v", err)
			}
		} else {
			prober, err := NewProber(targets, writer, probeID, cfg.Probe.Traceroute)
			if err != nil {
				log.Printf("ICMP probe unavailable (%v), the service keeps running "+
					"anyway: check CAP_NET_RAW", err)
			} else {
				defer prober.Close()
				go prober.Schedule(stop)
				log.Printf("probe %s running (embedded in the service)", cfg.Probe.Slug)
			}
		}
	}

	fed, err := NewFederation(store, cfg.DataDir, cfg.Federation)
	if err != nil {
		log.Fatalf("federation: %v", err)
	}
	go fed.Loop(stop)

	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		log.Fatalf("assets: %v", err)
	}
	// L'anglais est obligatoire : sans lui, on refuse de demarrer plutot
	// que de servir une interface a moitie traduite.
	i18n, err := NewI18n(sub, cfg.DataDir)
	if err != nil {
		log.Fatalf("i18n: %v", err)
	}

	asnSvc := NewASNService(store, fed, cfg.PeeringDBKey)
	go asnSvc.Loop(stop)

	upd := NewUpdater(cfg.Update, cfg.DataDir, store)
	upd.RecordAutoRollback()
	go upd.Loop(stop)
	go upd.ConfirmAfter(60*time.Second, stop)
	if !upd.Managed() {
		log.Printf("in-place updates unavailable: %s", upd.reason)
	}

	api := &API{store: store, archive: archive, fed: fed,
		token: cfg.AdminToken, probeID: probeID, limiter: newAttemptLimiter(),
		i18n: i18n, asn: asnSvc, upd: upd, writer: writer, probeMode: cfg.Probe.Mode}
	if !cfg.Probe.Enabled {
		api.probeMode = "disabled"
	} else if api.probeMode == "" {
		api.probeMode = "embedded"
	}
	api.initSetupCode(cfg.DataDir)
	mux := http.NewServeMux()
	api.Routes(mux)
	api.AuthRoutes(mux)
	api.FedRoutes(mux)
	api.PairingRoutes(mux)
	api.I18nRoutes(mux)
	api.ASNRoutes(mux)
	api.UpdateRoutes(mux)
	api.OverviewRoutes(mux)
	api.TracerouteRoutes(mux)
	api.SuggestedRoutes(mux)
	api.probeInProcess = cfg.Probe.Enabled && cfg.Probe.Mode != "external"
	api.ContactRoutes(mux)
	api.LogRoutes(mux)
	api.AdviceRoutes(mux)
	api.ShareRoutes(mux)

	// Public pages get their metadata and a no-JavaScript summary injected
	// on the way out, so that a crawler sees a real page.
	page := func(name string, meta func(*http.Request) (pageMeta, bool)) http.HandlerFunc {
		h := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, err := fs.ReadFile(sub, name)
			if err != nil {
				http.NotFound(w, r)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.Header().Set("X-Frame-Options", "DENY")
			w.Header().Set("Referrer-Policy", "same-origin")
			if meta != nil {
				m, ok := meta(r)
				if !ok {
					http.NotFound(w, r)
					return
				}
				b = inject(b, api.seoHead(r, m), m.Body)
			}
			w.Write(b)
		})
		if meta != nil {
			// An injected page carries the site settings and the current
			// state of the targets: it must not share the version-wide
			// ETag, which would let a cache answer "not modified" with a
			// stale description and summary.
			return func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Cache-Control", "no-cache")
				h.ServeHTTP(w, r)
			}
		}
		return withAssetCache(h, 0).ServeHTTP
	}
	fixed := func(m pageMeta) func(*http.Request) (pageMeta, bool) {
		return func(*http.Request) (pageMeta, bool) { return m, true }
	}
	// Chemins lisibles pour les deux pages qui ont une adresse a
	// communiquer : le back-office et la page d'appairage.
	noIndex := fixed(pageMeta{Path: "/admin", Title: "Back-office", NoIndex: true})
	mux.HandleFunc("GET /admin", page("admin.html", noIndex))
	mux.HandleFunc("GET /admin/", page("admin.html", noIndex))
	pairing := fixed(pageMeta{Path: "/pairing", Title: "Federation pairing",
		Description: "Identity, key fingerprint and anchors of this instance, and how to pair with it."})
	mux.HandleFunc("GET /pairing", page("pairing.html", pairing))
	mux.HandleFunc("GET /pairing/", page("pairing.html", pairing))
	mux.HandleFunc("GET /federation", page("federation.html", fixed(pageMeta{Path: "/federation",
		Title: "Federation",
		Description: "Networks paired with this instance, latency measured in both directions, " +
			"and the inter-AS matrix."})))
	mux.HandleFunc("GET /network", page("network.html", fixed(pageMeta{Path: "/network",
		Title: "Host network",
		Description: "The autonomous system these measurements are taken from, as seen in global " +
			"routing and as declared by its operator."})))
	mux.HandleFunc("GET /about", page("about.html", fixed(pageMeta{Path: "/about",
		Title:       "About and contact",
		Description: "Operator, contacts and measurement method of this latency monitoring instance."})))
	mux.HandleFunc("GET /{$}", page("index.html", func(r *http.Request) (pageMeta, bool) {
		return pageMeta{Path: "/", Body: api.homeBody(r), Schema: api.orgSchema(r)}, true
	}))
	// One readable address per target, listed in the sitemap.
	mux.HandleFunc("GET /t/{slug}", page("index.html", func(r *http.Request) (pageMeta, bool) {
		return api.targetMeta(r, r.PathValue("slug"))
	}))
	// A shared link renders the normal detail page for one target, and is
	// never indexed: it is a credential in a URL.
	mux.HandleFunc("GET /s/{token}", page("index.html", func(r *http.Request) (pageMeta, bool) {
		if _, ok := api.store.ShareTarget(r.PathValue("token")); !ok {
			return pageMeta{}, false
		}
		return pageMeta{Path: "/s/" + r.PathValue("token"), Title: "Shared graph", NoIndex: true}, true
	}))
	mux.HandleFunc("GET /robots.txt", api.robotsTxt)
	mux.HandleFunc("GET /sitemap.xml", api.sitemapXML)
	mux.Handle("GET /", withAssetCache(http.FileServer(http.FS(sub)), 300))
	mux.HandleFunc("GET /api/v1/admin/probe/status", api.need(RoleViewer, api.probeStatus))
	go ovCache.Loop(stop)
	alerter := NewAlerter(store)
	api.alerter = alerter
	go alerter.Loop(stop, api.probeID)

	// Compression et limite de debit devant toutes les routes.
	handler := withRateLimit(newRateLimiter(20, 80), withGzip(mux))

	srv := &http.Server{
		Addr:              cfg.Listen,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		if c := containerStatus(); c.InContainer {
			log.Printf("container: %s", c.Message)
		}
		log.Printf("listening on http://%s (%s)", cfg.Listen, listenNote(cfg.Listen))
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("http: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	restart := false
	select {
	case <-sig:
		log.Printf("shutting down")
	case <-upd.Restart:
		restart = true
		// Laisse le temps a la reponse HTTP de partir.
		time.Sleep(700 * time.Millisecond)
		log.Printf("restarting on the new version")
	}
	close(stop)
	// L'ecrivain vide sa file avant la fermeture de la base.
	writer.Wait(5 * time.Second)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
	time.Sleep(300 * time.Millisecond)
	if restart {
		store.Close()
		execBinary(upd.CurrentBinary())
	}
}

// execBinary remplace le processus courant par un binaire (meme PID,
// memes arguments) : compatible systemd, Docker ou lancement manuel.
func execBinary(path string) {
	if err := syscall.Exec(path, os.Args, os.Environ()); err != nil {
		log.Fatalf("cannot restart %s: %v", path, err)
	}
}

func backgroundLoop(store *Store, stop <-chan struct{}) {
	rollup := time.NewTicker(30 * time.Second)
	purge := time.NewTicker(6 * time.Hour)
	defer rollup.Stop()
	defer purge.Stop()
	for {
		select {
		case <-stop:
			return
		case <-rollup.C:
			if err := store.RollupTick(time.Now().Unix()); err != nil {
				log.Printf("rollup: %v", err)
			}
		case <-purge.C:
			store.PurgeSessions()
			if err := store.Purge(time.Now().Unix()); err != nil {
				log.Printf("purge: %v", err)
			}
		}
	}
}
