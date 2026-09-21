package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Le service d'informations d'AS decrit le reseau qui heberge l'instance
// et ceux de ses pairs, a partir de deux sources publiques :
//
//   - RIPEstat pour ce qui est observe dans le routage : titulaire, pays,
//     prefixes annonces, voisins vus dans les chemins d'AS ;
//   - PeeringDB pour ce que l'operateur declare : politique de peering,
//     presence aux points d'echange, datacenters, volume de trafic.
//
// Les reponses sont mises en cache 24 h en base. Le perimetre est limite a
// notre AS et a nos pairs approuves : l'instance ne doit pas devenir un
// proxy RIPEstat/PeeringDB ouvert a n'importe quelle requete.

const (
	asnCacheTTL      = 24 * time.Hour
	asnMinRetryDelay = 10 * time.Minute
	ripestatBase     = "https://stat.ripe.net/data/"
	peeringdbBase    = "https://www.peeringdb.com/api/"
)

type Neighbour struct {
	ASN   int    `json:"asn"`
	Name  string `json:"name,omitempty"`
	Power int    `json:"power"`
	V4    int    `json:"v4_peers"`
	V6    int    `json:"v6_peers"`
}

type PDBIX struct {
	Name   string `json:"name"`
	Speed  int    `json:"speed_mbps"`
	IPv4   string `json:"ipv4,omitempty"`
	IPv6   string `json:"ipv6,omitempty"`
	RSPeer bool   `json:"rs_peer"`
}

type PDBFac struct {
	Name    string `json:"name"`
	City    string `json:"city"`
	Country string `json:"country"`
}

type PDBNet struct {
	ID           int      `json:"id"`
	Name         string   `json:"name"`
	AKA          string   `json:"aka,omitempty"`
	Website      string   `json:"website,omitempty"`
	IRRASSet     string   `json:"irr_as_set,omitempty"`
	Types        []string `json:"types,omitempty"`
	Prefixes4    int      `json:"prefixes4"`
	Prefixes6    int      `json:"prefixes6"`
	Traffic      string   `json:"traffic,omitempty"`
	Ratio        string   `json:"ratio,omitempty"`
	Scope        string   `json:"scope,omitempty"`
	Policy       string   `json:"policy,omitempty"`
	PolicyURL    string   `json:"policy_url,omitempty"`
	LookingGlass string   `json:"looking_glass,omitempty"`
	IXs          []PDBIX  `json:"ixs"`
	Facilities   []PDBFac `json:"facilities"`
	URL          string   `json:"url"`
}

type ASNInfo struct {
	ASN          string      `json:"asn"`
	Holder       string      `json:"holder"`
	Country      string      `json:"country,omitempty"`
	Announced    bool        `json:"announced"`
	PrefixesV4   int         `json:"prefixes_v4"`
	PrefixesV6   int         `json:"prefixes_v6"`
	Prefixes     []string    `json:"prefixes"`
	Upstreams    []Neighbour `json:"upstreams"`
	NbUpstreams  int         `json:"nb_upstreams"`
	NbDownstream int         `json:"nb_downstreams"`
	NbNeighbours int         `json:"nb_neighbours"`
	PeeringDB    *PDBNet     `json:"peeringdb"`
	Links        struct {
		RIPEstat  string `json:"ripestat"`
		BGPTools  string `json:"bgptools"`
		PeeringDB string `json:"peeringdb,omitempty"`
	} `json:"links"`
	FetchedAt int64    `json:"fetched_at"`
	Errors    []string `json:"errors,omitempty"`
}

type ASNService struct {
	store  *Store
	fed    *Federation
	client *http.Client
	pdbKey string
	ua     string

	mu       sync.Mutex
	attempts map[string]int64
}

func NewASNService(store *Store, fed *Federation, pdbKey string) *ASNService {
	return &ASNService{
		store: store, fed: fed, pdbKey: pdbKey,
		client:   &http.Client{Timeout: 15 * time.Second},
		ua:       "smokestack/0.1 (+https://github.com/)",
		attempts: map[string]int64{},
	}
}

// normalizeASN accepte "AS64500", "as64500" ou "64500".
func normalizeASN(s string) (string, error) {
	s = strings.TrimSpace(strings.ToUpper(s))
	s = strings.TrimPrefix(s, "AS")
	n, err := strconv.ParseUint(s, 10, 32)
	if err != nil || n == 0 {
		return "", fmt.Errorf("numero d'AS invalide")
	}
	return strconv.FormatUint(n, 10), nil
}

