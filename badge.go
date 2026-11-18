package main

import (
	"bytes"
	"encoding/xml"
	"fmt"
	"math"
	"math/big"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Badges: a target's state as a small image an operator can put on a
// website, in a README or in a PeeringDB note, where a link to the target
// page would ask the reader to go and look. /badge/{slug}.svg shows the
// status the public pages show; ?metric=uptime with ?window= the availability
// figure, ?metric=latency the current median, and ?label= replaces the title.
//
// The image is the same for everybody. It is embedded in pages nobody here
// controls and fetched through image proxies that keep it, so nothing about
// the caller changes the answer: a session does not reveal a private target,
// and neither does a share token — a token in an <img src> is a credential
// handed to every reader of the page it sits on. A private target answers
// exactly like a slug that does not exist.
//
// The status and the median come from the overview the background loop
// builds; a badge never builds it, and answers 503 until the first build. The
// availability comes from the one-minute cache the target page reads, which
// one request fills per target while the others wait, so however busy the
// page a badge sits on, it costs at most one aggregation a minute.

const (
	badgeLabelMax = 40
	badgeMaxAge   = 60
)

// The public page's colours, so a badge reads like the dot beside the target.
var badgeColors = map[string]string{
	"ok":     "#138a4a",
	"warn":   "#b86a00",
	"crit":   "#d92d20",
	"maint":  "#4338ca",
	"nodata": "#8e99aa",
}

func (a *API) BadgeRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /badge/{file}", a.badge)
}

func (a *API) badge(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	metric := q.Get("metric")
	switch metric {
	case "":
		metric = "status"
	case "status", "uptime", "latency":
	default:
		writeErr(w, 400, "unknown metric: expected status, uptime or latency")
		return
	}
	window := q.Get("window")
	if window == "" {
		window = "30d"
	}
	if !knownAvailWindow(window) {
		writeErr(w, 400, "unknown window: expected 24h, 7d, 30d or 1y")
		return
	}

	slug, isSVG := strings.CutSuffix(r.PathValue("file"), ".svg")
	t, ready := publicTarget(slug)
	if !ready {
		cachePrivate(w)
		writeErr(w, 503, "the overview is not ready yet")
		return
	}
	// A cached 404 would outlive the target being made public.
	if !isSVG || t == nil {
		cachePrivate(w)
		writeErr(w, 404, "target not found")
		return
	}

	site := a.store.Site()
	word := func(key string) string { return a.i18n.Text(site.DefaultLang, key) }
	num := func(v float64) string { return pageNumber(v, word("badge.decimal_mark")) }
	label := clipLabel(q.Get("label"), badgeLabelMax)
	if label == "" {
		label = clipLabel(t.Title, badgeLabelMax)
	}

	var value, color, desc string
	switch metric {
	case "status":
		value, color = word("status."+t.Status), badgeColors[t.Status]
		desc = value
	case "latency":
		// Coloured by status: a median alone does not say whether 40 ms is
		// usual for this target, and the classification does.
		value, color = word("status.nodata"), badgeColors[t.Status]
		if t.MedMs != nil {
			value = num(*t.MedMs) + " " + word("metric.ms")
		}
		desc = word("metric.median") + " " + value
	case "uptime":
		value, color, desc = a.uptimeBadge(t.ID, window, site.PublicAvailability, word, num)
	}

	h := w.Header()
	h.Set("Content-Type", "image/svg+xml; charset=utf-8")
	h.Set("Cache-Control", fmt.Sprintf("public, max-age=%d", badgeMaxAge))
	h.Set("X-Content-Type-Options", "nosniff")
	// Opened on its own rather than through an <img>, an SVG is a document of
	// this origin; nothing in a badge needs to load or run anything.
	h.Set("Content-Security-Policy", "default-src 'none'")
	w.Write(badgeSVG(label, value, color, label+": "+desc))
}

// uptimeBadge is the availability figure, or a grey badge saying it is not
// published rather than a 404: the target and the absence of the figure are
// both on its public page already, and a broken image on a page the operator
// may not control reads as an instance that is down.
func (a *API) uptimeBadge(id int64, window string, published bool, word func(string) string, num func(float64) string) (value, color, desc string) {
	if !published {
		value = word("badge.uptime_not_published")
		return value, badgeColors["nodata"], value
	}
	period := word("avail.window_" + window)
	ws, _ := a.availabilityOf(id, a.probeID)["windows"].([]AvailWindow)
	for _, w := range ws {
		if w.Label != window {
			continue
		}
		if w.Pct == nil {
			break
		}
		pct := num(*w.Pct) + " %"
		desc = word("avail.title") + ", " + period + ": " + pct
		// A target measured for a week does not get to show a yearly figure:
		// the visible text names the span the figure covers.
		covered := period
		if w.Partial {
			desc += " · " + word("avail.partial")
			covered = strings.ReplaceAll(word("badge.since"), "{date}", time.Unix(w.First, 0).UTC().Format(time.DateOnly))
		}
		return pct + " · " + covered, uptimeColor(*w.Pct), desc
	}
	value = word("status.nodata")
	return value, badgeColors["nodata"], word("avail.title") + ", " + period + ": " + value
}

// publicTarget finds a target in the overview the background loop keeps. The
// bool is false until that loop has built one: an unthrottled public route
// must not be the one that builds it.
func publicTarget(slug string) (*OverviewTarget, bool) {
	ov := ovCache.ov.Load()
	if ov == nil {
		return nil, false
	}
	for _, c := range ov.Categories {
		for _, t := range c.Targets {
			if t.Slug == slug {
				return t, true
			}
		}
	}
	return nil, true
}

