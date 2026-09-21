package main

import (
	"compress/gzip"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// L'archive ecrit les echantillons bruts en NDJSON compresse, un
// fichier par heure et par sonde. A la cloture de l'heure le fichier
// est scelle, enregistre dans raw_chunks puis pousse sur S3 si une
// cible est configuree. Sinon il reste local et c'est la rotation par
// capacite qui borne l'occupation disque.

type S3Config struct {
	Endpoint     string `json:"endpoint"`
	Bucket       string `json:"bucket"`
	Prefix       string `json:"prefix"`
	Region       string `json:"region"`
	AccessKey    string `json:"access_key"`
	SecretKey    string `json:"secret_key"`
	PathStyle    bool   `json:"path_style"`
	StorageClass string `json:"storage_class"`
}

type LocalConfig struct {
	QuotaBytes     int64   `json:"quota_bytes"`
	HighWatermark  float64 `json:"high_watermark"`
	LowWatermark   float64 `json:"low_watermark"`
	KeepLocalHours int64   `json:"keep_local_hours"`
}

type StorageConfig struct {
	Mode  string      `json:"mode"` // s3 | local | hybrid
	S3    S3Config    `json:"s3"`
	Local LocalConfig `json:"local"`
}

func (c *StorageConfig) s3Enabled() bool {
	return c.Mode != "local" && c.S3.Bucket != "" && c.S3.Endpoint != ""
}

type rawRecord struct {
	TS       int64     `json:"ts"`
	TargetID int64     `json:"target_id"`
	ProbeID  int64     `json:"probe_id"`
	Host     string    `json:"host"`
	Sent     int       `json:"sent"`
	Lost     int       `json:"lost"`
	RTTus    []float64 `json:"rtt_us"`
	Err      string    `json:"err,omitempty"`
}

type Archive struct {
	store     *Store
	cfg       StorageConfig
	spool     string
	probeSlug string

	mu      sync.Mutex
	hourKey string
	path    string
	file    *os.File
	gz      *gzip.Writer
	rows    int64
	tsMin   int64
	tsMax   int64
}

func NewArchive(store *Store, dir, probeSlug string, cfg StorageConfig) (*Archive, error) {
	spool := filepath.Join(dir, "spool")
	if err := os.MkdirAll(spool, 0o700); err != nil {
		return nil, err
	}
	return &Archive{store: store, cfg: cfg, spool: spool, probeSlug: probeSlug}, nil
}

func (a *Archive) Config() StorageConfig {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg
}

func (a *Archive) SetConfig(c StorageConfig) {
	a.mu.Lock()
	a.cfg = c
	a.mu.Unlock()
}

func (a *Archive) Append(m Measurement, host string) {
	a.mu.Lock()
	defer a.mu.Unlock()

	key := time.Unix(m.TS, 0).UTC().Format("2006-01-02-15")
	if a.gz != nil && key != a.hourKey {
		a.sealLocked()
	}
	if a.gz == nil {
		if err := a.openLocked(key); err != nil {
			log.Printf("archive open: %v", err)
			return
		}
	}
	rec := rawRecord{TS: m.TS, TargetID: m.TargetID, ProbeID: m.ProbeID,
		Host: host, Sent: m.Sent, Lost: m.Lost, RTTus: m.RTTus, Err: m.Err}
	b, err := json.Marshal(&rec)
	if err != nil {
		return
	}
	a.gz.Write(b)
	a.gz.Write([]byte("\n"))
	a.rows++
	if a.tsMin == 0 || m.TS < a.tsMin {
		a.tsMin = m.TS
	}
	if m.TS > a.tsMax {
		a.tsMax = m.TS
	}
}

func (a *Archive) openLocked(key string) error {
	name := fmt.Sprintf("%s.%s.ndjson.gz", a.probeSlug, key)
	p := filepath.Join(a.spool, name)
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return err
	}
	a.file, a.path, a.hourKey = f, p, key
	a.gz = gzip.NewWriter(f)
	a.rows, a.tsMin, a.tsMax = 0, 0, 0
	return nil
}

