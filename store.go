package main

import (
	"database/sql"
	"fmt"
	"math"
	"net"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

// Toutes les tables de mesure partagent la meme forme et la meme
// colonne temporelle "bucket" (epoch en secondes), ce qui permet
// d'utiliser un seul jeu de fonctions pour la cascade d'agregation.
// samples porte une ligne par passe de mesure ; les tables roll_*
// portent une ligne par intervalle agrege.

type granularity struct {
	table string
	secs  int64
	keep  int64 // retention en secondes, 0 = illimite
}

var cascade = []granularity{
	{"samples", 0, 48 * 3600},
	{"roll_1m", 60, 365 * 86400},
	{"roll_5m", 300, 1095 * 86400},
	{"roll_1h", 3600, 0},
	{"roll_1d", 86400, 0},
}

const configSchema = `
CREATE TABLE IF NOT EXISTS settings (
  key   TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS users (
  id            INTEGER PRIMARY KEY,
  email         TEXT NOT NULL UNIQUE,
  display_name  TEXT NOT NULL,
  password_hash TEXT NOT NULL,
  role          TEXT NOT NULL CHECK (role IN ('master','admin','editor','viewer')),
  locale        TEXT NOT NULL DEFAULT 'fr',
  disabled      INTEGER NOT NULL DEFAULT 0,
  created_at    INTEGER NOT NULL,
  last_login_at INTEGER
);
CREATE TABLE IF NOT EXISTS sessions (
  token_hash TEXT PRIMARY KEY,
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  csrf       TEXT NOT NULL,
  ip         TEXT,
  user_agent TEXT,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_exp ON sessions(expires_at);
CREATE TABLE IF NOT EXISTS audit_log (
  id        INTEGER PRIMARY KEY,
  ts        INTEGER NOT NULL,
  user_id   INTEGER,
  email     TEXT,
  action    TEXT NOT NULL,
  entity    TEXT,
  entity_id TEXT,
  ip        TEXT
);
CREATE INDEX IF NOT EXISTS idx_audit_ts ON audit_log(ts);
CREATE TABLE IF NOT EXISTS probes (
  id           INTEGER PRIMARY KEY,
  slug         TEXT NOT NULL UNIQUE,
  name         TEXT NOT NULL,
  location     TEXT,
  token_hash   TEXT,
  local        INTEGER NOT NULL DEFAULT 0,
  enabled      INTEGER NOT NULL DEFAULT 1,
  last_seen_at INTEGER
);
CREATE TABLE IF NOT EXISTS categories (
  id        INTEGER PRIMARY KEY,
  parent_id INTEGER REFERENCES categories(id) ON DELETE CASCADE,
  slug      TEXT NOT NULL,
  menu_fr   TEXT NOT NULL,
  menu_en   TEXT NOT NULL,
  position  INTEGER NOT NULL DEFAULT 0,
  public    INTEGER NOT NULL DEFAULT 1
);
CREATE TABLE IF NOT EXISTS targets (
  id          INTEGER PRIMARY KEY,
  category_id INTEGER NOT NULL REFERENCES categories(id) ON DELETE CASCADE,
  slug        TEXT NOT NULL,
  title       TEXT NOT NULL,
  host        TEXT NOT NULL,
  proto       TEXT NOT NULL DEFAULT 'icmp',
  interval_s  INTEGER NOT NULL DEFAULT 60,
  packets     INTEGER NOT NULL DEFAULT 20,
  spacing_ms  INTEGER NOT NULL DEFAULT 500,
  timeout_ms  INTEGER NOT NULL DEFAULT 2000,
  public      INTEGER NOT NULL DEFAULT 1,
  enabled     INTEGER NOT NULL DEFAULT 1,
  note        TEXT,
  created_at  INTEGER NOT NULL,
  UNIQUE (category_id, slug)
);
CREATE TABLE IF NOT EXISTS events (
  id       INTEGER PRIMARY KEY,
  ts_start INTEGER NOT NULL,
  ts_end   INTEGER,
  kind     TEXT NOT NULL DEFAULT 'incident',
  title    TEXT NOT NULL,
  body     TEXT,
  scope    TEXT NOT NULL DEFAULT 'global',
  scope_id INTEGER,
  public   INTEGER NOT NULL DEFAULT 1
);
CREATE INDEX IF NOT EXISTS idx_events_ts ON events(ts_start);
`

const metricsSchemaTemplate = `
CREATE TABLE IF NOT EXISTS %s (
  target_id INTEGER NOT NULL,
  probe_id  INTEGER NOT NULL,
  bucket    INTEGER NOT NULL,
  sent      INTEGER NOT NULL,
  lost      INTEGER NOT NULL,
  cnt       INTEGER NOT NULL,
  min_us    REAL NOT NULL,
  max_us    REAL NOT NULL,
  sum_us    REAL NOT NULL,
  sumsq_us  REAL NOT NULL,
  sketch    BLOB NOT NULL,
  PRIMARY KEY (target_id, probe_id, bucket)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS idx_%s_bucket ON %s(bucket);
`

const metricsExtraSchema = `
CREATE TABLE IF NOT EXISTS target_addresses (
  target_id INTEGER NOT NULL,
  ip        TEXT NOT NULL,
  last_seen INTEGER NOT NULL,
  PRIMARY KEY (target_id, ip)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS target_errors (
  target_id INTEGER PRIMARY KEY,
  ts        INTEGER NOT NULL,
  err       TEXT NOT NULL
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS live (
  target_id INTEGER NOT NULL,
  probe_id  INTEGER NOT NULL,
  ts        INTEGER NOT NULL,
  med_us    REAL NOT NULL,
  p95_us    REAL NOT NULL,
  loss_pct  REAL NOT NULL,
  PRIMARY KEY (target_id, probe_id)
) WITHOUT ROWID;
CREATE TABLE IF NOT EXISTS raw_chunks (
  id         INTEGER PRIMARY KEY,
  probe_slug TEXT NOT NULL,
  ts_min     INTEGER NOT NULL,
  ts_max     INTEGER NOT NULL,
  location   TEXT NOT NULL,
  local_path TEXT,
  s3_key     TEXT,
  bytes      INTEGER NOT NULL,
  rows       INTEGER NOT NULL,
  sealed_at  INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_chunks_loc ON raw_chunks(location, ts_min);
CREATE TABLE IF NOT EXISTS upload_queue (
  chunk_id    INTEGER PRIMARY KEY,
  attempts    INTEGER NOT NULL DEFAULT 0,
  next_try_at INTEGER NOT NULL,
  last_error  TEXT
);
CREATE TABLE IF NOT EXISTS storage_stats (
  day           INTEGER PRIMARY KEY,
  local_bytes   INTEGER NOT NULL,
  s3_bytes      INTEGER NOT NULL,
  evicted_bytes INTEGER NOT NULL
);
`

type Store struct {
	cfg *sql.DB
	mx  *sql.DB // lectures (interface, API) : pool de connexions
	mxw *sql.DB // ecritures : une connexion dediee, jamais prise par une lecture

	targetsChanged chan struct{}
	gen            atomic.Int64 // incremente a chaque changement de cibles ou categories
}

func dsn(path string) string {
	return "file:" + path +
		"?_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)" +
		"&_pragma=synchronous(NORMAL)" +
		"&_pragma=foreign_keys(ON)"
}

func OpenStore(dir string) (*Store, error) {
	cfg, err := sql.Open("sqlite", dsn(filepath.Join(dir, "config.db")))
	if err != nil {
		return nil, err
	}
	cfg.SetMaxOpenConns(4)
	if _, err := cfg.Exec(configSchema); err != nil {
		return nil, fmt.Errorf("schema config: %w", err)
	}
	addColumn(cfg, "targets", "family INTEGER NOT NULL DEFAULT 0")
	addColumn(cfg, "targets", "port INTEGER NOT NULL DEFAULT 0")
	addColumn(cfg, "targets", "pin_ip TEXT NOT NULL DEFAULT ''")
	migrateTCPPorts(cfg)

	// Deux pools sur metrics.db : l'ecriture des mesures dispose de sa
	// propre connexion et ne fait jamais la queue derriere des lectures de
	// l'interface. En mode WAL, les lecteurs ne bloquent pas l'ecrivain.
	mxw, err := sql.Open("sqlite", dsn(filepath.Join(dir, "metrics.db")))
	if err != nil {
		return nil, err
	}
	mxw.SetMaxOpenConns(1)
	for _, g := range cascade {
		q := fmt.Sprintf(metricsSchemaTemplate, g.table, g.table, g.table)
		if _, err := mxw.Exec(q); err != nil {
			return nil, fmt.Errorf("schema %s: %w", g.table, err)
		}
	}
	if _, err := mxw.Exec(metricsExtraSchema); err != nil {
		return nil, fmt.Errorf("schema metrics: %w", err)
	}
	if _, err := mxw.Exec(tracerouteSchema); err != nil {
		return nil, fmt.Errorf("schema traceroutes: %w", err)
	}
	mx, err := sql.Open("sqlite", dsn(filepath.Join(dir, "metrics.db")))
	if err != nil {
		return nil, err
	}
	mx.SetMaxOpenConns(8)
	return &Store{cfg: cfg, mx: mx, mxw: mxw, targetsChanged: make(chan struct{}, 1)}, nil
}

func (s *Store) Close() {
	if s.mxw != nil {
		s.mxw.Close()
	}
	if s.cfg != nil {
		s.cfg.Close()
	}
	if s.mx != nil {
		s.mx.Close()
	}
}

// ---------------------------------------------------------------- config

type Target struct {
	ID         int64  `json:"id"`
	CategoryID int64  `json:"category_id"`
	Slug       string `json:"slug"`
	Title      string `json:"title"`
	Host       string `json:"host"`
	Proto      string `json:"proto"`
	IntervalS  int64  `json:"interval_s"`
	Packets    int    `json:"packets"`
	SpacingMs  int    `json:"spacing_ms"`
	TimeoutMs  int    `json:"timeout_ms"`
	Public     bool   `json:"public"`
	Enabled    bool   `json:"enabled"`
	// Family : 0 = automatique (adresse litterale, sinon IPv4 puis IPv6),
	// 4 = IPv4 seulement, 6 = IPv6 seulement.
	Family int `json:"family"`
	// Port : uniquement pour proto "tcp". 0 = port inclus dans Host
	// (ancienne forme "hote:port").
	Port int `json:"port"`
	// PinIP : adresse figee. Un nom qui tourne (pool.ntp.org) designe un
	// serveur different a chaque resolution ; figer l'adresse rend la
	// mesure comparable dans le temps.
	PinIP string `json:"pin_ip"`
}

type Category struct {
	ID       int64     `json:"id"`
	ParentID *int64    `json:"parent_id"`
	Slug     string    `json:"slug"`
	MenuFR   string    `json:"menu_fr"`
	MenuEN   string    `json:"menu_en"`
	Public   bool      `json:"public"`
	Targets  []*Target `json:"targets"`
}

func (s *Store) ProbeID(slug, name, location string, local bool) (int64, error) {
	l := 0
	if local {
		l = 1
	}
	_, err := s.cfg.Exec(
		`INSERT INTO probes(slug,name,location,local,enabled) VALUES(?,?,?,?,1)
		 ON CONFLICT(slug) DO UPDATE SET name=excluded.name, location=excluded.location`,
		slug, name, location, l)
	if err != nil {
		return 0, err
	}
	var id int64
	err = s.cfg.QueryRow(`SELECT id FROM probes WHERE slug=?`, slug).Scan(&id)
	return id, err
}

func (s *Store) TouchProbe(id int64) {
	s.cfg.Exec(`UPDATE probes SET last_seen_at=? WHERE id=?`, time.Now().Unix(), id)
}

func (s *Store) ActiveTargets() ([]*Target, error) {
	rows, err := s.cfg.Query(
		`SELECT id,category_id,slug,title,host,proto,interval_s,packets,
		        spacing_ms,timeout_ms,public,enabled,family,port,pin_ip
		   FROM targets WHERE enabled=1 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTargets(rows)
}

func scanTargets(rows *sql.Rows) ([]*Target, error) {
	var out []*Target
	for rows.Next() {
		t := &Target{}
		var pub, en int
		if err := rows.Scan(&t.ID, &t.CategoryID, &t.Slug, &t.Title, &t.Host,
			&t.Proto, &t.IntervalS, &t.Packets, &t.SpacingMs, &t.TimeoutMs,
			&pub, &en, &t.Family, &t.Port, &t.PinIP); err != nil {
			return nil, err
		}
		t.Public, t.Enabled = pub == 1, en == 1
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *Store) Tree(publicOnly bool) ([]*Category, error) {
	rows, err := s.cfg.Query(
		`SELECT id,parent_id,slug,menu_fr,menu_en,public
		   FROM categories ORDER BY position, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cats []*Category
	byID := map[int64]*Category{}
	for rows.Next() {
		c := &Category{Targets: []*Target{}}
		var pub int
		var parent sql.NullInt64
		if err := rows.Scan(&c.ID, &parent, &c.Slug, &c.MenuFR, &c.MenuEN, &pub); err != nil {
			return nil, err
		}
		if parent.Valid {
			v := parent.Int64
			c.ParentID = &v
		}
		c.Public = pub == 1
		if publicOnly && !c.Public {
			continue
		}
		cats = append(cats, c)
		byID[c.ID] = c
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	trows, err := s.cfg.Query(
		`SELECT id,category_id,slug,title,host,proto,interval_s,packets,
		        spacing_ms,timeout_ms,public,enabled,family,port,pin_ip
		   FROM targets ORDER BY title`)
	if err != nil {
		return nil, err
	}
	defer trows.Close()
	targets, err := scanTargets(trows)
	if err != nil {
		return nil, err
	}
	for _, t := range targets {
		if publicOnly && !t.Public {
			continue
		}
		if c, ok := byID[t.CategoryID]; ok {
			c.Targets = append(c.Targets, t)
		}
	}
	return cats, nil
}

func (s *Store) TargetByID(id int64) (*Target, error) {
	rows, err := s.cfg.Query(
		`SELECT id,category_id,slug,title,host,proto,interval_s,packets,
		        spacing_ms,timeout_ms,public,enabled,family,port,pin_ip
		   FROM targets WHERE id=?`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	ts, err := scanTargets(rows)
	if err != nil {
		return nil, err
	}
	if len(ts) == 0 {
		return nil, sql.ErrNoRows
	}
	return ts[0], nil
}

// UpdateCategory renames a category or changes its visibility. Only the
// fields given are changed.
func (s *Store) UpdateCategory(id int64, fr, en *string, public *bool) error {
	if fr != nil {
		if strings.TrimSpace(*fr) == "" {
			return fmt.Errorf("the French label cannot be empty")
		}
		if _, err := s.cfg.Exec(`UPDATE categories SET menu_fr=? WHERE id=?`, *fr, id); err != nil {
			return err
		}
	}
	if en != nil {
		if strings.TrimSpace(*en) == "" {
			return fmt.Errorf("the English label cannot be empty")
		}
		if _, err := s.cfg.Exec(`UPDATE categories SET menu_en=? WHERE id=?`, *en, id); err != nil {
			return err
		}
	}
	if public != nil {
		if _, err := s.cfg.Exec(`UPDATE categories SET public=? WHERE id=?`, b2i(*public), id); err != nil {
			return err
		}
	}
	s.notifyTargets()
	return nil
}

// DeleteCategory refuses to remove a category that still holds targets,
// rather than silently orphaning their measurements.
func (s *Store) DeleteCategory(id int64) error {
	var n int
	if err := s.cfg.QueryRow(`SELECT COUNT(*) FROM targets WHERE category_id=?`, id).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return fmt.Errorf("this category still holds %d target(s): move or delete them first", n)
	}
	res, err := s.cfg.Exec(`DELETE FROM categories WHERE id=?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("category not found")
	}
	s.notifyTargets()
	return nil
}

func (s *Store) CreateCategory(slug, fr, en string, public bool) (int64, error) {
	p := 0
	if public {
		p = 1
	}
	res, err := s.cfg.Exec(
		`INSERT INTO categories(slug,menu_fr,menu_en,public) VALUES(?,?,?,?)`,
		slug, fr, en, p)
	if err != nil {
		return 0, err
	}
	s.notifyTargets()
	return res.LastInsertId()
}

// migrateTCPPorts moves the port of older TCP targets ("host:443") into
// its own column, so that host and port can be edited separately.
func migrateTCPPorts(db *sql.DB) {
	rows, err := db.Query(`SELECT id,host FROM targets WHERE proto='tcp' AND port=0`)
	if err != nil {
		return
	}
	type fix struct {
		id         int64
		host, port string
	}
	var list []fix
	for rows.Next() {
		var f fix
		var h string
		if rows.Scan(&f.id, &h) == nil {
			if host, port, err := net.SplitHostPort(h); err == nil {
				f.host, f.port = host, port
				list = append(list, f)
			}
		}
	}
	rows.Close()
	for _, f := range list {
		n, err := strconv.Atoi(f.port)
		if err != nil || n < 1 || n > 65535 {
			continue
		}
		db.Exec(`UPDATE targets SET host=?, port=? WHERE id=?`, f.host, n, f.id)
	}
}

// checkTarget validates the settings shared by creation and update. The
// burst must fit in the interval, with a margin.
func checkTarget(t *Target) error {
	if t.Family != 0 && t.Family != 4 && t.Family != 6 {
		return fmt.Errorf("family must be 0 (auto), 4 or 6")
	}
	if t.Proto == "tcp" {
		if t.Port == 0 {
			if _, p, err := net.SplitHostPort(t.Host); err == nil {
				if n, err := strconv.Atoi(p); err == nil {
					t.Port = n
				}
			}
		}
		if t.Port < 1 || t.Port > 65535 {
			return fmt.Errorf("a TCP target needs a port between 1 and 65535")
		}
	}
	if t.Packets < 3 || t.Packets > 50 {
		return fmt.Errorf("packets must be between 3 and 50")
	}
	switch t.IntervalS {
	case 30, 60, 300, 600:
	default:
		return fmt.Errorf("interval_s must be 30, 60, 300 or 600")
	}
	if int64(t.Packets*t.SpacingMs+t.TimeoutMs) > t.IntervalS*1000*3/4 {
		return fmt.Errorf("packets x spacing_ms + timeout_ms exceeds 75%% of the interval")
	}
	return nil
}

// UpdateTarget saves an existing target, including its visibility.
func (s *Store) UpdateTarget(t *Target) error {
	if err := checkTarget(t); err != nil {
		return err
	}
	_, err := s.cfg.Exec(
		`UPDATE targets SET category_id=?,title=?,host=?,proto=?,family=?,interval_s=?,packets=?,
		        spacing_ms=?,timeout_ms=?,public=?,enabled=?,port=?,pin_ip=? WHERE id=?`,
		t.CategoryID, t.Title, t.Host, t.Proto, t.Family, t.IntervalS, t.Packets,
		t.SpacingMs, t.TimeoutMs, b2i(t.Public), b2i(t.Enabled), t.Port, t.PinIP, t.ID)
	s.notifyTargets()
	return err
}

func (s *Store) CreateTarget(t *Target) (int64, error) {
	if err := checkTarget(t); err != nil {
		return 0, err
	}
	res, err := s.cfg.Exec(
		`INSERT INTO targets(category_id,slug,title,host,proto,interval_s,packets,
		                     spacing_ms,timeout_ms,public,enabled,created_at,family,port,pin_ip)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.CategoryID, t.Slug, t.Title, t.Host, t.Proto, t.IntervalS, t.Packets,
		t.SpacingMs, t.TimeoutMs, b2i(t.Public), b2i(t.Enabled), time.Now().Unix(), t.Family, t.Port, t.PinIP)
	if err != nil {
		return 0, err
	}
	s.notifyTargets()
	return res.LastInsertId()
}

func (s *Store) DeleteTarget(id int64) error {
	_, err := s.cfg.Exec(`DELETE FROM targets WHERE id=?`, id)
	s.notifyTargets()
	return err
}

// notifyTargets reveille le cache des cibles de la sonde apres une
// modification, sans jamais bloquer l'appelant.
func (s *Store) notifyTargets() {
	s.gen.Add(1)
	select {
	case s.targetsChanged <- struct{}{}:
	default:
	}
}

func (s *Store) Setting(key, def string) string {
	var v string
	if err := s.cfg.QueryRow(`SELECT value FROM settings WHERE key=?`, key).Scan(&v); err != nil {
		return def
	}
	return v
}

func (s *Store) SetSetting(key, value string) error {
	_, err := s.cfg.Exec(
		`INSERT INTO settings(key,value) VALUES(?,?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

func ptr(v float64) *float64 { return &v }

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ------------------------------------------------------------- ingestion

type Measurement struct {
	TargetID int64     `json:"target_id"`
	ProbeID  int64     `json:"probe_id"`
	TS       int64     `json:"ts"` // epoch secondes
	Sent     int       `json:"sent"`
	Lost     int       `json:"lost"`
	RTTus    []float64 `json:"rtt_us"`
	Err      string    `json:"err,omitempty"`
	IP       string    `json:"ip,omitempty"` // adresse reellement sondee
}

// TargetAddresses returns the addresses each target was actually probed
// at recently. A name that answers from several addresses means the graph
// mixes different machines.
func (s *Store) TargetAddresses(since int64) map[int64][]string {
	out := map[int64][]string{}
	rows, err := s.mx.Query(`SELECT target_id,ip FROM target_addresses
	                         WHERE last_seen>=? ORDER BY last_seen DESC`, since)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var ip string
		if rows.Scan(&id, &ip) == nil {
			out[id] = append(out[id], ip)
		}
	}
	return out
}

// TargetError keeps the last reason a target could not be measured, so the
// back-office can say why instead of only showing 100 % loss.
type TargetError struct {
	TS  int64  `json:"ts"`
	Err string `json:"err"`
}

func (s *Store) SetTargetError(id int64, ts int64, msg string) {
	if msg == "" {
		s.mxw.Exec(`DELETE FROM target_errors WHERE target_id=?`, id)
		return
	}
	s.mxw.Exec(`INSERT INTO target_errors(target_id,ts,err) VALUES(?,?,?)
	            ON CONFLICT(target_id) DO UPDATE SET ts=excluded.ts, err=excluded.err`, id, ts, msg)
}

func (s *Store) TargetErrors() map[int64]TargetError {
	out := map[int64]TargetError{}
	rows, err := s.mx.Query(`SELECT target_id,ts,err FROM target_errors`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var e TargetError
		if rows.Scan(&id, &e.TS, &e.Err) == nil {
			out[id] = e
		}
	}
	return out
}

func (s *Store) Record(m Measurement) error {
	sk := NewSketch()
	var sum, sumsq float64
	for _, v := range m.RTTus {
		sk.Add(v)
		sum += v
		sumsq += v * v
	}
	_, err := s.mxw.Exec(
		`INSERT INTO samples(target_id,probe_id,bucket,sent,lost,cnt,
		                     min_us,max_us,sum_us,sumsq_us,sketch)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?)
		 ON CONFLICT(target_id,probe_id,bucket) DO NOTHING`,
		m.TargetID, m.ProbeID, m.TS, m.Sent, m.Lost, len(m.RTTus),
		sk.Min(), sk.Max(), sum, sumsq, sk.MarshalBinary())
	if err != nil {
		return err
	}

	loss := 0.0
	if m.Sent > 0 {
		loss = float64(m.Lost) * 100 / float64(m.Sent)
	}
	_, err = s.mxw.Exec(
		`INSERT INTO live(target_id,probe_id,ts,med_us,p95_us,loss_pct)
		 VALUES(?,?,?,?,?,?)
		 ON CONFLICT(target_id,probe_id) DO UPDATE SET
		   ts=excluded.ts, med_us=excluded.med_us,
		   p95_us=excluded.p95_us, loss_pct=excluded.loss_pct`,
		m.TargetID, m.ProbeID, m.TS, sk.Quantile(0.5), sk.Quantile(0.95), loss)
	return err
}

// --------------------------------------------------------------- rollups

type aggKey struct {
	target, probe, bucket int64
}

type agg struct {
	sent, lost, cnt      int64
	min, max, sum, sumsq float64
	sk                   *Sketch
}

// RollupRange recalcule les buckets de dst a partir de src sur la
// plage [from, to]. Le recalcul integral est volontaire : il est
// idempotent, il rattrape une sonde en retard qui envoie des donnees
// anciennes, et il coute quelques millisecondes.
func (s *Store) RollupRange(src, dst string, secs, from, to int64) error {
	rows, err := s.mx.Query(fmt.Sprintf(
		`SELECT target_id,probe_id,bucket,sent,lost,cnt,min_us,max_us,
		        sum_us,sumsq_us,sketch
		   FROM %s WHERE bucket>=? AND bucket<?`, src), from, to)
	if err != nil {
		return err
	}

	acc := map[aggKey]*agg{}
	for rows.Next() {
		var k aggKey
		var sent, lost, cnt int64
		var mn, mx, sum, sumsq float64
		var blob []byte
		if err := rows.Scan(&k.target, &k.probe, &k.bucket, &sent, &lost, &cnt,
			&mn, &mx, &sum, &sumsq, &blob); err != nil {
			rows.Close()
			return err
		}
		k.bucket = k.bucket - mod(k.bucket, secs)
		a := acc[k]
		if a == nil {
			a = &agg{min: math.Inf(1), sk: NewSketch()}
			acc[k] = a
		}
		a.sent += sent
		a.lost += lost
		a.cnt += cnt
		a.sum += sum
		a.sumsq += sumsq
		if cnt > 0 {
			if mn < a.min {
				a.min = mn
			}
			if mx > a.max {
				a.max = mx
			}
		}
		a.sk.Merge(UnmarshalSketch(blob))
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	tx, err := s.mxw.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(fmt.Sprintf(
		`DELETE FROM %s WHERE bucket>=? AND bucket<?`, dst),
		from-mod(from, secs), to); err != nil {
		return err
	}
	stmt, err := tx.Prepare(fmt.Sprintf(
		`INSERT INTO %s(target_id,probe_id,bucket,sent,lost,cnt,
		                min_us,max_us,sum_us,sumsq_us,sketch)
		 VALUES(?,?,?,?,?,?,?,?,?,?,?)`, dst))
	if err != nil {
		return err
	}
	defer stmt.Close()
	for k, a := range acc {
		mn := a.min
		if math.IsInf(mn, 1) {
			mn = 0
		}
		if _, err := stmt.Exec(k.target, k.probe, k.bucket, a.sent, a.lost,
			a.cnt, mn, a.max, a.sum, a.sumsq, a.sk.MarshalBinary()); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func mod(a, b int64) int64 {
	if b <= 0 {
		return 0
	}
	m := a % b
	if m < 0 {
		m += b
	}
	return m
}

// RollupTick fait tourner toute la cascade sur une fenetre glissante
// suffisamment large pour absorber les retards de collecte.
func (s *Store) RollupTick(now int64) error {
	windows := []int64{2 * 3600, 6 * 3600, 3 * 86400, 3 * 86400}
	for i := 0; i+1 < len(cascade); i++ {
		src, dst := cascade[i], cascade[i+1]
		from := now - windows[i]
		if err := s.RollupRange(src.table, dst.table, dst.secs, from, now+dst.secs); err != nil {
			return fmt.Errorf("rollup %s->%s: %w", src.table, dst.table, err)
		}
	}
	return nil
}

func (s *Store) Purge(now int64) error {
	s.PurgeTraceroutes(now)
	for _, g := range cascade {
		if g.keep == 0 {
			continue
		}
		if _, err := s.mxw.Exec(fmt.Sprintf(
			`DELETE FROM %s WHERE bucket < ?`, g.table), now-g.keep); err != nil {
			return err
		}
	}
	return nil
}

// --------------------------------------------------------------- lecture

type Series struct {
	TargetID int64      `json:"target_id"`
	ProbeID  int64      `json:"probe_id"`
	Table    string     `json:"resolution"`
	StepS    int64      `json:"step_s"`
	T        []int64    `json:"t"`
	Med      []*float64 `json:"med"`
	P05      []*float64 `json:"p05"`
	P25      []*float64 `json:"p25"`
	P75      []*float64 `json:"p75"`
	P95      []*float64 `json:"p95"`
	Loss     []*float64 `json:"loss"`
}

// pickTable choisit la resolution la plus fine qui produit un nombre
// de points raisonnable pour la fenetre demandee. C'est ce qui rend
// le zoom transparent : l'appelant ne connait que from et to.
func pickTable(span int64) (string, int64) {
	switch {
	case span <= 6*3600:
		return "samples", 0
	case span <= 4*86400:
		return "roll_1m", 60
	case span <= 45*86400:
		return "roll_5m", 300
	case span <= 500*86400:
		return "roll_1h", 3600
	default:
		return "roll_1d", 86400
	}
}

type seriesRow struct {
	bucket, sent, lost, cnt int64
	sk                      *Sketch
}

// Series lit une serie a la resolution adaptee a la fenetre. maxPoints
// (0 = sans limite) fusionne les points consecutifs pour alleger la
// reponse, sans fausser les percentiles.
func (s *Store) Series(targetID, probeID, from, to int64, maxPoints int) (*Series, error) {
	table, step := pickTable(to - from)
	out := &Series{TargetID: targetID, ProbeID: probeID, Table: table, StepS: step,
		T: []int64{}, Med: []*float64{}, P05: []*float64{}, P25: []*float64{},
		P75: []*float64{}, P95: []*float64{}, Loss: []*float64{}}

	rows, err := s.mx.Query(fmt.Sprintf(
		`SELECT bucket,sent,lost,cnt,sketch FROM %s
		  WHERE target_id=? AND probe_id=? AND bucket>=? AND bucket<=?
		  ORDER BY bucket`, table), targetID, probeID, from, to)
	if err != nil {
		return nil, err
	}
	var list []seriesRow
	for rows.Next() {
		var r seriesRow
		var blob []byte
		if err := rows.Scan(&r.bucket, &r.sent, &r.lost, &r.cnt, &blob); err != nil {
			rows.Close()
			return nil, err
		}
		r.sk = UnmarshalSketch(blob)
		list = append(list, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	list, factor := downsample(list, maxPoints)
	if factor > 1 && out.StepS > 0 {
		out.StepS *= int64(factor)
	}
	for _, r := range list {
		loss := 0.0
		if r.sent > 0 {
			loss = float64(r.lost) * 100 / float64(r.sent)
		}
		out.T = append(out.T, r.bucket)
		if r.cnt == 0 || r.sk.Count() == 0 {
			// JSON n'a pas de NaN : une valeur absente est un null,
			// que le traceur cote client dessine comme un trou.
			out.Med = append(out.Med, nil)
			out.P05 = append(out.P05, nil)
			out.P25 = append(out.P25, nil)
			out.P75 = append(out.P75, nil)
			out.P95 = append(out.P95, nil)
		} else {
			out.Med = append(out.Med, ptr(r.sk.Quantile(0.50)/1000))
			out.P05 = append(out.P05, ptr(r.sk.Quantile(0.05)/1000))
			out.P25 = append(out.P25, ptr(r.sk.Quantile(0.25)/1000))
			out.P75 = append(out.P75, ptr(r.sk.Quantile(0.75)/1000))
			out.P95 = append(out.P95, ptr(r.sk.Quantile(0.95)/1000))
		}
		out.Loss = append(out.Loss, ptr(loss))
	}
	return out, nil
}

type LiveRow struct {
	TargetID int64   `json:"target_id"`
	ProbeID  int64   `json:"probe_id"`
	TS       int64   `json:"ts"`
	MedMs    float64 `json:"med_ms"`
	P95Ms    float64 `json:"p95_ms"`
	LossPct  float64 `json:"loss_pct"`
}

func (s *Store) Live() ([]LiveRow, error) {
	rows, err := s.mx.Query(
		`SELECT target_id,probe_id,ts,med_us,p95_us,loss_pct FROM live`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LiveRow
	for rows.Next() {
		var r LiveRow
		var med, p95 float64
		if err := rows.Scan(&r.TargetID, &r.ProbeID, &r.TS, &med, &p95, &r.LossPct); err != nil {
			return nil, err
		}
		r.MedMs, r.P95Ms = med/1000, p95/1000
		out = append(out, r)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TargetID < out[j].TargetID })
	return out, rows.Err()
}

// Charts alimente les pages "palmares" : pire mediane, pire perte.
type ChartRow struct {
	TargetID int64   `json:"target_id"`
	Title    string  `json:"title"`
	MedMs    float64 `json:"med_ms"`
	LossPct  float64 `json:"loss_pct"`
}

func (s *Store) Charts(from, to int64, limit int) ([]ChartRow, error) {
	rows, err := s.mx.Query(
		`SELECT target_id, SUM(sent), SUM(lost), SUM(sum_us), SUM(cnt)
		   FROM roll_1m WHERE bucket>=? AND bucket<=?
		  GROUP BY target_id`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ChartRow
	for rows.Next() {
		var id, sent, lost, cnt int64
		var sum float64
		if err := rows.Scan(&id, &sent, &lost, &sum, &cnt); err != nil {
			return nil, err
		}
		r := ChartRow{TargetID: id}
		if cnt > 0 {
			r.MedMs = sum / float64(cnt) / 1000
		}
		if sent > 0 {
			r.LossPct = float64(lost) * 100 / float64(sent)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		if t, err := s.TargetByID(out[i].TargetID); err == nil {
			out[i].Title = t.Title
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].LossPct != out[j].LossPct {
			return out[i].LossPct > out[j].LossPct
		}
		return out[i].MedMs > out[j].MedMs
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
