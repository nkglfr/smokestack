package main

import (
	"net/http/httptest"
	"strconv"
	"testing"
)

// The public probes endpoint lists enabled probes only. The data endpoints
// took ?probe= on trust, so disabling a probe removed it from the listing and
// left its measurements answerable by id. That made the filter decorative.
func TestADisabledProbeIsNotReadableByAVisitor(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	on, err := store.CreateProbe("on", "On", "")
	if err != nil {
		t.Fatal(err)
	}
	off, err := store.CreateProbe("off", "Off", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetProbeEnabled(off.ID, false); err != nil {
		t.Fatal(err)
	}

	if !store.ProbeReadable(on.ID, false) {
		t.Error("an enabled probe must be readable by a visitor")
	}
	if store.ProbeReadable(off.ID, false) {
		t.Error("a disabled probe must not be readable by a visitor")
	}
	// The back-office is where enabling is decided, so it has to be able to
	// look at what the probe did before it was turned off.
	if !store.ProbeReadable(off.ID, true) {
		t.Error("a disabled probe must stay readable once authenticated")
	}
	// An id nobody registered, and the values that reach this from a URL.
	for _, id := range []int64{0, -1, 999999} {
		if store.ProbeReadable(id, true) {
			t.Errorf("probe id %d must not be readable", id)
		}
	}
}

// An unreadable id has to behave exactly like no parameter at all, so that
// walking the ids says nothing the public listing does not already say.
func TestResolveProbeFallsBackRatherThanRefusing(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	on, err := store.CreateProbe("on", "On", "")
	if err != nil {
		t.Fatal(err)
	}
	off, err := store.CreateProbe("off", "Off", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetProbeEnabled(off.ID, false); err != nil {
		t.Fatal(err)
	}

	a := &API{store: store, probeID: 1}

	cases := []struct {
		query    string
		fallback int64
		want     int64
		why      string
	}{
		{"", 1, 1, "no parameter uses the fallback"},
		{"?probe=", 1, 1, "an empty parameter uses the fallback"},
		{"?probe=abc", 1, 1, "an unparseable parameter uses the fallback"},
		{"?probe=0", 1, 1, "zero is not an id"},
		{"?probe=-3", 1, 1, "a negative id is not an id"},
		{"?probe=999999", 1, 1, "an id nobody registered uses the fallback"},
		{"?probe=abc", 0, 0, "the fallback is the caller's, not a constant"},
		{"?probe=999999", 0, 0, "an unknown id with a zero fallback stays zero"},
	}
	for _, c := range cases {
		r := httptest.NewRequest("GET", "/api/v1/series"+c.query, nil)
		if got, _ := a.resolveProbe(r, c.fallback); got != c.want {
			t.Errorf("%s: resolveProbe(%q, %d) = %d, want %d",
				c.why, c.query, c.fallback, got, c.want)
		}
	}

	id := func(n int64) string { return "/api/v1/series?probe=" + strconv.FormatInt(n, 10) }

	// An enabled probe is honoured, which is the whole point of the parameter.
	r := httptest.NewRequest("GET", id(on.ID), nil)
	if got, _ := a.resolveProbe(r, 1); got != on.ID {
		t.Errorf("an enabled probe was not honoured: got %d, want %d", got, on.ID)
	}

	// A disabled one is not, for a visitor, and the answer is the fallback
	// rather than a refusal: a stale bookmark still shows the page.
	r = httptest.NewRequest("GET", id(off.ID), nil)
	if got, _ := a.resolveProbe(r, 1); got != 1 {
		t.Errorf("a disabled probe leaked through to a visitor: got %d", got)
	}

	// The same request, authenticated with the admin token, reaches it. This
	// is the back-office path, and it must not be shut out by the fix.
	withToken := &API{store: store, probeID: 1, token: "s3cret"}
	r = httptest.NewRequest("GET", id(off.ID), nil)
	r.Header.Set("Authorization", "Bearer s3cret")
	if got, _ := withToken.resolveProbe(r, 1); got != off.ID {
		t.Errorf("the back-office cannot read a disabled probe: got %d, want %d",
			got, off.ID)
	}

	// And a wrong token is not authentication.
	r = httptest.NewRequest("GET", id(off.ID), nil)
	r.Header.Set("Authorization", "Bearer wrong")
	if got, _ := withToken.resolveProbe(r, 1); got != 1 {
		t.Errorf("a wrong bearer token reached a disabled probe: got %d", got)
	}
}

// The fix above reads the caller's authentication, which means the same URL
// can answer differently to two callers. series and availability carry no
// Vary, deliberately: one shared cache entry per URL. So the authenticated
// answer must never be the one marked publicly cacheable, or a CDN hands a
// disabled probe's measurements to every visitor — the leak the fix closes,
// arriving by the back door. This test is here because that defect was
// written and nearly shipped.
func TestAPrivilegedProbeChoiceIsNotPubliclyCacheable(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	on, err := store.CreateProbe("on", "On", "")
	if err != nil {
		t.Fatal(err)
	}
	off, err := store.CreateProbe("off", "Off", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetProbeEnabled(off.ID, false); err != nil {
		t.Fatal(err)
	}

	a := &API{store: store, probeID: 1, token: "s3cret"}
	url := func(n int64) string {
		return "/api/v1/series?probe=" + strconv.FormatInt(n, 10)
	}

	// Anonymous, enabled probe: ordinary, and cacheable.
	r := httptest.NewRequest("GET", url(on.ID), nil)
	if id, priv := a.resolveProbe(r, 1); id != on.ID || priv {
		t.Errorf("enabled probe anonymously: got (%d, %v), want (%d, false)",
			id, priv, on.ID)
	}

	// Authenticated, enabled probe: still the same answer anyone would get,
	// so still cacheable. Marking this private would cost the cache for
	// every graph an operator looks at.
	r = httptest.NewRequest("GET", url(on.ID), nil)
	r.Header.Set("Authorization", "Bearer s3cret")
	if id, priv := a.resolveProbe(r, 1); id != on.ID || priv {
		t.Errorf("enabled probe authenticated: got (%d, %v), want (%d, false)",
			id, priv, on.ID)
	}

	// Authenticated, disabled probe: the privileged case. The answer is the
	// probe, and it must be flagged.
	r = httptest.NewRequest("GET", url(off.ID), nil)
	r.Header.Set("Authorization", "Bearer s3cret")
	id, priv := a.resolveProbe(r, 1)
	if id != off.ID {
		t.Errorf("the back-office cannot read a disabled probe: got %d", id)
	}
	if !priv {
		t.Error("reading a disabled probe was not flagged as privileged, so the " +
			"response would be announced as publicly cacheable")
	}

	// Anonymous, disabled probe: the fallback, and nothing privileged about
	// it — this response is the same one every visitor gets.
	r = httptest.NewRequest("GET", url(off.ID), nil)
	if id, priv := a.resolveProbe(r, 1); id != 1 || priv {
		t.Errorf("disabled probe anonymously: got (%d, %v), want (1, false)", id, priv)
	}

	// An id nobody registered is never privileged, whoever asks.
	r = httptest.NewRequest("GET", url(999999), nil)
	r.Header.Set("Authorization", "Bearer s3cret")
	if id, priv := a.resolveProbe(r, 1); id != 1 || priv {
		t.Errorf("unknown id authenticated: got (%d, %v), want (1, false)", id, priv)
	}
}
