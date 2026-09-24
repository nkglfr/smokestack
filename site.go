package main

import (
	"encoding/json"
	"net/http"
	"strings"
)

// Site porte les metadonnees de l'instance : qui l'exploite, comment
// le joindre. C'est l'equivalent des champs owner et contact de
// SmokePing, en plus complet, parce qu'une sonde publique sans
// exploitant identifiable n'inspire pas confiance et ne sert a rien
// quand un confrere veut signaler une anomalie.
type Site struct {
	Title    string `json:"title"`
	Org      string `json:"org"`
	ASN      string `json:"asn"`
	Owner    string `json:"owner"`
	Email    string `json:"email"`
	NOCEmail string `json:"noc_email"`
	NOCPhone string `json:"noc_phone"`
	// Contact: the form keeps the operator's address private. ShowEmail
	// puts it back on the public page for those who prefer that.
	// SearchIndex: let search engines index the public pages.
	SearchIndex   bool   `json:"search_index"`
	ContactForm   bool   `json:"contact_form"`
	ShowEmail     bool   `json:"show_email"`
	ContactNotify string `json:"contact_notify,omitempty"` // never public
	URL           string `json:"url"`
	PeeringDB     string `json:"peeringdb"`
	Location      string `json:"location"`
	Description   string `json:"description"`
	Legal         string `json:"legal"`
	Timezone      string `json:"timezone"`
	DefaultLang   string `json:"default_lang"`
	// PublicTraceroutes shows anomaly traceroutes on public pages. Off by
	// default: hops reveal the inside of the operator's network.
	PublicTraceroutes bool `json:"public_traceroutes"`
}

func defaultSite() Site {
	return Site{
		Title:       "Latency monitoring",
		Org:         "",
		Owner:       "",
		Email:       "",
		ContactForm: true,
		SearchIndex: true,
		Timezone:    "Europe/Paris",
		DefaultLang: "en",
		Description: "Latency and packet loss measured from our network to public destinations.",
	}
}

func (s *Store) Site() Site {
	site := defaultSite()
	if raw := s.Setting("site", ""); raw != "" {
		var v Site
		if err := json.Unmarshal([]byte(raw), &v); err == nil {
			site = v
			// Settings added after an instance was first configured keep
			// their default instead of the zero value: an upgrade must not
			// silently de-index a site or switch its contact form off.
			if !strings.Contains(raw, `"search_index"`) {
				site.SearchIndex = true
			}
			if !strings.Contains(raw, `"contact_form"`) {
				site.ContactForm = true
			}
		}
	}
	return site
}

func (s *Store) SetSite(v Site) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.SetSetting("site", string(b))
}

func (a *API) siteGet(w http.ResponseWriter, r *http.Request) {
	site := a.store.Site()
	// The NOC phone only goes to authenticated federated instances, never
	// to the public page. Same for anything about the contact form: the
	// address that receives the notifications, and the public address when
	// the operator chose to keep it private behind the form.
	if !a.authenticated(r) {
		site.NOCPhone = ""
		site.ContactNotify = ""
		if !site.ShowEmail {
			site.Email = ""
		}
	}
	w.Header().Set("Cache-Control", "public, max-age=600")
	writeJSON(w, site)
}

func (a *API) sitePut(w http.ResponseWriter, r *http.Request) {
	var v Site
	if err := json.NewDecoder(r.Body).Decode(&v); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if v.Title == "" {
		v.Title = defaultSite().Title
	}
	if err := a.store.SetSite(v); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, v)
}
