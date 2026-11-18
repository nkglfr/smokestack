package main

import (
	"database/sql"
	"math"
	"net/http"
	"os"
	"runtime"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
)

// GET /metrics serves the instance in the Prometheus text exposition format
// (version 0.0.4), so that an operator who already graphs and alerts from
// Prometheus and Grafana finds smokestack there instead of in a tab of its
// own.
//
// The format is written here by hand. It is a few lines of text per sample,
// and the official client library would be the second module this binary
// depends on.
//
// A scrape reads only what is already at hand: the live table, one row per
// target and probe; the statuses the alerter worked out on its last round;
// the small tables of config.db; and the write pipeline's counters. Never the
// samples or the rollups, so that a scrape every fifteen seconds costs the
// same on the day an incident makes every page busy.
//
// Moments are exported as Unix timestamps rather than ages, as Prometheus
// recommends: `time() - x` gives the age, and a timestamp cannot get stuck.
// A certificate's remaining life is its expiry date, for the same reason.
//
// No host, port or address is exported, whatever the caller may read
// elsewhere: what is scraped ends up in a Prometheus and a Grafana that are
// not bound by this instance's visibility rules, so the series carry
// identifiers and figures, never the addressing.

func (a *API) MetricsRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /metrics", a.need(RoleViewer, a.metrics))
}

func (a *API) metrics(w http.ResponseWriter, r *http.Request, _ *User) {
	body, err := a.metricsText(time.Now())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	cachePrivate(w)
	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Write(body)
}

// targetStates are the values of smokestack_target_status, the same ones
// the public pages and the alerter use.
var targetStates = []string{"ok", "warn", "crit", "nodata", "maint"}

type metricTarget struct {
	t      *Target
	labels string
}

type metricPair struct {
	row      LiveRow
	interval int64
	labels   string
}

// answered is false for a pass that got nothing back, whose percentiles the
// live table records as zero rather than as missing.
func (p metricPair) answered() bool { return p.row.MedMs > 0 }

// fresh is false once the probe has missed three passes and stayed silent
// longer than the alerter tolerates: the row then holds the last figures of
// a measurement that has stopped, which nothing will ever overwrite.
func (p metricPair) fresh(now int64) bool {
	return now-p.row.TS <= max(3*p.interval, probeSilentAfter)
}

func (a *API) metricsText(now time.Time) ([]byte, error) {
	cats, err := a.store.Tree(false)
	if err != nil {
		return nil, err
	}
	var targets []metricTarget
	for _, c := range cats {
		for _, t := range c.Targets {
			if t.Enabled {
				targets = append(targets, metricTarget{t: t, labels: labelSet("target", t.Slug,
					"target_id", strconv.FormatInt(t.ID, 10), "category", c.Slug, "proto", t.Proto)})
			}
		}
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].t.ID < targets[j].t.ID })
	probes, err := a.store.Probes()
	if err != nil {
		return nil, err
	}
	pairs, err := a.metricPairs(targets, probes)
	if err != nil {
		return nil, err
	}

	var e exposition
	e.family("smokestack_build_info", "gauge", "Version of this instance and of the Go toolchain it was built with.")
	e.sample("smokestack_build_info", labelSet("version", Version, "goversion", runtime.Version()), 1)
	a.pipelineFamilies(&e)
	a.storageFamilies(&e)
	probeFamilies(&e, probes)
	a.targetFamilies(&e, targets, now)
	a.pairFamilies(&e, pairs, now.Unix())
	return e.b, nil
}

// metricPairs is every target and probe the live table holds a pass for,
// kept to the enabled probes that still measure an enabled target, which is
// what TargetWatchers lists. A row stays in the live table after its probe
// is unassigned or turned off, and exporting it would present a frozen
// figure as a current one.
func (a *API) metricPairs(targets []metricTarget, probes []Probe) ([]metricPair, error) {
	live, err := a.store.Live()
	if err != nil {
		return nil, err
	}
	watchers, err := a.store.TargetWatchers(a.probeID)
	if err != nil {
		return nil, err
	}
	byTarget := map[int64]metricTarget{}
	for _, mt := range targets {
		byTarget[mt.t.ID] = mt
	}
	byProbe := map[int64]Probe{}
	for _, p := range probes {
		byProbe[p.ID] = p
	}
	var out []metricPair
	for _, row := range live {
		mt, ok := byTarget[row.TargetID]
		p, known := byProbe[row.ProbeID]
		if !ok || !known || !slices.Contains(watchers[row.TargetID], row.ProbeID) {
			continue
		}
		out = append(out, metricPair{row: row, interval: mt.t.IntervalS,
			labels: mt.labels + "," + labelSet("probe", p.Slug)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].row.TargetID != out[j].row.TargetID {
			return out[i].row.TargetID < out[j].row.TargetID
		}
		return out[i].row.ProbeID < out[j].row.ProbeID
	})
	return out, nil
}

