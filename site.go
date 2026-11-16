package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
)

// Site porte les metadonnees de l'instance : qui l'exploite, comment
// le joindre. C'est l'equivalent des champs owner et contact de
// SmokePing, en plus complet, parce qu'une sonde publique sans
// exploitant identifiable n'inspire pas confiance et ne sert a rien
// quand un confrere veut signaler une anomalie.
type ContactLink struct {
	Label string `json:"label"`
	URL   string `json:"url"`
}

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
	SearchIndex bool `json:"search_index"`
	ContactForm bool `json:"contact_form"`
	// ContactMode : "form" (defaut), "email" (adresse assemblee en
	// JavaScript, illisible par un moissonneur), "links" (outils externes)
	// ou "off". Captcha : epreuve de calcul integree, sans service tiers.
	ContactMode   string        `json:"contact_mode,omitempty"`
	ContactLinks  []ContactLink `json:"contact_links,omitempty"`
	Captcha       bool          `json:"captcha,omitempty"`
	ShowEmail     bool          `json:"show_email"`
	ContactNotify string        `json:"contact_notify,omitempty"` // never public
	URL           string        `json:"url"`
	// FedURL : l'adresse publique de cette instance, celle qu'un pair doit
	// pouvoir joindre. Distincte de URL, qui est le site de l'operateur et
	// peut etre un tout autre domaine.
	//
	// It exists because federation.base_url in the configuration file was
	// the only way to set it: an operator who installed without
	// --public-url could not pair and had nowhere in the interface to fix
	// it. The configuration file still wins when both are set, exactly as
	// it does for the AS number and the organisation.
	FedURL      string `json:"fed_url"`
	PeeringDB   string `json:"peeringdb"`
	Location    string `json:"location"`
	Description string `json:"description"`
	Legal       string `json:"legal"`
	Timezone    string `json:"timezone"`
	DefaultLang string `json:"default_lang"`
	// PublicTraceroutes shows anomaly traceroutes on public pages. Off by
	// default: hops reveal the inside of the operator's network.
	PublicTraceroutes bool `json:"public_traceroutes"`
	// PublicAvailability publishes the availability figure on the target
	// pages. On by default. It is the one number on those pages a reader
	// is likely to take for a commitment, whatever the two warnings next
	// to it say, so an operator who does not want to be read that way can
	// stop publishing it rather than explain it.
	PublicAvailability bool `json:"public_availability"`
	// HiddenPages lists the optional public pages the operator took off the
	// site (pages.go). It names what is hidden rather than what is shown, so
	// an instance configured before it existed hides nothing.
	HiddenPages []string `json:"hidden_pages,omitempty"`
	// Thresholds applied to every target that does not set its own.
	Thresholds Thresholds `json:"thresholds,omitempty"`
	// MaskAddresses : sur les pages publiques, une adresse IP n'est montree
	// que par son reseau. Les noms ne sont pas touches : un nom n'est pas
	// une adresse, et c'est lui qui dit de quel service on parle.
	MaskAddresses bool `json:"mask_addresses"`
}

func defaultSite() Site {
	return Site{
		Title:         "Latency monitoring",
		Org:           "",
		Owner:         "",
		Email:         "",
		ContactForm:   true,
		ContactMode:   "form",
		SearchIndex:   true,
		Timezone:      "Europe/Paris",
		DefaultLang:   "en",
		Description:   "Latency and packet loss measured from our network to public destinations.",
		Thresholds:    DefaultThresholds(),
		MaskAddresses: true,
		// Publishing stays the default: the figure is honest, and what
		// this setting governs is how it can be read.
		PublicAvailability: true,
	}
}

// ContactModeOf keeps older instances working: they only had the
// contact_form switch, so an unset mode follows it.
func ContactModeOf(site Site) string {
	switch site.ContactMode {
	case "form", "email", "links", "off":
		return site.ContactMode
	}
	if site.ContactForm {
		return "form"
	}
	return "off"
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
			// Added after instances were already running. The default is on,
			// and an upgrade adopts it: the change is in the direction of
			// showing less, which is never the surprising direction.
			if !strings.Contains(raw, `"mask_addresses"`) {
				site.MaskAddresses = true
			}
			// Same reasoning the other way: an instance that was already
			// showing the figure keeps showing it after an upgrade, and
			// only an explicit choice takes it away.
			if !strings.Contains(raw, `"public_availability"`) {
				site.PublicAvailability = true
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
		// In "email" mode the address is what the page shows, assembled in
		// JavaScript so a harvester reading the HTML finds nothing.
		if !site.ShowEmail && ContactModeOf(site) != "email" {
			site.Email = ""
		}
	}
	// Which public pages make sense on this instance. An operator running
	// no federation should not see Federation and Pairing tabs leading to
	// empty pages, and a network page without an AS number says nothing.
	// Beyond that, the operator may hide any of the three (pages.go).
	out := map[string]any{}
	b, _ := json.Marshal(site)
	json.Unmarshal(b, &out)
	out["pages"] = map[string]bool{
		"federation": a.fed != nil && a.fed.Enabled() && !site.Hides("federation"),
		"pairing":    a.fed != nil && a.fed.Enabled() && site.ASN != "" && !site.Hides("pairing"),
		"network":    site.ASN != "" && !site.Hides("network"),
	}
	// Who asks changes the answer: an authenticated caller gets the NOC
	// phone, the address that receives contact notifications and the
	// operator's own address. Announced as publicly cacheable with no
	// Vary, that answer may be kept by a shared cache and handed to a
	// visitor — which is the one thing the stripping above is for.
	//
	// It also made the back-office look broken: it reads this endpoint,
	// and after saving kept showing values the browser had cached ten
	// minutes earlier.
	w.Header().Set("Vary", "Cookie, Authorization")
	if a.authenticated(r) {
		w.Header().Set("Cache-Control", "no-store")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=600")
	}
	writeJSON(w, out)
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
	// Des seuils vides ou incoherents reprennent les valeurs livrees plutot
	// que de rendre chaque cible « ok » en toutes circonstances.
	v.Thresholds = v.Thresholds.orDefaults()
	// An address a peer cannot reach is worse than none: it would be
	// announced in a pairing request and fetched back by whoever is asked.
	// safeFedURL is the same check the pairing code applies to a peer's.
	v.FedURL = strings.TrimSpace(v.FedURL)
	if v.FedURL != "" {
		clean, err := safeFedURL(v.FedURL)
		if err != nil {
			writeErr(w, 400, fmt.Sprintf("instance address: %v", err))
			return
		}
		v.FedURL = clean
	}
	hidden, err := cleanHiddenPages(v.HiddenPages)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	v.HiddenPages = hidden
	if err := a.store.SetSite(v); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, v)
}
