package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

// A status page only reaches whoever happens to have it open. This file
// publishes the same news as an Atom feed (RFC 4287) — for the whole instance
// at /feed.atom, for one target at /t/{slug}/feed.atom — so anyone can be told
// of an outage or a planned stop without an account, an e-mail address or a
// tab left open.
//
// The feed is a second door onto what the public pages already say, never a
// wider one. Its targets are those of the public overview, with the host
// masked or removed exactly as the pages show it; its maintenance entries are
// what the banner announced; its events are those /api/v1/events serves. The
// one thing it adds is a record of the incidents this instance detected, and
// that waits for the operator's say-so (Site.PublicIncidents): the current
// state is on the home page already, but a dated list of other people's
// outages, pushed to every subscriber, is a publication of its own.
//
// Entry ids are tag: URIs (RFC 4151) built from the instance's host, one date
// fixed for the whole instance and the row's own id, so an incident that
// closes is the same entry updated rather than a second one beside it.

const (
	feedWindow  = 30 * 86400 // how long an entry stays after it last changed
	feedEntries = 50
	// The horizon of the maintenance banner: a window further ahead has not
	// been announced on the public pages yet, so it is not announced here.
	feedAhead = 7 * 86400
	feedTTL   = 30 * time.Second
)

type atomFeed struct {
	XMLName xml.Name    `xml:"http://www.w3.org/2005/Atom feed"`
	Lang    string      `xml:"http://www.w3.org/XML/1998/namespace lang,attr"`
	ID      string      `xml:"id"`
	Title   string      `xml:"title"`
	Updated string      `xml:"updated"`
	Links   []atomLink  `xml:"link"`
	Author  atomAuthor  `xml:"author"`
	Entries []atomEntry `xml:"entry"`
}

type atomLink struct {
	Rel  string `xml:"rel,attr"`
	Type string `xml:"type,attr"`
	Href string `xml:"href,attr"`
}

type atomAuthor struct {
	Name string `xml:"name"`
	URI  string `xml:"uri"`
}

type atomCategory struct {
	Term string `xml:"term,attr"`
}

type atomEntry struct {
	ID        string       `xml:"id"`
	Title     string       `xml:"title"`
	Published string       `xml:"published"`
	Updated   string       `xml:"updated"`
	Link      atomLink     `xml:"link"`
	Category  atomCategory `xml:"category"`
	Summary   string       `xml:"summary"`
}

// feedItem is an entry before it is written: times stay numbers so the
// entries can be sorted, and the id is assembled once the host is known.
type feedItem struct {
	key       string // "incident/42": unique within the instance
	published int64
	updated   int64
	term      string
	title     string
	summary   string
	path      string // the public page the entry links to
}

// feedCache keeps each built feed for a few seconds: a feed reader polls,
// and a hundred of them polling the same address should cost one build.
type feedCache struct {
	mu sync.Mutex
	m  map[string]builtFeed
}

type builtFeed struct {
	body []byte
	etag string
	exp  time.Time
}

func (c *feedCache) get(key string, now time.Time) (builtFeed, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	f, ok := c.m[key]
	return f, ok && now.Before(f.exp)
}

func (c *feedCache) put(key string, f builtFeed) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Without a published address the key carries the Host header, which
	// the client chooses: bounded, so that choosing many costs nothing.
	if c.m == nil || len(c.m) >= 256 {
		c.m = map[string]builtFeed{}
	}
	c.m[key] = f
}

func (a *API) FeedRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /feed.atom", a.feed)
	mux.HandleFunc("GET /t/{slug}/feed.atom", a.feed)
}

// feed answers the same thing to everybody, operator included: it is meant
// to be cached and shared, and a private target has no feed for the same
// reason it has no /t/ page.
func (a *API) feed(w http.ResponseWriter, r *http.Request) {
	base, slug, now := a.baseURL(r), r.PathValue("slug"), time.Now()
	key := base + "\x00" + slug
	f, ok := a.feeds.get(key, now)
	if !ok {
		body, found, err := a.buildFeed(base, slug, now.Unix())
		switch {
		case err != nil:
			cachePrivate(w)
			writeErr(w, http.StatusServiceUnavailable, err.Error())
			return
		case !found:
			cachePrivate(w)
			http.NotFound(w, r)
			return
		}
		sum := sha256.Sum256(body)
		f = builtFeed{body: body, etag: `W/"` + hex.EncodeToString(sum[:8]) + `"`, exp: now.Add(feedTTL)}
		a.feeds.put(key, f)
	}
	w.Header().Set("Content-Type", "application/atom+xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=60")
	w.Header().Set("ETag", f.etag)
	if r.Header.Get("If-None-Match") == f.etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	w.Write(f.body)
}