func (s *ASNService) getJSON(url string, pdb bool, out any) error {
	req, err := http.NewRequest("GET", url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("User-Agent", s.ua)
	req.Header.Set("Accept", "application/json")
	if pdb && s.pdbKey != "" {
		req.Header.Set("Authorization", "Api-Key "+s.pdbKey)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 429 {
		return fmt.Errorf("limite de requetes atteinte (429)")
	}
	if resp.StatusCode != 200 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 8<<20)).Decode(out)
}

func ripestat(endpoint, resource string) string {
	return ripestatBase + endpoint + "/data.json?resource=" + resource +
		"&sourceapp=smokestack"
}

// fetch interroge les deux sources. Une source en echec n'empeche pas
// l'autre : les erreurs sont conservees et affichees.
func (s *ASNService) fetch(asn string) *ASNInfo {
	info := &ASNInfo{ASN: asn, FetchedAt: time.Now().Unix(),
		Prefixes: []string{}, Upstreams: []Neighbour{}}
	info.Links.RIPEstat = "https://stat.ripe.net/AS" + asn
	info.Links.BGPTools = "https://bgp.tools/as/" + asn
	res := "AS" + asn

	var ov struct {
		Data struct {
			Holder    string `json:"holder"`
			Announced bool   `json:"announced"`
		} `json:"data"`
	}
	if err := s.getJSON(ripestat("as-overview", res), false, &ov); err != nil {
		info.Errors = append(info.Errors, "RIPEstat as-overview: "+err.Error())
	} else {
		info.Holder, info.Announced = ov.Data.Holder, ov.Data.Announced
	}

	var rc struct {
		Data struct {
			Located []struct {
				Location string `json:"location"`
			} `json:"located_resources"`
		} `json:"data"`
	}
	if err := s.getJSON(ripestat("rir-stats-country", res), false, &rc); err == nil &&
		len(rc.Data.Located) > 0 {
		info.Country = rc.Data.Located[0].Location
	}

	var ap struct {
		Data struct {
			Prefixes []struct {
				Prefix string `json:"prefix"`
			} `json:"prefixes"`
		} `json:"data"`
	}
	if err := s.getJSON(ripestat("announced-prefixes", res), false, &ap); err != nil {
		info.Errors = append(info.Errors, "RIPEstat announced-prefixes: "+err.Error())
	} else {
		for _, p := range ap.Data.Prefixes {
			if strings.Contains(p.Prefix, ":") {
				info.PrefixesV6++
			} else {
				info.PrefixesV4++
			}
			if len(info.Prefixes) < 30 {
				info.Prefixes = append(info.Prefixes, p.Prefix)
			}
		}
	}

	var nb struct {
		Data struct {
			Counts struct {
				Left   int `json:"left"`
				Right  int `json:"right"`
				Unique int `json:"unique"`
			} `json:"neighbour_counts"`
			Neighbours []struct {
				ASN   int    `json:"asn"`
				Type  string `json:"type"`
				Power int    `json:"power"`
				V4    int    `json:"v4_peers"`
				V6    int    `json:"v6_peers"`
			} `json:"neighbours"`
		} `json:"data"`
	}
	if err := s.getJSON(ripestat("asn-neighbours", res), false, &nb); err != nil {
		info.Errors = append(info.Errors, "RIPEstat asn-neighbours: "+err.Error())
	} else {
		info.NbUpstreams = nb.Data.Counts.Left
		info.NbDownstream = nb.Data.Counts.Right
		info.NbNeighbours = nb.Data.Counts.Unique
		var ups []Neighbour
		for _, n := range nb.Data.Neighbours {
			// "left" : voisins observes a gauche de l'AS dans les chemins,
			// donc cote collecteur — en pratique fournisseurs et pairs.
			if n.Type == "left" {
				ups = append(ups, Neighbour{ASN: n.ASN, Power: n.Power, V4: n.V4, V6: n.V6})
			}
		}
		sort.Slice(ups, func(i, j int) bool { return ups[i].Power > ups[j].Power })
		if len(ups) > 6 {
			ups = ups[:6]
		}
		for i := range ups {
			var o struct {
				Data struct {
					Holder string `json:"holder"`
				} `json:"data"`
			}
			if s.getJSON(ripestat("as-overview", "AS"+strconv.Itoa(ups[i].ASN)), false, &o) == nil {
				ups[i].Name = o.Data.Holder
			}
		}
		info.Upstreams = ups
	}

	info.PeeringDB = s.fetchPeeringDB(asn, info)
	return info
}

