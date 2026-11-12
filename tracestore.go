package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sort"
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
	IX    string    `json:"ix,omitempty"`   // peering LAN this address sits on
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
//
// The event belongs to one target and to nothing else. It describes the
// route between this instance and that one destination, so it is scoped to
// the target and appears on its page alone: a route change towards Netflix
// says nothing about the path to anywhere else.
func (s *Store) notePathChange(tr *Traceroute) {
	prev, err := s.Traceroutes(tr.TargetID, []string{"reference"}, 2)
	if err != nil || len(prev) < 2 {
		return
	}
	now, before := asPath(prev[0]), asPath(prev[1])
	if len(now) == 0 || len(before) == 0 {
		return
	}
	// A rotating name answers from a different machine at every pass, so the
	// two reference traceroutes did not even go to the same place. The AS
	// path differs because the destination differs, not because anything was
	// rerouted — calling that a route change is what made pool targets noisy.
	if a, b := prev[0].Dest, prev[1].Dest; a != "" && b != "" && a != b {
		return
	}
	if samePath(now, before) {
		return
	}
	t, err := s.TargetByID(tr.TargetID)
	if err != nil {
		return
	}
	// The title is drawn on the graph, so it stays short; the page it appears
	// on already says which target this is. Both ends of the path go in the
	// body, because a path is only meaningful between two named networks.
	detail := s.pathChangeBody(t, prev[0].Dest, before, now)
	s.cfg.Exec(
		`INSERT INTO events(ts_start,kind,title,body,scope,scope_id,public)
		 VALUES(?,'path',?,?,'target',?,1)`,
		tr.TS, "Route changed", detail, t.ID)
	log.Printf("path change towards %s (%s) — %s", t.Title, t.Slug, detail)
}

// pathChangeBody writes the change as the route between this instance's AS
// and the AS announcing the address actually measured.
func (s *Store) pathChangeBody(t *Target, dest string, before, now []string) string {
	site := s.Site()
	from := "this instance"
	if n, err := normalizeASN(site.ASN); err == nil {
		from = "AS" + strings.TrimPrefix(n, "AS")
		if site.Org != "" {
			from += " (" + site.Org + ")"
		}
	}
	to := t.Title
	if last := now[len(now)-1]; last != "" {
		to = "AS" + strings.TrimPrefix(last, "AS") + " (" + t.Title + ")"
	}
	if dest != "" && !t.HideHost {
		to += " at " + dest
	}
	return fmt.Sprintf("Route from %s to %s: AS path %s → %s", from, to,
		strings.Join(before, " "), strings.Join(now, " "))
}

// RecentPathChange reports whether the AS path of a target changed in the
// last window, and what the change was. It reads the target's own events,
// never another target's.
func (s *Store) RecentPathChange(targetID int64, since int64) (string, bool) {
	var title, body string
	err := s.cfg.QueryRow(
		`SELECT title, COALESCE(body,'') FROM events
		  WHERE kind='path' AND scope='target' AND scope_id=? AND ts_start>=?
		  ORDER BY ts_start DESC LIMIT 1`, targetID, since).Scan(&title, &body)
	if err != nil {
		return "", false
	}
	if t, err := s.TargetByID(targetID); err == nil && t.Title != "" {
		title = t.Title + ": " + strings.ToLower(title)
	}
	if body != "" {
		title += " (" + body + ")"
	}
	return title, true
}

// Traceroutes keeps the shape every caller already had: every probe's, which
// is what a page that does not offer a choice of vantage point wants. The
// callers that do offer one use TraceroutesFrom, so adding the filter costs
// nothing to the six that do not.
func (s *Store) Traceroutes(targetID int64, kinds []string, limit int) ([]*Traceroute, error) {
	return s.TraceroutesFrom(targetID, 0, kinds, limit)
}

