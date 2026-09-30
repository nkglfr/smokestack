package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"sort"
	"strconv"
	"time"
)

// Decomposition du temps de reponse TCP. Une cible qui repond en
// quatre-vingts millisecondes ne dit pas ou passent ces quatre-vingts
// millisecondes : une resolution DNS lente et un reseau lent produisent
// le meme chiffre, et la correction n'est pas la meme.
//
// Volontairement a la demande et non historisee. Trois raisons. D'abord
// la mesure coute une resolution et une poignee de main complete, ce qui
// est cher a repeter toutes les soixante secondes. Ensuite la serie de
// latence ne doit pas changer de definition : la sonde continue de faire
// exactement ce qu'elle faisait, et cette decomposition est une mesure
// separee, a cote, qui ne touche pas au graphe. Enfin c'est un outil de
// diagnostic — on le regarde quand quelque chose ne va pas — et pas une
// metrique a suivre dans le temps, donc une ligne par cible, ecrasee,
// suffit et se voit dans le back-office plutot que sur la page publique.

const breakdownSchema = `
CREATE TABLE IF NOT EXISTS tcp_breakdown (
  target_id  INTEGER PRIMARY KEY REFERENCES targets(id) ON DELETE CASCADE,
  ts         INTEGER NOT NULL,
  dns_us     REAL NOT NULL DEFAULT 0,
  connect_us REAL NOT NULL DEFAULT 0,
  tls_us     REAL NOT NULL DEFAULT 0,
  ip         TEXT NOT NULL DEFAULT '',
  err        TEXT NOT NULL DEFAULT ''
);`

type Breakdown struct {
	TargetID int64 `json:"target_id"`
	TS       int64 `json:"ts"`
	// DNSus vaut zero quand la cible est une adresse litterale : il n'y a
	// rien a resoudre, et zero le dit mieux qu'une valeur inventee.
	DNSus float64 `json:"dns_us"`
	// ConnectUs : le temps du SYN -> SYN/ACK, c'est-a-dire un aller-retour
	// reseau et l'acceptation par la pile distante.
	ConnectUs float64 `json:"connect_us"`
	// TLSus : la poignee de main, quand le port en est un ou TLS est
	// attendu. Zero sinon, et l'interface ne montre alors pas la colonne.
	TLSus float64 `json:"tls_us"`
	IP    string  `json:"ip,omitempty"`
	Err   string  `json:"err,omitempty"`
	// Champs de commodite pour l'interface.
	Title string `json:"title,omitempty"`
	Host  string `json:"host,omitempty"`
	Port  int    `json:"port,omitempty"`
}

// Total est ce que la sonde mesure, decompose. La somme n'egale pas
// exactement la latence du graphe : celle-ci ne compte que la connexion,
// pas la resolution, et la poignee de main n'y est pas du tout.
func (b *Breakdown) Total() float64 { return b.DNSus + b.ConnectUs + b.TLSus }

// tlsPorts : les ports ou une poignee de main TLS est attendue sans
// negociation prealable. Ailleurs on ne tente rien, parce qu'envoyer un
// ClientHello a un port qui attend autre chose ne mesure que le temps
// d'un refus.
var tlsPorts = map[int]bool{443: true, 465: true, 636: true, 853: true,
	989: true, 990: true, 993: true, 995: true, 8443: true, 9443: true}

// measureBreakdown fait une resolution puis une connexion, chronometrees
// separement, puis la poignee de main si le port l'attend. Une mesure, pas
// une serie : l'appelant demande, on repond.
func measureBreakdown(host string, port, family int, timeout time.Duration) Breakdown {
	var b Breakdown
	b.TS = time.Now().Unix()
	name := hostOnly(host)
	network := "tcp"
	if family == 4 || family == 6 {
		network = fmt.Sprintf("tcp%d", family)
	}

	ip := net.ParseIP(name)
	if ip == nil {
		// Une resolution a part, chronometree, et sans cache : mesurer un
		// cache ne renseigne sur rien.
		ctx, cancel := context.WithTimeout(context.Background(), timeout)
		start := time.Now()
		addrs, err := (&net.Resolver{}).LookupIP(ctx, ipNetwork(family), name)
		b.DNSus = float64(time.Since(start).Microseconds())
		cancel()
		if err != nil || len(addrs) == 0 {
			b.Err = oneLine(fmt.Sprintf("the name %s does not resolve: %v", name, err), 200)
			return b
		}
		ip = addrs[0]
	}
	b.IP = ip.String()

	addr := net.JoinHostPort(ip.String(), strconv.Itoa(port))
	start := time.Now()
	conn, err := net.DialTimeout(network, addr, timeout)
	b.ConnectUs = float64(time.Since(start).Microseconds())
	if err != nil {
		b.Err = oneLine(fmt.Sprintf("no TCP connection to port %d: %v", port, err), 200)
		return b
	}
	defer conn.Close()
	// Le noyau connait le SYN -> SYN/ACK sans dependre de la charge du
	// processus ; le chronometre applicatif n'est que le repli.
	if rtt, ok := kernelTCPRTT(conn); ok && rtt > 0 {
		b.ConnectUs = rtt
	}
	if !tlsPorts[port] {
		return b
	}
	conn.SetDeadline(time.Now().Add(timeout))
	// InsecureSkipVerify : on mesure une duree, pas une confiance. La
	// validite du certificat est le travail de la surveillance des
	// certificats, qui la verifie pour de bon.
	tc := tls.Client(conn, &tls.Config{ServerName: name, InsecureSkipVerify: true}) //nolint:gosec
	start = time.Now()
	if err := tc.HandshakeContext(context.Background()); err != nil {
		b.Err = oneLine(fmt.Sprintf("the TLS handshake did not complete: %v", err), 200)
		return b
	}
	b.TLSus = float64(time.Since(start).Microseconds())
	return b
}

