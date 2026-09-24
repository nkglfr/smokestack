package main

import (
	"fmt"
	"html"
	"net/http"
	"strings"
	"sync"
	"time"
)

// The public pages are built in the browser, so a crawler that does not run
// JavaScript sees an empty document. This file gives every public page a
// real <title>, a description, a canonical URL, language alternates, social
// cards and structured data, plus a <noscript> summary with the actual
// measurements — and publishes robots.txt and a sitemap listing every public
// page and target.

// publicOverview returns the public overview for the pages a crawler reads.
// The main cache only keeps the compressed JSON, so this one keeps the
// values themselves, rebuilt at most every 30 seconds.
var seoOverview struct {
	sync.Mutex
	ov   *Overview
	when time.Time
}

func (a *API) publicOverview() *Overview {
	seoOverview.Lock()
	defer seoOverview.Unlock()
	if seoOverview.ov != nil && time.Since(seoOverview.when) < 30*time.Second {
		return seoOverview.ov
	}
	ov, err := a.store.Overview(a.probeID, time.Now().Unix(), true)
	if err != nil {
		return seoOverview.ov
	}
	seoOverview.ov, seoOverview.when = ov, time.Now()
	return ov
}

type pageMeta struct {
	Path        string // canonical path, e.g. "/federation"
	Title       string // without the site name, added afterwards
	Description string
	NoIndex     bool
	Body        string // server-rendered summary, inside <noscript>
	Schema      string // JSON-LD
}

// baseURL prefers the address the operator published: it is the one that
// must appear in canonical links, whatever host the request came through.
func (a *API) baseURL(r *http.Request) string {
	if u := strings.TrimSuffix(strings.TrimSpace(a.store.Site().URL), "/"); u != "" &&
		strings.HasPrefix(u, "http") {
		return u
	}
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	return scheme + "://" + r.Host
}

func esc(s string) string { return html.EscapeString(s) }

// shorten keeps a description within what search engines display.
func shorten(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) <= n {
		return s
	}
	if i := strings.LastIndex(s[:n], " "); i > 40 {
		return s[:i] + "…"
	}
	return s[:n] + "…"
}

// seoHead builds everything that goes before </head>.
func (a *API) seoHead(r *http.Request, m pageMeta) string {
	site := a.store.Site()
	name := site.Title
	if name == "" {
		name = "smokestack"
	}
	org := site.Org
	if org == "" {
		org = name
	}
	base := a.baseURL(r)
	canonical := base + m.Path
	title := m.Title
	if title == "" {
		title = name
	} else {
		title = title + " · " + name
	}
	desc := m.Description
	if desc == "" {
		desc = site.Description
	}
	if desc == "" {
		desc = fmt.Sprintf("Latency and packet loss measured from %s towards public destinations, "+
			"with exact percentiles and a one-year history.", org)
	}
	desc = shorten(desc, 300)

	var b strings.Builder
	fmt.Fprintf(&b, "<title>%s</title>\n", esc(title))
	fmt.Fprintf(&b, `<meta name="description" content="%s">`+"\n", esc(desc))
	fmt.Fprintf(&b, `<link rel="canonical" href="%s">`+"\n", esc(canonical))
	if m.NoIndex || !site.SearchIndex {
		b.WriteString(`<meta name="robots" content="noindex, nofollow">` + "\n")
	} else {
		b.WriteString(`<meta name="robots" content="index, follow, max-image-preview:large">` + "\n")
	}
	// One alternate per shipped language: the same page, another language.
	if a.i18n != nil && !m.NoIndex {
		for _, l := range a.i18n.Languages() {
			fmt.Fprintf(&b, `<link rel="alternate" hreflang="%s" href="%s?lang=%s">`+"\n",
				esc(l.Code), esc(canonical), esc(l.Code))
		}
		fmt.Fprintf(&b, `<link rel="alternate" hreflang="x-default" href="%s">`+"\n", esc(canonical))
	}
	fmt.Fprintf(&b, `<meta property="og:type" content="website">`+"\n")
	fmt.Fprintf(&b, `<meta property="og:site_name" content="%s">`+"\n", esc(name))
	fmt.Fprintf(&b, `<meta property="og:title" content="%s">`+"\n", esc(title))
	fmt.Fprintf(&b, `<meta property="og:description" content="%s">`+"\n", esc(desc))
	fmt.Fprintf(&b, `<meta property="og:url" content="%s">`+"\n", esc(canonical))
	fmt.Fprintf(&b, `<meta name="twitter:card" content="summary">`+"\n")
	if m.Schema != "" {
		fmt.Fprintf(&b, `<script type="application/ld+json">%s</script>`+"\n", m.Schema)
	}
	return b.String()
}

func (a *API) orgSchema(r *http.Request) string {
	site := a.store.Site()
	base := a.baseURL(r)
	name := site.Org
	if name == "" {
		name = site.Title
	}
	var same []string
	for _, u := range []string{site.PeeringDB, site.URL} {
		if u != "" && !strings.HasPrefix(u, base) {
			same = append(same, `"`+esc(u)+`"`)
		}
	}
	sameAs := ""
	if len(same) > 0 {
		sameAs = fmt.Sprintf(`,"sameAs":[%s]`, strings.Join(same, ","))
	}
	return fmt.Sprintf(`{"@context":"https://schema.org","@type":"WebSite","name":%q,"url":%q,`+
		`"inLanguage":%q,"publisher":{"@type":"Organization","name":%q%s}}`,
		site.Title, base, site.DefaultLang, name, sameAs)
}

