package main

import (
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type API struct {
	store     *Store
	archive   *Archive
	fed       *Federation
	token     string
	probeID   int64
	limiter   *attemptLimiter
	i18n      *I18n
	asn       *ASNService
	upd       *Updater
	writer    *Writer
	probeMode string

	setupMu   sync.Mutex
	setupCode string
	setupFile string
}

func (a *API) Routes(mux *http.ServeMux) {
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	})
	mux.HandleFunc("GET /api/v1/tree", a.tree)
	mux.HandleFunc("GET /api/v1/series", a.series)
	mux.HandleFunc("GET /api/v1/charts", a.charts)
	mux.HandleFunc("GET /api/v1/events", a.events)
	mux.HandleFunc("GET /api/v1/live", a.live)
	mux.HandleFunc("GET /api/v1/site", a.siteGet)
	mux.HandleFunc("POST /api/v1/ingest", a.ingest)

	mux.HandleFunc("PUT /api/v1/admin/site", a.auth(a.sitePut))

	mux.HandleFunc("GET /api/v1/admin/storage", a.auth(a.storageGet))
	mux.HandleFunc("PUT /api/v1/admin/storage", a.auth(a.storagePut))
	mux.HandleFunc("GET /api/v1/admin/targets", a.auth(a.targetsGet))
	mux.HandleFunc("POST /api/v1/admin/targets", a.auth(a.targetsPost))
	mux.HandleFunc("DELETE /api/v1/admin/targets/{id}", a.auth(a.targetsDelete))
	mux.HandleFunc("POST /api/v1/admin/categories", a.auth(a.categoriesPost))
	mux.HandleFunc("POST /api/v1/admin/events", a.auth(a.eventsPost))
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	if err := enc.Encode(v); err != nil {
		log.Printf("encode: %v", err)
	}
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

// parseTime accepte un epoch en secondes, une date RFC3339, ou une
// expression relative du type now-3h / now-45d. C'est ce qui rend les
// permaliens lisibles et partageables.
func parseTime(s string, def int64) int64 {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	if strings.HasPrefix(s, "now") {
		rest := strings.TrimPrefix(s, "now")
		if rest == "" {
			return time.Now().Unix()
		}
		sign := int64(1)
		if rest[0] == '-' {
			sign = -1
		} else if rest[0] != '+' {
			return def
		}
		rest = rest[1:]
		if len(rest) < 2 {
			return def
		}
		n, err := strconv.ParseInt(rest[:len(rest)-1], 10, 64)
		if err != nil {
			return def
		}
		var unit int64
		switch rest[len(rest)-1] {
		case 's':
			unit = 1
		case 'm':
			unit = 60
		case 'h':
			unit = 3600
		case 'd':
			unit = 86400
		case 'w':
			unit = 7 * 86400
		case 'y':
			unit = 365 * 86400
		default:
			return def
		}
		return time.Now().Unix() + sign*n*unit
	}
	if v, err := strconv.ParseInt(s, 10, 64); err == nil {
		if v > 1e12 { // millisecondes
			v /= 1000
		}
		return v
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.Unix()
	}
	return def
}

// L'arbre public change rarement : il est servi depuis la memoire et
// recalcule seulement apres une modification de cible ou de categorie
// (ou au plus tard toutes les 60 s).
var treeCache struct {
	sync.Mutex
	gen  int64
	at   time.Time
	data []byte
}

func (a *API) tree(w http.ResponseWriter, r *http.Request) {
	publicOnly := r.Header.Get("Authorization") == ""
	if !publicOnly {
		cats, err := a.store.Tree(false)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		writeJSON(w, cats)
		return
	}
	gen := a.store.gen.Load()
	treeCache.Lock()
	if treeCache.data == nil || treeCache.gen != gen || time.Since(treeCache.at) > time.Minute {
		cats, err := a.store.Tree(true)
		if err != nil {
			treeCache.Unlock()
			writeErr(w, 500, err.Error())
			return
		}
		b, _ := json.Marshal(cats)
		treeCache.data, treeCache.gen, treeCache.at = b, gen, time.Now()
	}
	b := treeCache.data
	treeCache.Unlock()
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=30")
	w.Write(b)
}