func ipNetwork(family int) string {
	switch family {
	case 4:
		return "ip4"
	case 6:
		return "ip6"
	}
	return "ip"
}

func (s *Store) saveBreakdown(b Breakdown) {
	s.cfg.Exec(
		`INSERT INTO tcp_breakdown(target_id,ts,dns_us,connect_us,tls_us,ip,err)
		 VALUES(?,?,?,?,?,?,?)
		 ON CONFLICT(target_id) DO UPDATE SET
		   ts=excluded.ts, dns_us=excluded.dns_us, connect_us=excluded.connect_us,
		   tls_us=excluded.tls_us, ip=excluded.ip, err=excluded.err`,
		b.TargetID, b.TS, b.DNSus, b.ConnectUs, b.TLSus, b.IP, b.Err)
}

// Breakdowns renvoie la derniere decomposition de chaque cible TCP, et une
// ligne vide pour celles qui n'ont pas encore ete decomposees : « rien a
// signaler » et « pas encore regarde » ne sont pas la meme chose.
func (s *Store) Breakdowns() []Breakdown {
	last := map[int64]Breakdown{}
	rows, err := s.cfg.Query(
		`SELECT target_id,ts,dns_us,connect_us,tls_us,ip,err FROM tcp_breakdown`)
	if err == nil {
		for rows.Next() {
			var b Breakdown
			if rows.Scan(&b.TargetID, &b.TS, &b.DNSus, &b.ConnectUs, &b.TLSus,
				&b.IP, &b.Err) == nil {
				last[b.TargetID] = b
			}
		}
		rows.Close()
	}
	all, err := s.ActiveTargets()
	if err != nil {
		return nil
	}
	var out []Breakdown
	for _, t := range all {
		if t.Proto != "tcp" || t.Port <= 0 || t.DCCheck != "" {
			continue
		}
		b := last[t.ID]
		b.TargetID, b.Title, b.Host, b.Port = t.ID, t.Title, t.Host, t.Port
		out = append(out, b)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Title < out[j].Title })
	return out
}

// ------------------------------------------------------------------ API
//
// Reserve au back-office. Une decomposition nomme l'adresse reellement
// atteinte et le temps que met chaque etape : c'est du diagnostic, pas de
// la publication.

func (a *API) BreakdownRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/admin/breakdown", a.auth(a.breakdownList))
	mux.HandleFunc("POST /api/v1/admin/breakdown/{id}", a.auth(a.breakdownRun))
}

func (a *API) breakdownList(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, map[string]any{"breakdowns": a.store.Breakdowns()})
}

func (a *API) breakdownRun(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, 400, "bad id")
		return
	}
	t, err := a.store.TargetByID(id)
	if err != nil {
		writeErr(w, 404, "target not found")
		return
	}
	if t.Proto != "tcp" || t.Port <= 0 {
		writeErr(w, 400, "a breakdown needs a TCP target with a port: there is no connection "+
			"to time on an ICMP target, and no handshake either")
		return
	}
	timeout := time.Duration(t.TimeoutMs) * time.Millisecond
	if timeout < time.Second {
		timeout = 2 * time.Second
	}
	b := measureBreakdown(t.Host, t.Port, t.Family, timeout)
	b.TargetID = id
	a.store.saveBreakdown(b)
	b.Title, b.Host, b.Port = t.Title, t.Host, t.Port
	writeJSON(w, b)
}
