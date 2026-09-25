package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Traceroutes: stored in metrics.db, one row per run. Three kinds:
//   - anomaly:   triggered by the probe when a target leaves its baseline
//     (packet loss or latency jump), to show where the path changed;
//   - reference: taken now and then while the target is healthy, so an
//     anomaly trace can be compared with a known-good path;
//   - manual:    requested from the back-office.

const tracerouteSchema = `
CREATE TABLE IF NOT EXISTS traceroutes (
  id        INTEGER PRIMARY KEY,
  target_id INTEGER NOT NULL,
  probe_id  INTEGER NOT NULL,
  ts        INTEGER NOT NULL,
  kind      TEXT NOT NULL,
  reason    TEXT NOT NULL DEFAULT '',
  family    INTEGER NOT NULL DEFAULT 4,
  dest      TEXT NOT NULL DEFAULT '',
  reached   INTEGER NOT NULL DEFAULT 0,
  hops      TEXT NOT NULL DEFAULT '[]'
);
CREATE INDEX IF NOT EXISTS idx_traceroutes_target ON traceroutes(target_id, ts);
`

type Hop struct {
	TTL   int       `json:"ttl"`
	Addr  string    `json:"addr,omitempty"`
	Name  string    `json:"name,omitempty"`
	ASN   string    `json:"asn,omitempty"`
	Note  string    `json:"note,omitempty"` // !N, !H, !A: the router refused to forward
	RTTms []float64 `json:"rtt_ms"`
	Sent  int       `json:"sent"`
}

type Traceroute struct {
	ID       int64  `json:"id"`
	TargetID int64  `json:"target_id"`
	ProbeID  int64  `json:"probe_id"`
	TS       int64  `json:"ts"`
	Kind     string `json:"kind"`
	Reason   string `json:"reason"`
	Family   int    `json:"family"`
	Dest     string `json:"dest"`
	Reached  bool   `json:"reached"`
	Hops     []Hop  `json:"hops"`
}

// asPath is the sequence of autonomous systems a traceroute crossed, with
// repetitions collapsed. Comparing AS paths rather than addresses is what
// makes topology changes visible without crying at every load-balanced hop:
// two parallel links of the same operator give different addresses but the
// same AS path.
func asPath(tr *Traceroute) []string {
	var out []string
	for _, h := range tr.Hops {
		as := strings.TrimSpace(h.ASN)
		if as == "" {
			continue
		}
		if len(out) == 0 || out[len(out)-1] != as {
			out = append(out, as)
		}
	}
	return out
}

