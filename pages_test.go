package main

import (
	"encoding/json"
	"io/fs"
	"maps"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"
	"time"
)

// pagesAPI is an instance that federates and has an AS number, so every
// optional page is published unless the operator hides it.
func pagesAPI(t *testing.T, hidden ...string) *API {
	t.Helper()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	fed, err := NewFederation(store, t.TempDir(), FedConfig{Enabled: true, ASN: "AS64500",
		Org: "Example Telecom", BaseURL: "https://latency.example.net"})
	if err != nil {
		t.Fatal(err)
	}
	site := store.Site()
	site.ASN, site.URL, site.HiddenPages = "AS64500", "https://latency.example.net", hidden
	if err := store.SetSite(site); err != nil {
		t.Fatal(err)
	}
	return &API{store: store, fed: fed, token: "secret", probeID: 1}
}

func TestHiddenPagesSetting(t *testing.T) {
	api := pagesAPI(t)
	put := func(body string) int {
		w := httptest.NewRecorder()
		api.sitePut(w, httptest.NewRequest("PUT", "/api/v1/admin/site", strings.NewReader(body)))
		return w.Code
	}

	// An instance configured before the setting existed hides nothing.
	api.store.SetSetting("site", `{"title":"Older instance","asn":"AS64500"}`)
	if got := api.store.Site().HiddenPages; len(got) != 0 {
		t.Errorf("an upgrade must not hide a page somebody links to: %v", got)
	}

	if code := put(`{"title":"Example","hidden_pages":["pairing","federation","pairing"]}`); code != 200 {
		t.Fatalf("saving hidden pages: HTTP %d", code)
	}
	if got := api.store.Site().HiddenPages; !slices.Equal(got, []string{"federation", "pairing"}) {
		t.Errorf("hidden pages are kept once each, in menu order: %v", got)
	}

	// About is where the operator can be found: it is not a choice.
	for _, page := range []string{"about", "home", "status"} {
		if code := put(`{"title":"Example","hidden_pages":["` + page + `"]}`); code != 400 {
			t.Errorf("hiding %q: HTTP %d, want 400", page, code)
		}
	}
	if got := api.store.Site().HiddenPages; !slices.Equal(got, []string{"federation", "pairing"}) {
		t.Errorf("a refused choice must leave the saved one alone: %v", got)
	}

	if code := put(`{"title":"Example","hidden_pages":[]}`); code != 200 {
		t.Fatalf("showing every page again: HTTP %d", code)
	}
	if got := api.store.Site().HiddenPages; len(got) != 0 {
		t.Errorf("every page should be back: %v", got)
	}
}

func TestSitePagesFollowHiddenPages(t *testing.T) {
	type answer struct {
		Pages  map[string]bool `json:"pages"`
		Hidden []string        `json:"hidden_pages"`
	}
	get := func(api *API, auth bool) answer {
		t.Helper()
		r := httptest.NewRequest("GET", "/api/v1/site", nil)
		if auth {
			r.Header.Set("Authorization", "Bearer secret")
		}
		w := httptest.NewRecorder()
		api.siteGet(w, r)
		var out answer
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}

	all := get(pagesAPI(t), false).Pages
	if !all["federation"] || !all["network"] || !all["pairing"] {
		t.Errorf("nothing hidden, every page is published: %v", all)
	}

	api := pagesAPI(t, "network", "pairing")
	if got := get(api, false).Pages; !got["federation"] || got["network"] || got["pairing"] {
		t.Errorf("network and pairing hidden: %v", got)
	}
	if got := get(api, true).Hidden; !slices.Equal(got, []string{"network", "pairing"}) {
		t.Errorf("the back-office reads the choice back to show it: %v", got)
	}
}

// With nothing hidden, the pages map is what the configuration alone made
// it before the setting existed, and the public answer has no new key.
func TestSitePagesFollowConfiguration(t *testing.T) {
	cases := []struct {
		name    string
		fed     bool
		enabled bool
		asn     string
		want    map[string]bool
	}{
		{"no federation, no AS number", false, false, "",
			map[string]bool{"federation": false, "network": false, "pairing": false}},
		{"no federation, an AS number", false, false, "AS64500",
			map[string]bool{"federation": false, "network": true, "pairing": false}},
		{"federation off, an AS number", true, false, "AS64500",
			map[string]bool{"federation": false, "network": true, "pairing": false}},
		{"federation on, no AS number", true, true, "",
			map[string]bool{"federation": true, "network": false, "pairing": false}},
		{"federation on, an AS number", true, true, "AS64500",
			map[string]bool{"federation": true, "network": true, "pairing": true}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			store, err := OpenStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(store.Close)
			api := &API{store: store, token: "secret", probeID: 1}
			if c.fed {
				if api.fed, err = NewFederation(store, t.TempDir(), FedConfig{Enabled: c.enabled}); err != nil {
					t.Fatal(err)
				}
			}
			site := store.Site()
			site.ASN = c.asn
			if err := store.SetSite(site); err != nil {
				t.Fatal(err)
			}
			w := httptest.NewRecorder()
			api.siteGet(w, httptest.NewRequest("GET", "/api/v1/site", nil))
			var out map[string]json.RawMessage
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			if _, ok := out["hidden_pages"]; ok {
				t.Error("an instance that hides nothing announces hidden_pages")
			}
			var pages map[string]bool
			if err := json.Unmarshal(out["pages"], &pages); err != nil {
				t.Fatal(err)
			}
			if !maps.Equal(pages, c.want) {
				t.Errorf("pages %v, want %v", pages, c.want)
			}
		})
	}
}

