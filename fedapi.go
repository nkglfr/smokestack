package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"strconv"
	"time"
)

func (a *API) FedRoutes(mux *http.ServeMux) {
	if a.fed == nil {
		return
	}
	// Le profil est public : c'est la carte de visite de l'instance.
	mux.HandleFunc("GET /api/v1/fed/profile", a.fedProfile)
	mux.HandleFunc("GET /api/v1/fed/matrix", a.fedMatrix)
	mux.HandleFunc("GET /api/v1/fed/incidents", a.fedIncidents)
	mux.HandleFunc("GET /api/v1/fed/peers", a.fedPeersPublic)

	// Ces trois routes exigent une signature Ed25519 d'un pair approuve.
	mux.HandleFunc("POST /api/v1/fed/report", a.fedReport)
	mux.HandleFunc("POST /api/v1/fed/incident", a.fedIncident)
	mux.HandleFunc("POST /api/v1/fed/ack", a.fedAck)

	mux.HandleFunc("GET /api/v1/admin/fed/identity", a.auth(a.fedIdentity))
	mux.HandleFunc("GET /api/v1/admin/fed/peers", a.auth(a.fedPeersAdmin))
	mux.HandleFunc("POST /api/v1/admin/fed/peers", a.auth(a.fedPeerAdd))
	mux.HandleFunc("POST /api/v1/admin/fed/peers/{id}/trust", a.auth(a.fedPeerTrust))
	mux.HandleFunc("DELETE /api/v1/admin/fed/peers/{id}", a.auth(a.fedPeerDelete))
	mux.HandleFunc("PUT /api/v1/admin/fed/notify", a.auth(a.fedNotifyPut))
}

func (a *API) fedProfile(w http.ResponseWriter, r *http.Request) {
	p := a.fed.Profile()
	p.NOCPhone = ""
	writeJSON(w, p)
}

func (a *API) fedIdentity(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{
		"profile":     a.fed.Profile(),
		"fingerprint": a.fed.Fingerprint(),
		"notify":      a.fed.notifyConfig(),
	})
}

// MatrixCell agrege la derniere fenetre connue pour un couple d'AS.
type MatrixCell struct {
	FromASN string  `json:"from_asn"`
	ToASN   string  `json:"to_asn"`
	MedMs   float64 `json:"med_ms"`
	P95Ms   float64 `json:"p95_ms"`
	LossPct float64 `json:"loss_pct"`
	WinEnd  int64   `json:"window_end"`
}

func (a *API) fedMatrix(w http.ResponseWriter, r *http.Request) {
	since := time.Now().Unix() - 3600
	rows, err := a.store.cfg.Query(
		`SELECT from_asn,to_asn,AVG(med_ms),MAX(p95_ms),AVG(loss_pct),MAX(window_end)
		   FROM fed_reports WHERE window_end > ?
		  GROUP BY from_asn,to_asn`, since)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	cells := []MatrixCell{}
	for rows.Next() {
		var c MatrixCell
		if err := rows.Scan(&c.FromASN, &c.ToASN, &c.MedMs, &c.P95Ms,
			&c.LossPct, &c.WinEnd); err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		cells = append(cells, c)
	}
	sort.Slice(cells, func(i, j int) bool {
		if cells[i].FromASN != cells[j].FromASN {
			return cells[i].FromASN < cells[j].FromASN
		}
		return cells[i].ToASN < cells[j].ToASN
	})
	writeJSON(w, cells)
}

func (a *API) fedIncidents(w http.ResponseWriter, r *http.Request) {
	incs, err := a.fed.Incidents(50)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, incs)
}

// PublicPeer est ce qu'un visiteur voit d'un pair : l'annuaire et les
// mesures croisees, jamais les coordonnees du NOC ni les notes privees.
type PublicPeer struct {
	ASN        string   `json:"asn"`
	Org        string   `json:"org"`
	URL        string   `json:"url"`
	Anchors    []string `json:"anchors"`
	Since      int64    `json:"since"`
	LastSeenAt int64    `json:"last_seen_at"`
	ToMedMs    *float64 `json:"to_med_ms"`
	ToLossPct  *float64 `json:"to_loss_pct"`
	FromMedMs  *float64 `json:"from_med_ms"`
	FromLoss   *float64 `json:"from_loss_pct"`
}