func samePath(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func (s *Store) SaveTraceroute(tr *Traceroute) error {
	hops, _ := json.Marshal(tr.Hops)
	_, err := s.mxw.Exec(`INSERT INTO traceroutes(target_id,probe_id,ts,kind,reason,family,dest,reached,hops)
	                      VALUES(?,?,?,?,?,?,?,?,?)`,
		tr.TargetID, tr.ProbeID, tr.TS, tr.Kind, tr.Reason, tr.Family, tr.Dest, b2i(tr.Reached), string(hops))
	if err == nil && tr.Kind == "reference" {
		s.notePathChange(tr)
	}
	return err
}

// notePathChange compares a fresh healthy path with the previous one and
// records an event when the AS path changed. A transit provider
// decommissioning a peering degrades nothing measurable: latency moves by a
// millisecond, and the change would otherwise go unnoticed.
func (s *Store) notePathChange(tr *Traceroute) {
	prev, err := s.Traceroutes(tr.TargetID, []string{"reference"}, 2)
	if err != nil || len(prev) < 2 {
		return
	}
	now, before := asPath(prev[0]), asPath(prev[1])
	if len(now) == 0 || len(before) == 0 || samePath(now, before) {
		return
	}
	title := ""
	if t, err := s.TargetByID(tr.TargetID); err == nil {
		title = t.Title
	}
	// The title is what gets drawn on the graph, so it stays short; the
	// paths themselves go in the body, which the page lists underneath.
	short := fmt.Sprintf("%s: route changed", title)
	detail := fmt.Sprintf("AS path %s → %s", strings.Join(before, " "), strings.Join(now, " "))
	s.cfg.Exec(`INSERT INTO events(ts_start,kind,title,body,public) VALUES(?,?,?,?,1)`,
		tr.TS, "path", short, detail)
	log.Printf("path change: %s — %s", short, detail)
}

// RecentPathChange reports whether the AS path of a target changed in the
// last window, and what the change was.
func (s *Store) RecentPathChange(targetID int64, since int64) (string, bool) {
	title := ""
	if t, err := s.TargetByID(targetID); err == nil {
		title = t.Title
	}
	if title == "" {
		return "", false
	}
	var detail string
	err := s.cfg.QueryRow(`SELECT title FROM events WHERE kind='path' AND ts_start>=?
	                       AND title LIKE ? ORDER BY ts_start DESC LIMIT 1`,
		since, title+":%").Scan(&detail)
	if err != nil {
		return "", false
	}
	var body string
	s.cfg.QueryRow(`SELECT COALESCE(body,'') FROM events WHERE kind='path' AND title=?
	                ORDER BY ts_start DESC LIMIT 1`, detail).Scan(&body)
	if body != "" {
		detail += " (" + body + ")"
	}
	return detail, true
}

func (s *Store) Traceroutes(targetID int64, kinds []string, limit int) ([]*Traceroute, error) {
	q := `SELECT id,target_id,probe_id,ts,kind,reason,family,dest,reached,hops
	        FROM traceroutes WHERE target_id=?`
	args := []any{targetID}
	if len(kinds) > 0 {
		q += ` AND kind IN (`
		for i, k := range kinds {
			if i > 0 {
				q += ","
			}
			q += "?"
			args = append(args, k)
		}
		q += `)`
	}
	q += ` ORDER BY ts DESC LIMIT ?`
	args = append(args, limit)
	rows, err := s.mx.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Traceroute{}
	for rows.Next() {
		tr := &Traceroute{}
		var reached int
		var hops string
		if err := rows.Scan(&tr.ID, &tr.TargetID, &tr.ProbeID, &tr.TS, &tr.Kind, &tr.Reason,
			&tr.Family, &tr.Dest, &reached, &hops); err != nil {
			return nil, err
		}
		tr.Reached = reached == 1
		json.Unmarshal([]byte(hops), &tr.Hops)
		out = append(out, tr)
	}
	return out, rows.Err()
}

// PurgeTraceroutes keeps 90 days of history.
func (s *Store) PurgeTraceroutes(now int64) {
	s.mxw.Exec(`DELETE FROM traceroutes WHERE ts < ?`, now-90*86400)
}

// ------------------------------------------------------ manual requests

// traceQueue holds traceroutes requested from the back-office until the
// probe picks them up (immediately when embedded, within 5 s otherwise).
type traceQueue struct {
	mu  sync.Mutex
	ids []int64
}

var (
	traceRequests traceQueue
	// checkRequests holds targets waiting for an immediate measurement
	// (just created, or "check now" from the back-office).
	checkRequests traceQueue
)

func (q *traceQueue) Push(id int64) {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, x := range q.ids {
		if x == id {
			return
		}
	}
	if len(q.ids) < 100 {
		q.ids = append(q.ids, id)
	}
}

func (q *traceQueue) Pop() []int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	out := q.ids
	q.ids = nil
	return out
}

// ------------------------------------------------------------------ routes

// HopSeries is the hop count of each traceroute over a window: the visual
// signal of a topology change, alongside the AS path comparison.
type HopPoint struct {
	TS      int64  `json:"ts"`
	Hops    int    `json:"hops"`
	Reached bool   `json:"reached"`
	Kind    string `json:"kind"`
	ASPath  string `json:"as_path,omitempty"`
}

func (s *Store) HopSeries(targetID, since int64) ([]HopPoint, error) {
	trs, err := s.Traceroutes(targetID, nil, 500)
	if err != nil {
		return nil, err
	}
	out := []HopPoint{}
	for i := len(trs) - 1; i >= 0; i-- { // oldest first
		tr := trs[i]
		if tr.TS < since {
			continue
		}
		out = append(out, HopPoint{TS: tr.TS, Hops: len(tr.Hops), Reached: tr.Reached,
			Kind: tr.Kind, ASPath: strings.Join(asPath(tr), " ")})
	}
	return out, nil
}

func (a *API) hopSeries(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.URL.Query().Get("target"), 10, 64)
	if err != nil {
		writeErr(w, 400, "target is required")
		return
	}
	days := 30
	if n, err := strconv.Atoi(r.URL.Query().Get("days")); err == nil && n > 0 && n <= 365 {
		days = n
	}
	pts, err := a.store.HopSeries(id, time.Now().Unix()-int64(days)*86400)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, pts)
}