func (a *API) series(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	targetID, err := strconv.ParseInt(q.Get("target"), 10, 64)
	if err != nil {
		writeErr(w, 400, "parametre target manquant")
		return
	}
	probeID := a.probeID
	if v := q.Get("probe"); v != "" {
		if p, err := strconv.ParseInt(v, 10, 64); err == nil {
			probeID = p
		}
	}
	now := time.Now().Unix()
	from := parseTime(q.Get("from"), now-3*3600)
	to := parseTime(q.Get("to"), now)
	if to <= from {
		writeErr(w, 400, "la fenetre demandee est vide")
		return
	}

	// La visibilite de la cible est verifiee avant toute lecture ou
	// reponse depuis le cache.
	if t, err := a.store.TargetByID(targetID); err != nil ||
		(!t.Public && r.Header.Get("Authorization") == "") {
		writeErr(w, 404, "cible introuvable")
		return
	}
	points, _ := strconv.Atoi(q.Get("points"))
	if points < 0 || points > 5000 {
		points = 0
	}
	// Arrondi de la fenetre au pas de la resolution : deux visiteurs qui
	// regardent le meme graphe a quelques secondes d'ecart partagent la
	// meme entree de cache.
	_, step := pickTable(to - from)
	quant := step
	if quant < 30 {
		quant = 30
	}
	from, to = from-from%quant, to-to%quant+quant
	key := fmt.Sprintf("%d|%d|%d|%d|%d", targetID, probeID, from, to, points)
	s := seriesCache.get(key)
	if s == nil {
		select {
		case seriesSlots <- struct{}{}:
		case <-r.Context().Done():
			return
		}
		var err error
		s, err = a.store.Series(targetID, probeID, from, to, points)
		<-seriesSlots
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		ttl := time.Duration(min(max(quant, 15), 300)) * time.Second
		seriesCache.put(key, s, ttl)
	}
	// Le cache suit la granularite : inutile de revalider plus souvent
	// que le pas de la serie.
	maxAge := s.StepS
	if maxAge <= 0 {
		maxAge = 15
	}
	if maxAge > 300 {
		maxAge = 300
	}
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", maxAge))
	writeJSON(w, s)
}

func (a *API) charts(w http.ResponseWriter, r *http.Request) {
	now := time.Now().Unix()
	from := parseTime(r.URL.Query().Get("from"), now-24*3600)
	to := parseTime(r.URL.Query().Get("to"), now)
	rows, err := a.store.Charts(from, to, 20)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, rows)
}

type Event struct {
	ID      int64  `json:"id"`
	TSStart int64  `json:"ts_start"`
	TSEnd   *int64 `json:"ts_end"`
	Kind    string `json:"kind"`
	Title   string `json:"title"`
	Body    string `json:"body"`
}

func (a *API) events(w http.ResponseWriter, r *http.Request) {
	now := time.Now().Unix()
	from := parseTime(r.URL.Query().Get("from"), now-7*86400)
	to := parseTime(r.URL.Query().Get("to"), now)
	rows, err := a.store.cfg.Query(
		`SELECT id,ts_start,ts_end,kind,title,COALESCE(body,'')
		   FROM events WHERE public=1 AND ts_start<=? AND (ts_end IS NULL OR ts_end>=?)
		  ORDER BY ts_start`, to, from)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	out := []Event{}
	for rows.Next() {
		var e Event
		var end *int64
		if err := rows.Scan(&e.ID, &e.TSStart, &end, &e.Kind, &e.Title, &e.Body); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		e.TSEnd = end
		out = append(out, e)
	}
	writeJSON(w, out)
}

func (a *API) eventsPost(w http.ResponseWriter, r *http.Request) {
	var e Event
	if err := json.NewDecoder(r.Body).Decode(&e); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if e.TSStart == 0 {
		e.TSStart = time.Now().Unix()
	}
	if e.Kind == "" {
		e.Kind = "incident"
	}
	res, err := a.store.cfg.Exec(
		`INSERT INTO events(ts_start,ts_end,kind,title,body) VALUES(?,?,?,?,?)`,
		e.TSStart, e.TSEnd, e.Kind, e.Title, e.Body)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	e.ID, _ = res.LastInsertId()
	writeJSON(w, e)
}

// live pousse le dernier point de chaque cible en SSE. Le client
// n'a donc jamais besoin de recharger une serie entiere pour rester
// a jour.
func (a *API) live(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, 500, "streaming non supporte")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		rows, err := a.store.Live()
		if err == nil {
			b, _ := json.Marshal(rows)
			fmt.Fprintf(w, "data: %s\n\n", b)
			flusher.Flush()
		}
		select {
		case <-r.Context().Done():
			return
		case <-tick.C:
		}
	}
}

