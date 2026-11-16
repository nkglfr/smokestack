package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
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
	ID   int64  `json:"id"`
	Slug string `json:"slug"`
	// AddrCount: combien d'adresses différentes ont répondu sur 24 h.
	// Plusieurs adresses = service en répartition de charge, donc des
	// mesures qui mélangent des machines différentes. La liste elle-même
	// ne sort jamais publiquement : elle décrit l'intérieur d'un service
	// tiers et peut être longue. Addresses n'est rempli que pour un
	// appelant authentifié. PinIP: adresse figée.
	AddrCount int      `json:"addr_count,omitempty"`
	Addresses []string `json:"addresses,omitempty"`
	PinIP     string   `json:"pin_ip,omitempty"`
	// PinFamily : 4 ou 6, pour que la mention publique nomme la famille
	// alors que l'adresse elle-meme y est masquee.
	PinFamily int      `json:"pin_family,omitempty"`
	Title     string   `json:"title"`
	Host      string   `json:"host"`
	Proto     string   `json:"proto"`
	Interval  int64    `json:"interval_s"`
	Featured  bool     `json:"featured"`
	Public    bool     `json:"public"`
	Status    string   `json:"status"`
	MedMs     *float64 `json:"med_ms"`
	LossPct   *float64 `json:"loss_pct"`
	BaseMs    *float64 `json:"base_ms"`
	Ratio     *float64 `json:"ratio"`
	Since     *int64   `json:"since"`
	Hours     []string `json:"hours"`
	Spark     Spark    `json:"spark"`
	// Family, and the twin measuring the same service in the other one.
	// The pair is the point: two families cross different networks, and the
	// comparison is what the split exists for.
	Family    int    `json:"family,omitempty"`
	TwinSlug  string `json:"twin_slug,omitempty"`
	TwinTitle string `json:"twin_title,omitempty"`
	// Maint : maintenance programmee en cours sur cette cible. Presente,
	// elle explique un trou ou une degradation voulue, et le statut passe
	// a "maint" quand la mesure elle-meme est arretee, pour que la cible
	// ne soit pas comptee parmi les defauts.
	Maint *MaintNote `json:"maint,omitempty"`
}