// ASPathHop : un AS du chemin, avec son nom quand on le connait.
type ASPathHop struct {
	ASN  string `json:"asn"`
	Name string `json:"name,omitempty"`
	Hops int    `json:"hops"` // combien de sauts dans cet AS
}

type ASPathView struct {
	TS      int64       `json:"ts"`
	Reached bool        `json:"reached"`
	Kind    string      `json:"kind"`
	Path    []ASPathHop `json:"path"`
}

// ASRoute is the route as the page shows it: our own network first, the
// destination's network last, and what the traceroute saw in between. The
// two ends never come from the traceroute — a first hop in private space or
// a silent last hop would otherwise drop them, which made the chain look
// wrong for exactly the targets people care about.
type ASRoute struct {
	TS      int64       `json:"ts,omitempty"`
	Kind    string      `json:"kind,omitempty"`
	Reached bool        `json:"reached"`
	Origin  *ASPathHop  `json:"origin,omitempty"`
	Path    []ASPathHop `json:"path"`
	Dest    *ASPathHop  `json:"dest,omitempty"`
	DestIP  string      `json:"dest_ip,omitempty"`
	Gap     bool        `json:"gap"`     // silent hops before the destination
	Pending bool        `json:"pending"` // the destination AS is being looked up
}

// asRouteFrom builds the middle of the route from a traceroute, leaving out
// the ends, and reports whether the path went silent before arriving.
func asRouteFrom(tr *Traceroute, originASN, destASN string) ([]ASPathHop, bool) {
	var mid []ASPathHop
	lastKnown := -1
	for i, h := range tr.Hops {
		as := strings.TrimSpace(h.ASN)
		if as == "" {
			continue
		}
		lastKnown = i
		if as == originASN || as == destASN {
			continue
		}
		if n := len(mid); n > 0 && mid[n-1].ASN == as {
			mid[n-1].Hops++
			continue
		}
		mid = append(mid, ASPathHop{ASN: as, Hops: 1})
	}
	// A gap when the traceroute never reached the destination, or when its
	// last hops answered nothing: the segment before the destination is
	// unknown, and saying so is better than implying a direct link.
	gap := !tr.Reached
	if lastKnown >= 0 && lastKnown < len(tr.Hops)-1 {
		gap = true
	}
	if destASN != "" && lastKnown >= 0 {
		if as := strings.TrimSpace(tr.Hops[lastKnown].ASN); as == destASN {
			gap = !tr.Reached
		}
	}
	return mid, gap
}

// asPathView serves the route from this instance's network to the target's,
// under the same rules as the traceroutes it comes from.
func (a *API) asPathView(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.URL.Query().Get("target"), 10, 64)
	if err != nil {
		writeErr(w, 400, "missing target parameter")
		return
	}
	t, err := a.store.TargetByID(id)
	if err != nil {
		writeErr(w, 404, "target not found")
		return
	}
	authed := a.authenticated(r)
	if shared, ok := a.shareGrant(r); ok && shared == id {
		authed = true
	}
	hideAddr := t.HideHost
	if !authed && (!t.Public || t.HideHost || !a.store.Site().PublicTraceroutes) {
		writeJSON(w, ASRoute{Path: []ASPathHop{}})
		return
	}

	site := a.store.Site()
	out := ASRoute{Path: []ASPathHop{}}

	// One end is us, always, whatever the traceroute shows.
	originASN := ""
	if n, err := normalizeASN(site.ASN); err == nil {
		originASN = "AS" + strings.TrimPrefix(n, "AS")
		name := site.Org
		if name == "" {
			name = site.Title
		}
		out.Origin = &ASPathHop{ASN: originASN, Name: name}
	}

	// The other end is the AS announcing the address actually probed: the
	// pinned one, or the last one a measurement used.
	destIP := t.PinIP
	if destIP == "" {
		if addrs := a.store.TargetAddresses(time.Now().Unix() - 30*86400)[id]; len(addrs) > 0 {
			destIP = addrs[0]
		}
	}
	destASN := ""
	if destIP != "" {
		if !hideAddr {
			out.DestIP = destIP
		}
		if as, known := a.asn.ASNOfIP(destIP); known {
			destASN = as
		} else {
			go a.asn.RefreshIPASN(destIP)
			out.Pending = true
		}
	}
	if destASN != "" {
		hop := ASPathHop{ASN: destASN, Name: t.Title}
		if info, _ := a.asn.Cached(strings.TrimPrefix(destASN, "AS")); info != nil {
			if info.PeeringDB != nil && info.PeeringDB.Name != "" {
				hop.Name = info.PeeringDB.Name
			} else if info.Holder != "" {
				hop.Name = info.Holder
			}
		} else {
			go a.asn.Refresh(strings.TrimPrefix(destASN, "AS"))
		}
		out.Dest = &hop
	}

	// The middle comes from the last healthy traceroute, or the last one of
	// any kind if none is healthy yet.
	trs, err := a.store.Traceroutes(id, []string{"reference"}, 1)
	if err != nil || len(trs) == 0 {
		trs, _ = a.store.Traceroutes(id, nil, 1)
	}
	if len(trs) > 0 {
		tr := trs[0]
		out.TS, out.Kind, out.Reached = tr.TS, tr.Kind, tr.Reached
		out.Path, out.Gap = asRouteFrom(tr, originASN, destASN)
	} else {
		// No traceroute at all: the two ends are still worth showing, with
		// the middle explicitly unknown.
		out.Gap = true
	}
	for i := range out.Path {
		if info, _ := a.asn.Cached(strings.TrimPrefix(out.Path[i].ASN, "AS")); info != nil {
			if info.PeeringDB != nil && info.PeeringDB.Name != "" {
				out.Path[i].Name = info.PeeringDB.Name
			} else {
				out.Path[i].Name = info.Holder
			}
		}
	}
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, out)
}