type ingestBody struct {
	Probe string        `json:"probe"`
	Seq   int64         `json:"seq"`
	Batch []Measurement `json:"batch"`
}

func (a *API) ingest(w http.ResponseWriter, r *http.Request) {
	var body ingestBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	var id int64
	var hash string
	err := a.store.cfg.QueryRow(
		`SELECT id, COALESCE(token_hash,'') FROM probes WHERE slug=? AND enabled=1`,
		body.Probe).Scan(&id, &hash)
	if err != nil {
		writeErr(w, 404, "sonde inconnue")
		return
	}
	// Une sonde sans jeton ne peut rien envoyer par HTTP : sans cette
	// regle, n'importe qui pourrait injecter de fausses mesures dans les
	// graphes publics. La sonde locale passe par le socket Unix.
	if hash == "" || tok == "" ||
		subtle.ConstantTimeCompare([]byte(hashToken(tok)), []byte(hash)) != 1 {
		writeErr(w, 401, "jeton de sonde invalide")
		return
	}
	n := 0
	for _, m := range body.Batch {
		m.ProbeID = id
		a.writer.Submit(m, "")
		n++
	}
	a.store.TouchProbe(id)
	writeJSON(w, map[string]any{"ack_seq": body.Seq, "accepted": n})
}

// -------------------------------------------------------------- admin

func (a *API) storageGet(w http.ResponseWriter, r *http.Request) {
	cfg := a.archive.Config()
	// Les secrets ne ressortent jamais de l'API.
	cfg.S3.SecretKey = ""
	writeJSON(w, map[string]any{"config": cfg, "report": a.archive.Report()})
}

func (a *API) storagePut(w http.ResponseWriter, r *http.Request) {
	var c StorageConfig
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if c.Mode != "s3" && c.Mode != "local" && c.Mode != "hybrid" {
		writeErr(w, 400, "mode doit valoir s3, local ou hybrid")
		return
	}
	if c.S3.SecretKey == "" {
		c.S3.SecretKey = a.archive.Config().S3.SecretKey
	}
	b, _ := json.Marshal(c)
	if err := a.store.SetSetting("storage", string(b)); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	a.archive.SetConfig(c)
	c.S3.SecretKey = ""
	writeJSON(w, c)
}

func (a *API) targetsGet(w http.ResponseWriter, r *http.Request) {
	ts, err := a.store.ActiveTargets()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, ts)
}

func (a *API) targetsPost(w http.ResponseWriter, r *http.Request) {
	t := &Target{Proto: "icmp", IntervalS: 60, Packets: 20,
		SpacingMs: 500, TimeoutMs: 2000, Public: true, Enabled: true}
	if err := json.NewDecoder(r.Body).Decode(t); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if t.Host == "" || t.Title == "" || t.CategoryID == 0 {
		writeErr(w, 400, "title, host et category_id sont requis")
		return
	}
	if t.Slug == "" {
		t.Slug = strings.ToLower(strings.ReplaceAll(t.Title, " ", "-"))
	}
	id, err := a.store.CreateTarget(t)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	t.ID = id
	writeJSON(w, t)
}

func (a *API) targetsDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, 400, "identifiant invalide")
		return
	}
	if err := a.store.DeleteTarget(id); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) categoriesPost(w http.ResponseWriter, r *http.Request) {
	var c Category
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if c.Slug == "" || c.MenuFR == "" {
		writeErr(w, 400, "slug et menu_fr sont requis")
		return
	}
	if c.MenuEN == "" {
		c.MenuEN = c.MenuFR
	}
	id, err := a.store.CreateCategory(c.Slug, c.MenuFR, c.MenuEN, true)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	c.ID = id
	writeJSON(w, c)
}

// probeStatus expose l'etat de la chaine de mesure : file d'ecriture,
// mesures ecartees, duree du calcul de la vue d'ensemble.
func (a *API) probeStatus(w http.ResponseWriter, r *http.Request, u *User) {
	var seen *int64
	a.store.cfg.QueryRow(`SELECT last_seen_at FROM probes WHERE id=?`, a.probeID).Scan(&seen)
	writeJSON(w, map[string]any{
		"mode": a.probeMode, "last_seen_at": seen,
		"writer":            a.writer.Stats(),
		"overview_build_ms": ovCache.lastMs.Load(),
		"overview_builds":   ovCache.builds.Load(),
	})
}
