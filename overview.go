package main

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// Vue d'ensemble de l'accueil, calculee en un seul appel a partir de
// roll_1m (24 h) et roll_1h (reference sur 7 jours).
//
// Regles d'etat, identiques partout (accueil, barre 24 h, rail) :
//   - crit : perte > 3 %
//   - warn : perte > 0,4 %, ou mediane > 1,4 x la reference ET au moins
//     1 ms au-dessus (sans ce plancher, une cible a 0,1 ms passerait en
//     alerte pour 0,05 ms de gigue)
//   - nodata : aucun paquet envoye sur la periode

type Spark struct {
	T   []int64    `json:"t"`
	Med []*float64 `json:"med"`
	P25 []*float64 `json:"p25"`
	P75 []*float64 `json:"p75"`
}

type OverviewTarget struct {
	ID       int64    `json:"id"`
	Title    string   `json:"title"`
	Host     string   `json:"host"`
	Proto    string   `json:"proto"`
	Interval int64    `json:"interval_s"`
	Featured bool     `json:"featured"`
	Status   string   `json:"status"`
	MedMs    *float64 `json:"med_ms"`
	LossPct  *float64 `json:"loss_pct"`
	BaseMs   *float64 `json:"base_ms"`
	Ratio    *float64 `json:"ratio"`
	Since    *int64   `json:"since"`
	Hours    []string `json:"hours"`
	Spark    Spark    `json:"spark"`
}

type OverviewCategory struct {
	ID      int64             `json:"id"`
	Slug    string            `json:"slug"`
	MenuFR  string            `json:"menu_fr"`
	MenuEN  string            `json:"menu_en"`
	Targets []*OverviewTarget `json:"targets"`
}

type Overview struct {
	GeneratedAt int64               `json:"generated_at"`
	Categories  []*OverviewCategory `json:"categories"`
	Counts      map[string]int      `json:"counts"`
}

func classify(sent, lost int64, med, base float64) string {
	if sent == 0 {
		return "nodata"
	}
	loss := float64(lost) * 100 / float64(sent)
	switch {
	case loss > 3:
		return "crit"
	case loss > 0.4:
		return "warn"
	case base > 0 && med > base*1.4 && med-base > 1:
		return "warn"
	}
	return "ok"
}

type ovAgg struct {
	sent, lost int64
	sk         *Sketch
}

func (g *ovAgg) add(sent, lost int64, sk *Sketch) {
	if g.sk == nil {
		g.sk = NewSketch()
	}
	g.sent += sent
	g.lost += lost
	g.sk.Merge(sk)
}

func (g *ovAgg) q(p float64) *float64 {
	if g.sk == nil || g.sk.Count() == 0 {
		return nil
	}
	v := g.sk.Quantile(p) / 1000
	return &v
}

func (s *Store) featured() map[int64]bool {
	var ids []int64
	json.Unmarshal([]byte(s.Setting("featured_targets", "[]")), &ids)
	out := map[int64]bool{}
	for _, id := range ids {
		out[id] = true
	}
	return out
}