func (s *ASNService) fetchPeeringDB(asn string, info *ASNInfo) *PDBNet {
	var net struct {
		Data []struct {
			ID        int      `json:"id"`
			Name      string   `json:"name"`
			AKA       string   `json:"aka"`
			Website   string   `json:"website"`
			IRR       string   `json:"irr_as_set"`
			Type      string   `json:"info_type"`
			Types     []string `json:"info_types"`
			P4        *int     `json:"info_prefixes4"`
			P6        *int     `json:"info_prefixes6"`
			Traffic   string   `json:"info_traffic"`
			Ratio     string   `json:"info_ratio"`
			Scope     string   `json:"info_scope"`
			Policy    string   `json:"policy_general"`
			PolicyURL string   `json:"policy_url"`
			LG        string   `json:"looking_glass"`
		} `json:"data"`
	}
	if err := s.getJSON(peeringdbBase+"net?asn="+asn, true, &net); err != nil {
		info.Errors = append(info.Errors, "PeeringDB net: "+err.Error())
		return nil
	}
	if len(net.Data) == 0 {
		// Absence de fiche : pas une erreur, juste un reseau non declare.
		return nil
	}
	d := net.Data[0]
	p := &PDBNet{
		ID: d.ID, Name: d.Name, AKA: d.AKA, Website: d.Website,
		IRRASSet: d.IRR, Traffic: d.Traffic, Ratio: d.Ratio, Scope: d.Scope,
		Policy: d.Policy, PolicyURL: d.PolicyURL, LookingGlass: d.LG,
		IXs: []PDBIX{}, Facilities: []PDBFac{},
		URL: "https://www.peeringdb.com/net/" + strconv.Itoa(d.ID),
	}
	p.Types = d.Types
	if len(p.Types) == 0 && d.Type != "" {
		p.Types = []string{d.Type}
	}
	if d.P4 != nil {
		p.Prefixes4 = *d.P4
	}
	if d.P6 != nil {
		p.Prefixes6 = *d.P6
	}
	info.Links.PeeringDB = p.URL

	var ix struct {
		Data []struct {
			Name  string  `json:"name"`
			Speed int     `json:"speed"`
			IPv4  *string `json:"ipaddr4"`
			IPv6  *string `json:"ipaddr6"`
			RS    bool    `json:"is_rs_peer"`
		} `json:"data"`
	}
	if err := s.getJSON(peeringdbBase+"netixlan?net_id="+strconv.Itoa(d.ID), true, &ix); err != nil {
		info.Errors = append(info.Errors, "PeeringDB netixlan: "+err.Error())
	} else {
		for _, x := range ix.Data {
			e := PDBIX{Name: x.Name, Speed: x.Speed, RSPeer: x.RS}
			if x.IPv4 != nil {
				e.IPv4 = *x.IPv4
			}
			if x.IPv6 != nil {
				e.IPv6 = *x.IPv6
			}
			p.IXs = append(p.IXs, e)
		}
		sort.Slice(p.IXs, func(i, j int) bool { return p.IXs[i].Speed > p.IXs[j].Speed })
	}

	var fac struct {
		Data []struct {
			Name    string `json:"name"`
			City    string `json:"city"`
			Country string `json:"country"`
		} `json:"data"`
	}
	if err := s.getJSON(peeringdbBase+"netfac?net_id="+strconv.Itoa(d.ID), true, &fac); err != nil {
		info.Errors = append(info.Errors, "PeeringDB netfac: "+err.Error())
	} else {
		for _, f := range fac.Data {
			p.Facilities = append(p.Facilities, PDBFac{Name: f.Name, City: f.City, Country: f.Country})
		}
	}
	return p
}

// ------------------------------------------------------------------ cache

func (s *ASNService) cached(asn string) *ASNInfo {
	raw := s.store.Setting("asn:"+asn, "")
	if raw == "" {
		return nil
	}
	var info ASNInfo
	if json.Unmarshal([]byte(raw), &info) != nil {
		return nil
	}
	return &info
}

