package main

import (
	"encoding/json"
	"net/http"
)

// Site porte les metadonnees de l'instance : qui l'exploite, comment
// le joindre. C'est l'equivalent des champs owner et contact de
// SmokePing, en plus complet, parce qu'une sonde publique sans
// exploitant identifiable n'inspire pas confiance et ne sert a rien
// quand un confrere veut signaler une anomalie.
type Site struct {
	Title       string `json:"title"`
	Org         string `json:"org"`
	ASN         string `json:"asn"`
	Owner       string `json:"owner"`
	Email       string `json:"email"`
	NOCEmail    string `json:"noc_email"`
	NOCPhone    string `json:"noc_phone"`
	URL         string `json:"url"`
	PeeringDB   string `json:"peeringdb"`
	Location    string `json:"location"`
	Description string `json:"description"`
	Legal       string `json:"legal"`
	Timezone    string `json:"timezone"`
	DefaultLang string `json:"default_lang"`
}

func defaultSite() Site {
	return Site{
		Title:       "Supervision de latence",
		Org:         "",
		Owner:       "",
		Email:       "",
		Timezone:    "Europe/Paris",
		DefaultLang: "en",
		Description: "Mesures de latence et de perte depuis notre réseau vers des destinations publiques.",
	}
}

func (s *Store) Site() Site {
	site := defaultSite()
	if raw := s.Setting("site", ""); raw != "" {
		var v Site
		if err := json.Unmarshal([]byte(raw), &v); err == nil {
			site = v
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
	// Le telephone du NOC ne sort qu'aux instances federees
	// authentifiees, jamais sur la page publique.
	if r.Header.Get("Authorization") == "" {
		site.NOCPhone = ""
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
