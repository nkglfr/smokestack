package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func seoStore(t *testing.T) *Store {
	t.Helper()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cat, err := store.CreateCategory("transit", "Transitaires", "Transit", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateTarget(&Target{CategoryID: cat, Slug: "example-transit", Title: "Example transit",
		Host: "192.0.2.1", Proto: "icmp", IntervalS: 60, Packets: 10, SpacingMs: 100,
		TimeoutMs: 1000, Public: true, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateTarget(&Target{CategoryID: cat, Slug: "internal", Title: "Internal test",
		Host: "10.0.0.1", Proto: "icmp", IntervalS: 60, Packets: 10, SpacingMs: 100,
		TimeoutMs: 1000, Public: false, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	site := store.Site()
	site.Title, site.Org, site.URL = "Example Latency", "Example Telecom", "https://latency.example.net"
	site.Description = "Latency measured from our network."
	if err := store.SetSite(site); err != nil {
		t.Fatal(err)
	}
	return store
}

func TestSEOHeadAndSitemap(t *testing.T) {
	store := seoStore(t)
	defer store.Close()
	api := &API{store: store, probeID: 1}
	seoOverview.ov, seoOverview.when = nil, time.Time{}

	r := httptest.NewRequest("GET", "/", nil)
	head := api.seoHead(r, pageMeta{Path: "/", Schema: api.orgSchema(r)})
	for _, want := range []string{
		"<title>Example Latency</title>",
		`<link rel="canonical" href="https://latency.example.net/">`,
		`content="index, follow`,
		`property="og:title"`,
		`"@type":"WebSite"`,
	} {
		if !strings.Contains(head, want) {
			t.Errorf("the head of the home page lacks %s", want)
		}
	}
	// The back-office must never be indexed.
	if !strings.Contains(api.seoHead(r, pageMeta{Path: "/admin", NoIndex: true}), "noindex") {
		t.Error("the back-office must carry noindex")
	}
	// A private target has no public page.
	if _, ok := api.targetMeta(r, "internal"); ok {
		t.Error("a private target must not get an indexable page")
	}
	m, ok := api.targetMeta(r, "example-transit")
	if !ok || !strings.Contains(m.Description, "192.0.2.1") || m.Path != "/t/example-transit" {
		t.Errorf("target page: %+v", m)
	}
	// Sitemap: public pages and public targets, never the private one.
	rec := httptest.NewRecorder()
	api.sitemapXML(rec, httptest.NewRequest("GET", "/sitemap.xml", nil))
	body := rec.Body.String()
	for _, want := range []string{"/t/example-transit", "<loc>https://latency.example.net/</loc>", "/federation"} {
		if !strings.Contains(body, want) {
			t.Errorf("the sitemap lacks %s", want)
		}
	}
	if strings.Contains(body, "/t/internal") {
		t.Error("the sitemap exposes a private target")
	}
	// robots.txt points at the sitemap and keeps crawlers out of the API.
	rec = httptest.NewRecorder()
	api.robotsTxt(rec, httptest.NewRequest("GET", "/robots.txt", nil))
	if !strings.Contains(rec.Body.String(), "Sitemap: https://latency.example.net/sitemap.xml") ||
		!strings.Contains(rec.Body.String(), "Disallow: /admin") {
		t.Errorf("robots.txt: %s", rec.Body.String())
	}

	// Switched off, everything must close: noindex, no sitemap, no crawling.
	site := store.Site()
	site.SearchIndex = false
	store.SetSite(site)
	if !strings.Contains(api.seoHead(r, pageMeta{Path: "/"}), "noindex") {
		t.Error("with indexing off, pages must carry noindex")
	}
	rec = httptest.NewRecorder()
	api.robotsTxt(rec, httptest.NewRequest("GET", "/robots.txt", nil))
	if !strings.Contains(rec.Body.String(), "Disallow: /\n") {
		t.Error("with indexing off, robots.txt must refuse everything")
	}
	rec = httptest.NewRecorder()
	api.sitemapXML(rec, httptest.NewRequest("GET", "/sitemap.xml", nil))
	if rec.Code != 404 {
		t.Errorf("with indexing off, the sitemap must be gone: HTTP %d", rec.Code)
	}
}

// An instance configured before these settings existed must keep the
// defaults instead of being silently de-indexed by an upgrade.
func TestSiteDefaultsAfterUpgrade(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.SetSetting("site", `{"title":"Older instance","org":"Example"}`)
	site := store.Site()
	if !site.SearchIndex || !site.ContactForm {
		t.Errorf("defaults lost on upgrade: search_index=%v contact_form=%v", site.SearchIndex, site.ContactForm)
	}
	// An explicit choice is of course kept.
	store.SetSetting("site", `{"title":"Older instance","search_index":false,"contact_form":false}`)
	if s := store.Site(); s.SearchIndex || s.ContactForm {
		t.Error("an explicit choice must be kept")
	}
}

func TestInjectAndShorten(t *testing.T) {
	out := string(inject([]byte(`<html><head><title>x</title><meta name="robots" content="noindex"></head><body><div id="a"></div></body></html>`),
		"<title>real</title>\n", "<h1>summary</h1>"))
	if strings.Count(out, "<title>") != 1 || !strings.Contains(out, "<title>real</title>") {
		t.Errorf("the placeholder title must be replaced: %s", out)
	}
	if strings.Contains(out, `content="noindex"`) {
		t.Error("the page's own robots tag must be dropped")
	}
	if !strings.Contains(out, "<noscript>\n<h1>summary</h1></noscript>") {
		t.Errorf("the summary must be inside noscript: %s", out)
	}
	if len(shorten(strings.Repeat("mot ", 200), 100)) > 104 {
		t.Error("descriptions must be shortened")
	}
}

// An injected page must not carry the version-wide ETag: its description
// and its summary change with every measurement, so a cache answering
// "not modified" would serve a stale page.
func TestInjectedPagesAreNotCachedByETag(t *testing.T) {
	store := seoStore(t)
	defer store.Close()
	api := &API{store: store, probeID: 1}
	injected := func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		w.Write(inject([]byte("<html><head></head><body></body></html>"),
			api.seoHead(r, pageMeta{Path: "/"}), ""))
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/", nil)
	req.Header.Set("If-None-Match", `W/"0.2.5-"`)
	injected(rec, req)
	if rec.Code != 200 {
		t.Errorf("an injected page must always be served: HTTP %d", rec.Code)
	}
	if rec.Header().Get("ETag") != "" {
		t.Error("an injected page must not carry a shared ETag")
	}
	if rec.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("Cache-Control: %q", rec.Header().Get("Cache-Control"))
	}
}

// The catalogue must not suggest a rotating name: a pool points at a
// different machine every few minutes, which makes its graph meaningless.
func TestCatalogueHasNoRotatingName(t *testing.T) {
	seen := map[string]bool{}
	for _, s := range suggestedCatalogue {
		if seen[s.Key] {
			t.Errorf("duplicate key %q", s.Key)
		}
		seen[s.Key] = true
		if strings.Contains(s.Host, "pool.ntp.org") {
			t.Errorf("%s suggests a pool name: %s", s.Key, s.Host)
		}
		if s.Proto == "tcp" && s.Port == 0 {
			t.Errorf("%s is a TCP target without a port", s.Key)
		}
	}
}
