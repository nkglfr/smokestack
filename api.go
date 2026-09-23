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
	mux.HandleFunc("PATCH /api/v1/admin/targets/{id}", a.auth(a.targetsPatch))
	mux.HandleFunc("DELETE /api/v1/admin/targets/{id}", a.auth(a.targetsDelete))
	mux.HandleFunc("POST /api/v1/admin/targets/{id}/check", a.auth(a.targetsCheck))
	mux.HandleFunc("POST /api/v1/admin/categories", a.auth(a.categoriesPost))
	mux.HandleFunc("PATCH /api/v1/admin/categories/{id}", a.auth(a.categoriesPatch))
	mux.HandleFunc("DELETE /api/v1/admin/categories/{id}", a.auth(a.categoriesDelete))
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
	// "-30h" is a shorthand for "now-30h".
	if strings.HasPrefix(s, "-") || strings.HasPrefix(s, "+") {
		s = "now" + s
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

// authenticated reports whether the request comes from the API token or a
// valid back-office session. Private targets are only ever served to these.
func (a *API) authenticated(r *http.Request) bool {
	bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	if bearer != "" && a.token != "" &&
		subtle.ConstantTimeCompare([]byte(bearer), []byte(a.token)) == 1 {
		return true
	}
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return false
	}
	_, err = a.store.Session(c.Value)
	return err == nil
}

// publicIDs is the set of targets an anonymous visitor may see. Rebuilt
// only when targets or categories change.
var publicIDs struct {
	sync.Mutex
	gen int64
	set map[int64]bool
	ok  bool
}

func (a *API) publicTargetIDs() map[int64]bool {
	gen := a.store.gen.Load()
	publicIDs.Lock()
	defer publicIDs.Unlock()
	if publicIDs.ok && publicIDs.gen == gen {
		return publicIDs.set
	}
	set := map[int64]bool{}
	if cats, err := a.store.Tree(true); err == nil {
		for _, c := range cats {
			for _, t := range c.Targets {
				set[t.ID] = true
			}
		}
		publicIDs.set, publicIDs.gen, publicIDs.ok = set, gen, true
	}
	return set
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
	publicOnly := !a.authenticated(r)
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
		writeErr(w, 400, "missing target parameter")
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
		writeErr(w, 400, "the requested time window is empty")
		return
	}

	// La visibilite de la cible est verifiee avant toute lecture ou
	// reponse depuis le cache.
	if t, err := a.store.TargetByID(targetID); err != nil ||
		(!t.Public && !a.authenticated(r)) {
		writeErr(w, 404, "target not found")
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
	if !a.authenticated(r) {
		visible := a.publicTargetIDs()
		kept := rows[:0]
		for _, row := range rows {
			if visible[row.TargetID] {
				kept = append(kept, row)
			}
		}
		rows = kept
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
		writeErr(w, 500, "streaming not supported")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")

	// Private targets are left out unless the caller is authenticated.
	var visible map[int64]bool
	if !a.authenticated(r) {
		visible = a.publicTargetIDs()
	}
	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()
	for {
		rows, err := a.store.Live()
		if err == nil {
			if visible != nil {
				kept := rows[:0]
				for _, row := range rows {
					if visible[row.TargetID] {
						kept = append(kept, row)
					}
				}
				rows = kept
			}
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
		writeErr(w, 404, "unknown probe")
		return
	}
	// Une sonde sans jeton ne peut rien envoyer par HTTP : sans cette
	// regle, n'importe qui pourrait injecter de fausses mesures dans les
	// graphes publics. La sonde locale passe par le socket Unix.
	if hash == "" || tok == "" ||
		subtle.ConstantTimeCompare([]byte(hashToken(tok)), []byte(hash)) != 1 {
		writeErr(w, 401, "invalid probe token")
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
		writeErr(w, 400, "mode must be s3, local or hybrid")
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
	t.Public = true
	if err := json.NewDecoder(r.Body).Decode(t); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if t.Host == "" || t.Title == "" || t.CategoryID == 0 {
		writeErr(w, 400, "title, host and category_id are required")
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
	// Measure it right away instead of waiting a whole interval.
	checkRequests.Push(id)
	writeJSON(w, t)
}

func (a *API) targetsDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, 400, "invalid identifier")
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
		writeErr(w, 400, "slug and menu_fr are required")
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

// targetsPatch updates an existing target. Only the fields present in the
// body change; "public": false keeps a target out of the public site while
// it keeps being measured.
func (a *API) targetsPatch(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, 400, "invalid identifier")
		return
	}
	t, err := a.store.TargetByID(id)
	if err != nil {
		writeErr(w, 404, "target not found")
		return
	}
	var in struct {
		Title, Host, Proto                    *string
		Family, Packets, SpacingMs, TimeoutMs *int
		IntervalS                             *int64
		Public, Enabled                       *bool
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	set := func(dst *string, v *string) {
		if v != nil {
			*dst = *v
		}
	}
	set(&t.Title, in.Title)
	set(&t.Host, in.Host)
	set(&t.Proto, in.Proto)
	if in.Family != nil {
		t.Family = *in.Family
	}
	if in.Packets != nil {
		t.Packets = *in.Packets
	}
	if in.IntervalS != nil {
		t.IntervalS = *in.IntervalS
	}
	if in.SpacingMs != nil {
		t.SpacingMs = *in.SpacingMs
	}
	if in.TimeoutMs != nil {
		t.TimeoutMs = *in.TimeoutMs
	}
	if in.Public != nil {
		t.Public = *in.Public
	}
	if in.Enabled != nil {
		t.Enabled = *in.Enabled
	}
	if err := a.store.UpdateTarget(t); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	go ovCache.build()
	writeJSON(w, t)
}

// targetsCheck asks the probe to measure a target right away.
func (a *API) targetsCheck(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, 400, "invalid identifier")
		return
	}
	if _, err := a.store.TargetByID(id); err != nil {
		writeErr(w, 404, "target not found")
		return
	}
	checkRequests.Push(id)
	writeJSON(w, map[string]any{"queued": true})
}

func (a *API) categoriesPatch(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, 400, "invalid identifier")
		return
	}
	var in struct {
		MenuFR, MenuEN *string
		Public         *bool
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := a.store.UpdateCategory(id, in.MenuFR, in.MenuEN, in.Public); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	go ovCache.build()
	writeJSON(w, map[string]any{"ok": true})
}

func (a *API) categoriesDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, 400, "invalid identifier")
		return
	}
	if err := a.store.DeleteCategory(id); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	go ovCache.build()
	w.WriteHeader(http.StatusNoContent)
}