// TraceroutesFrom narrows to one probe. Zero means any, so that the caller
// deciding this does not have to special-case "no choice made" itself.
//
// The existing index is on (target_id, ts), so the probe is a filter inside
// one target's rows rather than a seek. That is cheap at the number of
// traceroutes a target keeps; it would want (target_id, probe_id, ts) if that
// ever stopped being true, and this comment is here so that the next person
// measuring a slow page knows where to look.
func (s *Store) TraceroutesFrom(targetID, probeID int64, kinds []string, limit int) ([]*Traceroute, error) {
	q := `SELECT id,target_id,probe_id,ts,kind,reason,family,dest,reached,hops
	        FROM traceroutes WHERE target_id=?`
	args := []any{targetID}
	if probeID > 0 {
		q += ` AND probe_id=?`
		args = append(args, probeID)
	}
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
	ids []queued
}

// A queued request remembers when it was made, so that one aimed at a probe
// that never comes back does not sit here forever. See traceClaimGrace.
type queued struct {
	id int64
	at int64
}

// How long a request waits for the probe that should run it before any probe
// may. A traceroute is wanted because something looked wrong, and a primary
// probe that is not answering is itself something wrong — running it from
// elsewhere then says more than not running it at all. Probes ask for work
// every five seconds, so a primary that is up claims its requests long before
// this.
const traceClaimGrace = 120

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
		if x.id == id {
			return
		}
	}
	if len(q.ids) < 100 {
		q.ids = append(q.ids, queued{id: id, at: time.Now().Unix()})
	}
}

// PopFor hands a probe the requests it should run: the targets it is primary
// for, plus anything the primary has left waiting past the grace period.
// Everything else stays queued for the probe it belongs to.
//
// mine is nil for a caller with no routing at all, which takes everything —
// that is the shape the queue had before targets had a primary probe, and it
// is what the tests of everything else still expect.
func (q *traceQueue) PopFor(mine map[int64]bool, now int64) []int64 {
	q.mu.Lock()
	defer q.mu.Unlock()
	if mine == nil {
		out := make([]int64, 0, len(q.ids))
		for _, x := range q.ids {
			out = append(out, x.id)
		}
		q.ids = nil
		return out
	}
	out, kept := []int64{}, q.ids[:0]
	for _, x := range q.ids {
		if mine[x.id] || now-x.at >= traceClaimGrace {
			out = append(out, x.id)
			continue
		}
		kept = append(kept, x)
	}
	q.ids = kept
	return out
}

func (q *traceQueue) Pop() []int64 { return q.PopFor(nil, 0) }

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
	Hops int    `json:"hops"`          // combien de sauts dans cet AS
	Via  string `json:"via,omitempty"` // l'IX traverse pour entrer dans cet AS
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
	Graph   *ASGraph    `json:"graph,omitempty"`
	RIS     *RISView    `json:"ris,omitempty"`
	// Why the middle of the route is empty, when it is. Drawing the two
	// ends with nothing between them says "these networks are adjacent",
	// which is a claim, and usually a false one. Naming the reason is the
	// difference between an answer and a blank.
	Empty string `json:"empty,omitempty"` // "no-trace" | "no-as"
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
		mid = append(mid, ASPathHop{ASN: as, Hops: 1, Via: h.IX})
	}
	// A gap when the traceroute never reached the destination, or when its
	// last hops answered nothing: the segment before the destination is
	// unknown, and saying so is better than implying a direct link.
	gap := !tr.Reached
	if lastKnown >= 0 && lastKnown < len(tr.Hops)-1 {
		gap = true
	}
	// Not one hop carried an autonomous system, although the traceroute ran:
	// the path between the two ends is unknown, not empty. Without this a
	// traceroute that reached its destination drew the two ends touching.
	if lastKnown < 0 && len(tr.Hops) > 0 {
		gap = true
	}
	if destASN != "" && lastKnown >= 0 {
		if as := strings.TrimSpace(tr.Hops[lastKnown].ASN); as == destASN {
			gap = !tr.Reached
		}
	}
	return mid, gap
}

