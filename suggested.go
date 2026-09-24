package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// A short catalogue of well-known public services, so that a fresh
// instance shows something useful in two clicks. Only operators of
// anycast services that are publicly documented and expect to be
// reached from anywhere, in their IPv4 and IPv6 form. Nothing is added
// without the administrator ticking it.

type suggestedTarget struct {
	Key      string `json:"key"`
	Group    string `json:"group"`
	GroupFR  string `json:"group_fr"`
	Title    string `json:"title"`
	Host     string `json:"host"`
	Proto    string `json:"proto"`
	Port     int    `json:"port,omitempty"`
	Family   int    `json:"family"`
	Note     string `json:"note"`
	Existing bool   `json:"existing"` // already configured on this instance
}

var suggestedCatalogue = []suggestedTarget{
	{Key: "cf4", Group: "Public resolvers", GroupFR: "Résolveurs publics", Title: "Cloudflare DNS", Host: "1.1.1.1", Proto: "icmp", Family: 4, Note: "Anycast resolver, useful as a general reference"},
	{Key: "cf6", Group: "Public resolvers", GroupFR: "Résolveurs publics", Title: "Cloudflare DNS (IPv6)", Host: "2606:4700:4700::1111", Proto: "icmp", Family: 6, Note: "Same service over IPv6: compares both stacks"},
	{Key: "g4", Group: "Public resolvers", GroupFR: "Résolveurs publics", Title: "Google Public DNS", Host: "8.8.8.8", Proto: "icmp", Family: 4},
	{Key: "g6", Group: "Public resolvers", GroupFR: "Résolveurs publics", Title: "Google Public DNS (IPv6)", Host: "2001:4860:4860::8888", Proto: "icmp", Family: 6},
	{Key: "q4", Group: "Public resolvers", GroupFR: "Résolveurs publics", Title: "Quad9", Host: "9.9.9.9", Proto: "icmp", Family: 4},
	{Key: "q6", Group: "Public resolvers", GroupFR: "Résolveurs publics", Title: "Quad9 (IPv6)", Host: "2620:fe::fe", Proto: "icmp", Family: 6},
	{Key: "od4", Group: "Public resolvers", GroupFR: "Résolveurs publics", Title: "OpenDNS", Host: "208.67.222.222", Proto: "icmp", Family: 4},

	{Key: "cfhttp", Group: "Reference services", GroupFR: "Services de référence", Title: "Cloudflare, HTTPS", Host: "one.one.one.one", Proto: "tcp", Port: 443, Family: 4, Note: "TCP handshake: shows what a browser experiences"},
	{Key: "ghhttp", Group: "Reference services", GroupFR: "Services de référence", Title: "GitHub, HTTPS", Host: "github.com", Proto: "tcp", Port: 443, Family: 0},
	{Key: "wiki", Group: "Reference services", GroupFR: "Services de référence", Title: "Wikipedia, HTTPS", Host: "www.wikipedia.org", Proto: "tcp", Port: 443, Family: 0},

	// University time servers from the RENATER list of French NTP servers,
	// all with a stable address. A pool name is deliberately absent: it
	// points at a different machine every few minutes, so its graph mixes
	// servers and the silent ones look like packet loss.
	{Key: "ntp-jussieu", Group: "University time servers", GroupFR: "Serveurs de temps universitaires", Title: "Sorbonne Université (Jussieu)", Host: "ntp1.jussieu.fr", Proto: "icmp", Family: 4, Note: "Stratum 1, GPS reference; listed as open access"},
	{Key: "ntp-lyon", Group: "University time servers", GroupFR: "Serveurs de temps universitaires", Title: "Université Lyon 1", Host: "ntp.univ-lyon1.fr", Proto: "icmp", Family: 4, Note: "Stratum 2, open access"},
	{Key: "ntp-lyon6", Group: "University time servers", GroupFR: "Serveurs de temps universitaires", Title: "Université Lyon 1 (IPv6)", Host: "ntp.univ-lyon1.fr", Proto: "icmp", Family: 6, Note: "The same server over IPv6: compares both stacks on one path"},
	{Key: "ntp-caen", Group: "University time servers", GroupFR: "Serveurs de temps universitaires", Title: "Université de Caen", Host: "ntp.unicaen.fr", Proto: "icmp", Family: 4, Note: "Stratum 2, open access"},
	{Key: "ntp-nice", Group: "University time servers", GroupFR: "Serveurs de temps universitaires", Title: "Université Côte d'Azur (Nice)", Host: "ntp.unice.fr", Proto: "icmp", Family: 4, Note: "Stratum 2, open access"},
}

// Reasonable settings for a first target: a gentle burst that fits well
// within a minute and does not trip the ICMP rate limits of home routers.
func (s suggestedTarget) target(categoryID int64) *Target {
	return &Target{
		CategoryID: categoryID, Slug: "sug-" + s.Key, Title: s.Title, Host: s.Host,
		Proto: s.Proto, Port: s.Port, Family: s.Family,
		IntervalS: 60, Packets: 10, SpacingMs: 200, TimeoutMs: 1500,
		Public: true, Enabled: true,
	}
}

func (a *API) SuggestedRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/admin/suggested", a.auth(a.suggestedGet))
	mux.HandleFunc("POST /api/v1/admin/suggested", a.auth(a.suggestedPost))
}

// suggestedGet marks the entries already configured, so the back-office
// can show them as added rather than offering them twice.
func (a *API) suggestedGet(w http.ResponseWriter, r *http.Request) {
	have := map[string]bool{}
	if list, err := a.store.ActiveTargets(); err == nil {
		for _, t := range list {
			have[strings.ToLower(t.Host)+"|"+t.Proto] = true
		}
	}
	out := make([]suggestedTarget, 0, len(suggestedCatalogue))
	for _, s := range suggestedCatalogue {
		s.Existing = have[strings.ToLower(s.Host)+"|"+s.Proto]
		out = append(out, s)
	}
	writeJSON(w, out)
}

// suggestedPost adds the chosen entries, creating their category if
// needed, and measures them right away.
func (a *API) suggestedPost(w http.ResponseWriter, r *http.Request) {
	var in struct {
		Keys []string `json:"keys"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	wanted := map[string]bool{}
	for _, k := range in.Keys {
		wanted[k] = true
	}
	cats, err := a.store.Tree(false)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	byName := map[string]int64{}
	for _, c := range cats {
		byName[strings.ToLower(c.MenuEN)] = c.ID
	}
	added, skipped := 0, 0
	for _, s := range suggestedCatalogue {
		if !wanted[s.Key] {
			continue
		}
		id, ok := byName[strings.ToLower(s.Group)]
		if !ok {
			slug := strings.ReplaceAll(strings.ToLower(s.Group), " ", "-")
			if id, err = a.store.CreateCategory(slug, s.GroupFR, s.Group, true); err != nil {
				writeErr(w, 400, fmt.Sprintf("category %q: %v", s.Group, err))
				return
			}
			byName[strings.ToLower(s.Group)] = id
		}
		tid, err := a.store.CreateTarget(s.target(id))
		if err != nil {
			skipped++
			continue
		}
		checkRequests.Push(tid)
		added++
	}
	go ovCache.build()
	writeJSON(w, map[string]any{"added": added, "skipped": skipped})
}