// fedPeersPublic ne publie que les pairs dont l'affichage a ete accepte
// des deux cotes, avec la latence dans chaque sens sur la derniere heure.
func (a *API) fedPeersPublic(w http.ResponseWriter, r *http.Request) {
	peers, err := a.fed.Peers()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	me := a.fed.Profile().ASN
	since := time.Now().Unix() - 3600
	stat := func(from, to string) (*float64, *float64) {
		var med, loss *float64
		a.store.cfg.QueryRow(
			`SELECT AVG(med_ms), AVG(loss_pct) FROM fed_reports
			  WHERE from_asn=? AND to_asn=? AND window_end>?`,
			from, to, since).Scan(&med, &loss)
		return med, loss
	}
	out := []PublicPeer{}
	for _, p := range peers {
		if p.State != "trusted" || !p.Public {
			continue
		}
		pp := PublicPeer{ASN: p.ASN, Org: p.Org, URL: p.URL, Anchors: p.Anchors,
			Since: p.TrustedAt, LastSeenAt: p.LastSeenAt}
		pp.ToMedMs, pp.ToLossPct = stat(me, p.ASN)
		pp.FromMedMs, pp.FromLoss = stat(p.ASN, me)
		out = append(out, pp)
	}
	w.Header().Set("Cache-Control", "public, max-age=60")
	writeJSON(w, out)
}

func (a *API) fedReport(w http.ResponseWriter, r *http.Request) {
	peer, body, err := a.fed.verify(r)
	if err != nil {
		writeErr(w, 401, err.Error())
		return
	}
	var batch reportBatch
	if err := json.Unmarshal(body, &batch); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	var reports []anchorReport
	for _, r0 := range batch.Reports {
		// Un pair ne peut declarer que ses propres mesures.
		if r0.FromASN != peer.ASN {
			continue
		}
		reports = append(reports, anchorReport{
			FromASN: r0.FromASN, ToASN: r0.ToASN, Anchor: r0.Anchor,
			WinEnd: r0.WinEnd, MedMs: r0.MedMs, P95Ms: r0.P95Ms,
			LossPct: r0.LossPct,
		})
	}
	a.fed.storeReports(reports)
	a.store.cfg.Exec(`UPDATE fed_peers SET last_seen_at=?, last_error=NULL WHERE id=?`,
		time.Now().Unix(), peer.ID)
	writeJSON(w, map[string]any{"accepted": len(reports)})
}

func (a *API) fedIncident(w http.ResponseWriter, r *http.Request) {
	peer, body, err := a.fed.verify(r)
	if err != nil {
		writeErr(w, 401, err.Error())
		return
	}
	var i Incident
	if err := json.Unmarshal(body, &i); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if i.ID == "" || i.SuspectASN == "" {
		writeErr(w, 400, "incomplete incident")
		return
	}
	i.ObserverASN = peer.ASN
	a.fed.saveIncident(i)
	a.fed.addCorroboration(i.ID, peer.ASN, i.MedMs, i.LossPct, i.Detail)

	// Si c'est nous qui sommes mis en cause, on corrobore ou on infirme
	// depuis nos propres mesures avant la fin du preavis.
	me := a.fed.Profile().ASN
	if i.SuspectASN == me {
		writeJSON(w, map[string]any{"received": true, "self": true,
			"ack_endpoint": "/api/v1/fed/ack"})
		return
	}
	writeJSON(w, map[string]any{"received": true})
}

func (a *API) fedAck(w http.ResponseWriter, r *http.Request) {
	peer, body, err := a.fed.verify(r)
	if err != nil {
		writeErr(w, 401, err.Error())
		return
	}
	var in struct {
		IncidentID string `json:"incident_id"`
		Note       string `json:"note"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	// Seul l'AS mis en cause peut acquitter.
	res, err := a.store.cfg.Exec(
		`UPDATE fed_incidents SET ack_at=?, ack_by=?, detail=COALESCE(detail,'')||?
		  WHERE id=? AND suspect_asn=? AND ack_at IS NULL`,
		time.Now().Unix(), peer.ASN, " — ack: "+in.Note, in.IncidentID, peer.ASN)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		writeErr(w, 403, "acknowledgement not applicable")
		return
	}
	writeJSON(w, map[string]any{"acked": true})
}

func (a *API) fedPeersAdmin(w http.ResponseWriter, r *http.Request) {
	peers, err := a.fed.Peers()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, peers)
}

func (a *API) fedPeerAdd(w http.ResponseWriter, r *http.Request) {
	var in struct {
		URL string `json:"url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	p, err := a.fed.AddPeer(in.URL)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]any{
		"peer": p,
		"next": "Compare the fingerprint with the one given by the operator, " +
			"then approve the peer.",
	})
}

func (a *API) fedPeerTrust(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, 400, "invalid identifier")
		return
	}
	if err := a.fed.TrustPeer(id); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]any{"trusted": id})
}

func (a *API) fedPeerDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, 400, "invalid identifier")
		return
	}
	if _, err := a.store.cfg.Exec(`DELETE FROM fed_peers WHERE id=?`, id); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) fedNotifyPut(w http.ResponseWriter, r *http.Request) {
	var c NotifyConfig
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if c.SMTPPass == "" {
		c.SMTPPass = a.fed.notifyConfig().SMTPPass
	}
	b, _ := json.Marshal(c)
	if err := a.store.SetSetting("notify", string(b)); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	c.SMTPPass = ""
	writeJSON(w, c)
}
