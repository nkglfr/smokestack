package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

// "0 = no expiry" is what the prompt promises, so an explicit 0 must not be
// read as a missing value and replaced by the 30-day default (issue #18).
func TestShareDaysZeroMeansNoExpiry(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	shareSchema(store)
	cat, _ := store.CreateCategory("c", "C", "C", true)
	id, err := store.CreateTarget(&Target{CategoryID: cat, Slug: "t", Title: "T", Host: "192.0.2.1",
		Proto: "icmp", IntervalS: 60, Packets: 10, SpacingMs: 100, TimeoutMs: 1000,
		Public: true, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	api := &API{store: store}
	create := func(body string) ShareLink {
		t.Helper()
		r := httptest.NewRequest("POST", "/api/v1/admin/targets/1/share", strings.NewReader(body))
		r.SetPathValue("id", "1")
		w := httptest.NewRecorder()
		api.shareCreate(w, r, &User{Email: "noc@example.net"})
		if w.Code != 200 {
			t.Fatalf("HTTP %d for %s: %s", w.Code, body, w.Body.String())
		}
		var l ShareLink
		if err := json.Unmarshal(w.Body.Bytes(), &l); err != nil {
			t.Fatal(err)
		}
		return l
	}
	_ = id
	if l := create(`{"days":0,"note":"forever"}`); l.ExpiresAt != 0 {
		t.Errorf("0 days must mean no expiry, got an expiry at %d", l.ExpiresAt)
	}
	if l := create(`{"note":"default"}`); l.ExpiresAt == 0 {
		t.Error("with no days given, the 30-day default must still apply")
	}
	if l := create(`{"days":7,"note":"a week"}`); l.ExpiresAt == 0 {
		t.Error("7 days must set an expiry")
	}
}
