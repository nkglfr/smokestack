package main

import (
	"encoding/json"
	"net/http"
	"strconv"
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

func (s *Store) SaveTraceroute(tr *Traceroute) error {
	hops, _ := json.Marshal(tr.Hops)
	_, err := s.mxw.Exec(`INSERT INTO traceroutes(target_id,probe_id,ts,kind,reason,family,dest,reached,hops)
	                      VALUES(?,?,?,?,?,?,?,?,?)`,
		tr.TargetID, tr.ProbeID, tr.TS, tr.Kind, tr.Reason, tr.Family, tr.Dest, b2i(tr.Reached), string(hops))
	return err
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

var traceRequests traceQueue

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

func (a *API) TracerouteRoutes(mux *http.ServeMux) {
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
	a.listTraceroutes(w, r, true)
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