func knownAvailWindow(label string) bool {
	for _, w := range availWindows {
		if w.label == label {
			return true
		}
	}
	return false
}

// uptimeColor uses the target page's thresholds for the same figure.
func uptimeColor(pct float64) string {
	switch {
	case pct >= 99.9:
		return badgeColors["ok"]
	case pct >= 99:
		return badgeColors["warn"]
	}
	return badgeColors["crit"]
}

// pageNumber writes v as the public pages do (n2 in web/index.html), so a
// badge never disagrees with the page it links to: two decimals under 1, one
// above, rounded half away from zero from the shortest decimal form of v, as
// the browser's Intl.NumberFormat does, with the language's decimal mark.
func pageNumber(v float64, mark string) string {
	r, ok := new(big.Rat).SetString(strconv.FormatFloat(v, 'f', -1, 64))
	if !ok {
		return "—"
	}
	digits := 1
	if v < 1 {
		digits = 2
	}
	return strings.Replace(r.FloatString(digits), ".", mark, 1)
}

// clipLabel keeps a label on one line and within n characters.
func clipLabel(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return strings.TrimSpace(string([]rune(s)[:n-1])) + "…"
}

// verdana11 is the advance of each printable ASCII character, ' ' to '~', in
// Verdana at 11 px: the font and size shields.io draws its badges with.
var verdana11 = [95]float64{
	3.87, 4.33, 5.05, 9.00, 6.99, 11.86, 7.99, 2.95, 5.00, 5.00,
	6.99, 9.00, 4.00, 5.00, 4.00, 5.00, 6.99, 6.99, 6.99, 6.99,
	6.99, 6.99, 6.99, 6.99, 6.99, 6.99, 5.00, 5.00, 9.00, 9.00,
	9.00, 6.00, 11.00, 7.52, 7.54, 7.68, 8.48, 6.96, 6.32, 8.53,
	8.27, 4.63, 5.00, 7.62, 6.12, 9.27, 8.23, 8.67, 6.63, 8.67,
	7.65, 7.52, 6.78, 8.05, 7.52, 10.88, 7.54, 6.77, 7.54, 5.00,
	5.00, 5.00, 9.00, 6.99, 6.99, 6.61, 6.83, 5.73, 6.83, 6.56,
	3.86, 6.83, 6.97, 3.00, 3.79, 6.51, 3.00, 10.69, 6.97, 6.68,
	6.83, 6.83, 4.69, 5.73, 4.34, 6.97, 6.51, 8.99, 6.51, 6.51,
	5.75, 6.98, 5.00, 6.98, 9.00,
}

// textWidth estimates the rendered width of s. Anything outside ASCII is
// given the width of a digit, or a full em in the scripts drawn that wide;
// the estimate only sizes the box, since textLength fits the text to it.
func textWidth(s string) float64 {
	var w float64
	for _, r := range s {
		switch {
		case r >= ' ' && r <= '~':
			w += verdana11[r-' ']
		case unicode.Is(unicode.Mn, r):
		case unicode.In(r, unicode.Han, unicode.Hangul, unicode.Hiragana, unicode.Katakana):
			w += 11
		default:
			w += 6.99
		}
	}
	return w
}

func xmlText(s string) string {
	var b strings.Builder
	xml.EscapeText(&b, []byte(s))
	return b.String()
}

// badgeSVG draws the flat two-part shape shields.io made familiar: a grey
// label, a coloured value, five pixels of padding on each side, and each text
// drawn twice for the faint shadow under it.
func badgeSVG(label, value, color, title string) []byte {
	lt, vt := textWidth(label), textWidth(value)
	lw, vw := int(math.Round(lt))+10, int(math.Round(vt))+10
	var b bytes.Buffer
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="20" role="img" aria-label="%s">`,
		lw+vw, xmlText(title))
	fmt.Fprintf(&b, `<title>%s</title>`, xmlText(title))
	b.WriteString(`<linearGradient id="s" x2="0" y2="100%"><stop offset="0" stop-color="#bbb" stop-opacity=".1"/>` +
		`<stop offset="1" stop-opacity=".1"/></linearGradient>`)
	fmt.Fprintf(&b, `<clipPath id="r"><rect width="%d" height="20" rx="3" fill="#fff"/></clipPath>`, lw+vw)
	fmt.Fprintf(&b, `<g clip-path="url(#r)"><rect width="%d" height="20" fill="#555"/>`+
		`<rect x="%d" width="%d" height="20" fill="%s"/><rect width="%d" height="20" fill="url(#s)"/></g>`,
		lw, lw, vw, color, lw+vw)
	b.WriteString(`<g fill="#fff" text-anchor="middle" font-family="Verdana,Geneva,DejaVu Sans,sans-serif" ` +
		`text-rendering="geometricPrecision" font-size="11">`)
	text := func(x, width float64, s string) {
		s = xmlText(s)
		fmt.Fprintf(&b, `<text aria-hidden="true" x="%.1f" y="15" fill="#010101" fill-opacity=".3" `+
			`textLength="%.1f">%s</text>`, x, width, s)
		fmt.Fprintf(&b, `<text x="%.1f" y="14" textLength="%.1f">%s</text>`, x, width, s)
	}
	text(float64(lw)/2, lt, label)
	text(float64(lw)+float64(vw)/2, vt, value)
	b.WriteString(`</g></svg>`)
	return b.Bytes()
}