func (s *Store) Overview(probeID int64, now int64) (*Overview, error) {
	cats, err := s.Tree(true)
	if err != nil {
		return nil, err
	}
	feat := s.featured()
	out := &Overview{GeneratedAt: now, Categories: []*OverviewCategory{},
		Counts: map[string]int{"targets": 0, "ok": 0, "warn": 0, "crit": 0, "nodata": 0}}
	dayStart := now - 86400
	slot0 := dayStart - dayStart%1800 + 1800 // 48 demi-heures alignees

	for _, c := range cats {
		oc := &OverviewCategory{ID: c.ID, Slug: c.Slug, MenuFR: c.MenuFR, MenuEN: c.MenuEN,
			Targets: []*OverviewTarget{}}
		for _, t := range c.Targets {
			if !t.Enabled {
				continue
			}
			ot := &OverviewTarget{ID: t.ID, Title: t.Title, Host: t.Host, Proto: t.Proto,
				Interval: t.IntervalS, Featured: feat[t.ID], Hours: make([]string, 48)}

			// Reference : mediane sur 7 jours (roll_1h), a defaut 24 h.
			var base ovAgg
			if rows, err := s.mx.Query(`SELECT sent,lost,sketch FROM roll_1h
				 WHERE target_id=? AND probe_id=? AND bucket>=?`, t.ID, probeID, now-7*86400); err == nil {
				for rows.Next() {
					var sent, lost int64
					var blob []byte
					if rows.Scan(&sent, &lost, &blob) == nil {
						base.add(sent, lost, UnmarshalSketch(blob))
					}
				}
				rows.Close()
			}

			type minute struct {
				b, sent, lost int64
				sk            *Sketch
			}
			var mins []minute
			rows, err := s.mx.Query(`SELECT bucket,sent,lost,sketch FROM roll_1m
				 WHERE target_id=? AND probe_id=? AND bucket>=? ORDER BY bucket`, t.ID, probeID, dayStart)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var m minute
				var blob []byte
				if rows.Scan(&m.b, &m.sent, &m.lost, &blob) == nil {
					m.sk = UnmarshalSketch(blob)
					mins = append(mins, m)
				}
			}
			rows.Close()

			var day, cur ovAgg
			slots := make([]ovAgg, 48)
			sparks := make([]ovAgg, 36)
			sparkStart := now - 3*3600
			for _, m := range mins {
				day.add(m.sent, m.lost, m.sk)
				if m.b >= now-900 {
					cur.add(m.sent, m.lost, m.sk)
				}
				if i := int((m.b - slot0 + 1800) / 1800); i >= 0 && i < 48 {
					slots[i].add(m.sent, m.lost, m.sk)
				}
				if m.b >= sparkStart {
					if i := int((m.b - sparkStart) / 300); i >= 0 && i < 36 {
						sparks[i].add(m.sent, m.lost, m.sk)
					}
				}
			}
			baseMed := base.q(0.5)
			if baseMed == nil {
				baseMed = day.q(0.5)
			}
			bm := 0.0
			if baseMed != nil {
				bm = *baseMed
			}
			med := cur.q(0.5)
			m := 0.0
			if med != nil {
				m = *med
			}
			ot.Status = classify(cur.sent, cur.lost, m, bm)
			ot.MedMs, ot.BaseMs = med, baseMed
			if cur.sent > 0 {
				l := float64(cur.lost) * 100 / float64(cur.sent)
				ot.LossPct = &l
			}
			if med != nil && bm > 0 {
				r := m / bm
				ot.Ratio = &r
			}
			for i := range slots {
				sm := 0.0
				if v := slots[i].q(0.5); v != nil {
					sm = *v
				}
				ot.Hours[i] = classify(slots[i].sent, slots[i].lost, sm, bm)
			}
			for i := range sparks {
				ot.Spark.T = append(ot.Spark.T, sparkStart+int64(i)*300)
				ot.Spark.Med = append(ot.Spark.Med, sparks[i].q(0.5))
				ot.Spark.P25 = append(ot.Spark.P25, sparks[i].q(0.25))
				ot.Spark.P75 = append(ot.Spark.P75, sparks[i].q(0.75))
			}
			// Debut du defaut : on remonte minute par minute jusqu'a
			// trouver deux minutes conformes consecutives.
			if ot.Status == "warn" || ot.Status == "crit" {
				okRun := 0
				since := now
				for i := len(mins) - 1; i >= 0; i-- {
					mm := 0.0
					if mins[i].sk.Count() > 0 {
						mm = mins[i].sk.Quantile(0.5) / 1000
					}
					if classify(mins[i].sent, mins[i].lost, mm, bm) == "ok" {
						okRun++
						if okRun >= 2 {
							break
						}
						continue
					}
					okRun = 0
					since = mins[i].b
				}
				ot.Since = &since
			}
			out.Counts["targets"]++
			out.Counts[ot.Status]++
			oc.Targets = append(oc.Targets, ot)
		}
		if len(oc.Targets) > 0 {
			out.Categories = append(out.Categories, oc)
		}
	}
	return out, nil
}

// ------------------------------------------------------------------ routes

type overviewCache struct {
	mu   sync.Mutex
	at   time.Time
	data []byte
}

var ovCache overviewCache

func (a *API) OverviewRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/overview", a.overview)
	mux.HandleFunc("GET /api/v1/admin/featured", a.need(RoleViewer, a.featuredGet))
	mux.HandleFunc("PUT /api/v1/admin/featured", a.need(RoleEditor, a.featuredPut))
}

func (a *API) overview(w http.ResponseWriter, r *http.Request) {
	ovCache.mu.Lock()
	defer ovCache.mu.Unlock()
	if ovCache.data == nil || time.Since(ovCache.at) > 20*time.Second {
		ov, err := a.store.Overview(a.probeID, time.Now().Unix())
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		b, err := json.Marshal(ov)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		ovCache.data, ovCache.at = b, time.Now()
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=20")
	w.Write(ovCache.data)
}

// featuredPut fixe la liste des cibles critiques, affichees en tete de
// l'accueil meme quand tout va bien.
func (a *API) featuredPut(w http.ResponseWriter, r *http.Request, u *User) {
	var ids []int64
	if err := json.NewDecoder(r.Body).Decode(&ids); err != nil {
		writeErr(w, 400, "liste d'identifiants attendue")
		return
	}
	b, _ := json.Marshal(ids)
	a.store.SetSetting("featured_targets", string(b))
	ovCache.mu.Lock()
	ovCache.data = nil
	ovCache.mu.Unlock()
	a.store.Audit(u, clientIP(r), "featured_update", "targets", "")
	writeJSON(w, ids)
}

func (a *API) featuredGet(w http.ResponseWriter, r *http.Request, u *User) {
	ids := []int64{}
	for id := range a.store.featured() {
		ids = append(ids, id)
	}
	writeJSON(w, ids)
}