func (a *API) pipelineFamilies(e *exposition) {
	w := a.writer
	if w == nil {
		return
	}
	e.family("smokestack_write_queue_length", "gauge", "Measurements waiting to be written to metrics.db.")
	e.sample("smokestack_write_queue_length", "", float64(len(w.ch)))
	e.family("smokestack_write_queue_capacity", "gauge", "Measurements the write queue holds before it drops them.")
	e.sample("smokestack_write_queue_capacity", "", float64(cap(w.ch)))
	e.family("smokestack_write_last_batch_duration_seconds", "gauge", "Time the last batch took to commit.")
	e.sample("smokestack_write_last_batch_duration_seconds", "", float64(w.lastLag.Load())/1e6)
	e.family("smokestack_measurements_written_total", "counter", "Measurements written to metrics.db since the service started.")
	e.sample("smokestack_measurements_written_total", "", float64(w.written.Load()))
	e.family("smokestack_measurements_dropped_total", "counter",
		"Measurements dropped because the write queue was full, since the service started.")
	e.sample("smokestack_measurements_dropped_total", "", float64(w.dropped.Load()))
	e.family("smokestack_measurements_write_failed_total", "counter",
		"Measurements not written to metrics.db because their batch failed to commit, since the service started.")
	e.sample("smokestack_measurements_write_failed_total", "", float64(w.failed.Load()))
}

func (a *API) storageFamilies(e *exposition) {
	if a.archive != nil {
		rep := a.archive.Report()
		e.family("smokestack_archive_chunks_pending_upload", "gauge", "Sealed hours of raw samples waiting to be uploaded to S3.")
		e.sample("smokestack_archive_chunks_pending_upload", "", float64(rep.PendingUpl))
		e.family("smokestack_archive_local_bytes", "gauge", "Size of the raw archive kept on local disk.")
		e.sample("smokestack_archive_local_bytes", "", float64(rep.LocalBytes))
	}
	e.family("smokestack_database_size_bytes", "gauge", "Size on disk of each SQLite database, main file and write-ahead log.")
	for _, db := range []struct {
		name string
		pool *sql.DB
	}{{"config", a.store.cfg}, {"metrics", a.store.mx}} {
		path := databaseFile(db.pool)
		if path == "" {
			continue
		}
		for _, f := range []struct{ kind, path string }{{"main", path}, {"wal", path + "-wal"}} {
			if st, err := os.Stat(f.path); err == nil {
				e.sample("smokestack_database_size_bytes", labelSet("database", db.name, "file", f.kind), float64(st.Size()))
			}
		}
	}
}

// databaseFile asks SQLite where the main database of a pool lives, which
// spares the store from keeping its directory for this one use.
func databaseFile(db *sql.DB) string {
	var seq int
	var name, file string
	if db.QueryRow(`PRAGMA database_list`).Scan(&seq, &name, &file) != nil {
		return ""
	}
	return file
}

func probeFamilies(e *exposition, probes []Probe) {
	e.family("smokestack_probe_enabled", "gauge", "1 when the probe is enabled, 0 when an operator turned it off.")
	for _, p := range probes {
		e.sample("smokestack_probe_enabled", labelSet("probe", p.Slug), float64(b2i(p.Enabled)))
	}
	e.family("smokestack_probe_last_report_timestamp_seconds", "gauge",
		"When the probe last reported, as a Unix timestamp. Only for enabled probes that have reported.")
	for _, p := range probes {
		if p.Enabled && p.LastSeenAt > 0 {
			e.sample("smokestack_probe_last_report_timestamp_seconds", labelSet("probe", p.Slug), float64(p.LastSeenAt))
		}
	}
}

func (a *API) targetFamilies(e *exposition, targets []metricTarget, now time.Time) {
	e.family("smokestack_target_info", "gauge", "Title and visibility of each enabled target.")
	for _, mt := range targets {
		e.sample("smokestack_target_info", mt.labels+","+labelSet("title", mt.t.Title,
			"public", strconv.FormatBool(mt.t.Public)), 1)
	}
	maint := a.store.ActiveMaintenances(now.Unix())
	e.family("smokestack_target_in_maintenance", "gauge", "1 while a maintenance window is open on the target.")
	for _, mt := range targets {
		_, on := maint[mt.t.ID]
		e.sample("smokestack_target_in_maintenance", mt.labels, float64(b2i(on)))
	}
	if open, ok := a.openIncidents(); ok {
		e.family("smokestack_target_incident_open", "gauge", "1 while the alerter has an incident open on the target.")
		for _, mt := range targets {
			e.sample("smokestack_target_incident_open", mt.labels, float64(b2i(open[mt.t.ID])))
		}
	}
	a.certFamilies(e, targets)
}

