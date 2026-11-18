package main

import (
	"encoding/xml"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

// badgeAPI serves badges over one public target per status, a private one
// and a public one whose address is hidden, with the overview built the way
// the background loop builds it.
func badgeAPI(t *testing.T) (*API, *Store) {
	t.Helper()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	i18n, err := NewI18n(os.DirFS("web"), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	cat, err := store.CreateCategory("c", "C", "C", true)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	mk := func(slug, title string, public, hide bool, sent, lost int) int64 {
		id, err := store.CreateTarget(&Target{CategoryID: cat, Slug: slug, Title: title,
			Host: "192.0.2.10", Proto: "icmp", IntervalS: 60, Packets: 5, SpacingMs: 100,
			TimeoutMs: 1000, Public: public, HideHost: hide, Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		for p := 0; sent > 0 && p < 5; p++ {
			m := Measurement{TargetID: id, ProbeID: 1, TS: now - 30 - int64(p)*60, Sent: sent, Lost: lost}
			for j := 0; j < sent-lost; j++ {
				m.RTTus = append(m.RTTus, 20000)
			}
			if err := store.Record(m); err != nil {
				t.Fatal(err)
			}
		}
		return id
	}
	mk("ok", "Example transit", true, false, 100, 0)
	mk("warn", "Lossy peer", true, false, 100, 1)
	mk("crit", "Broken peer", true, false, 100, 10)
	mk("nodata", "Silent peer", true, false, 0, 0)
	maint := mk("maint", "Resting peer", true, false, 100, 0)
	mk("hidden", "Private one", false, false, 100, 0)
	mk("nohost", "Address kept private", true, true, 100, 0)
	if _, err := store.CreateMaintenance(&Maintenance{TargetID: maint, Title: "Upgrade",
		StartsAt: now - 600, EndsAt: now + 3600, StopProbe: true}); err != nil {
		t.Fatal(err)
	}
	if err := store.RollupTick(now); err != nil {
		t.Fatal(err)
	}

	ovCache.store, ovCache.probe = store, 1
	t.Cleanup(func() {
		ovCache.store = nil
		ovCache.ov.Store(nil)
		ovCache.data.Store(nil)
		ovCache.gz.Store(nil)
	})
	if err := ovCache.build(); err != nil {
		t.Fatal(err)
	}
	availCache.mu.Lock()
	availCache.items = map[string]availEntry{}
	availCache.mu.Unlock()
	return &API{store: store, token: "secret", probeID: 1, i18n: i18n}, store
}

func getBadge(api *API, path string, header ...string) *httptest.ResponseRecorder {
	mux := http.NewServeMux()
	api.BadgeRoutes(mux)
	req := httptest.NewRequest("GET", path, nil)
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	return rec
}

// badgeTitle checks that a badge is well-formed XML and returns its <title>.
func badgeTitle(t *testing.T, body string) string {
	t.Helper()
	dec := xml.NewDecoder(strings.NewReader(body))
	var root, title string
	for {
		tok, err := dec.Token()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("badge is not well-formed XML: %v\n%s", err, body)
		}
		if se, ok := tok.(xml.StartElement); ok {
			if root == "" {
				root = se.Name.Local
			}
			if se.Name.Local == "title" {
				if err := dec.DecodeElement(&title, &se); err != nil {
					t.Fatal(err)
				}
			}
		}
	}
	if root != "svg" || title == "" {
		t.Fatalf("expected an <svg> with a <title>, got root %q, title %q", root, title)
	}
	return title
}

// A badge is embedded in public pages and fetched through shared caches, so
// a private target must answer exactly like a slug that never existed —
// whoever asks, and whatever they hold.
func TestBadgePrivateAnswersLikeUnknown(t *testing.T) {
	api, store := badgeAPI(t)
	unknown := getBadge(api, "/badge/nope.svg")
	if unknown.Code != 404 {
		t.Fatalf("unknown slug: HTTP %d, expected 404", unknown.Code)
	}
	shareSchema(store)
	priv, _ := store.TargetBySlug("hidden")
	share, err := store.CreateShare(priv.ID, 1, "", "test")
	if err != nil {
		t.Fatal(err)
	}
	for name, rec := range map[string]*httptest.ResponseRecorder{
		"private":               getBadge(api, "/badge/hidden.svg"),
		"private, signed in":    getBadge(api, "/badge/hidden.svg", "Authorization", "Bearer secret"),
		"private, share token":  getBadge(api, "/badge/hidden.svg?share="+share.Token),
		"public, not .svg":      getBadge(api, "/badge/ok"),
		"private, other metric": getBadge(api, "/badge/hidden.svg?metric=uptime"),
	} {
		if rec.Code != unknown.Code || rec.Body.String() != unknown.Body.String() {
			t.Errorf("%s: HTTP %d %q, an unknown slug gives HTTP %d %q",
				name, rec.Code, rec.Body.String(), unknown.Code, unknown.Body.String())
		}
		for _, h := range []string{"Content-Type", "Cache-Control"} {
			if rec.Header().Get(h) != unknown.Header().Get(h) {
				t.Errorf("%s: %s %q, an unknown slug gives %q",
					name, h, rec.Header().Get(h), unknown.Header().Get(h))
			}
		}
		if strings.Contains(rec.Body.String(), "Private one") {
			t.Errorf("%s: the 404 names the private target", name)
		}
	}
	if cc := unknown.Header().Get("Cache-Control"); !strings.Contains(cc, "no-store") {
		t.Errorf("a 404 must not be cached past the target being made public: %q", cc)
	}

	// Made private, a public target loses its badge at the next build, which
	// is what every target change triggers.
	tg, _ := store.TargetBySlug("ok")
	tg.Public = false
	if err := store.UpdateTarget(tg); err != nil {
		t.Fatal(err)
	}
	if err := ovCache.build(); err != nil {
		t.Fatal(err)
	}
	if rec := getBadge(api, "/badge/ok.svg"); rec.Code != 404 {
		t.Errorf("a target switched to private still has a badge: HTTP %d", rec.Code)
	}
}

// Before the background loop's first build there is nothing to read, and the
// badge says so rather than building the overview itself: the route is not
// rate-limited, and each such build scans every target.
func TestBadgeWaitsForTheOverview(t *testing.T) {
	api, _ := badgeAPI(t)
	ovCache.ov.Store(nil)
	builds := ovCache.builds.Load()
	rec := getBadge(api, "/badge/ok.svg")
	if rec.Code != 503 || rec.Header().Get("Cache-Control") != "private, no-store" {
		t.Errorf("no overview yet: HTTP %d, Cache-Control %q", rec.Code, rec.Header().Get("Cache-Control"))
	}
	if ovCache.builds.Load() != builds || ovCache.ov.Load() != nil {
		t.Error("a badge request built the overview")
	}
	if err := ovCache.build(); err != nil {
		t.Fatal(err)
	}
	if rec := getBadge(api, "/badge/ok.svg"); rec.Code != 200 {
		t.Errorf("once built: HTTP %d", rec.Code)
	}
}

func TestBadgeStatusColours(t *testing.T) {
	api, _ := badgeAPI(t)
	for slug, want := range map[string]struct{ word, colour string }{
		"ok":     {"nominal", "#138a4a"},
		"warn":   {"degraded", "#b86a00"},
		"crit":   {"incident", "#d92d20"},
		"nodata": {"no data", "#8e99aa"},
		"maint":  {"maintenance", "#4338ca"},
		"nohost": {"nominal", "#138a4a"},
	} {
		rec := getBadge(api, "/badge/"+slug+".svg")
		if rec.Code != 200 {
			t.Fatalf("%s: HTTP %d", slug, rec.Code)
		}
		h := rec.Header()
		if h.Get("Content-Type") != "image/svg+xml; charset=utf-8" ||
			h.Get("X-Content-Type-Options") != "nosniff" ||
			h.Get("Cache-Control") != "public, max-age=60" {
			t.Errorf("%s: headers %v", slug, h)
		}
		body := rec.Body.String()
		if title := badgeTitle(t, body); !strings.HasSuffix(title, ": "+want.word) {
			t.Errorf("%s: title %q, expected the status %q", slug, title, want.word)
		}
		if !strings.Contains(body, `fill="`+want.colour+`"`) {
			t.Errorf("%s: expected the colour %s", slug, want.colour)
		}
		if strings.Contains(body, "192.0.2.") {
			t.Errorf("%s: the badge shows the target's address", slug)
		}
	}
}

// Words come in the instance's default language, and so does the decimal mark.
func TestBadgeLanguageAndLatency(t *testing.T) {
	api, store := badgeAPI(t)
	site := store.Site()
	site.DefaultLang = "fr"
	if err := store.SetSite(site); err != nil {
		t.Fatal(err)
	}
	if title := badgeTitle(t, getBadge(api, "/badge/warn.svg").Body.String()); title != "Lossy peer: dégradé" {
		t.Errorf("status in French: %q", title)
	}
	title := badgeTitle(t, getBadge(api, "/badge/ok.svg?metric=latency").Body.String())
	if !regexp.MustCompile(`^Example transit: Médiane (19|20),\d ms$`).MatchString(title) {
		t.Errorf("latency badge: %q", title)
	}
	if title := badgeTitle(t, getBadge(api, "/badge/nodata.svg?metric=latency").Body.String()); title != "Silent peer: Médiane sans données" {
		t.Errorf("latency without data: %q", title)
	}
}

// The uptime badge is the availability figure, so it is published exactly
// when the figure is.
func TestBadgeUptimeFollowsPublicAvailability(t *testing.T) {
	api, store := badgeAPI(t)
	tg, _ := store.TargetBySlug("ok")
	partial := ""
	for _, w := range api.availabilityOf(tg.ID, 1)["windows"].([]AvailWindow) {
		if w.Label == "7d" && w.Partial {
			partial = ">100.0 % · since " + time.Unix(w.First, 0).UTC().Format(time.DateOnly) + "<"
		}
	}
	if partial == "" {
		t.Fatal("five minutes of data should make the 7d window partial")
	}
	rec := getBadge(api, "/badge/ok.svg?metric=uptime&window=7d")
	body := rec.Body.String()
	if rec.Code != 200 || !strings.Contains(body, partial) || !strings.Contains(body, `fill="#138a4a"`) {
		t.Fatalf("published: HTTP %d, expected %q\n%s", rec.Code, partial, body)
	}
	if title := badgeTitle(t, body); title != "Example transit: Availability, 7 days: 100.0 % · period only partly covered" {
		t.Errorf("published, partial: title %q", title)
	}

	site := store.Site()
	site.PublicAvailability = false
	if err := store.SetSite(site); err != nil {
		t.Fatal(err)
	}
	rec = getBadge(api, "/badge/ok.svg?metric=uptime&window=7d")
	body = rec.Body.String()
	if title := badgeTitle(t, body); rec.Code != 200 || title != "Example transit: availability not published" {
		t.Errorf("not published: HTTP %d, title %q", rec.Code, title)
	}
	if strings.Contains(body, "100.0 %") || strings.Contains(body, "7 days") || strings.Contains(body, "since") {
		t.Errorf("not published, yet the figure is in the badge:\n%s", body)
	}

	site.PublicAvailability = true
	if err := store.SetSite(site); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(getBadge(api, "/badge/ok.svg?metric=uptime&window=7d").Body.String(), partial) {
		t.Error("published again, the figure does not come back")
	}
}

// A window measured from its start is named as the window, not as a date.
func TestBadgeUptimeFullWindow(t *testing.T) {
	api, store := badgeAPI(t)
	tg, _ := store.TargetBySlug("ok")
	now := time.Now().Unix()
	m := Measurement{TargetID: tg.ID, ProbeID: 1, TS: now - 86400 + 120, Sent: 5, RTTus: []float64{20000, 20000, 20000, 20000, 20000}}
	if err := store.Record(m); err != nil {
		t.Fatal(err)
	}
	if err := store.RollupRange(cascade[0].table, cascade[1].table, cascade[1].secs, now-2*86400, now+60); err != nil {
		t.Fatal(err)
	}
	body := getBadge(api, "/badge/ok.svg?metric=uptime&window=24h").Body.String()
	if !strings.Contains(body, ">100.0 % · 24 hours<") {
		t.Errorf("a fully covered window is named as the window:\n%s", body)
	}
	if title := badgeTitle(t, body); title != "Example transit: Availability, 24 hours: 100.0 %" {
		t.Errorf("a fully covered window carries no partial note: %q", title)
	}
}

// The badge writes a figure the way the page it links to does (n2 in
// web/index.html, through Intl.NumberFormat), so the two never disagree.
// The expected strings are what a browser prints for the same numbers.
func TestBadgeNumbersAsThePageWritesThem(t *testing.T) {
	for _, c := range []struct {
		v    float64
		mark string
		want string
	}{
		{100, ".", "100.0"},
		{75, ".", "75.0"},
		{99.94, ".", "99.9"},
		{99.96, ".", "100.0"},
		{99.25, ".", "99.3"}, // a tie goes up, not to even
		{99.85, ".", "99.9"}, // just under the tie in binary, still up in a browser
		{12.35, ".", "12.4"},
		{float64(1999) * 100 / 2000, ".", "100.0"},
		{float64(9997) * 100 / 10000, ".", "100.0"},
		{0.5, ".", "0.50"},
		{0.125, ".", "0.13"},
		{0.005, ".", "0.01"},
		{99.25, ",", "99,3"},
		{math.NaN(), ".", "—"},
	} {
		if got := pageNumber(c.v, c.mark); got != c.want {
			t.Errorf("%v with %q: %s, expected %s", c.v, c.mark, got, c.want)
		}
	}
	for pct, want := range map[float64]string{100: "#138a4a", 99.9: "#138a4a", 99.5: "#b86a00", 98: "#d92d20"} {
		if got := uptimeColor(pct); got != want {
			t.Errorf("%.1f %%: %s, expected %s", pct, got, want)
		}
	}
}

func TestBadgeLabel(t *testing.T) {
	api, _ := badgeAPI(t)
	if title := badgeTitle(t, getBadge(api, "/badge/ok.svg").Body.String()); title != "Example transit: nominal" {
		t.Errorf("the default label is the title: %q", title)
	}

	long := strings.Repeat("é", 30) + ` <b>&"x"</b> ` + strings.Repeat("z", 30)
	body := getBadge(api, "/badge/ok.svg?label="+strings.NewReplacer(" ", "%20", "&", "%26", `"`, "%22").Replace(long)).Body.String()
	label, _, _ := strings.Cut(badgeTitle(t, body), ": ")
	if n := utf8.RuneCountInString(label); n != badgeLabelMax || !strings.HasSuffix(label, "…") {
		t.Errorf("label cut to %d characters (%q), expected %d ending in an ellipsis", n, label, badgeLabelMax)
	}
	if !strings.HasPrefix(label, strings.Repeat("é", 30)+" <b>&") {
		t.Errorf("label %q does not keep its beginning", label)
	}
	if strings.Contains(body, "<b>") {
		t.Error("the label is not escaped")
	}
	if title := badgeTitle(t, getBadge(api, "/badge/ok.svg?label=%20%20").Body.String()); title != "Example transit: nominal" {
		t.Errorf("a blank label falls back to the title: %q", title)
	}
}

func TestBadgeBadRequest(t *testing.T) {
	api, _ := badgeAPI(t)
	for _, q := range []string{"?metric=loss", "?metric=uptime&window=2d", "?window=forever"} {
		if rec := getBadge(api, "/badge/ok.svg"+q); rec.Code != 400 {
			t.Errorf("%s: HTTP %d, expected 400", q, rec.Code)
		}
	}
	for _, w := range []string{"24h", "7d", "30d", "1y"} {
		if rec := getBadge(api, "/badge/ok.svg?metric=uptime&window="+w); rec.Code != 200 {
			t.Errorf("window %s: HTTP %d", w, rec.Code)
		}
	}
}

// The box is sized from the text: wider text, wider badge, and a character
// the table does not know still takes room.
func TestBadgeTextWidth(t *testing.T) {
	if textWidth("") != 0 || textWidth("ii") >= textWidth("mm") {
		t.Error("widths do not follow the characters")
	}
	if textWidth("é") <= 0 || textWidth("東") <= textWidth("e") {
		t.Error("characters outside ASCII take no room")
	}
}
