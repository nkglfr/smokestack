package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The public listing exists so that a page can offer a choice of vantage
// point. It is the same answer for everybody, and these pin the three things
// that makes true: no Vary, no unmasked address, and nothing about a probe
// that is not measuring.
func TestThePublicProbeListIsTheSameForEveryone(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	api := &API{store: store, token: "secret", probeID: 1}

	if _, err := store.ProbeID("local", "Local", "Besançon, FR", true); err != nil {
		t.Fatal(err)
	}
	live, err := store.CreateProbe("lyon-1", "Lyon 1", "Lyon, FR")
	if err != nil {
		t.Fatal(err)
	}
	off, err := store.CreateProbe("off-1", "Switched off", "Nowhere")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetProbeEnabled(off.ID, false); err != nil {
		t.Fatal(err)
	}
	const addr = "198.51.100.77"
	if err := store.SetProbeAddress(live.ID, addr); err != nil {
		t.Fatal(err)
	}

	w := httptest.NewRecorder()
	api.probesPublic(w, httptest.NewRequest("GET", "/api/v1/probes", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("got %d", w.Code)
	}
	body := w.Body.String()

	// The address of a machine the operator runs is not a visitor's business
	// at full precision, the same rule every other public page follows.
	if strings.Contains(body, addr) {
		t.Errorf("the full address of a probe is public: %s", body)
	}
	if !strings.Contains(body, maskIP(addr)) {
		t.Errorf("the masked address is missing: %s", body)
	}

	// A disabled probe is not measuring. Offering it would show an empty
	// graph and say nothing about why it is empty.
	if strings.Contains(body, "off-1") {
		t.Errorf("a disabled probe is offered as a vantage point: %s", body)
	}
	if !strings.Contains(body, "lyon-1") || !strings.Contains(body, "local") {
		t.Errorf("a probe that is measuring is missing: %s", body)
	}

	// Nothing here depends on who asked, which is what lets this be cached
	// once for everybody. Vary would mean one cache entry per visitor, on
	// exactly the pages that need the cache most during an incident.
	if v := w.Header().Get("Vary"); v != "" {
		t.Errorf("the public listing varies by %q, so it cannot be shared", v)
	}
	if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "public") {
		t.Errorf("Cache-Control is %q, want it publicly cacheable", cc)
	}

	// And none of the operator-only fields leaked in with the rest.
	var out struct {
		Probes []map[string]any `json:"probes"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	for _, p := range out.Probes {
		for _, forbidden := range []string{"has_token", "token", "last_seen_at", "seen_address", "address", "enabled", "all_targets"} {
			if _, ok := p[forbidden]; ok {
				t.Errorf("the public listing carries %q", forbidden)
			}
		}
	}
}