// certFamilies covers the targets the watcher has inspected: one never
// looked at has no series, rather than a zero that would read as a broken
// certificate.
func (a *API) certFamilies(e *exposition, targets []metricTarget) {
	if !a.store.CertConfig().Enabled {
		return
	}
	certs := a.store.Certs()
	e.family("smokestack_target_certificate_valid", "gauge",
		"1 when the last inspection found a certificate a client would accept, 0 otherwise.")
	for _, mt := range targets {
		if st, ok := certs[mt.t.ID]; ok && mt.t.certWatched() {
			e.sample("smokestack_target_certificate_valid", mt.labels, float64(b2i(st.Problem == "")))
		}
	}
	e.family("smokestack_target_certificate_expiry_timestamp_seconds", "gauge",
		"When the certificate expires, as a Unix timestamp.")
	for _, mt := range targets {
		if st, ok := certs[mt.t.ID]; ok && mt.t.certWatched() && st.NotAfter > 0 {
			e.sample("smokestack_target_certificate_expiry_timestamp_seconds", mt.labels, float64(st.NotAfter))
		}
	}
}

// openIncidents reports false when there is no incident table to read,
// which is an instance without an alerter rather than one without incidents.
func (a *API) openIncidents() (map[int64]bool, bool) {
	rows, err := a.store.cfg.Query(`SELECT DISTINCT target_id FROM local_incidents WHERE closed_at=0`)
	if err != nil {
		return nil, false
	}
	defer rows.Close()
	open := map[int64]bool{}
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			open[id] = true
		}
	}
	return open, rows.Err() == nil
}

func (a *API) pairFamilies(e *exposition, pairs []metricPair, now int64) {
	e.family("smokestack_target_rtt_median_seconds", "gauge",
		"Median round-trip time of the last pass. Absent when nothing came back, or when the pass is older than the larger of three intervals and five minutes.")
	for _, p := range pairs {
		if p.fresh(now) && p.answered() {
			e.sample("smokestack_target_rtt_median_seconds", p.labels, p.row.MedMs/1000)
		}
	}
	e.family("smokestack_target_rtt_p95_seconds", "gauge",
		"95th percentile round-trip time of the last pass. Absent when nothing came back, or when the pass is older than the larger of three intervals and five minutes.")
	for _, p := range pairs {
		if p.fresh(now) && p.answered() {
			e.sample("smokestack_target_rtt_p95_seconds", p.labels, p.row.P95Ms/1000)
		}
	}
	e.family("smokestack_target_loss_ratio", "gauge",
		"Share of the last pass's packets that were lost, from 0 to 1. Absent when the pass is older than the larger of three intervals and five minutes.")
	for _, p := range pairs {
		if p.fresh(now) {
			e.sample("smokestack_target_loss_ratio", p.labels, p.row.LossPct/100)
		}
	}
	e.family("smokestack_target_last_pass_timestamp_seconds", "gauge", "When the last pass was taken, as a Unix timestamp.")
	for _, p := range pairs {
		e.sample("smokestack_target_last_pass_timestamp_seconds", p.labels, float64(p.row.TS))
	}
	if a.alerter == nil {
		return
	}
	views := a.alerter.lastViews.Load()
	if views == nil {
		return
	}
	e.family("smokestack_target_status", "gauge",
		"Status of the target as seen from the probe, one series per state, 1 for the current one. Updated once a minute.")
	for _, p := range pairs {
		st, ok := (*views)[p.row.ProbeID].status[p.row.TargetID]
		if !ok {
			continue
		}
		for _, s := range targetStates {
			e.sample("smokestack_target_status", p.labels+","+labelSet("state", s), float64(b2i(s == st)))
		}
	}
}

// ------------------------------------------------------------ the format

// exposition accumulates the text format. A family's HELP and TYPE lines
// are written once, then all of its samples, which the format requires to
// be contiguous.
type exposition struct{ b []byte }

func (e *exposition) family(name, typ, help string) {
	e.b = append(e.b, "# HELP "+name+" "+help+"\n# TYPE "+name+" "+typ+"\n"...)
}

func (e *exposition) sample(name, labels string, v float64) {
	e.b = append(e.b, name...)
	if labels != "" {
		e.b = append(e.b, '{')
		e.b = append(e.b, labels...)
		e.b = append(e.b, '}')
	}
	e.b = append(e.b, ' ')
	// Whole numbers — counts, sizes, timestamps — are written as integers,
	// so that a timestamp reads 1763300000 rather than 1.7633e+09.
	if v == math.Trunc(v) && math.Abs(v) < 1<<53 {
		e.b = strconv.AppendInt(e.b, int64(v), 10)
	} else {
		e.b = strconv.AppendFloat(e.b, v, 'g', -1, 64)
	}
	e.b = append(e.b, '\n')
}

// labelEscaper applies the format's three escapes inside a label value.
var labelEscaper = strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`)

// labelSet renders name and value pairs once, so that labels shared by
// several families are escaped once rather than once per sample.
func labelSet(kv ...string) string {
	var b strings.Builder
	for i := 0; i+1 < len(kv); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(kv[i])
		b.WriteString(`="`)
		labelEscaper.WriteString(&b, kv[i+1])
		b.WriteByte('"')
	}
	return b.String()
}
