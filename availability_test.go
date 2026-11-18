package main

import (
	"bytes"
	"encoding/json"
	"net/http/httptest"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func availStore(t *testing.T) (*Store, int64) {
	t.Helper()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	cat, _ := store.CreateCategory("c", "C", "C", true)
	id, err := store.CreateTarget(&Target{CategoryID: cat, Slug: "t", Title: "T",
		Host: "192.0.2.1", Proto: "icmp", IntervalS: 60, Packets: 5, SpacingMs: 100,
		TimeoutMs: 1000, Public: true, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	return store, id
}

// A pass counts as down only when nothing came back. Partial loss proves
// the target answers, which is the whole point of the definition.
func TestAvailabilityCountsPasses(t *testing.T) {
	store, id := availStore(t)
	now := time.Now()
	base := now.Unix() - 3000
	rec := func(i int, sent, lost int) {
		m := Measurement{TargetID: id, ProbeID: 1, TS: base + int64(i)*60,
			Sent: sent, Lost: lost}
		for j := 0; j < sent-lost; j++ {
			m.RTTus = append(m.RTTus, 1000)
		}
		if err := store.Record(m); err != nil {
			t.Fatal(err)
		}
	}
	// Ten passes: eight clean, one losing four packets out of five, one
	// entirely silent. Availability is 90 %, not the 92 % a packet count
	// would give, and the two numbers must not be confused.
	for i := 0; i < 8; i++ {
		rec(i, 5, 0)
	}
	rec(8, 5, 4)
	rec(9, 5, 5)

	w := store.availWindow(id, 1, "1h", base-60, now.Unix()+60)
	if w.Passes != 10 || w.Down != 1 {
		t.Fatalf("ten passes, one silent, got passes=%d down=%d", w.Passes, w.Down)
	}
	if w.Pct == nil || *w.Pct < 89.9 || *w.Pct > 90.1 {
		t.Fatalf("availability should be 90 %%, got %v", w.Pct)
	}
}

// Nothing measured is not the same as nothing available: the percentage
// stays absent rather than reading zero.
func TestAvailabilityWithoutData(t *testing.T) {
	store, id := availStore(t)
	w := store.availWindow(id, 1, "24h", time.Now().Unix()-86400, time.Now().Unix())
	if w.Pct != nil {
		t.Errorf("an empty window has no percentage, got %v", *w.Pct)
	}
	if w.Passes != 0 || w.Partial {
		t.Errorf("an empty window is empty, not partial: %+v", w)
	}
}

// A window whose measurements start well after its beginning is partial,
// so a target created last week does not advertise a yearly figure.
func TestAvailabilityPartialWindow(t *testing.T) {
	store, id := availStore(t)
	now := time.Now()
	// One full hour of passes, and nothing before it.
	for i := 0; i < 60; i++ {
		if err := store.Record(Measurement{TargetID: id, ProbeID: 1,
			TS: now.Unix() - int64(i)*60, Sent: 5, Lost: 0,
			RTTus: []float64{1000, 1000, 1000, 1000, 1000}}); err != nil {
			t.Fatal(err)
		}
	}
	full := store.availWindow(id, 1, "1h", now.Unix()-3600, now.Unix()+60)
	if full.Partial {
		t.Errorf("an hour covered from its start is not partial: %+v", full)
	}
	for i := 0; i+1 < len(cascade); i++ {
		if err := store.RollupRange(cascade[i].table, cascade[i+1].table,
			cascade[i+1].secs, now.Unix()-7200, now.Unix()+60); err != nil {
			t.Fatal(err)
		}
	}
	// Two days asked for, one hour measured: the figure exists but must
	// say it does not cover the window.
	old := store.availWindow(id, 1, "2d", now.Unix()-2*86400, now.Unix()+60)
	if old.Pct == nil {
		t.Fatalf("the hour measured should still produce a figure: %+v", old)
	}
	if !old.Partial {
		t.Errorf("two days covered only by the last hour is partial: %+v", old)
	}
	// And a window with no measurement at all is unknown rather than
	// partial: there is nothing to qualify.
	empty := store.availWindow(id, 1, "1y", now.Unix()-365*86400, now.Unix()-300*86400)
	if empty.Pct != nil || empty.Partial {
		t.Errorf("an unmeasured window is unknown, not partial: %+v", empty)
	}
}

// The counters must survive the aggregation cascade, otherwise every
// window longer than two days would read as unknown.
func TestAvailabilitySurvivesRollup(t *testing.T) {
	store, id := availStore(t)
	now := time.Now().Unix()
	from := now - 7200
	for i := 0; i < 60; i++ {
		lost := 0
		if i%10 == 0 {
			lost = 5 // six silent passes out of sixty
		}
		m := Measurement{TargetID: id, ProbeID: 1, TS: from + int64(i)*60,
			Sent: 5, Lost: lost}
		for j := 0; j < 5-lost; j++ {
			m.RTTus = append(m.RTTus, 1000)
		}
		if err := store.Record(m); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i+1 < len(cascade); i++ {
		if err := store.RollupRange(cascade[i].table, cascade[i+1].table,
			cascade[i+1].secs, from-86400, now+86400); err != nil {
			t.Fatal(err)
		}
	}
	for _, tbl := range []string{"roll_1m", "roll_5m", "roll_1h", "roll_1d"} {
		var passes, down int64
		if err := store.mx.QueryRow("SELECT COALESCE(SUM(passes),0),COALESCE(SUM(down),0) FROM "+
			tbl+" WHERE target_id=?", id).Scan(&passes, &down); err != nil {
			t.Fatal(err)
		}
		if passes != 60 || down != 6 {
			t.Errorf("%s: passes=%d down=%d, expected 60 and 6", tbl, passes, down)
		}
	}
}

// Which table answers which window. Reading a yearly figure out of
// samples would return almost nothing, since samples keep two days.
func TestAvailTable(t *testing.T) {
	cases := []struct {
		span int64
		want string
	}{
		{3600, "samples"},
		{86400, "roll_1m"},
		{7 * 86400, "roll_1h"},
		{30 * 86400, "roll_1h"},
		{365 * 86400, "roll_1d"},
	}
	for _, c := range cases {
		if got := availTable(c.span); got != c.want {
			t.Errorf("a %d s window should read %s, got %s", c.span, c.want, got)
		}
	}
	// Every table the picker names must be one the cascade keeps long
	// enough, otherwise the figure silently covers less than it claims.
	keep := map[string]int64{}
	for _, g := range cascade {
		keep[g.table] = g.keep
	}
	for _, w := range availWindows {
		tbl := availTable(w.secs)
		if k, ok := keep[tbl]; !ok {
			t.Errorf("window %s reads unknown table %s", w.label, tbl)
		} else if k != 0 && k < w.secs {
			t.Errorf("window %s reads %s, kept only %d s", w.label, tbl, k)
		}
	}
}

// The probe writes through RecordBatch, never through Record. When the two
// were separate copies of the same INSERT, a column added to the tested one
// alone was written nowhere in production while every test passed. This
// test goes through the batch path on purpose, and the one below pins the
// two paths together.
func TestAvailabilityThroughTheBatchPath(t *testing.T) {
	store, id := availStore(t)
	now := time.Now()
	base := now.Unix() - 1800
	var batch []queuedMeasure
	for i := 0; i < 20; i++ {
		lost := 0
		if i%5 == 0 {
			lost = 5 // four silent passes out of twenty
		}
		m := Measurement{TargetID: id, ProbeID: 1, TS: base + int64(i)*60,
			Sent: 5, Lost: lost}
		for j := 0; j < 5-lost; j++ {
			m.RTTus = append(m.RTTus, 1000)
		}
		batch = append(batch, queuedMeasure{m: m, host: "192.0.2.1"})
	}
	if err := store.RecordBatch(batch); err != nil {
		t.Fatal(err)
	}
	w := store.availWindow(id, 1, "1h", base-60, now.Unix()+60)
	if w.Passes != 20 || w.Down != 4 {
		t.Fatalf("the batch path must count passes too: passes=%d down=%d", w.Passes, w.Down)
	}
	if w.Pct == nil || *w.Pct < 79.9 || *w.Pct > 80.1 {
		t.Errorf("availability should be 80 %%, got %v", w.Pct)
	}
}

// Whatever a single write and a batched write do, they must do the same
// thing, so a column can never again be filled by one and not the other.
func TestRecordAndRecordBatchAgree(t *testing.T) {
	store, id := availStore(t)
	tg, err := store.TargetByID(id)
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.CreateTarget(&Target{CategoryID: tg.CategoryID, Slug: "t2", Title: "T2",
		Host: "192.0.2.2", Proto: "icmp", IntervalS: 60, Packets: 5, SpacingMs: 100,
		TimeoutMs: 1000, Public: true, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	ts := time.Now().Unix() - 600
	mk := func(target int64, lost int) Measurement {
		m := Measurement{TargetID: target, ProbeID: 1, TS: ts, Sent: 5, Lost: lost}
		for j := 0; j < 5-lost; j++ {
			m.RTTus = append(m.RTTus, 1234)
		}
		return m
	}
	for _, lost := range []int{0, 3, 5} {
		ts++
		if err := store.Record(mk(id, lost)); err != nil {
			t.Fatal(err)
		}
		if err := store.RecordBatch([]queuedMeasure{{m: mk(other, lost), host: ""}}); err != nil {
			t.Fatal(err)
		}
	}
	cols := "sent,lost,cnt,min_us,max_us,sum_us,sumsq_us,passes,down"
	rows := func(target int64) [][]float64 {
		r, err := store.mx.Query("SELECT "+cols+
			" FROM samples WHERE target_id=? ORDER BY bucket", target)
		if err != nil {
			t.Fatal(err)
		}
		defer r.Close()
		var out [][]float64
		for r.Next() {
			v := make([]float64, 9)
			ptr := make([]any, 9)
			for i := range v {
				ptr[i] = &v[i]
			}
			if err := r.Scan(ptr...); err != nil {
				t.Fatal(err)
			}
			out = append(out, v)
		}
		return out
	}
	a, b := rows(id), rows(other)
	if len(a) != 3 || len(b) != 3 {
		t.Fatalf("three passes each, got %d and %d", len(a), len(b))
	}
	for i := range a {
		for j := range a[i] {
			if a[i][j] != b[i][j] {
				t.Errorf("pass %d: Record and RecordBatch disagree on column %d of (%s): %v vs %v",
					i, j, cols, a[i][j], b[i][j])
			}
		}
	}
}

// Publishing the figure is a setting. Switched off, it must not leave by
// the API either: hiding the block in the browser would leave the
// endpoint serving it to whoever knows its address.
func TestAvailabilityNotPublished(t *testing.T) {
	store, id := availStore(t)
	now := time.Now().Unix()
	for i := 0; i < 5; i++ {
		if err := store.Record(Measurement{TargetID: id, ProbeID: 1,
			TS: now - int64(i)*60, Sent: 5, Lost: 0,
			RTTus: []float64{1000, 1000, 1000, 1000, 1000}}); err != nil {
			t.Fatal(err)
		}
	}
	api := &API{store: store, token: "secret", probeID: 1}

	// The answer is cached for a minute, which would otherwise hide the
	// effect of the setting on the second call.
	get := func(auth bool) int {
		availCache.mu.Lock()
		availCache.items = map[string]availEntry{}
		availCache.mu.Unlock()
		req := httptest.NewRequest("GET",
			"/api/v1/availability?target="+strconv.FormatInt(id, 10), nil)
		if auth {
			req.Header.Set("Authorization", "Bearer secret")
		}
		w := httptest.NewRecorder()
		api.availability(w, req)
		return w.Code
	}

	// The default, and what an instance configured before the setting
	// existed keeps after an upgrade.
	if code := get(false); code != 200 {
		t.Errorf("published by default: %d", code)
	}

	site := store.Site()
	site.PublicAvailability = false
	if err := store.SetSite(site); err != nil {
		t.Fatal(err)
	}
	if code := get(false); code != 404 {
		t.Errorf("switched off, a visitor must get 404, not %d", code)
	}
	if code := get(true); code != 200 {
		t.Errorf("switched off, the back-office still reads it: %d", code)
	}

	site.PublicAvailability = true
	if err := store.SetSite(site); err != nil {
		t.Fatal(err)
	}
	if code := get(false); code != 200 {
		t.Errorf("published again: %d", code)
	}
}

// The target page's endpoint answers what it answered before its cache was
// shared with the badges: the same three fields, the windows the store
// computes for the same data, and the same bytes again from the cache.
func TestAvailabilityEndpointAnswer(t *testing.T) {
	store, id := availStore(t)
	now := time.Now().Unix()
	for i := 0; i < 30; i++ {
		m := Measurement{TargetID: id, ProbeID: 1, TS: now - int64(i)*60, Sent: 5}
		if i%10 == 0 {
			m.Lost = 5
		}
		for j := 0; j < m.Sent-m.Lost; j++ {
			m.RTTus = append(m.RTTus, 1000)
		}
		if err := store.Record(m); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.RollupTick(now); err != nil {
		t.Fatal(err)
	}
	availCache.mu.Lock()
	availCache.items = map[string]availEntry{}
	availCache.mu.Unlock()
	api := &API{store: store, token: "secret", probeID: 1}
	get := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		api.availability(w, httptest.NewRequest("GET", "/api/v1/availability?target="+strconv.FormatInt(id, 10), nil))
		return w
	}

	first := get()
	if first.Code != 200 ||
		first.Header().Get("Content-Type") != "application/json; charset=utf-8" ||
		first.Header().Get("Cache-Control") != "public, max-age=60" {
		t.Fatalf("HTTP %d, headers %v", first.Code, first.Header())
	}
	var got struct {
		Windows []AvailWindow `json:"windows"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &got); err != nil || len(got.Windows) != len(availWindows) {
		t.Fatalf("windows: %v\n%s", err, first.Body)
	}
	at := time.Unix(got.Windows[0].Since+availWindows[0].secs, 0)
	var want bytes.Buffer
	json.NewEncoder(&want).Encode(map[string]any{
		"target_id":  id,
		"windows":    store.Availability(id, 1, at),
		"disclaimer": "measured from a single vantage point; not a contractual SLA",
	})
	if first.Body.String() != want.String() {
		t.Errorf("the endpoint's answer changed:\n got %s\nwant %s", first.Body, &want)
	}
	if again := get(); again.Body.String() != first.Body.String() {
		t.Errorf("the cached answer differs from the first:\n%s\n%s", again.Body, first.Body)
	}
}

// Badges are not rate-limited, so a missing entry is computed by one caller
// however many miss it together, and the others get that same answer.
func TestAvailabilityFilledOnce(t *testing.T) {
	c := &availCacheT{items: map[string]availEntry{}, filling: map[string]chan struct{}{}}
	var runs atomic.Int32
	started, release := make(chan struct{}), make(chan struct{})
	compute := func() map[string]any {
		if runs.Add(1) == 1 {
			close(started)
		}
		<-release
		return map[string]any{"n": 1}
	}
	got := make([]map[string]any, 20)
	var wg sync.WaitGroup
	for i := range got {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got[i] = c.fill("k", time.Minute, compute)
		}()
	}
	<-started
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	if n := runs.Load(); n != 1 {
		t.Errorf("%d concurrent misses ran the computation %d times", len(got), n)
	}
	for i, v := range got {
		if v["n"] != 1 {
			t.Errorf("caller %d got %v", i, v)
		}
	}
	if len(c.filling) != 0 {
		t.Errorf("fills left behind: %v", c.filling)
	}

	// An expired entry is computed again.
	c.fill("e", -time.Second, compute)
	c.fill("e", -time.Second, compute)
	if n := runs.Load(); n != 3 {
		t.Errorf("an expired entry was not computed again: %d runs", n)
	}
}