// homeBody is the summary a crawler without JavaScript reads: the state of
// every public target, with a link to its page.
func (a *API) homeBody(r *http.Request) string {
	ov := a.publicOverview()
	if ov == nil {
		return ""
	}
	var b strings.Builder
	site := a.store.Site()
	fmt.Fprintf(&b, "<h1>%s</h1>\n<p>%s</p>\n", esc(site.Title), esc(site.Description))
	for _, c := range ov.Categories {
		menu := c.MenuEN
		if menu == "" {
			menu = c.MenuFR
		}
		fmt.Fprintf(&b, "<h2>%s</h2>\n<ul>\n", esc(menu))
		for _, t := range c.Targets {
			med := "no data"
			if t.MedMs != nil {
				med = fmt.Sprintf("median %.2f ms", *t.MedMs)
			}
			loss := ""
			if t.LossPct != nil && *t.LossPct > 0 {
				loss = fmt.Sprintf(", %.1f %% packet loss", *t.LossPct)
			}
			fmt.Fprintf(&b, `<li><a href="/t/%s">%s</a> (%s): %s%s</li>`+"\n",
				esc(t.Slug), esc(t.Title), esc(t.Host), esc(med), esc(loss))
		}
		b.WriteString("</ul>\n")
	}
	return b.String()
}

// targetMeta describes one target page, /t/{slug}.
func (a *API) targetMeta(r *http.Request, slug string) (pageMeta, bool) {
	ov := a.publicOverview()
	if ov == nil {
		return pageMeta{}, false
	}
	site := a.store.Site()
	org := site.Org
	if org == "" {
		org = site.Title
	}
	for _, c := range ov.Categories {
		for _, t := range c.Targets {
			if t.Slug != slug {
				continue
			}
			state := "no measurement yet"
			if t.MedMs != nil {
				state = fmt.Sprintf("median %.2f ms", *t.MedMs)
				if t.LossPct != nil && *t.LossPct > 0 {
					state += fmt.Sprintf(", %.1f %% packet loss", *t.LossPct)
				}
			}
			desc := fmt.Sprintf("Latency and packet loss measured from %s towards %s (%s), %s. "+
				"Percentiles over one year, %s every %d s.",
				org, t.Title, t.Host, state, strings.ToUpper(t.Proto), t.Interval)
			body := fmt.Sprintf("<h1>%s</h1>\n<p>%s</p>\n<p>Measured from %s. <a href=\"/\">All targets</a></p>\n",
				esc(t.Title), esc(desc), esc(org))
			return pageMeta{
				Path:        "/t/" + slug,
				Title:       t.Title,
				Description: desc,
				Body:        body,
				Schema: fmt.Sprintf(`{"@context":"https://schema.org","@type":"WebPage","name":%q,`+
					`"description":%q,"url":%q,"isPartOf":{"@type":"WebSite","name":%q,"url":%q}}`,
					t.Title, shorten(desc, 300), a.baseURL(r)+"/t/"+slug, site.Title, a.baseURL(r)),
			}, true
		}
	}
	return pageMeta{}, false
}

// inject puts the head tags and the noscript summary into a page.
func inject(page []byte, head, body string) []byte {
	s := string(page)
	// Remove the placeholder title of the static file, if any.
	if i, j := strings.Index(s, "<title>"), strings.Index(s, "</title>"); i >= 0 && j > i {
		s = s[:i] + s[j+len("</title>"):]
	}
	// A page may already carry its own robots tag: drop it, ours decides.
	for {
		i := strings.Index(s, `<meta name="robots"`)
		if i < 0 {
			break
		}
		j := strings.Index(s[i:], ">")
		if j < 0 {
			break
		}
		s = s[:i] + s[i+j+1:]
	}
	s = strings.Replace(s, "</head>", head+"</head>", 1)
	if body != "" {
		s = strings.Replace(s, "<body>", "<body>\n<noscript>\n"+body+"</noscript>\n", 1)
	}
	return []byte(s)
}

func (a *API) robotsTxt(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	base := a.baseURL(r)
	if !a.store.Site().SearchIndex {
		fmt.Fprint(w, "User-agent: *\nDisallow: /\n")
		return
	}
	fmt.Fprintf(w, `User-agent: *
Allow: /
Disallow: /admin
Disallow: /api/

Sitemap: %s/sitemap.xml
`, base)
}

// sitemapXML lists the public pages and every public target, so a new
// instance is indexed without anyone maintaining a list.
func (a *API) sitemapXML(w http.ResponseWriter, r *http.Request) {
	site := a.store.Site()
	if !site.SearchIndex {
		http.NotFound(w, r)
		return
	}
	base := a.baseURL(r)
	day := time.Now().UTC().Format("2006-01-02")
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` + "\n")
	add := func(path, freq, prio string) {
		fmt.Fprintf(&b, "<url><loc>%s%s</loc><lastmod>%s</lastmod>"+
			"<changefreq>%s</changefreq><priority>%s</priority></url>\n",
			esc(base), esc(path), day, freq, prio)
	}
	add("/", "hourly", "1.0")
	add("/federation", "daily", "0.7")
	add("/network", "weekly", "0.6")
	add("/about", "monthly", "0.5")
	add("/pairing", "monthly", "0.4")
	if ov := a.publicOverview(); ov != nil {
		for _, c := range ov.Categories {
			for _, t := range c.Targets {
				add("/t/"+t.Slug, "hourly", "0.8")
			}
		}
	}
	b.WriteString("</urlset>\n")
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=1800")
	w.Write([]byte(b.String()))
}