// MaintNote est ce qu'une page publique dit d'une maintenance : de quoi
// comprendre l'arret, sans le detail interne.
type MaintNote struct {
	Title     string `json:"title"`
	Note      string `json:"note,omitempty"`
	StartsAt  int64  `json:"starts_at"`
	EndsAt    int64  `json:"ends_at"`
	StopProbe bool   `json:"stop_probe"`
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

// classify turns one window of measurements into a state. The thresholds
// come from the target, which falls back to the instance defaults: a link
// known to be poor should not shout every day, and a link that matters can
// be watched more closely than the rest.
// maskIP keeps enough of an address to say which network it is in, and no
// more. It is what a visitor sees of a pinned address: the notice exists to
// explain that the figures describe one machine, which does not require
// naming that machine to everyone who opens the page.
//
// An IPv4 keeps its first two octets, an IPv6 its first two groups — the
// same order of magnitude of disclosure in each family, roughly the
// operator's network rather than the host.
func maskIP(addr string) string {
	ip := net.ParseIP(addr)
	if ip == nil {
		return ""
	}
	if v4 := ip.To4(); v4 != nil {
		return fmt.Sprintf("%d.%d.XXX.XXX", v4[0], v4[1])
	}
	return fmt.Sprintf("%x:%x:XXXX:XXXX::",
		int(ip[0])<<8|int(ip[1]), int(ip[2])<<8|int(ip[3]))
}

// maskHost masks a host that is a literal address, and leaves a name alone.
// A name is not an address: it is what says which service the page is about,
// and hiding it would only make the page useless.
func maskHost(h string) string {
	if net.ParseIP(h) == nil {
		return h
	}
	return maskIP(h)
}

// familyOf says whether an address is IPv4 or IPv6, for a notice that names
// the family. Read from the address rather than from the target's setting,
// because a target on automatic has no setting and the address does.
func familyOf(addr string) int {
	ip := net.ParseIP(addr)
	switch {
	case ip == nil:
		return 0
	case ip.To4() != nil:
		return 4
	}
	return 6
}

func classify(sent, lost int64, med, base float64, th Thresholds) string {
	if sent == 0 {
		return "nodata"
	}
	th = th.orDefaults()
	loss := float64(lost) * 100 / float64(sent)
	switch {
	case loss > th.LossCrit:
		return "crit"
	case loss > th.LossWarn:
		return "warn"
	case base > 0 && med > base*th.LatFactor && med-base > latMinDeltaMs:
		return "warn"
	}
	return "ok"
}

// latMinDeltaMs keeps a proportional rise from raising anything when the
// absolute move is negligible: 40 % more than 0.2 ms is still 0.2 ms.
const latMinDeltaMs = 1.0

// Thresholds is what separates ok, warn and crit for one target. A zero
// field means "follow the instance default", so a target created before
// these existed behaves exactly as it did.
type Thresholds struct {
	LossWarn  float64 `json:"loss_warn,omitempty"`  // percent
	LossCrit  float64 `json:"loss_crit,omitempty"`  // percent
	LatFactor float64 `json:"lat_factor,omitempty"` // median over baseline
}

// DefaultThresholds are the values used before they could be set, kept as
// the shipped defaults so nothing moves for an instance that never touches
// them.
func DefaultThresholds() Thresholds {
	return Thresholds{LossWarn: 0.4, LossCrit: 3, LatFactor: 1.4}
}

func (t Thresholds) orDefaults() Thresholds {
	d := DefaultThresholds()
	if t.LossWarn <= 0 {
		t.LossWarn = d.LossWarn
	}
	if t.LossCrit <= 0 {
		t.LossCrit = d.LossCrit
	}
	if t.LatFactor <= 1 {
		t.LatFactor = d.LatFactor
	}
	if t.LossCrit < t.LossWarn {
		t.LossCrit = t.LossWarn
	}
	return t
}

// merge resolves one target's thresholds against the instance defaults.
func (t Thresholds) merge(base Thresholds) Thresholds {
	if t.LossWarn <= 0 {
		t.LossWarn = base.LossWarn
	}
	if t.LossCrit <= 0 {
		t.LossCrit = base.LossCrit
	}
	if t.LatFactor <= 1 {
		t.LatFactor = base.LatFactor
	}
	return t.orDefaults()
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

type ovRow struct {
	b, sent, lost int64
	sk            *Sketch
}

// scanRows lit une table d'agregats pour toutes les cibles en une seule
// requete, sur la connexion de lecture.
func (s *Store) scanRows(table string, probeID, from int64) (map[int64][]ovRow, error) {
	rows, err := s.mx.Query(`SELECT target_id,bucket,sent,lost,sketch FROM `+table+
		` WHERE probe_id=? AND bucket>=? ORDER BY target_id,bucket`, probeID, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]ovRow{}
	for rows.Next() {
		var id int64
		var r ovRow
		var blob []byte
		if err := rows.Scan(&id, &r.b, &r.sent, &r.lost, &blob); err != nil {
			return nil, err
		}
		r.sk = UnmarshalSketch(blob)
		out[id] = append(out[id], r)
	}
	return out, rows.Err()
}

func rowMed(r ovRow) float64 {
	if r.sk.Count() == 0 {
		return 0
	}
	return r.sk.Quantile(0.5) / 1000
}

// Overview calcule l'etat de toutes les cibles publiques. Trois requetes
// en tout : reference 7 jours (roll_1h), 24 h par tranches de 5 min
// (roll_5m) et 2 h a la minute (roll_1m) pour l'etat courant et le
// debut du defaut.
func (s *Store) Overview(probeID int64, now int64, publicOnly bool) (*Overview, error) {
	addrs := s.TargetAddresses(now - 24*3600)
	site := s.Site()
	siteTh := site.Thresholds.orDefaults()
	mask := site.MaskAddresses
	cats, err := s.Tree(publicOnly)
	if err != nil {
		return nil, err
	}
	feat := s.featured()
	baseRows, err := s.scanRows("roll_1h", probeID, now-7*86400)
	if err != nil {
		return nil, err
	}
	dayRows, err := s.scanRows("roll_5m", probeID, now-86400)
	if err != nil {
		return nil, err
	}
	minRows, err := s.scanRows("roll_1m", probeID, now-7200)
	if err != nil {
		return nil, err
	}

	out := &Overview{GeneratedAt: now, Categories: []*OverviewCategory{},
		Counts: map[string]int{"targets": 0, "ok": 0, "warn": 0, "crit": 0, "nodata": 0, "maint": 0}}
	maint := s.ActiveMaintenances(now)
	dayStart := now - 86400
	slot0 := dayStart - dayStart%1800 + 1800
	sparkStart := now - 3*3600

	for _, c := range cats {
		oc := &OverviewCategory{ID: c.ID, Slug: c.Slug, MenuFR: c.MenuFR, MenuEN: c.MenuEN,
			Targets: []*OverviewTarget{}}
		for _, t := range c.Targets {
			if !t.Enabled {
				continue
			}
			ot := &OverviewTarget{ID: t.ID, Slug: t.Slug, Title: t.Title, Host: t.Host, Proto: t.Proto,
				Interval: t.IntervalS, Featured: feat[t.ID], Public: t.Public,
				AddrCount: len(addrs[t.ID]), PinIP: t.PinIP, Family: t.Family,
				PinFamily: familyOf(t.PinIP),
				Hours:     make([]string, 48)}
			// The target measuring the same service in the other family, so
			// each page can offer the comparison the pair exists for. Only
			// when that one is visible to this caller.
			if twin, err := s.FindTwin(t); err == nil && (!publicOnly || twin.Public) {
				ot.TwinSlug, ot.TwinTitle = twin.Slug, twin.Title
			}

			var base, day, cur ovAgg
			for _, r := range baseRows[t.ID] {
				base.add(r.sent, r.lost, r.sk)
			}
			slots := make([]ovAgg, 48)
			sparks := make([]ovAgg, 36)
			for _, r := range dayRows[t.ID] {
				day.add(r.sent, r.lost, r.sk)
				if i := int((r.b - slot0 + 1800) / 1800); i >= 0 && i < 48 {
					slots[i].add(r.sent, r.lost, r.sk)
				}
				if r.b >= sparkStart {
					if i := int((r.b - sparkStart) / 300); i >= 0 && i < 36 {
						sparks[i].add(r.sent, r.lost, r.sk)
					}
				}
			}
			mins := minRows[t.ID]
			// The "current" window is 15 minutes, but never less than three
			// passes: a target measured every 30 minutes would otherwise
			// always look as if it had no data.
			window := int64(900)
			if w := 3 * t.IntervalS; w > window {
				window = w
			}
			for _, r := range mins {
				if r.b >= now-window {
					cur.add(r.sent, r.lost, r.sk)
				}
			}
			// The target's own thresholds, or the instance defaults.
			th := Thresholds{LossWarn: t.LossWarn, LossCrit: t.LossCrit,
				LatFactor: t.LatFactor}.merge(siteTh)
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
			ot.Status = classify(cur.sent, cur.lost, m, bm, th)
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
				ot.Hours[i] = classify(slots[i].sent, slots[i].lost, sm, bm, th)
			}
			for i := range sparks {
				ot.Spark.T = append(ot.Spark.T, sparkStart+int64(i)*300)
				ot.Spark.Med = append(ot.Spark.Med, sparks[i].q(0.5))
				ot.Spark.P25 = append(ot.Spark.P25, sparks[i].q(0.25))
				ot.Spark.P75 = append(ot.Spark.P75, sparks[i].q(0.75))
			}
			// Debut du defaut : a la minute sur 2 h, puis par tranches de
			// 5 min si le defaut est plus ancien.
			if ot.Status == "warn" || ot.Status == "crit" {
				since, okRun, open := now, 0, true
				for i := len(mins) - 1; i >= 0 && open; i-- {
					if classify(mins[i].sent, mins[i].lost, rowMed(mins[i]), bm, th) == "ok" {
						if okRun++; okRun >= 2 {
							open = false
						}
						continue
					}
					okRun, since = 0, mins[i].b
				}
				if open {
					rows := dayRows[t.ID]
					for i := len(rows) - 1; i >= 0; i-- {
						if rows[i].b >= since {
							continue
						}
						if classify(rows[i].sent, rows[i].lost, rowMed(rows[i]), bm, th) == "ok" {
							break
						}
						since = rows[i].b
					}
				}
				ot.Since = &since
			}
			// Une maintenance annoncee explique ce que le graphe montre.
			// Quand elle arrete la mesure, le statut le dit plutot que
			// d'afficher un defaut ou un trou inexplique.
			if m, ok := maint[t.ID]; ok {
				ot.Maint = &MaintNote{Title: m.Title, Note: m.Note,
					StartsAt: m.StartsAt, EndsAt: m.EndsAt, StopProbe: m.StopProbe}
				if m.StopProbe {
					ot.Status = "maint"
					ot.Since = nil
				}
			}
			out.Counts["targets"]++
			out.Counts[ot.Status]++
			if !publicOnly {
				ot.Addresses = addrs[t.ID]
			} else {
				// A visitor is told that the address is fixed, and which
				// family it belongs to, without being handed the address.
				ot.PinIP = maskIP(ot.PinIP)
				if mask {
					ot.Host = maskHost(ot.Host)
				}
				if t.HideHost {
					// A public target whose address stays private: the graph
					// is shown, the host is not, and neither is anything that
					// would give it away — the pinned address included, which
					// used to go out in full.
					ot.Host = ""
					ot.AddrCount = 0
					ot.PinIP, ot.PinFamily = "", 0
				}
			}
			oc.Targets = append(oc.Targets, ot)
		}
		if len(oc.Targets) > 0 {
			out.Categories = append(out.Categories, oc)
		}
	}
	return out, nil
}

// ------------------------------------------------------------------ routes

// La vue d'ensemble est calculee en tache de fond et servie depuis la
// memoire, deja compressee : une requete ne declenche jamais le calcul
// (sauf la toute premiere apres le demarrage).
type overviewCache struct {
	mu     sync.Mutex // serialise les calculs
	data   atomic.Pointer[[]byte]
	gz     atomic.Pointer[[]byte]
	ov     atomic.Pointer[Overview] // the same build, decoded, for the badges
	probe  int64
	store  *Store
	builds atomic.Int64
	lastMs atomic.Int64
}

var ovCache overviewCache

func (c *overviewCache) build() error {
	// Defensive: the cache is rebuilt in the background from several
	// handlers, and a nil store must never take the service down.
	if c == nil || c.store == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	start := time.Now()
	ov, err := c.store.Overview(c.probe, time.Now().Unix(), true)
	if err != nil {
		return err
	}
	b, err := json.Marshal(ov)
	if err != nil {
		return err
	}
	var zb bytes.Buffer
	zw, _ := gzip.NewWriterLevel(&zb, gzip.BestSpeed)
	zw.Write(b)
	zw.Close()
	z := zb.Bytes()
	c.ov.Store(ov)
	c.data.Store(&b)
	c.gz.Store(&z)
	c.builds.Add(1)
	c.lastMs.Store(time.Since(start).Milliseconds())
	return nil
}

func (c *overviewCache) Loop(stop <-chan struct{}) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()
	for {
		if err := c.build(); err != nil {
			log.Printf("overview: %v", err)
		}
		select {
		case <-stop:
			return
		case <-t.C:
		}
	}
}