// buildFeed writes the feed of the whole instance, or of the target whose
// page is /t/{slug}. found is false when no public page has that slug.
func (a *API) buildFeed(base, slug string, now int64) (body []byte, found bool, err error) {
	ov := a.publicOverview()
	if ov == nil {
		return nil, false, errors.New("the overview is not ready yet")
	}
	targets := map[int64]*OverviewTarget{}
	var only *OverviewTarget
	for _, c := range ov.Categories {
		for _, t := range c.Targets {
			switch {
			case slug == "":
				targets[t.ID] = t
			case only == nil && t.Slug == slug:
				only = t
				targets[t.ID] = t
			}
		}
	}
	if slug != "" && only == nil {
		return nil, false, nil
	}

	site := a.store.Site()
	tx, lang := a.feedText(site.DefaultLang)
	from := now - feedWindow
	var items []feedItem
	if site.PublicIncidents {
		hold := int64(a.store.AlertConfig().AfterMinutes) * 60
		items = append(items, a.feedIncidents(tx, targets, from, hold, now)...)
	}
	items = append(items, a.feedMaintenances(tx, targets, only, from, now)...)
	items = append(items, a.feedEvents(tx, only, from, now)...)
	sort.Slice(items, func(i, j int) bool {
		if items[i].updated != items[j].updated {
			return items[i].updated > items[j].updated
		}
		return items[i].key < items[j].key
	})
	if len(items) > feedEntries {
		items = items[:feedEntries]
	}

	name := site.Title
	if name == "" {
		name = "smokestack"
	}
	self, page := "/feed.atom", "/"
	title := tx.t("feed.title", "name", name)
	if only != nil {
		page = "/t/" + url.PathEscape(only.Slug)
		self = page + "/feed.atom"
		title = tx.t("feed.title", "name", only.Title+" · "+name)
	}
	author := site.Org
	if author == "" {
		author = name
	}
	updated := startedAt.Unix()
	if len(items) > 0 {
		updated = items[0].updated
	}
	host, day := feedHost(base), a.store.feedTagDate(now)
	feed := atomFeed{
		Lang:    lang,
		ID:      base + self,
		Title:   title,
		Updated: atomTime(updated),
		Links: []atomLink{
			{Rel: "self", Type: "application/atom+xml", Href: base + self},
			{Rel: "alternate", Type: "text/html", Href: base + page},
		},
		Author: atomAuthor{Name: author, URI: base + "/about"},
	}
	for _, it := range items {
		feed.Entries = append(feed.Entries, atomEntry{
			ID:        fmt.Sprintf("tag:%s,%s:%s", host, day, it.key),
			Title:     it.title,
			Published: atomTime(it.published),
			Updated:   atomTime(it.updated),
			Link:      atomLink{Rel: "alternate", Type: "text/html", Href: base + it.path},
			Category:  atomCategory{Term: it.term},
			Summary:   it.summary,
		})
	}
	out, err := xml.MarshalIndent(feed, "", "  ")
	if err != nil {
		return nil, false, err
	}
	return append([]byte(xml.Header), out...), true, nil
}