func (a *Archive) sealLocked() {
	if a.gz == nil {
		return
	}
	a.gz.Close()
	a.file.Close()
	path, rows, tsMin, tsMax, key := a.path, a.rows, a.tsMin, a.tsMax, a.hourKey
	a.gz, a.file, a.path, a.hourKey = nil, nil, "", ""

	st, err := os.Stat(path)
	if err != nil {
		return
	}
	s3Key := fmt.Sprintf("raw/probe=%s/date=%s/h=%s.ndjson.gz",
		a.probeSlug, key[:10], key[11:])
	res, err := a.store.mx.Exec(
		`INSERT INTO raw_chunks(probe_slug,ts_min,ts_max,location,local_path,
		                        s3_key,bytes,rows,sealed_at)
		 VALUES(?,?,?,'local',?,?,?,?,?)`,
		a.probeSlug, tsMin, tsMax, path, s3Key, st.Size(), rows, time.Now().Unix())
	if err != nil {
		log.Printf("archive seal: %v", err)
		return
	}
	id, _ := res.LastInsertId()
	if a.cfg.s3Enabled() {
		a.store.mx.Exec(
			`INSERT INTO upload_queue(chunk_id,next_try_at) VALUES(?,?)`,
			id, time.Now().Unix())
	}
}

// Rotate ferme le fichier courant s'il appartient a une heure revolue.
func (a *Archive) Rotate() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.gz == nil {
		return
	}
	if a.hourKey != time.Now().UTC().Format("2006-01-02-15") {
		a.sealLocked()
	}
}

// --------------------------------------------------------------- uploads