func (a *API) OverviewRoutes(mux *http.ServeMux) {
	ovCache.store, ovCache.probe = a.store, a.probeID
	mux.HandleFunc("GET /api/v1/overview", a.overview)
	mux.HandleFunc("GET /api/v1/admin/featured", a.need(RoleViewer, a.featuredGet))
	mux.HandleFunc("PUT /api/v1/admin/featured", a.need(RoleEditor, a.featuredPut))
}

func (a *API) overview(w http.ResponseWriter, r *http.Request) {
	// Authenticated callers can ask for everything, private targets
	// included; the cached payload stays public-only.
	// A share link renders the normal detail page, so it needs an overview
	// holding its one target — whatever that target's visibility is.
	if id, ok := a.shareGrant(r); ok && !a.authenticated(r) {
		ov, err := a.store.Overview(a.probeID, time.Now().Unix(), false)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		keep := &Overview{GeneratedAt: ov.GeneratedAt, Counts: map[string]int{}}
		for _, c := range ov.Categories {
			for _, t := range c.Targets {
				if t.ID != id {
					continue
				}
				// Whoever holds a share link is a third party: the same
				// masking applies as on a public page.
				t.PinIP = maskIP(t.PinIP)
				if a.store.Site().MaskAddresses {
					t.Host = maskHost(t.Host)
				}
				if tg, err := a.store.TargetByID(id); err == nil && tg.HideHost {
					t.Host, t.AddrCount, t.Addresses = "", 0, nil
					t.PinIP, t.PinFamily = "", 0
				} else {
					t.Addresses = nil
				}
				keep.Categories = []*OverviewCategory{{ID: c.ID, Slug: c.Slug,
					MenuFR: c.MenuFR, MenuEN: c.MenuEN, Targets: []*OverviewTarget{t}}}
				keep.Counts["targets"] = 1
				keep.Counts[t.Status] = 1
			}
		}
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Robots-Tag", "noindex")
		writeJSON(w, keep)
		return
	}
	if r.URL.Query().Get("all") != "" && a.authenticated(r) {
		ov, err := a.store.Overview(a.probeID, time.Now().Unix(), false)
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		writeJSON(w, ov)
		return
	}
	if ovCache.data.Load() == nil {
		if err := ovCache.build(); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		// build() declines rather than fails when it has no store to read
		// from, returning a nil error without filling the cache — so the
		// pointer can still be nil here, and dereferencing it below was the
		// one way its "a nil store must never take the service down" guard
		// could take a request down.
		if ovCache.data.Load() == nil || ovCache.gz.Load() == nil {
			writeErr(w, 503, "the overview is not ready yet")
			return
		}
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=15")
	w.Header().Set("Vary", "Accept-Encoding")
	if acceptsGzip(r) {
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(*ovCache.gz.Load())
		return
	}
	w.Write(*ovCache.data.Load())
}

// featuredPut fixe la liste des cibles critiques, affichees en tete de
// l'accueil meme quand tout va bien.
func (a *API) featuredPut(w http.ResponseWriter, r *http.Request, u *User) {
	var ids []int64
	if err := json.NewDecoder(r.Body).Decode(&ids); err != nil {
		writeErr(w, 400, "a list of identifiers is expected")
		return
	}
	b, _ := json.Marshal(ids)
	a.store.SetSetting("featured_targets", string(b))
	go ovCache.build()
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
