package main

import (
	"fmt"
	"net/http"
	"slices"
	"strings"
)

// Most instances watch a handful of targets and federate with nobody. A
// Federation tab, a Pairing page or a page about the host network is then
// something the operator has to explain rather than something a visitor
// reads. The navigation already leaves out a page the configuration makes
// empty; this lets the operator take the others off the public site too.
//
// A hidden page is gone wherever a visitor could reach it: the menu, the
// links of the other pages, the sitemap, and its own address, which answers
// 404 rather than redirecting, so that a search engine drops it instead of
// following it somewhere else.
//
// It is presentation, not policy. The APIs behind those pages keep
// answering: peers call /api/v1/fed/* whatever this instance publishes, and
// pairing between instances never goes through the HTML page, so hiding
// Pairing must not quietly break it. Federation is turned off in the
// configuration file, not here.
//
// Home and About cannot be hidden. About is where the operator is
// identifiable and reachable, which is what makes a public probe worth
// reading at all (see site.go).

// hideablePages are the public pages an operator may hide, in menu order.
var hideablePages = []string{"federation", "network", "pairing"}

// Hides reports whether the operator took the public page id off the site.
func (s Site) Hides(id string) bool { return slices.Contains(s.HiddenPages, id) }

// cleanHiddenPages keeps each hideable page once, in menu order, and refuses
// any other name rather than storing a choice that would do nothing.
func cleanHiddenPages(ids []string) ([]string, error) {
	for _, id := range ids {
		if !slices.Contains(hideablePages, id) {
			return nil, fmt.Errorf("page %q cannot be hidden; those that can: %s",
				id, strings.Join(hideablePages, ", "))
		}
	}
	var out []string
	for _, id := range hideablePages {
		if slices.Contains(ids, id) {
			out = append(out, id)
		}
	}
	return out, nil
}

// pageAt names the hideable page a path serves, or "" for any other path:
// its route, the /pairing/ subtree, and the file the static server would
// otherwise hand out as it is.
func pageAt(path string) string {
	switch p := strings.TrimSuffix(path, ".html"); {
	case p == "/federation":
		return "federation"
	case p == "/network":
		return "network"
	case p == "/pairing", strings.HasPrefix(p, "/pairing/"):
		return "pairing"
	}
	return ""
}

// withHiddenPages answers 404 for a page the operator hid. It stands in
// front of the whole mux, so that /federation.html is closed along with
// /federation, and never looks at a path that is not a page.
func (a *API) withHiddenPages(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if id := pageAt(r.URL.Path); id != "" && a.store.Site().Hides(id) {
			// The page comes back the moment the operator unticks the box.
			w.Header().Set("Cache-Control", "no-cache")
			http.NotFound(w, r)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// frontHandler is the chain main puts between the rate limiter and the mux.
// The tests serve through it too, so a change that drops the gate fails one.
func (a *API) frontHandler(mux http.Handler) http.Handler {
	return withGzip(a.withHiddenPages(mux))
}
