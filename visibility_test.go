package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// A private target is measured like any other but must never appear on a
// public page or in the public API.
func TestPrivateTargetsHidden(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cat, err := store.CreateCategory("c", "Catégorie", "Category", true)
	if err != nil {
		t.Fatal(err)
	}
	mk := func(slug, title string, public bool) int64 {
		id, err := store.CreateTarget(&Target{CategoryID: cat, Slug: slug, Title: title,
			Host: "192.0.2.1", Proto: "icmp", IntervalS: 60, Packets: 10,
			SpacingMs: 100, TimeoutMs: 1000, Public: public, Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	pub, priv := mk("pub", "Public one", true), mk("priv", "Private one", false)

	// One measurement for each, so that every endpoint has data to return.
	now := time.Now().Unix() / 60 * 60
	for _, id := range []int64{pub, priv} {
		sk := NewSketch()
		sk.Add(1000)
		for _, tbl := range []string{"samples", "roll_1m"} {
			if _, err := store.mxw.Exec(`INSERT INTO `+tbl+`(target_id,probe_id,bucket,sent,lost,cnt,
			     min_us,max_us,sum_us,sumsq_us,sketch) VALUES(?,1,?,10,0,1,1000,1000,1000,1000000,?)`,
				id, now-60, sk.MarshalBinary()); err != nil {
				t.Fatal(err)
			}
		}
		store.mxw.Exec(`INSERT INTO live(target_id,probe_id,ts,med_us,p95_us,loss_pct) VALUES(?,1,?,1000,1000,0)`, id, now)
	}
	api := &API{store: store, token: "secret", probeID: 1}

	call := func(h http.HandlerFunc, path string, auth bool) (int, string) {
		req := httptest.NewRequest("GET", path, nil)
		if auth {
			req.Header.Set("Authorization", "Bearer secret")
		}
		rec := httptest.NewRecorder()
		h(rec, req)
		return rec.Code, rec.Body.String()
	}
	for _, c := range []struct {
		name string
		h    http.HandlerFunc
		path string
	}{
		{"tree", api.tree, "/api/v1/tree"},
		{"charts", api.charts, "/api/v1/charts?from=-24h"},
	} {
		if _, body := call(c.h, c.path, false); strings.Contains(body, "Private one") ||
			strings.Contains(body, `"target_id":`+strconv.FormatInt(priv, 10)) {
			t.Errorf("%s exposes the private target to an anonymous visitor", c.name)
		}
		if _, body := call(c.h, c.path, true); !strings.Contains(body, "Private one") &&
			!strings.Contains(body, `"target_id":`+strconv.FormatInt(priv, 10)) {
			t.Errorf("%s hides the private target from an authenticated caller", c.name)
		}
	}
	// Series: not found for a visitor, served to an authenticated caller.
	if code, _ := call(api.series, "/api/v1/series?target="+strconv.FormatInt(priv, 10)+"&from=-3h", false); code != 404 {
		t.Errorf("series of a private target: HTTP %d for a visitor, expected 404", code)
	}
	if code, _ := call(api.series, "/api/v1/series?target="+strconv.FormatInt(priv, 10)+"&from=-3h", true); code != 200 {
		t.Errorf("series of a private target: HTTP %d for an authenticated caller, expected 200", code)
	}
	if code, _ := call(api.series, "/api/v1/series?target="+strconv.FormatInt(pub, 10)+"&from=-3h", false); code != 200 {
		t.Errorf("series of a public target: HTTP %d for a visitor, expected 200", code)
	}
	// Overview: the public build must leave it out, the full one keep it.
	for _, publicOnly := range []bool{true, false} {
		ov, err := store.Overview(1, time.Now().Unix(), publicOnly)
		if err != nil {
			t.Fatal(err)
		}
		b, _ := json.Marshal(ov)
		if strings.Contains(string(b), "Private one") == publicOnly {
			t.Errorf("overview(publicOnly=%v) handles the private target wrongly", publicOnly)
		}
	}
	// Making it public again puts it back on the public side.
	tg, _ := store.TargetByID(priv)
	tg.Public = true
	if err := store.UpdateTarget(tg); err != nil {
		t.Fatal(err)
	}
	if _, body := call(api.tree, "/api/v1/tree", false); !strings.Contains(body, "Private one") {
		t.Error("a target switched back to public stays hidden")
	}
}

// Every field of a target must really be editable: the JSON tags of the
// PATCH body are easy to forget, and a missing one fails silently.
func TestTargetPatchFields(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	c1, _ := store.CreateCategory("a", "A", "A", true)
	c2, _ := store.CreateCategory("b", "B", "B", true)
	id, err := store.CreateTarget(&Target{CategoryID: c1, Slug: "t", Title: "Before", Host: "192.0.2.1",
		Proto: "icmp", IntervalS: 60, Packets: 10, SpacingMs: 100, TimeoutMs: 1000, Public: true, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	api := &API{store: store, token: "secret", probeID: 1}
	body := fmt.Sprintf(`{"category_id":%d,"title":"After","host":"198.51.100.9","proto":"tcp","port":443,
	        "family":6,"interval_s":300,"packets":12,"spacing_ms":200,"timeout_ms":1500,"public":false}`, c2)
	req := httptest.NewRequest("PATCH", "/api/v1/admin/targets/"+strconv.FormatInt(id, 10),
		strings.NewReader(body))
	req.SetPathValue("id", strconv.FormatInt(id, 10))
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	api.targetsPatch(rec, req)
	if rec.Code != 200 {
		t.Fatalf("HTTP %d: %s", rec.Code, rec.Body.String())
	}
	got, err := store.TargetByID(id)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		field string
		got   any
		want  any
	}{
		{"category_id", got.CategoryID, c2}, {"title", got.Title, "After"},
		{"host", got.Host, "198.51.100.9"}, {"proto", got.Proto, "tcp"},
		{"port", got.Port, 443}, {"family", got.Family, 6},
		{"interval_s", got.IntervalS, int64(300)}, {"packets", got.Packets, 12},
		{"spacing_ms", got.SpacingMs, 200}, {"timeout_ms", got.TimeoutMs, 1500},
		{"public", got.Public, false},
	} {
		if fmt.Sprint(c.got) != fmt.Sprint(c.want) {
			t.Errorf("%s was not saved: got %v, want %v", c.field, c.got, c.want)
		}
	}
}

// Deleting a target archives it: the name becomes free, the history stays
// attached to the archived one, and above all the new target must not
// inherit its measurements — SQLite reuses identifiers otherwise.
func TestArchiveFreesTheNameWithoutMixingHistory(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cat, _ := store.CreateCategory("c", "C", "C", true)
	mk := func() (int64, error) {
		return store.CreateTarget(&Target{CategoryID: cat, Slug: "transit-paris", Title: "Transit Paris",
			Host: "192.0.2.1", Proto: "icmp", IntervalS: 60, Packets: 10, SpacingMs: 100,
			TimeoutMs: 1000, Public: true, Enabled: true})
	}
	first, err := mk()
	if err != nil {
		t.Fatal(err)
	}
	// One measurement, so history mixing would be visible.
	if err := store.RecordBatch([]queuedMeasure{{m: Measurement{TargetID: first, ProbeID: 1,
		TS: time.Now().Unix(), Sent: 10, RTTus: []float64{1000, 1100, 1050}}}}); err != nil {
		t.Fatal(err)
	}
	if err := store.ArchiveTarget(first); err != nil {
		t.Fatal(err)
	}
	// The name is free again.
	second, err := mk()
	if err != nil {
		t.Fatalf("the name should be free after archiving: %v", err)
	}
	if second == first {
		t.Fatal("the new target reuses the archived one's identifier, and would inherit its history")
	}
	// The archived one keeps its history and is out of the active lists.
	act, _ := store.ActiveTargets()
	for _, tg := range act {
		if tg.ID == first {
			t.Error("an archived target must not be measured any more")
		}
	}
	arch, err := store.ArchivedTargets()
	if err != nil || len(arch) != 1 || !strings.Contains(arch[0].Title, "archived") {
		t.Errorf("archived list: %+v %v", arch, err)
	}
	if arch[0].Slug == "transit-paris" {
		t.Error("the archived slug must have been renamed")
	}
	// Archiving twice is refused; purging needs an archived target.
	if err := store.ArchiveTarget(first); err == nil {
		t.Error("archiving twice should be refused")
	}
	if err := store.PurgeTarget(second); err == nil {
		t.Error("purging an active target should be refused")
	}
	if err := store.PurgeTarget(first); err != nil {
		t.Errorf("purge: %v", err)
	}
	if arch, _ := store.ArchivedTargets(); len(arch) != 0 {
		t.Error("the purged target is still listed")
	}
}
