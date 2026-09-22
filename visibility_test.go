package main

import (
	"encoding/json"
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