// feedIncidents lists the incidents of the given targets that were still
// open, or closed, within the window. What the alerting would have kept
// quiet is not pushed to every subscriber either: an incident shorter than
// the alert delay, one opened during a window that stops alerts or
// measurement, one on a target left out of alerting, and one decided by a
// probe since withdrawn from the public pages.
func (a *API) feedIncidents(tx feedText, targets map[int64]*OverviewTarget, from, hold, now int64) []feedItem {
	if len(targets) == 0 {
		return nil
	}
	ids := make([]any, 0, len(targets))
	for id := range targets {
		ids = append(ids, id)
	}
	args := append([]any{from, now, hold}, ids...)
	args = append(args, feedEntries)
	// An incident older than its target was left by a purged one whose id
	// SQLite handed on: purges used to keep those rows.
	rows, err := a.store.cfg.Query(`SELECT i.id,i.target_id,i.opened_at,i.closed_at
		FROM local_incidents i JOIN targets t ON t.id=i.target_id
		WHERE (i.closed_at=0 OR i.closed_at>=?)
		  AND (CASE i.closed_at WHEN 0 THEN ? ELSE i.closed_at END) - i.opened_at >= ?
		  AND i.target_id IN (?`+strings.Repeat(",?", len(ids)-1)+`)
		  AND i.opened_at >= t.created_at
		  AND t.alerts_off=0
		  AND i.probe_id NOT IN (SELECT id FROM probes WHERE enabled=0)
		  AND NOT EXISTS (SELECT 1 FROM maintenances m
		        WHERE m.target_id=i.target_id AND m.starts_at<=i.opened_at AND m.ends_at>i.opened_at
		          AND (m.stop_alerts=1 OR m.stop_probe=1))
		ORDER BY CASE i.closed_at WHEN 0 THEN i.opened_at ELSE i.closed_at END DESC LIMIT ?`, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []feedItem
	for rows.Next() {
		var i LocalIncident
		if rows.Scan(&i.ID, &i.TargetID, &i.OpenedAt, &i.ClosedAt) != nil {
			continue
		}
		t := targets[i.TargetID]
		it := feedItem{key: fmt.Sprintf("incident/%d", i.ID),
			published: i.OpenedAt, updated: i.OpenedAt, term: "incident",
			title:   tx.t("feed.incident_open", "target", t.Title),
			summary: tx.t("feed.incident_open_text", "target", feedName(t), "since", feedWhen(i.OpenedAt)),
			path:    "/t/" + url.PathEscape(t.Slug)}
		if i.ClosedAt > 0 {
			it.updated = i.ClosedAt
			it.title = tx.t("feed.incident_closed", "target", t.Title)
			it.summary = tx.t("feed.incident_closed_text", "target", feedName(t),
				"from", feedWhen(i.OpenedAt), "until", feedWhen(i.ClosedAt),
				"duration", humanSince(i.OpenedAt, i.ClosedAt))
		}
		out = append(out, it)
	}
	return out
}

// feedMaintenances says of a window what the public pages said of it, from
// the moment the banner first showed it: title, note, times, and whether the
// measurement stops. A window recorded after it had already ended was never
// announced to anyone, so it is not announced here either.
func (a *API) feedMaintenances(tx feedText, targets map[int64]*OverviewTarget, only *OverviewTarget, from, now int64) []feedItem {
	var id int64
	if only != nil {
		id = only.ID
	}
	var out []feedItem
	for _, m := range a.store.Maintenances(id, from) {
		t, ok := targets[m.TargetID]
		if !ok || m.StartsAt > now+feedAhead || m.CreatedAt >= m.EndsAt {
			continue
		}
		shown := max(m.CreatedAt, m.StartsAt-feedAhead)
		it := feedItem{key: fmt.Sprintf("maintenance/%d", m.ID),
			published: shown, updated: shown, term: "maintenance",
			title: tx.t("feed.maint_title", "target", t.Title, "title", m.Title),
			path:  "/t/" + url.PathEscape(t.Slug)}
		state := "feed.maint_planned"
		if m.StartsAt <= now {
			state, it.updated = "feed.maint_live", max(shown, m.StartsAt)
		}
		if m.EndsAt <= now {
			state, it.updated = "feed.maint_done", max(shown, m.EndsAt)
		}
		parts := []string{tx.t(state, "from", feedWhen(m.StartsAt), "until", feedWhen(m.EndsAt))}
		if m.StopProbe {
			parts = append(parts, tx.t("feed.maint_paused"))
		}
		if m.Note != "" {
			parts = append(parts, m.Note)
		}
		it.summary = strings.Join(parts, " ")
		out = append(out, it)
	}
	return out
}

// feedEvents carries the events the operator published, on the terms of
// /api/v1/events: the instance's own, plus a target's when the feed is that
// target's. Route changes stay on the graph: they describe a path, not an
// outage, and their text names the address measured.
func (a *API) feedEvents(tx feedText, only *OverviewTarget, from, now int64) []feedItem {
	where := `public=1 AND kind<>'path' AND ts_start<=? AND COALESCE(ts_end,ts_start)>=?`
	args := []any{now, from}
	path := "/"
	if only != nil {
		where += ` AND (scope='global' OR (scope='target' AND scope_id=?))`
		args = append(args, only.ID)
		path = "/t/" + url.PathEscape(only.Slug)
	} else {
		where += ` AND scope='global'`
	}
	args = append(args, feedEntries)
	rows, err := a.store.cfg.Query(`SELECT id,ts_start,ts_end,kind,title,COALESCE(body,'')
		FROM events WHERE `+where+` ORDER BY COALESCE(ts_end,ts_start) DESC LIMIT ?`, args...)
	if err != nil {
		return nil
	}
	defer rows.Close()
	var out []feedItem
	for rows.Next() {
		var e Event
		if rows.Scan(&e.ID, &e.TSStart, &e.TSEnd, &e.Kind, &e.Title, &e.Body) != nil {
			continue
		}
		it := feedItem{key: fmt.Sprintf("event/%d", e.ID),
			published: e.TSStart, updated: e.TSStart, term: e.Kind,
			title: e.Title, summary: e.Body, path: path}
		if it.title == "" {
			it.title = e.Kind
		}
		if e.TSEnd != nil {
			span := tx.t("feed.event_span", "from", feedWhen(e.TSStart), "until", feedWhen(*e.TSEnd))
			it.summary = strings.TrimSpace(span + " " + e.Body)
			if *e.TSEnd <= now {
				it.updated = max(it.updated, *e.TSEnd)
			}
		}
		out = append(out, it)
	}
	return out
}

// feedLinks advertises the feeds of a public page: the instance's on every
// one, and a target's own beside it on that target's page.
func (a *API) feedLinks(base string, m pageMeta) string {
	site := a.store.Site()
	tx, _ := a.feedText(site.DefaultLang)
	name := site.Title
	if name == "" {
		name = "smokestack"
	}
	var b strings.Builder
	link := func(href, title string) {
		fmt.Fprintf(&b, `<link rel="alternate" type="application/atom+xml" title="%s" href="%s">`+"\n",
			esc(title), esc(href))
	}
	link(base+"/feed.atom", tx.t("feed.title", "name", name))
	if strings.HasPrefix(m.Path, "/t/") {
		link(base+m.Path+"/feed.atom", tx.t("feed.title", "name", m.Title+" · "+name))
	}
	return b.String()
}

// feedText is the server's side of the pages' t(): the instance's default
// language, English for any key it lacks, placeholders filled in once.
type feedText map[string]string

func (a *API) feedText(lang string) (feedText, string) {
	if a.i18n == nil {
		return feedText{}, baseLang
	}
	if !a.i18n.Has(lang) {
		lang = baseLang
	}
	d, _ := a.i18n.Dict(lang)
	return d, lang
}

func (tx feedText) t(key string, vars ...string) string {
	pairs := make([]string, 0, len(vars))
	for i := 0; i+1 < len(vars); i += 2 {
		pairs = append(pairs, "{"+vars[i]+"}", vars[i+1])
	}
	return strings.NewReplacer(pairs...).Replace(tx[key])
}

// feedName is a target as its public page names it: the host comes from the
// public overview, already masked or emptied.
func feedName(t *OverviewTarget) string {
	if t.Host == "" {
		return t.Title
	}
	return t.Title + " (" + t.Host + ")"
}

// feedWhen is written in UTC, like the alerts: a feed is read anywhere, and
// the reader shows <updated> in the subscriber's own time anyway.
func feedWhen(ts int64) string {
	return time.Unix(ts, 0).UTC().Format("2006-01-02 15:04 UTC")
}

func atomTime(ts int64) string {
	return time.Unix(ts, 0).UTC().Format(time.RFC3339)
}

// feedTagDate is the date in every entry id. RFC 4151 only asks for a day on
// which the host was held, so one day, fixed when the first feed is built,
// serves every entry and tells nothing about when any row was written.
func (s *Store) feedTagDate(now int64) string {
	if d := s.Setting("feed_tag_date", ""); d != "" {
		return d
	}
	day := time.Unix(now, 0).UTC().Format(time.DateOnly)
	s.cfg.Exec(`INSERT OR IGNORE INTO settings(key,value) VALUES('feed_tag_date',?)`, day)
	return s.Setting("feed_tag_date", day)
}

func feedHost(base string) string {
	if u, err := url.Parse(base); err == nil && u.Hostname() != "" {
		return strings.ToLower(u.Hostname())
	}
	return "localhost"
}