// ixEntering names the exchange the path crossed to enter a network: the
// first hop answering for that AS is the member's own interface on the
// peering LAN. A peering is where a route changes hands, and the chain says
// so rather than drawing a bare arrow.
func ixEntering(tr *Traceroute, asn string) string {
	prev := ""
	for _, h := range tr.Hops {
		as := strings.TrimSpace(h.ASN)
		if as == "" {
			continue
		}
		if as == asn && prev != asn {
			return h.IX
		}
		prev = as
	}
	return ""
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
		cachePrivate(w)
		writeErr(w, 404, "target not found")
		return
	}
	authed := a.authenticated(r)
	if shared, ok := a.shareGrant(r); ok && shared == id {
		authed = true
	}
	// Decided here rather than at the end of the function, which is where it
	// used to be. The empty path below is an answer too, and it returned
	// carrying no directive at all — including for a target no visitor may
	// read, where a shared cache had nothing stopping it from keeping the
	// answer under the address a visitor uses.
	cacheByViewer(w, authed, 300)
	hideAddr := t.HideHost
	if !authed && (!t.Public || t.HideHost || !a.store.Site().PublicTraceroutes) {
		writeJSON(w, ASRoute{Path: []ASPathHop{}})
		return
	}

	site := a.store.Site()
	out := ASRoute{Path: []ASPathHop{}}
	// A hop on a peering LAN is in no routing table, and a lookup that
	// failed the day the traceroute ran left its AS blank for good. Both are
	// named here, from the caches, so the drawing follows what is known now
	// rather than what was known then.
	fill := func([]Hop) {}
	if a.asn != nil {
		fill = a.asn.HopFiller()
	}

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
			// The address actually probed. For a target given by name this
			// is the one thing on the page the operator never typed, and it
			// went out in full: masking it is the same rule as everywhere
			// else, and an authenticated operator still sees it whole.
			if authed || !site.MaskAddresses {
				out.DestIP = destIP
			} else {
				out.DestIP = maskIP(destIP)
			}
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

	// What the RIS collectors see for that address: the prefix, who
	// announces it, and its upstreams as the world sees them. A different
	// question from ours, kept in its own block on the page.
	if destIP != "" {
		if v, ok := a.asn.RISFor(destIP); ok {
			out.RIS = v
			if time.Now().Unix()-v.FetchedAt > 7*86400 {
				go a.asn.RefreshRIS(destIP)
			}
		} else {
			go a.asn.RefreshRIS(destIP)
		}
	}

	// The graph of the recent traceroutes, for the map drawn on the page.
	if g := a.store.BuildASGraph(id, originASN, destASN, 25, fill); g != nil {
		g.DestIP = out.DestIP
		g.Pending = out.Pending
		for i := range g.Nodes {
			if g.Nodes[i].Name != "" || g.Nodes[i].Unknown {
				continue
			}
			switch g.Nodes[i].ASN {
			case originASN:
				if out.Origin != nil {
					g.Nodes[i].Name = out.Origin.Name
				}
			case destASN:
				if out.Dest != nil {
					g.Nodes[i].Name = out.Dest.Name
				}
			default:
				if info, _ := a.asn.Cached(strings.TrimPrefix(g.Nodes[i].ASN, "AS")); info != nil {
					if info.PeeringDB != nil && info.PeeringDB.Name != "" {
						g.Nodes[i].Name = info.PeeringDB.Name
					} else {
						g.Nodes[i].Name = info.Holder
					}
				} else {
					go a.asn.Refresh(strings.TrimPrefix(g.Nodes[i].ASN, "AS"))
				}
			}
		}
		out.Graph = g
	}

	// The middle comes from the last healthy traceroute, or the last one of
	// any kind if none is healthy yet.
	// 0 keeps today's meaning here: the reference path from whichever probe
	// recorded one, when the caller named none.
	probe := a.resolveProbe(r, 0)
	trs, err := a.store.TraceroutesFrom(id, probe, []string{"reference"}, 1)
	if err != nil || len(trs) == 0 {
		trs, _ = a.store.TraceroutesFrom(id, probe, nil, 1)
	}
	if len(trs) == 0 {
		// Nothing was recorded yet. A reference path is taken once a day by
		// default, and an anomaly traceroute only when something degrades,
		// so a freshly added target legitimately has none.
		out.Empty = "no-trace"
	}
	if len(trs) > 0 {
		tr := trs[0]
		fill(tr.Hops)
		out.TS, out.Kind, out.Reached = tr.TS, tr.Kind, tr.Reached
		out.Path, out.Gap = asRouteFrom(tr, originASN, destASN)
		if out.Dest != nil {
			out.Dest.Via = ixEntering(tr, destASN)
		}
		if len(out.Path) == 0 {
			// The traceroute ran but no hop could be attributed to an AS:
			// the lookups have not answered yet, or the routers in between
			// stayed silent.
			out.Empty = "no-as"
		}
	} else {
		// No traceroute at all: the two ends are still worth showing, with
		// the middle explicitly unknown. RIS can even supply the
		// destination AS when the address lookup has not answered yet.
		out.Gap = true
		if out.Dest == nil && out.RIS != nil && out.RIS.OriginASN != "" {
			out.Dest = &ASPathHop{ASN: out.RIS.OriginASN, Name: t.Title}
			out.Pending = false
		}
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
	// Same reasoning as the publisher page, and it matters more here: an
	// anonymous caller gets an empty path for a private target, a target
	// that hides its host, or an instance that does not publish
	// traceroutes, and masked addresses when the operator masks them. A
	// shared cache must not be allowed to keep the authenticated answer
	// under the address a visitor uses.
	writeJSON(w, out)
}

