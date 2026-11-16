package main

import (
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The text exposition format, version 0.0.4, one regular expression per
// kind of line.
var (
	expoName   = `[a-zA-Z_:][a-zA-Z0-9_:]*`
	expoPair   = `[a-zA-Z_][a-zA-Z0-9_]*="(?:[^"\\\n]|\\[\\"n])*"`
	expoHelp   = regexp.MustCompile(`^# HELP (` + expoName + `) [^\\]*$`)
	expoType   = regexp.MustCompile(`^# TYPE (` + expoName + `) (counter|gauge|histogram|summary|untyped)$`)
	expoSample = regexp.MustCompile(`^(` + expoName + `)(\{` + expoPair + `(?:,` + expoPair + `)*\})? (\S+)$`)
)

// parseExposition checks every line of a scrape and returns each series with
// its value, and each family with its type. A sample before its TYPE, a
// family split in two and a series given twice fail, as they do in
// Prometheus.
func parseExposition(t *testing.T, body string) (map[string]float64, map[string]string) {
	t.Helper()
	if !strings.HasSuffix(body, "\n") {
		t.Error("the exposition must end with a line feed")
	}
	series, types := map[string]float64{}, map[string]string{}
	closed, current := map[string]bool{}, ""
	for i, line := range strings.Split(strings.TrimSuffix(body, "\n"), "\n") {
		if m := expoType.FindStringSubmatch(line); m != nil {
			if _, dup := types[m[1]]; dup {
				t.Errorf("line %d: a second TYPE for %s", i+1, m[1])
			}
			types[m[1]] = m[2]
			continue
		}
		if expoHelp.MatchString(line) {
			continue
		}
		m := expoSample.FindStringSubmatch(line)
		if m == nil {
			t.Errorf("line %d is neither HELP, TYPE nor a sample: %q", i+1, line)
			continue
		}
		if types[m[1]] == "" {
			t.Errorf("line %d: %s has no TYPE before its first sample", i+1, m[1])
		}
		if m[1] != current {
			if closed[m[1]] {
				t.Errorf("line %d: the samples of %s are not contiguous", i+1, m[1])
			}
			closed[current], current = true, m[1]
		}
		v, err := strconv.ParseFloat(m[3], 64)
		if err != nil {
			t.Errorf("line %d: value %q: %v", i+1, m[3], err)
		}
		key := m[1] + m[2]
		if _, dup := series[key]; dup {
			t.Errorf("line %d: %s is given twice", i+1, key)
		}
		series[key] = v
	}
	return series, types
}

func seriesKey(name string, kv ...string) string {
	return name + "{" + labelSet(kv...) + "}"
}

func scrape(t *testing.T, mux *http.ServeMux, header, value string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", "/metrics", nil)
	if header != "" {
		req.Header.Set(header, value)
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// Every role may read the metrics, nobody else may, and the answer is never
// kept by a shared cache.
func TestMetricsNeedAuthentication(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	res, err := store.cfg.Exec(`INSERT INTO users(email,display_name,password_hash,role,created_at)
	                            VALUES('viewer@example.net','Viewer','-','viewer',0)`)
	if err != nil {
		t.Fatal(err)
	}
	uid, _ := res.LastInsertId()
	session, _, err := store.OpenSession(uid, "192.0.2.1", "test")
	if err != nil {
		t.Fatal(err)
	}
	api := &API{store: store, token: "secret", probeID: 1}
	mux := http.NewServeMux()
	api.MetricsRoutes(mux)

	for _, c := range []struct {
		name, header, value string
		code                int
	}{
		{"anonymous", "", "", 401},
		{"wrong token", "Authorization", "Bearer wrong", 401},
		{"unknown session", "Cookie", sessionCookie + "=nope", 401},
		{"admin token", "Authorization", "Bearer secret", 200},
		{"viewer session", "Cookie", sessionCookie + "=" + session, 200},
	} {
		rec := scrape(t, mux, c.header, c.value)
		if rec.Code != c.code {
			t.Errorf("%s: HTTP %d, expected %d", c.name, rec.Code, c.code)
			continue
		}
		if c.code != 200 {
			if strings.Contains(rec.Body.String(), "smokestack_") {
				t.Errorf("%s: a refusal carries metrics", c.name)
			}
			continue
		}
		if ct := rec.Header().Get("Content-Type"); ct != "text/plain; version=0.0.4; charset=utf-8" {
			t.Errorf("%s: Content-Type %q", c.name, ct)
		}
		if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
			t.Errorf("%s: Cache-Control %q lets a cache keep the answer", c.name, cc)
		}
		parseExposition(t, rec.Body.String())
	}
}

func TestMetricsLabelEscaping(t *testing.T) {
	got := labelSet("title", "Edge \"north\" \\ rack\nB", "probe", "par-01")
	want := `title="Edge \"north\" \\ rack\nB",probe="par-01"`
	if got != want {
		t.Errorf("labelSet = %s, want %s", got, want)
	}
}

// What a scrape says about targets and probes: private targets included,
// rows that no longer describe a measurement left out, and no address at all.
func TestMetricsExposition(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cat, err := store.CreateCategory("core", "Cœur", "Core", true)
	if err != nil {
		t.Fatal(err)
	}
	mk := func(slug, title, proto string, port int, public, enabled bool) int64 {
		id, err := store.CreateTarget(&Target{CategoryID: cat, Slug: slug, Title: title,
			Host: "192.0.2.77", Port: port, Proto: proto, IntervalS: 60, Packets: 10,
			SpacingMs: 100, TimeoutMs: 1000, Public: public, Enabled: enabled})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	pub := mk("pub", `Transit "north"`, "icmp", 0, true, true)
	priv := mk("priv", "Private link", "icmp", 0, false, true)
	web := mk("web", "Web front", "tcp", 443, true, true)
	off := mk("off", "Paused", "icmp", 0, true, false)

	local, err := store.ProbeID("local-01", "Local", "here", true)
	if err != nil {
		t.Fatal(err)
	}
	remote, err := store.CreateProbe("par-01", "Paris", "Paris")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetProbeTargets(remote.ID, []int64{pub}); err != nil {
		t.Fatal(err)
	}
	gone, err := store.CreateProbe("old-01", "Retired", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetProbeTargets(gone.ID, []int64{pub}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetProbeEnabled(gone.ID, false); err != nil {
		t.Fatal(err)
	}
	silent, err := store.CreateProbe("lon-01", "London", "London")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetProbeTargets(silent.ID, []int64{pub}); err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	store.TouchProbe(local)
	store.TouchProbe(gone.ID)
	for _, r := range []struct {
		target, probe   int64
		med, p95, lossP float64
	}{
		{pub, local, 12300, 20000, 25},
		{priv, local, 0, 0, 100},
		{web, local, 800, 900, 0},
		{off, local, 800, 900, 0},
		{pub, remote.ID, 15000, 18000, 0},
		{priv, remote.ID, 1000, 1000, 0},
		{pub, gone.ID, 1000, 1000, 0},
	} {
		if _, err := store.mxw.Exec(`INSERT INTO live(target_id,probe_id,ts,med_us,p95_us,loss_pct)
		                             VALUES(?,?,?,?,?,?)`, r.target, r.probe, now, r.med, r.p95, r.lossP); err != nil {
			t.Fatal(err)
		}
	}
	// A probe that went quiet a day ago during an outage: its row still reads
	// 80 % loss, and nothing will ever overwrite it.
	dayAgo := now - 86400
	if _, err := store.mxw.Exec(`INSERT INTO live(target_id,probe_id,ts,med_us,p95_us,loss_pct)
	                             VALUES(?,?,?,?,?,?)`, pub, silent.ID, dayAgo, 30000, 40000, 80); err != nil {
		t.Fatal(err)
	}

	alerter := NewAlerter(store)
	alerter.lastViews.Store(&map[int64]probeView{
		local:     {status: map[int64]string{pub: "warn", priv: "crit", web: "ok"}},
		remote.ID: {status: map[int64]string{pub: "ok"}},
	})
	alerter.openIncident(priv, local, now, "100.0 % packet loss")
	if _, err := store.CreateMaintenance(&Maintenance{TargetID: pub, Title: "Line card swap",
		StartsAt: now - 60, EndsAt: now + 3600, StopAlerts: true}); err != nil {
		t.Fatal(err)
	}
	store.cfg.Exec(certSchema)
	expiry := now + 10*86400
	store.saveCert(web, CertState{CheckedAt: now, NotAfter: expiry})

	writer := NewWriter(store, nil)
	writer.written.Add(7)
	api := &API{store: store, token: "secret", probeID: local, alerter: alerter, writer: writer}
	mux := http.NewServeMux()
	api.MetricsRoutes(mux)
	rec := scrape(t, mux, "Authorization", "Bearer secret")
	if rec.Code != 200 {
		t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	got, types := parseExposition(t, body)

	if strings.Contains(body, "192.0.2.77") {
		t.Error("a target's address appears in the metrics")
	}
	labels := func(id int64, slug, proto string, more ...string) []string {
		return append([]string{"target", slug, "target_id", strconv.FormatInt(id, 10),
			"category", "core", "proto", proto}, more...)
	}
	pair := func(id int64, slug, probe string, more ...string) []string {
		return labels(id, slug, "icmp", append([]string{"probe", probe}, more...)...)
	}
	expect := func(want float64, name string, kv ...string) {
		t.Helper()
		key := name
		if len(kv) > 0 {
			key = seriesKey(name, kv...)
		}
		g, ok := got[key]
		if !ok {
			t.Errorf("missing %s", key)
		} else if d := g - want; d > 1e-12 || d < -1e-12 {
			t.Errorf("%s = %v, want %v", key, g, want)
		}
	}
	expect(1, "smokestack_target_info", labels(pub, "pub", "icmp", "title", `Transit "north"`, "public", "true")...)
	expect(1, "smokestack_target_info", labels(priv, "priv", "icmp", "title", "Private link", "public", "false")...)
	expect(0.0123, "smokestack_target_rtt_median_seconds", pair(pub, "pub", "local-01")...)
	expect(0.02, "smokestack_target_rtt_p95_seconds", pair(pub, "pub", "local-01")...)
	expect(0.25, "smokestack_target_loss_ratio", pair(pub, "pub", "local-01")...)
	expect(float64(now), "smokestack_target_last_pass_timestamp_seconds", pair(pub, "pub", "local-01")...)
	expect(0.015, "smokestack_target_rtt_median_seconds", pair(pub, "pub", "par-01")...)
	expect(1, "smokestack_target_loss_ratio", pair(priv, "priv", "local-01")...)
	expect(1, "smokestack_target_status", pair(pub, "pub", "local-01", "state", "warn")...)
	expect(0, "smokestack_target_status", pair(pub, "pub", "local-01", "state", "ok")...)
	expect(1, "smokestack_target_status", pair(pub, "pub", "par-01", "state", "ok")...)
	expect(1, "smokestack_target_status", pair(priv, "priv", "local-01", "state", "crit")...)
	expect(1, "smokestack_target_in_maintenance", labels(pub, "pub", "icmp")...)
	expect(0, "smokestack_target_in_maintenance", labels(priv, "priv", "icmp")...)
	expect(1, "smokestack_target_incident_open", labels(priv, "priv", "icmp")...)
	expect(0, "smokestack_target_incident_open", labels(pub, "pub", "icmp")...)
	expect(1, "smokestack_target_certificate_valid", labels(web, "web", "tcp")...)
	expect(float64(expiry), "smokestack_target_certificate_expiry_timestamp_seconds", labels(web, "web", "tcp")...)
	expect(1, "smokestack_probe_enabled", "probe", "local-01")
	expect(0, "smokestack_probe_enabled", "probe", "old-01")
	expect(1, "smokestack_build_info", "version", Version, "goversion", runtime.Version())
	expect(7, "smokestack_measurements_written_total")
	expect(0, "smokestack_measurements_dropped_total")
	expect(writerQueueSize, "smokestack_write_queue_capacity")
	if got[seriesKey("smokestack_database_size_bytes", "database", "metrics", "file", "main")] <= 0 {
		t.Error("the size of metrics.db is missing")
	}
	if got[seriesKey("smokestack_probe_last_report_timestamp_seconds", "probe", "local-01")] < float64(now) {
		t.Error("the local probe's last report is missing")
	}
	if _, ok := got[seriesKey("smokestack_probe_last_report_timestamp_seconds", "probe", "old-01")]; ok {
		t.Error("a disabled probe exports its last report, which would read as a probe gone silent")
	}
	expect(float64(dayAgo), "smokestack_target_last_pass_timestamp_seconds", pair(pub, "pub", "lon-01")...)
	for _, name := range []string{"smokestack_target_rtt_median_seconds", "smokestack_target_rtt_p95_seconds",
		"smokestack_target_loss_ratio"} {
		if _, ok := got[seriesKey(name, pair(pub, "pub", "lon-01")...)]; ok {
			t.Errorf("%s is exported for a probe silent for a day", name)
		}
	}
	// A disabled target, a pair whose probe is no longer assigned the target,
	// and a disabled probe: the live table still holds a row for each.
	unassigned := labelSet(pair(priv, "priv", "par-01")...)
	for k := range got {
		if strings.Contains(k, `target="off"`) || strings.Contains(k, unassigned) ||
			strings.Contains(k, `probe="old-01",`) {
			t.Errorf("%s should not be exported", k)
		}
	}
	if _, ok := got[seriesKey("smokestack_target_rtt_median_seconds", pair(priv, "priv", "local-01")...)]; ok {
		t.Error("a pass that got nothing back exports a round-trip time")
	}
	for name, typ := range types {
		if strings.HasSuffix(name, "_total") != (typ == "counter") {
			t.Errorf("%s is a %s: only counters end in _total, and every counter does", name, typ)
		}
	}
}

// A pass stays current for three intervals, and never less than the silence
// the alerter tolerates from a probe before it says anything.
func TestMetricsPairFreshness(t *testing.T) {
	const now = 1_800_000_000
	for _, c := range []struct {
		interval, age int64
		fresh         bool
	}{
		{60, 0, true},
		{60, probeSilentAfter, true},
		{60, probeSilentAfter + 1, false},
		{600, 1800, true},
		{600, 1801, false},
		{86400, 3 * 86400, true},
	} {
		p := metricPair{row: LiveRow{TS: now - c.age}, interval: c.interval}
		if got := p.fresh(now); got != c.fresh {
			t.Errorf("interval %d s, last pass %d s ago: fresh = %v, want %v", c.interval, c.age, got, c.fresh)
		}
	}
}

// A batch that fails to commit is counted apart from the queue's drops, and
// the figures the probe status endpoint returns keep their shape.
func TestWriterCountsFailedBatches(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.mxw.Exec(`CREATE TRIGGER full_disk BEFORE INSERT ON samples
	                             BEGIN SELECT RAISE(ABORT, 'database or disk is full'); END`); err != nil {
		t.Fatal(err)
	}
	writer := NewWriter(store, nil)
	now := time.Now().Unix()
	for i := int64(0); i < 3; i++ {
		writer.Submit(Measurement{TargetID: 1, ProbeID: 1, TS: now + i, Sent: 1, RTTus: []float64{1000}}, "")
	}
	stop := make(chan struct{})
	go writer.Loop(stop)
	close(stop)
	writer.Wait(5 * time.Second)

	if n := writer.failed.Load(); n != 3 {
		t.Errorf("failed = %d, want 3", n)
	}
	if n := writer.written.Load(); n != 0 {
		t.Errorf("written = %d after a batch that did not commit", n)
	}
	if n := writer.dropped.Load(); n != 0 {
		t.Errorf("dropped = %d, but the queue never filled", n)
	}
	live, err := store.Live()
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 0 {
		t.Errorf("the live table kept %d rows of a batch that was rolled back", len(live))
	}
	keys := slices.Sorted(maps.Keys(writer.Stats()))
	if want := []string{"capacity", "dropped", "last_batch_ms", "queued", "written"}; !slices.Equal(keys, want) {
		t.Errorf("writer stats carry %v, want %v", keys, want)
	}

	api := &API{store: store, token: "secret", probeID: 1, writer: writer}
	mux := http.NewServeMux()
	api.MetricsRoutes(mux)
	got, types := parseExposition(t, scrape(t, mux, "Authorization", "Bearer secret").Body.String())
	if types["smokestack_measurements_write_failed_total"] != "counter" ||
		got["smokestack_measurements_write_failed_total"] != 3 {
		t.Errorf("write_failed_total: %v (%s)", got["smokestack_measurements_write_failed_total"],
			types["smokestack_measurements_write_failed_total"])
	}
}

// A counter never goes back while the process lives, whatever happens
// between two scrapes.
func TestMetricsCountersNeverGoBack(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	writer := NewWriter(store, nil)
	api := &API{store: store, token: "secret", probeID: 1, writer: writer}
	mux := http.NewServeMux()
	api.MetricsRoutes(mux)

	first, types := parseExposition(t, scrape(t, mux, "Authorization", "Bearer secret").Body.String())
	writer.Submit(Measurement{TargetID: 1, ProbeID: 1, TS: time.Now().Unix(), Sent: 1}, "")
	stop := make(chan struct{})
	go writer.Loop(stop)
	close(stop)
	writer.Wait(5 * time.Second)
	writer.dropped.Add(2)
	second, _ := parseExposition(t, scrape(t, mux, "Authorization", "Bearer secret").Body.String())

	counters := 0
	for name, typ := range types {
		if typ != "counter" {
			continue
		}
		counters++
		if second[name] < first[name] {
			t.Errorf("%s went back from %v to %v", name, first[name], second[name])
		}
	}
	if counters == 0 {
		t.Fatal("no counter exported")
	}
	if second["smokestack_measurements_written_total"] != first["smokestack_measurements_written_total"]+1 ||
		second["smokestack_measurements_dropped_total"] != first["smokestack_measurements_dropped_total"]+2 {
		t.Errorf("counters did not follow the pipeline: %v then %v", first, second)
	}
}

// A thousand targets seen from three probes, scraped as Prometheus would.
// What it costs is logged; the bound is loose, there to catch a scrape that
// has become expensive rather than to time it.
func TestMetricsScrapeAtScale(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cat, err := store.CreateCategory("transit", "Transit", "Transit", true)
	if err != nil {
		t.Fatal(err)
	}
	local, err := store.ProbeID("local-01", "Local", "", true)
	if err != nil {
		t.Fatal(err)
	}
	probes := []int64{local}
	for _, slug := range []string{"par-01", "fra-01"} {
		p, err := store.CreateProbe(slug, slug, "")
		if err != nil {
			t.Fatal(err)
		}
		if err := store.SetProbeAllTargets(p.ID, true); err != nil {
			t.Fatal(err)
		}
		probes = append(probes, p.ID)
	}
	const n = 1000
	now := time.Now().Unix()
	tx, err := store.cfg.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < n; i++ {
		if _, err := tx.Exec(`INSERT INTO targets(category_id,slug,title,host,proto,interval_s,packets,
		                      spacing_ms,timeout_ms,public,enabled,created_at)
		                      VALUES(?,?,?,'192.0.2.1','icmp',60,10,100,1000,?,1,?)`,
			cat, fmt.Sprintf("t-%04d", i), fmt.Sprintf("Target %d", i), i%2, now); err != nil {
			t.Fatal(err)
		}
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	views := map[int64]probeView{}
	mtx, err := store.mxw.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range probes {
		views[p] = probeView{status: map[int64]string{}}
		for id := int64(1); id <= n; id++ {
			views[p].status[id] = targetStates[id%int64(len(targetStates))]
			if _, err := mtx.Exec(`INSERT INTO live(target_id,probe_id,ts,med_us,p95_us,loss_pct)
			                       VALUES(?,?,?,?,?,?)`, id, p, now, 10000+float64(id), 12000+float64(id), 0); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := mtx.Commit(); err != nil {
		t.Fatal(err)
	}
	alerter := NewAlerter(store)
	alerter.lastViews.Store(&views)
	api := &API{store: store, token: "secret", probeID: local, alerter: alerter, writer: NewWriter(store, nil)}
	mux := http.NewServeMux()
	api.MetricsRoutes(mux)

	scrape(t, mux, "Authorization", "Bearer secret") // warm the connection pools
	start := time.Now()
	rec := scrape(t, mux, "Authorization", "Bearer secret")
	took := time.Since(start)
	if rec.Code != 200 {
		t.Fatalf("HTTP %d", rec.Code)
	}
	got, _ := parseExposition(t, rec.Body.String())
	statuses := 0
	for k := range got {
		if strings.HasPrefix(k, "smokestack_target_status{") {
			statuses++
		}
	}
	if want := n * len(probes) * len(targetStates); statuses != want {
		t.Errorf("%d status series, want %d", statuses, want)
	}
	t.Logf("%d targets × %d probes: %d series, %d KiB, scraped in %v",
		n, len(probes), len(got), rec.Body.Len()/1024, took)
	if took > time.Second {
		t.Errorf("a scrape of %d targets took %v", n, took)
	}
}

// /metrics is held to the API's per-address budget. Pages are not, and
// neither is a caller on the loopback with no proxy in front of it.
func TestRateLimitCoversMetrics(t *testing.T) {
	for _, c := range []struct {
		path, remote, forwarded string
		limited                 bool
	}{
		{"/api/v1/tree", "198.51.100.7:40000", "", true},
		{"/api/v1/tree", "127.0.0.1:40000", "203.0.113.9", true},
		{"/api/v1/tree", "[::1]:40000", "", false},
		{"/metrics", "198.51.100.8:40000", "", true},
		{"/metrics", "127.0.0.1:40000", "203.0.113.10", true},
		{"/metrics", "127.0.0.1:40000", "", false},
		{"/metrics/", "198.51.100.9:40000", "", false},
		{"/", "198.51.100.9:40000", "", false},
		{"/admin", "198.51.100.9:40000", "", false},
	} {
		rl := &rateLimiter{buckets: map[string]*bucket{}, burst: 2}
		h := withRateLimit(rl, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
		var codes []int
		for range 4 {
			req := httptest.NewRequest("GET", c.path, nil)
			req.RemoteAddr = c.remote
			if c.forwarded != "" {
				req.Header.Set("X-Forwarded-For", c.forwarded)
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			codes = append(codes, rec.Code)
			if rec.Code == http.StatusTooManyRequests && rec.Header().Get("Retry-After") != "1" {
				t.Errorf("%s from %s: a refusal without Retry-After", c.path, c.remote)
			}
		}
		want := []int{200, 200, 200, 200}
		if c.limited {
			want = []int{200, 200, 429, 429}
		}
		if !slices.Equal(codes, want) {
			t.Errorf("%s from %s (forwarded %q): %v, want %v", c.path, c.remote, c.forwarded, codes, want)
		}
	}
}
