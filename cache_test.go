package main

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// Two rules, checked across every read endpoint at once rather than one
// endpoint at a time, because the two holes this test was written for were
// both found by someone reading the code and not by the suite.
//
//	A. An answer that differs by caller must say so. A shared cache in front
//	   of an instance keys on the URL; without Vary it may keep the operator's
//	   answer and hand it to the next visitor.
//	B. An answer only an operator or a share link can obtain must not be
//	   announced as publicly cacheable. That holds for the 404 as well: a 404
//	   is cacheable by default, so a visitor's "target not found" could be
//	   kept and handed back to the operator whose target it is.
//
// Adding an endpoint to the table is cheaper than remembering the rules.
func TestReadEndpointsTellSharedCachesTheTruth(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cat, err := store.CreateCategory("c", "Catégorie", "Category", true)
	if err != nil {
		t.Fatal(err)
	}
	// Distinct from the public target's, so that finding either string in a
	// visitor's answer means the private target leaked into it.
	const privHost, privTitle = "198.51.100.77", "Private one"
	mk := func(slug, title, host string, public bool) int64 {
		id, err := store.CreateTarget(&Target{CategoryID: cat, Slug: slug, Title: title,
			Host: host, Proto: "icmp", IntervalS: 60, Packets: 10,
			SpacingMs: 100, TimeoutMs: 1000, Public: public, Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	pub := mk("pub", "Public one", "192.0.2.1", true)
	priv := mk("priv", privTitle, privHost, false)
	now := time.Now().Unix() / 60 * 60
	for _, id := range []int64{pub, priv} {
		sk := NewSketch()
		sk.Add(1000)
		for _, tbl := range []string{"samples", "roll_1m", "roll_1h"} {
			if _, err := store.mxw.Exec(`INSERT INTO `+tbl+`(target_id,probe_id,bucket,sent,lost,cnt,
			     min_us,max_us,sum_us,sumsq_us,sketch) VALUES(?,1,?,10,0,1,1000,1000,1000,1000000,?)`,
				id, now-3600, sk.MarshalBinary()); err != nil {
				t.Fatal(err)
			}
		}
	}
	// The ASN service is wired because the instance endpoint starts a
	// background lookup through it; main.go always sets it, so leaving it nil
	// here would test a shape production never has.
	api := &API{store: store, token: "secret", probeID: 1,
		asn: NewASNService(store, nil, "")}

	type call struct {
		name string
		path string
		h    func(http.ResponseWriter, *http.Request)
		// private is true when the request is about a target no visitor may
		// read: rule B applies to it whatever the body turns out to be.
		private bool
	}
	q := func(id int64) string { return "target=" + strconv.FormatInt(id, 10) }
	calls := []call{
		{"tree", "/api/v1/tree", api.tree, false},
		{"charts", "/api/v1/charts", api.charts, false},
		{"events", "/api/v1/events", api.events, false},
		{"site", "/api/v1/site", api.siteGet, false},
		{"instance", "/api/v1/instance", api.instanceGet, false},
		{"overview", "/api/v1/overview", api.overview, false},
		{"series, public target", "/api/v1/series?" + q(pub) + "&span=3600", api.series, false},
		{"series, private target", "/api/v1/series?" + q(priv) + "&span=3600", api.series, true},
		{"availability, public target", "/api/v1/availability?" + q(pub), api.availability, false},
		{"availability, private target", "/api/v1/availability?" + q(priv), api.availability, true},
		{"aspath, public target", "/api/v1/aspath?" + q(pub), api.asPathView, false},
		{"aspath, private target", "/api/v1/aspath?" + q(priv), api.asPathView, true},
		{"traceroutes", "/api/v1/traceroutes", api.traceroutesPublic, false},
	}

	hit := func(c call, auth bool) *httptest.ResponseRecorder {
		req := httptest.NewRequest("GET", c.path, nil)
		if auth {
			req.Header.Set("Authorization", "Bearer secret")
		}
		w := httptest.NewRecorder()
		c.h(w, req)
		return w
	}
	public := func(cc string) bool { return strings.Contains(cc, "public") }
	kept := func(cc string) bool {
		// What a shared cache is allowed to store and reuse for someone else.
		return public(cc) || (!strings.Contains(cc, "no-store") &&
			!strings.Contains(cc, "private") && !strings.Contains(cc, "no-cache"))
	}

	// Whether the answer depends on WHO ASKED, which is the question this
	// whole test exists to answer. Comparing one visitor response to one
	// operator response does not answer it: a body carrying the clock differs
	// from itself between two calls, and /api/v1/instance reports uptime in
	// whole seconds, so two successive requests either side of a second
	// boundary looked exactly like a caller-dependent answer. That is what
	// made this test fail at random on branches that had changed nothing near
	// it.
	//
	// A caller-dependent difference is deterministic and survives every
	// attempt; a clock-driven one does not. So the pair is taken again when
	// it differs, and only a difference that persists counts. This cannot
	// hide a real one, which is the property that matters.
	callerDependent := func(c call) (bool, *httptest.ResponseRecorder, *httptest.ResponseRecorder) {
		var anon, authed *httptest.ResponseRecorder
		for attempt := 0; attempt < 3; attempt++ {
			anon, authed = hit(c, false), hit(c, true)
			if anon.Body.String() == authed.Body.String() {
				return false, anon, authed
			}
		}
		return true, anon, authed
	}

	for _, c := range calls {
		differs, anon, authed := callerDependent(c)
		aCC, aVary := anon.Header().Get("Cache-Control"), anon.Header().Get("Vary")
		uCC := authed.Header().Get("Cache-Control")

		// Vary is only owed on an answer a cache may actually store. Where
		// the visitor's answer is no-store there is nothing to vary on, and
		// asking for the header anyway would push it onto the public pages
		// where it costs an entry per visitor.
		if differs && kept(aCC) && !strings.Contains(aVary, "Authorization") {
			t.Errorf("%s: the answer differs by caller, the visitor's is Cache-Control=%q "+
				"which a cache may store, and it carries Vary=%q — a shared cache cannot "+
				"tell the two apart", c.name, aCC, aVary)
		}
		if differs && kept(uCC) {
			t.Errorf("%s: the operator's answer differs from the visitor's and is Cache-Control=%q, "+
				"which a shared cache may keep and hand to a visitor", c.name, uCC)
		}
		if c.private {
			// The leak direction: the operator's answer holds a target no
			// visitor may read, so no shared cache may keep it. The 404 is
			// included on purpose — a 404 is cacheable by default, so a
			// visitor's "target not found" could be stored and handed back
			// to the operator whose target it is.
			if kept(uCC) {
				t.Errorf("%s: the operator's answer is Cache-Control=%q; nothing about a "+
					"target no visitor may read belongs in a shared cache, the 404 included",
					c.name, uCC)
			}
			// The visitor's answer may well be cacheable — an endpoint that
			// deliberately answers an empty path to everyone is doing the
			// right thing, and caching that reveals nothing. What it must
			// not do is carry the target.
			for _, secret := range []string{privHost, privTitle} {
				if strings.Contains(anon.Body.String(), secret) {
					t.Errorf("%s: the visitor's answer carries %q", c.name, secret)
				}
			}
		}
		t.Logf("%-30s visitor %-24q vary=%-22q operator %-24q differs=%v",
			c.name, aCC, aVary, uCC, differs)
	}
	_ = fmt.Sprint
}

// An SSE stream holds a goroutine and a poll until the client goes away.
// Bounding only the total would be its own denial of service: one client
// opening the ceiling would take the endpoint from everybody.
func TestLiveStreamsAreBoundedPerAddressAndInTotal(t *testing.T) {
	var l liveStreams
	for i := 0; i < maxLivePerAddr; i++ {
		if !l.take("198.51.100.1") {
			t.Fatalf("stream %d from one address refused below the per-address cap", i+1)
		}
	}
	if l.take("198.51.100.1") {
		t.Error("one address went past its cap")
	}
	if !l.take("198.51.100.2") {
		t.Error("a second address was refused because the first had reached its cap")
	}

	// Giving a stream back frees the slot, and the per-address counter is
	// dropped rather than left at zero.
	l.done("198.51.100.2")
	l.mu.Lock()
	_, stillThere := l.per["198.51.100.2"]
	l.mu.Unlock()
	if stillThere {
		t.Error("the counter for an address with no stream left is kept")
	}
	if !l.take("198.51.100.2") {
		t.Error("the freed slot was not reusable")
	}

	// And the total holds, across as many addresses as it takes.
	var g liveStreams
	n := 0
	for i := 0; n < maxLiveTotal+50; i++ {
		if g.take(fmt.Sprintf("198.51.100.%d", i%256)) {
			n++
			continue
		}
		break
	}
	if n != maxLiveTotal {
		t.Errorf("accepted %d streams in total, the ceiling is %d", n, maxLiveTotal)
	}
}