func (a *API) TracerouteRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/aspath", a.asPathView)
	mux.HandleFunc("GET /api/v1/admin/hops", a.auth(a.hopSeries))
	mux.HandleFunc("GET /api/v1/traceroutes", a.traceroutesPublic)
	mux.HandleFunc("GET /api/v1/admin/traceroutes", a.need(RoleViewer, a.traceroutesAdmin))
	mux.HandleFunc("POST /api/v1/admin/traceroutes", a.need(RoleEditor, a.traceroutesRequest))
}

// probeParam reads an optional ?probe=. Anything unparseable is no choice at
// all rather than an error: a stale bookmark naming a probe that has since
// gone should show the page, not a refusal.
//
// It does not say whether the caller may read that probe. Use resolveProbe,
// which applies the same rule to a probe that exists but is not theirs to
// read, so one function decides it for every endpoint that takes ?probe=.
func probeParam(r *http.Request) int64 {
	if v := r.URL.Query().Get("probe"); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
			return n
		}
	}
	return 0
}

// resolveProbe turns an optional ?probe= into the probe to read from, and
// returns fallback when the parameter names no probe this caller may read.
//
// A probe the caller may not read is treated exactly as no parameter at all,
// which is the rule probeParam already applied to a malformed one. Falling
// back rather than refusing keeps a stale link working, and makes a request
// for an unreadable id indistinguishable from one that named no probe: there
// is nothing to learn by walking the ids that the public probes endpoint does
// not already say.
//
// The fallback is the caller's because it is not the same everywhere. A
// series and an availability figure are read from one probe, so theirs is the
// instance's own. A traceroute list reads from every probe when none is
// named, and 0 is what says so — handing it a probe id there would silently
// narrow a page that has always shown the lot.
func (a *API) resolveProbe(r *http.Request, fallback int64) int64 {
	n := probeParam(r)
	if n == 0 || !a.store.ProbeReadable(n, a.authenticated(r)) {
		return fallback
	}
	return n
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
	list, err := a.store.TraceroutesFrom(id, a.resolveProbe(r, 0), kinds, limit)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	if a.asn != nil {
		fill := a.asn.HopFiller()
		for _, tr := range list {
			fill(tr.Hops)
		}
	}
	w.Header().Set("Vary", "Cookie, Authorization")
	if public {
		w.Header().Set("Cache-Control", "public, max-age=30")
	} else {
		w.Header().Set("Cache-Control", "no-store")
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

// ---------------------------------------------------------------- AS graph
//
// A route drawn as a chain hides what matters when a path is not stable: the
// same target is often reached through two transits, and a traceroute taken
// during an anomaly may follow a different one. So the page draws a graph:
// nodes are autonomous systems, edges are adjacencies actually observed, and
// the current path is highlighted among the others.

type ASGraphNode struct {
	ASN     string  `json:"asn"` // "?" and the AS before it for an unmeasured stretch
	Name    string  `json:"name,omitempty"`
	Layer   int     `json:"layer"` // distance from us, in AS crossed
	Origin  bool    `json:"origin,omitempty"`
	Dest    bool    `json:"dest,omitempty"`
	Unknown bool    `json:"unknown,omitempty"`
	RTTms   float64 `json:"rtt_ms,omitempty"` // median on entering this AS
	Seen    int     `json:"seen"`             // traceroutes crossing it
}

type ASGraphEdge struct {
	From    string `json:"from"`
	To      string `json:"to"`
	Seen    int    `json:"seen"`
	Current bool   `json:"current,omitempty"`
	IX      string `json:"ix,omitempty"` // the exchange this link crossed
}

type ASGraph struct {
	Nodes      []ASGraphNode `json:"nodes"`
	Edges      []ASGraphEdge `json:"edges"`
	Traces     int           `json:"traces"`
	Since      int64         `json:"since,omitempty"`
	CurrentTS  int64         `json:"current_ts,omitempty"`
	DestIP     string        `json:"dest_ip,omitempty"`
	Reached    bool          `json:"reached"`
	Pending    bool          `json:"pending,omitempty"`
	Incomplete bool          `json:"incomplete,omitempty"`
	// OtherAddrs counts the traceroutes left out because they went to
	// another address of the same name. A route is a route to one machine,
	// so mixing several would draw a path that was never taken.
	OtherAddrs int `json:"other_addrs,omitempty"`
}

// asSeq turns one traceroute into the sequence of AS it crossed, with the
// median RTT on entering each, our own AS first and the destination's last.
// An unmeasured stretch becomes the pseudo-AS "?", so the drawing shows a
// break instead of implying adjacency.
func asSeq(tr *Traceroute, originASN, destASN string) ([]string, map[string]float64, map[string]string) {
	seq := []string{}
	rtt := map[string]float64{}
	ix := map[string]string{} // "from>to" : the exchange that link crossed
	last := ""                // the network we are in, silent hops included
	if originASN != "" {
		seq = append(seq, originASN)
		last = originASN
	}
	gapPending := false
	for _, h := range tr.Hops {
		as := strings.TrimSpace(h.ASN)
		if as == "" {
			if len(h.RTTms) == 0 {
				gapPending = true // a hop that answered nothing
			}
			continue
		}
		// Routers staying silent between two hops of the same network are
		// internal to it: the path never left, so there is no hole to draw.
		// Counting them as a gap drew the network twice, with a detour
		// through the unknown in between.
		if as == last {
			gapPending = false
			continue
		}
		if gapPending && len(seq) > 0 && seq[len(seq)-1] != "?" {
			seq = append(seq, "?")
		}
		gapPending = false
		if h.IX != "" && len(seq) > 0 {
			ix[seq[len(seq)-1]+">"+as] = h.IX
		}
		seq = append(seq, as)
		last = as
		if _, ok := rtt[as]; !ok && len(h.RTTms) > 0 {
			rtt[as] = medianOf(h.RTTms)
		}
	}
	if destASN != "" && last != destASN {
		if !tr.Reached || gapPending {
			if len(seq) > 0 && seq[len(seq)-1] != "?" {
				seq = append(seq, "?")
			}
		}
		if v := ixEntering(tr, destASN); v != "" && len(seq) > 0 {
			ix[seq[len(seq)-1]+">"+destASN] = v
		}
		seq = append(seq, destASN)
	}
	return seq, rtt, ix
}

func medianOf(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return s[len(s)/2]
}

// BuildASGraph assembles the graph from the recent traceroutes of a target.
// fill, when given, names the hops the probe left bare before they are read.
func (s *Store) BuildASGraph(targetID int64, originASN, destASN string, limit int, fill func([]Hop)) *ASGraph {
	trs, err := s.Traceroutes(targetID, nil, limit)
	if err != nil {
		return nil
	}
	g := &ASGraph{Nodes: []ASGraphNode{}, Edges: []ASGraphEdge{}}
	nodes := map[string]*ASGraphNode{}
	edges := map[string]*ASGraphEdge{}
	current := map[string]bool{}

	// A name that answers from several machines — a pool, an anycast name
	// resolved differently at each pass — was traced to different places. The
	// graph keeps the address of the most recent traceroute, or the pinned
	// one, and counts the rest: drawing them together would show a path that
	// no packet ever took.
	want := ""
	if t, err := s.TargetByID(targetID); err == nil && t.PinIP != "" {
		want = t.PinIP
	}
	for _, tr := range trs {
		if want == "" && tr.Dest != "" {
			want = tr.Dest
			break // newest first, so this is the address in use now
		}
	}
	kept := trs[:0:0]
	for _, tr := range trs {
		if want != "" && tr.Dest != "" && tr.Dest != want {
			g.OtherAddrs++
			continue
		}
		kept = append(kept, tr)
	}
	trs = kept

	for i, tr := range trs {
		if fill != nil {
			fill(tr.Hops)
		}
		seq, rtt, ix := asSeq(tr, originASN, destASN)
		if len(seq) == 0 {
			continue
		}
		g.Traces++
		if g.Since == 0 || tr.TS < g.Since {
			g.Since = tr.TS
		}
		if i == 0 {
			g.CurrentTS, g.Reached = tr.TS, tr.Reached
		}
		// Each unmeasured stretch is a node of its own, named after the
		// network it follows. One "?" shared by all of them joined stretches
		// that have nothing in common and drew loops no packet ever took.
		ids := make([]string, len(seq))
		for j, as := range seq {
			ids[j] = as
			if as == "?" && j > 0 {
				ids[j] = "?" + seq[j-1]
			}
		}
		for j, as := range seq {
			id := ids[j]
			n := nodes[id]
			if n == nil {
				n = &ASGraphNode{ASN: id, Layer: j, Unknown: as == "?"}
				nodes[id] = n
			}
			if j > n.Layer {
				n.Layer = j // keep it to the right of everything it follows
			}
			n.Seen++
			if v, ok := rtt[as]; ok && (n.RTTms == 0 || i == 0) {
				n.RTTms = v
			}
			if j > 0 {
				key := ids[j-1] + ">" + id
				e := edges[key]
				if e == nil {
					e = &ASGraphEdge{From: ids[j-1], To: id}
					edges[key] = e
				}
				e.Seen++
				if e.IX == "" {
					e.IX = ix[seq[j-1]+">"+as]
				}
				if i == 0 {
					e.Current = true
					current[key] = true
				}
			}
		}
	}
	if len(nodes) == 0 {
		return nil
	}
	for _, n := range nodes {
		n.Origin = n.ASN == originASN
		n.Dest = n.ASN == destASN
		if n.Unknown {
			g.Incomplete = true
		}
		g.Nodes = append(g.Nodes, *n)
	}
	sort.Slice(g.Nodes, func(i, j int) bool {
		if g.Nodes[i].Layer != g.Nodes[j].Layer {
			return g.Nodes[i].Layer < g.Nodes[j].Layer
		}
		return g.Nodes[i].Seen > g.Nodes[j].Seen
	})
	for _, e := range edges {
		g.Edges = append(g.Edges, *e)
	}
	sort.Slice(g.Edges, func(i, j int) bool { return g.Edges[i].Seen > g.Edges[j].Seen })
	return g
}