func (s *ASNService) Refresh(asn string) (*ASNInfo, error) {
	s.mu.Lock()
	last := s.attempts[asn]
	now := time.Now().Unix()
	if now-last < int64(asnMinRetryDelay.Seconds()) {
		s.mu.Unlock()
		if c := s.cached(asn); c != nil {
			return c, nil
		}
		return nil, fmt.Errorf("actualisation trop rapprochee, reessayez plus tard")
	}
	s.attempts[asn] = now
	s.mu.Unlock()

	info := s.fetch(asn)
	// Si tout a echoue, on garde l'ancien cache plutot que d'ecraser
	// des donnees valides par un resultat vide.
	if info.Holder == "" && info.PeeringDB == nil {
		if c := s.cached(asn); c != nil {
			c.Errors = info.Errors
			return c, nil
		}
	}
	b, _ := json.Marshal(info)
	s.store.SetSetting("asn:"+asn, string(b))
	return info, nil
}

func (s *ASNService) Get(asn string) (*ASNInfo, error) {
	if c := s.cached(asn); c != nil &&
		time.Since(time.Unix(c.FetchedAt, 0)) < asnCacheTTL {
		return c, nil
	}
	return s.Refresh(asn)
}

// ourASN renvoie l'AS de l'instance, pris dans la page editeur puis
// dans la configuration de federation.
func (s *ASNService) ourASN() string {
	if n, err := normalizeASN(s.store.Site().ASN); err == nil {
		return n
	}
	if s.fed != nil {
		if n, err := normalizeASN(s.fed.Profile().ASN); err == nil {
			return n
		}
	}
	return ""
}

// allowed limite le service a notre AS et a nos pairs approuves.
func (s *ASNService) allowed(asn string) bool {
	if asn == s.ourASN() {
		return true
	}
	if s.fed == nil {
		return false
	}
	for _, p := range s.fed.trustedPeers() {
		if n, err := normalizeASN(p.ASN); err == nil && n == asn {
			return true
		}
	}
	return false
}

func (s *ASNService) Loop(stop <-chan struct{}) {
	tick := time.NewTicker(6 * time.Hour)
	defer tick.Stop()
	run := func() {
		targets := []string{}
		if a := s.ourASN(); a != "" {
			targets = append(targets, a)
		}
		if s.fed != nil {
			for _, p := range s.fed.trustedPeers() {
				if n, err := normalizeASN(p.ASN); err == nil {
					targets = append(targets, n)
				}
			}
		}
		for _, a := range targets {
			if _, err := s.Get(a); err != nil {
				log.Printf("infos AS%s: %v", a, err)
			}
			time.Sleep(3 * time.Second) // courtoisie envers les API publiques
		}
	}
	time.Sleep(20 * time.Second)
	run()
	for {
		select {
		case <-stop:
			return
		case <-tick.C:
			run()
		}
	}
}

// ------------------------------------------------------------------ routes

func (a *API) ASNRoutes(mux *http.ServeMux) {
	if a.asn == nil {
		return
	}
	mux.HandleFunc("GET /api/v1/asn", a.asnOurs)
	mux.HandleFunc("GET /api/v1/asn/{asn}", a.asnOne)
	mux.HandleFunc("POST /api/v1/admin/asn/refresh", a.need(RoleAdmin, a.asnRefresh))
}

func (a *API) asnOurs(w http.ResponseWriter, r *http.Request) {
	asn := a.asn.ourASN()
	if asn == "" {
		writeErr(w, 404, "numero d'AS non renseigne")
		return
	}
	info, err := a.asn.Get(asn)
	if err != nil {
		writeErr(w, 503, err.Error())
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=900")
	writeJSON(w, info)
}

func (a *API) asnOne(w http.ResponseWriter, r *http.Request) {
	asn, err := normalizeASN(r.PathValue("asn"))
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if !a.asn.allowed(asn) {
		writeErr(w, 404, "AS hors du perimetre de cette instance")
		return
	}
	info, err := a.asn.Get(asn)
	if err != nil {
		writeErr(w, 503, err.Error())
		return
	}
	w.Header().Set("Cache-Control", "public, max-age=900")
	writeJSON(w, info)
}

func (a *API) asnRefresh(w http.ResponseWriter, r *http.Request, u *User) {
	asn := a.asn.ourASN()
	if v := r.URL.Query().Get("asn"); v != "" {
		if n, err := normalizeASN(v); err == nil && a.asn.allowed(n) {
			asn = n
		}
	}
	if asn == "" {
		writeErr(w, 400, "numero d'AS non renseigne")
		return
	}
	info, err := a.asn.Refresh(asn)
	if err != nil {
		writeErr(w, 429, err.Error())
		return
	}
	a.store.Audit(u, clientIP(r), "asn_refresh", "asn", asn)
	writeJSON(w, info)
}