// hiddenPagesMux answers these paths the way main does — the public pages,
// here without their injected head, the static files and the federation
// APIs — behind the same front chain.
func hiddenPagesMux(t *testing.T, api *API) http.Handler {
	t.Helper()
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	api.FedRoutes(mux)
	api.PairingRoutes(mux)
	for route, file := range map[string]string{
		"GET /federation": "federation.html", "GET /network": "network.html",
		"GET /pairing": "pairing.html", "GET /pairing/": "pairing.html", "GET /about": "about.html",
	} {
		mux.HandleFunc(route, func(w http.ResponseWriter, r *http.Request) {
			http.ServeFileFS(w, r, sub, file)
		})
	}
	mux.Handle("GET /", http.FileServer(http.FS(sub)))
	return api.frontHandler(mux)
}

func pageStatus(h http.Handler, method, path string) int {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest(method, path, nil))
	return w.Code
}

func TestHiddenPageAnswers404(t *testing.T) {
	paths := map[string][]string{
		"federation": {"/federation", "/federation.html"},
		"network":    {"/network", "/network.html"},
		"pairing":    {"/pairing", "/pairing/", "/pairing/anything", "/pairing.html"},
	}
	shown := hiddenPagesMux(t, pagesAPI(t))
	for _, list := range paths {
		for _, p := range list {
			if code := pageStatus(shown, "GET", p); code != 200 {
				t.Errorf("nothing hidden, GET %s: HTTP %d", p, code)
			}
		}
	}

	h := hiddenPagesMux(t, pagesAPI(t, hideablePages...))
	for page, list := range paths {
		for _, p := range list {
			for _, method := range []string{"GET", "HEAD"} {
				if code := pageStatus(h, method, p); code != 404 {
					t.Errorf("%s hidden, %s %s: HTTP %d, want 404", page, method, p, code)
				}
			}
		}
	}
	for _, p := range []string{"/about", "/about.html", "/app.js"} {
		if code := pageStatus(h, "GET", p); code != 200 {
			t.Errorf("hiding pages must leave %s alone: HTTP %d", p, code)
		}
	}
}

// The gate went in between the compression and the mux: pages and APIs are
// still compressed, and a hidden page is still a 404 when they are.
func TestFrontHandlerCompresses(t *testing.T) {
	h := hiddenPagesMux(t, pagesAPI(t, "network"))
	for path, want := range map[string]int{
		"/about": 200, "/app.js": 200, "/api/v1/fed/profile": 200, "/network": 404,
	} {
		r := httptest.NewRequest("GET", path, nil)
		r.Header.Set("Accept-Encoding", "gzip")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != want || w.Header().Get("Content-Encoding") != "gzip" {
			t.Errorf("GET %s with gzip: HTTP %d, Content-Encoding %q; want %d, gzip",
				path, w.Code, w.Header().Get("Content-Encoding"), want)
		}
	}
}

// Peers call these whatever this instance publishes: hiding a page must not
// quietly end a federation or break a pairing.
func TestFederationAPIOutlivesHiddenPages(t *testing.T) {
	h := hiddenPagesMux(t, pagesAPI(t, hideablePages...))
	for _, p := range []string{"/api/v1/fed/profile", "/api/v1/fed/peers", "/api/v1/fed/pairing/info"} {
		if code := pageStatus(h, "GET", p); code != 200 {
			t.Errorf("with every page hidden, GET %s: HTTP %d", p, code)
		}
	}
	// An empty request is refused by the pairing handler, not by the gate.
	if code := pageStatus(h, "POST", "/api/v1/fed/pairing/request"); code == 404 {
		t.Error("a pairing request must still reach its handler with the Pairing page hidden")
	}
}

func TestSitemapLeavesOutHiddenPages(t *testing.T) {
	api := pagesAPI(t, "network")
	seoOverview.ov, seoOverview.when = nil, time.Time{}
	w := httptest.NewRecorder()
	api.sitemapXML(w, httptest.NewRequest("GET", "/sitemap.xml", nil))
	body := w.Body.String()
	if strings.Contains(body, "/network</loc>") {
		t.Error("the sitemap lists a hidden page")
	}
	for _, p := range []string{"/</loc>", "/federation</loc>", "/pairing</loc>", "/about</loc>"} {
		if !strings.Contains(body, p) {
			t.Errorf("the sitemap lacks %s", p)
		}
	}
}
