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
		if got := a.resolveProbe(r, c.fallback); got != c.want {
			t.Errorf("%s: resolveProbe(%q, %d) = %d, want %d",
				c.why, c.query, c.fallback, got, c.want)
		}
	}

	id := func(n int64) string { return "/api/v1/series?probe=" + strconv.FormatInt(n, 10) }

	// An enabled probe is honoured, which is the whole point of the parameter.
	r := httptest.NewRequest("GET", id(on.ID), nil)
	if got := a.resolveProbe(r, 1); got != on.ID {
		t.Errorf("an enabled probe was not honoured: got %d, want %d", got, on.ID)
	}

	// A disabled one is not, for a visitor, and the answer is the fallback
	// rather than a refusal: a stale bookmark still shows the page.
	r = httptest.NewRequest("GET", id(off.ID), nil)
	if got := a.resolveProbe(r, 1); got != 1 {
		t.Errorf("a disabled probe leaked through to a visitor: got %d", got)
	}

	// The same request, authenticated with the admin token, reaches it. This
	// is the back-office path, and it must not be shut out by the fix.
	withToken := &API{store: store, probeID: 1, token: "s3cret"}
	r = httptest.NewRequest("GET", id(off.ID), nil)
	r.Header.Set("Authorization", "Bearer s3cret")
	if got := withToken.resolveProbe(r, 1); got != off.ID {
		t.Errorf("the back-office cannot read a disabled probe: got %d, want %d",
			got, off.ID)
	}

	// And a wrong token is not authentication.
	r = httptest.NewRequest("GET", id(off.ID), nil)
	r.Header.Set("Authorization", "Bearer wrong")
	if got := withToken.resolveProbe(r, 1); got != 1 {
		t.Errorf("a wrong bearer token reached a disabled probe: got %d", got)
	}
}