func (a *API) TracerouteRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/aspath", a.asPathView)
	mux.HandleFunc("GET /api/v1/admin/hops", a.auth(a.hopSeries))
	mux.HandleFunc("GET /api/v1/traceroutes", a.traceroutesPublic)
	mux.HandleFunc("GET /api/v1/admin/traceroutes", a.need(RoleViewer, a.traceroutesAdmin))
	mux.HandleFunc("POST /api/v1/admin/traceroutes", a.need(RoleEditor, a.traceroutesRequest))
}

func (a *API) listTraceroutes(w http.ResponseWriter, r *http.Request, public bool) {
	id, err := strconv.ParseInt(r.URL.Query().Get("target"), 10, 64)
	if err != nil {
		writeErr(w, 400, "missing target parameter")
		return
	}
	t, err := a.store.TargetByID(id)
	if err != nil || (public && !t.Public) {
		writeErr(w, 404, "target not found")
		return
	}
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 20
	}
	var kinds []string
	if k := r.URL.Query().Get("kind"); k != "" {
		kinds = []string{k}
	}
	list, err := a.store.Traceroutes(id, kinds, limit)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if public {
		w.Header().Set("Cache-Control", "public, max-age=30")
	}
	writeJSON(w, list)
}

// Traceroutes reveal the inside of the operator's network: they are public
// only if the publisher enabled it.
func (a *API) traceroutesPublic(w http.ResponseWriter, r *http.Request) {
	if !a.store.Site().PublicTraceroutes {
		writeJSON(w, []*Traceroute{})
		return
	}
	// A public target whose address is hidden must not expose its path
	// either: the hops would give the address away immediately.
	if !a.authenticated(r) {
		if id, err := strconv.ParseInt(r.URL.Query().Get("target"), 10, 64); err == nil {
			if t, err := a.store.TargetByID(id); err == nil && t.HideHost {
				writeJSON(w, []any{})
				return
			}
		}
	}
	a.listTraceroutes(w, r, !a.authenticated(r))
}

func (a *API) traceroutesAdmin(w http.ResponseWriter, r *http.Request, u *User) {
	a.listTraceroutes(w, r, false)
}

func (a *API) traceroutesRequest(w http.ResponseWriter, r *http.Request, u *User) {
	var in struct {
		TargetID int64 `json:"target_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil || in.TargetID == 0 {
		writeErr(w, 400, "target_id is required")
		return
	}
	if _, err := a.store.TargetByID(in.TargetID); err != nil {
		writeErr(w, 404, "target not found")
		return
	}
	traceRequests.Push(in.TargetID)
	a.store.Audit(u, clientIP(r), "traceroute_request", "target", strconv.FormatInt(in.TargetID, 10))
	writeJSON(w, map[string]any{"queued": true, "at": time.Now().Unix()})
}