func (a *Archive) drainUploads() {
	cfg := a.Config()
	if !cfg.s3Enabled() {
		return
	}
	rows, err := a.store.mx.Query(
		`SELECT q.chunk_id, c.local_path, c.s3_key, q.attempts
		   FROM upload_queue q JOIN raw_chunks c ON c.id=q.chunk_id
		  WHERE q.next_try_at<=? ORDER BY q.chunk_id LIMIT 8`,
		time.Now().Unix())
	if err != nil {
		return
	}
	type job struct {
		id       int64
		path     string
		key      string
		attempts int
	}
	var jobs []job
	for rows.Next() {
		var j job
		if err := rows.Scan(&j.id, &j.path, &j.key, &j.attempts); err == nil {
			jobs = append(jobs, j)
		}
	}
	rows.Close()

	for _, j := range jobs {
		body, err := os.ReadFile(j.path)
		if err != nil {
			a.store.mx.Exec(`DELETE FROM upload_queue WHERE chunk_id=?`, j.id)
			continue
		}
		if err := s3Put(cfg.S3, j.key, body); err != nil {
			backoff := int64(30) << minInt(j.attempts, 6)
			a.store.mx.Exec(
				`UPDATE upload_queue SET attempts=attempts+1,next_try_at=?,last_error=?
				  WHERE chunk_id=?`, time.Now().Unix()+backoff, err.Error(), j.id)
			log.Printf("upload s3 %s: %v", j.key, err)
			continue
		}
		a.store.mx.Exec(`UPDATE raw_chunks SET location='both' WHERE id=?`, j.id)
		a.store.mx.Exec(`DELETE FROM upload_queue WHERE chunk_id=?`, j.id)
	}
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

// s3Put écrit un objet via une signature AWS SigV4 calculée à la main,
// ce qui évite d'embarquer un SDK complet pour un seul appel.
func s3Put(c S3Config, key string, body []byte) error {
	host := strings.TrimPrefix(strings.TrimPrefix(c.Endpoint, "https://"), "http://")
	host = strings.TrimSuffix(host, "/")
	var uri string
	if c.PathStyle {
		uri = "/" + c.Bucket + "/" + joinKey(c.Prefix, key)
	} else {
		host = c.Bucket + "." + host
		uri = "/" + joinKey(c.Prefix, key)
	}

	now := time.Now().UTC()
	amzDate := now.Format("20060102T150405Z")
	dateOnly := now.Format("20060102")
	payload := sha256Hex(body)

	hdrs := map[string]string{
		"host":                 host,
		"x-amz-content-sha256": payload,
		"x-amz-date":           amzDate,
	}
	if c.StorageClass != "" {
		hdrs["x-amz-storage-class"] = c.StorageClass
	}
	names := make([]string, 0, len(hdrs))
	for k := range hdrs {
		names = append(names, k)
	}
	sort.Strings(names)
	var canon strings.Builder
	for _, n := range names {
		canon.WriteString(n)
		canon.WriteString(":")
		canon.WriteString(strings.TrimSpace(hdrs[n]))
		canon.WriteString("\n")
	}
	signed := strings.Join(names, ";")

	canonReq := strings.Join([]string{
		"PUT", uri, "", canon.String(), signed, payload,
	}, "\n")
	scope := dateOnly + "/" + c.Region + "/s3/aws4_request"
	toSign := strings.Join([]string{
		"AWS4-HMAC-SHA256", amzDate, scope, sha256Hex([]byte(canonReq)),
	}, "\n")

	k := hmacSHA256([]byte("AWS4"+c.SecretKey), []byte(dateOnly))
	k = hmacSHA256(k, []byte(c.Region))
	k = hmacSHA256(k, []byte("s3"))
	k = hmacSHA256(k, []byte("aws4_request"))
	sig := hex.EncodeToString(hmacSHA256(k, []byte(toSign)))

	req, err := http.NewRequest("PUT", "https://"+host+uri, strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	for n, v := range hdrs {
		if n != "host" {
			req.Header.Set(n, v)
		}
	}
	req.Header.Set("Authorization", fmt.Sprintf(
		"AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		c.AccessKey, scope, signed, sig))
	req.ContentLength = int64(len(body))

	resp, err := (&http.Client{Timeout: 120 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("s3 %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

func joinKey(prefix, key string) string {
	prefix = strings.Trim(prefix, "/")
	if prefix == "" {
		return key
	}
	return prefix + "/" + key
}

// ---------------------------------------------------- rotation par quota

type StorageReport struct {
	Mode         string  `json:"mode"`
	LocalBytes   int64   `json:"local_bytes"`
	QuotaBytes   int64   `json:"quota_bytes"`
	DiskTotal    int64   `json:"disk_total"`
	DiskFree     int64   `json:"disk_free"`
	DiskUsedPct  float64 `json:"disk_used_pct"`
	Chunks       int64   `json:"chunks"`
	PendingUpl   int64   `json:"pending_uploads"`
	Degraded     bool    `json:"degraded"`
	ProjectedDay float64 `json:"projected_days"`
}

func diskUsage(path string) (total, free int64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0
	}
	return int64(st.Blocks) * int64(st.Bsize), int64(st.Bavail) * int64(st.Bsize)
}

func (a *Archive) Report() StorageReport {
	cfg := a.Config()
	r := StorageReport{Mode: cfg.Mode, QuotaBytes: cfg.Local.QuotaBytes}
	a.store.mx.QueryRow(
		`SELECT COALESCE(SUM(bytes),0), COUNT(*) FROM raw_chunks
		  WHERE local_path IS NOT NULL AND location IN ('local','both')`).
		Scan(&r.LocalBytes, &r.Chunks)
	a.store.mx.QueryRow(`SELECT COUNT(*) FROM upload_queue`).Scan(&r.PendingUpl)

	r.DiskTotal, r.DiskFree = diskUsage(a.spool)
	if r.DiskTotal > 0 {
		r.DiskUsedPct = float64(r.DiskTotal-r.DiskFree) / float64(r.DiskTotal)
		r.Degraded = float64(r.DiskFree)/float64(r.DiskTotal) < 0.02
	}
	// Projection : combien de jours tient le quota au rythme observe.
	var bytes24h int64
	a.store.mx.QueryRow(
		`SELECT COALESCE(SUM(bytes),0) FROM raw_chunks WHERE sealed_at > ?`,
		time.Now().Unix()-86400).Scan(&bytes24h)
	if bytes24h > 0 && cfg.Local.QuotaBytes > 0 {
		r.ProjectedDay = float64(cfg.Local.QuotaBytes) / float64(bytes24h)
	}
	return r
}

// Enforce applique le quota : au-dessus du seuil haut, on supprime les
// chunks locaux les plus anciens jusqu'a repasser sous le seuil bas.
// Les chunks deja pousses sur S3 partent en premier puisqu'ils restent
// recuperables.
func (a *Archive) Enforce() {
	cfg := a.Config()
	rep := a.Report()

	quota := cfg.Local.QuotaBytes
	high, low := cfg.Local.HighWatermark, cfg.Local.LowWatermark
	if high <= 0 {
		high = 0.85
	}
	if low <= 0 || low >= high {
		low = high - 0.15
	}

	overQuota := quota > 0 && rep.LocalBytes > quota
	overDisk := rep.DiskTotal > 0 && rep.DiskUsedPct > high
	keepBefore := int64(0)
	if cfg.s3Enabled() && cfg.Local.KeepLocalHours > 0 {
		keepBefore = time.Now().Unix() - cfg.Local.KeepLocalHours*3600
	}

	if !overQuota && !overDisk && keepBefore == 0 {
		return
	}

	rows, err := a.store.mx.Query(
		`SELECT id, local_path, bytes, location, ts_max FROM raw_chunks
		  WHERE local_path IS NOT NULL AND location IN ('local','both')
		  ORDER BY CASE location WHEN 'both' THEN 0 ELSE 1 END, ts_min`)
	if err != nil {
		return
	}
	type chunk struct {
		id    int64
		path  string
		bytes int64
		loc   string
		tsMax int64
	}
	var chunks []chunk
	for rows.Next() {
		var c chunk
		if err := rows.Scan(&c.id, &c.path, &c.bytes, &c.loc, &c.tsMax); err == nil {
			chunks = append(chunks, c)
		}
	}
	rows.Close()

	local := rep.LocalBytes
	target := quota
	if target > 0 {
		target = int64(float64(quota) * (low / high))
	}
	var evicted int64

	for _, c := range chunks {
		expired := keepBefore > 0 && c.loc == "both" && c.tsMax < keepBefore
		need := (quota > 0 && local > target) || overDisk
		if !expired && !need {
			break
		}
		if err := os.Remove(c.path); err != nil && !os.IsNotExist(err) {
			continue
		}
		if c.loc == "both" {
			a.store.mx.Exec(
				`UPDATE raw_chunks SET location='s3', local_path=NULL WHERE id=?`, c.id)
		} else {
			a.store.mx.Exec(`DELETE FROM raw_chunks WHERE id=?`, c.id)
		}
		local -= c.bytes
		evicted += c.bytes
		if overDisk {
			_, free := diskUsage(a.spool)
			if rep.DiskTotal > 0 &&
				float64(rep.DiskTotal-free)/float64(rep.DiskTotal) < low {
				overDisk = false
			}
		}
	}

	if evicted > 0 {
		day := time.Now().Unix() / 86400
		a.store.mx.Exec(
			`INSERT INTO storage_stats(day,local_bytes,s3_bytes,evicted_bytes)
			 VALUES(?,?,0,?)
			 ON CONFLICT(day) DO UPDATE SET
			   local_bytes=excluded.local_bytes,
			   evicted_bytes=storage_stats.evicted_bytes+excluded.evicted_bytes`,
			day, local, evicted)
		log.Printf("rotation: %d octets liberes", evicted)
	}
}

// Loop fait tourner la rotation, les uploads et le scellement horaire.
func (a *Archive) Loop(stop <-chan struct{}) {
	tick := time.NewTicker(30 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-stop:
			a.mu.Lock()
			a.sealLocked()
			a.mu.Unlock()
			return
		case <-tick.C:
			a.Rotate()
			a.drainUploads()
			a.Enforce()
		}
	}
}
