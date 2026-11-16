package main

import (
	"bufio"
	"bytes"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// keyFixture is an instance with the routes a key can reach, and a way to
// make accounts without paying for PBKDF2 on each one.
func keyFixture(t *testing.T) (*Store, http.Handler, func(email, role string) *User) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	api := &API{store: store, token: "secret", probeID: 1, limiter: newAttemptLimiter()}
	mux := http.NewServeMux()
	api.Routes(mux)
	api.AuthRoutes(mux)
	api.ShareRoutes(mux)
	api.APIKeyRoutes(mux)
	mkUser := func(email, role string) *User {
		res, err := store.cfg.Exec(`INSERT INTO users(email,display_name,password_hash,role,created_at)
			VALUES(?,?,'unusable',?,?)`, email, email, role, time.Now().Unix())
		if err != nil {
			t.Fatal(err)
		}
		id, _ := res.LastInsertId()
		u, err := scanUser(store.cfg.QueryRow(`SELECT `+userCols+` FROM users WHERE id=?`, id).Scan)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	return store, mux, mkUser
}

func keyCall(h http.Handler, method, path, body, bearer string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// A key acts as its account at the key's role, with no CSRF token, and every
// way of not being a valid key gets the same answer as having no key at all.
func TestAPIKeyAuthenticates(t *testing.T) {
	store, mux, mkUser := keyFixture(t)
	owner := mkUser("noc@example.net", "admin")
	cat, _ := store.CreateCategory("c", "C", "C", true)
	tid, err := store.CreateTarget(&Target{CategoryID: cat, Slug: "t", Title: "Before", Host: "192.0.2.1",
		Proto: "icmp", IntervalS: 60, Packets: 10, SpacingMs: 100, TimeoutMs: 1000, Public: true, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	target := "/api/v1/admin/targets/" + strconv.FormatInt(tid, 10)
	mk := func(name, role string) *APIKey {
		k, err := store.CreateAPIKey(owner, name, role, 0)
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	viewer, editor, admin := mk("prometheus", "viewer"), mk("ci", "editor"), mk("ansible", "admin")
	if !strings.HasPrefix(viewer.Token, "ssk_") || len(viewer.Token) != len("ssk_")+64 {
		t.Fatalf("unexpected key shape %q", viewer.Token)
	}

	w := keyCall(mux, "GET", "/api/v1/auth/me", "", viewer.Token)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"role":"viewer"`) ||
		!strings.Contains(w.Body.String(), `"key_name":"prometheus"`) {
		t.Fatalf("a valid viewer key: HTTP %d %s", w.Code, w.Body)
	}
	if w := keyCall(mux, "PATCH", target, `{"title":"After"}`, viewer.Token); w.Code != 403 {
		t.Errorf("a viewer key patching a target: HTTP %d, want 403", w.Code)
	}
	// No X-CSRF-Token on either request: a bearer caller has no session for
	// one to be tied to.
	if w := keyCall(mux, "PATCH", target, `{"title":"After"}`, admin.Token); w.Code != 200 {
		t.Errorf("an admin key patching a target: HTTP %d %s", w.Code, w.Body)
	}
	if tg, _ := store.TargetByID(tid); tg.Title != "After" {
		t.Errorf("the patch was not applied: %q", tg.Title)
	}
	if w := keyCall(mux, "POST", target+"/share", `{"days":1}`, editor.Token); w.Code != 200 {
		t.Fatalf("an editor key sharing a target: HTTP %d %s", w.Code, w.Body)
	}
	var email, keyName string
	var keyID int64
	store.cfg.QueryRow(`SELECT email,key_id,key_name FROM audit_log WHERE action='share_create'`).
		Scan(&email, &keyID, &keyName)
	if email != owner.Email || keyID != editor.ID || keyName != "ci" {
		t.Errorf("audit entry: %q key %d %q, want the account and the key", email, keyID, keyName)
	}

	// Master stays out of reach of every key, and the instance token keeps it.
	if w := keyCall(mux, "GET", "/api/v1/admin/users", "", admin.Token); w.Code != 403 {
		t.Errorf("an admin key listing accounts: HTTP %d, want 403", w.Code)
	}
	if w := keyCall(mux, "GET", "/api/v1/admin/users", "", "secret"); w.Code != 200 {
		t.Errorf("the admin token listing accounts: HTTP %d, want 200", w.Code)
	}
	if w := keyCall(mux, "PATCH", target, `{"title":"Token"}`, "secret"); w.Code != 200 {
		t.Errorf("the admin token patching a target: HTTP %d, want 200", w.Code)
	}

	expired, err := store.CreateAPIKey(owner, "old", "viewer", 1)
	if err != nil {
		t.Fatal(err)
	}
	store.cfg.Exec(`UPDATE api_keys SET expires_at=? WHERE id=?`, time.Now().Unix()-60, expired.ID)
	revoked := mk("gone", "viewer")
	if _, err := store.RevokeAPIKey(revoked.ID, 0); err != nil {
		t.Fatal(err)
	}
	anon := keyCall(mux, "GET", "/api/v1/auth/me", "", "")
	if anon.Code != 401 {
		t.Fatalf("no credential: HTTP %d, want 401", anon.Code)
	}
	off := []byte(viewer.Token)
	off[len(off)-1] ^= 1
	for what, tok := range map[string]string{
		"one character off": string(off),
		"truncated":         viewer.Token[:apiKeyHintLen],
		"expired":           expired.Token,
		"revoked":           revoked.Token,
	} {
		w := keyCall(mux, "GET", "/api/v1/auth/me", "", tok)
		if w.Code != anon.Code || w.Body.String() != anon.Body.String() {
			t.Errorf("%s key: HTTP %d %s, want the same answer as no key", what, w.Code, w.Body)
		}
	}

	// last_used is a write, so it is kept to one a minute however busy the key.
	var used int64
	store.cfg.QueryRow(`SELECT last_used FROM api_keys WHERE id=?`, viewer.ID).Scan(&used)
	if used == 0 {
		t.Fatal("last_used was not recorded")
	}
	recent := time.Now().Unix() - 30
	store.cfg.Exec(`UPDATE api_keys SET last_used=? WHERE id=?`, recent, viewer.ID)
	keyCall(mux, "GET", "/api/v1/auth/me", "", viewer.Token)
	store.cfg.QueryRow(`SELECT last_used FROM api_keys WHERE id=?`, viewer.ID).Scan(&used)
	if used != recent {
		t.Error("last_used was rewritten within the minute")
	}
	store.cfg.Exec(`UPDATE api_keys SET last_used=? WHERE id=?`, recent-60, viewer.ID)
	keyCall(mux, "GET", "/api/v1/auth/me", "", viewer.Token)
	store.cfg.QueryRow(`SELECT last_used FROM api_keys WHERE id=?`, viewer.ID).Scan(&used)
	if used <= recent {
		t.Error("last_used was not refreshed after a minute")
	}
}

// A key never outranks its account: not when it is made, and not after the
// account is demoted. Disabling or deleting the account ends it.
func TestAPIKeyFollowsItsAccount(t *testing.T) {
	store, mux, mkUser := keyFixture(t)
	editor := mkUser("editor@example.net", "editor")
	master := mkUser("master@example.net", "master")
	mkUser("other@example.net", "master")

	if _, err := store.CreateAPIKey(editor, "too-high", "admin", 0); err == nil {
		t.Error("an editor was given an admin key")
	}
	if _, err := store.CreateAPIKey(master, "all-powerful", "master", 0); err == nil {
		t.Error("a key was given the master role")
	}
	if _, err := store.CreateAPIKey(master, "ci\tforged row", "viewer", 0); err == nil {
		t.Error("a key name with a control character was accepted")
	}
	sess, csrf, err := store.OpenSession(editor.ID, "192.0.2.1", "test")
	if err != nil {
		t.Fatal(err)
	}
	create := func(body string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/api/v1/admin/apikeys", strings.NewReader(body))
		r.AddCookie(&http.Cookie{Name: sessionCookie, Value: sess})
		r.Header.Set("X-CSRF-Token", csrf)
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	if w := create(`{"name":"too-high","role":"admin"}`); w.Code != 400 {
		t.Errorf("an editor creating an admin key over HTTP: HTTP %d, want 400", w.Code)
	}
	key, err := store.CreateAPIKey(editor, "deploy", "editor", 0)
	if err != nil {
		t.Fatal(err)
	}
	shares := func() int { return keyCall(mux, "GET", "/api/v1/admin/shares", "", key.Token).Code }
	if got := shares(); got != 200 {
		t.Fatalf("an editor key on an editor route: HTTP %d", got)
	}
	viewer, promoted := "viewer", "master"
	store.UpdateUser(editor.ID, nil, &viewer, nil, nil)
	if got := shares(); got != 403 {
		t.Errorf("the key of a demoted account: HTTP %d, want 403", got)
	}
	// Promotion does not carry the key past the role it was given.
	store.UpdateUser(editor.ID, nil, &promoted, nil, nil)
	if got := shares(); got != 200 {
		t.Errorf("the key of a promoted account on its own level: HTTP %d, want 200", got)
	}
	if got := keyCall(mux, "GET", "/api/v1/admin/targets", "", key.Token).Code; got != 403 {
		t.Errorf("an editor key on an admin route after promotion: HTTP %d, want 403", got)
	}

	off, on := true, false
	store.UpdateUser(editor.ID, nil, nil, nil, &off)
	store.UpdateUser(editor.ID, nil, nil, nil, &on)
	if got := shares(); got != 401 {
		t.Errorf("a key that outlived the account being disabled: HTTP %d, want 401", got)
	}

	kept, err := store.CreateAPIKey(master, "kept", "admin", 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteUser(master.ID); err != nil {
		t.Fatal(err)
	}
	if got := keyCall(mux, "GET", "/api/v1/admin/shares", "", kept.Token).Code; got != 401 {
		t.Errorf("the key of a deleted account: HTTP %d, want 401", got)
	}
	var n int
	store.cfg.QueryRow(`SELECT COUNT(*) FROM api_keys`).Scan(&n)
	if n != 0 {
		t.Errorf("%d key(s) left behind by their accounts", n)
	}
}

// A copy of config.db, WAL included, must not hold a working key.
func TestAPIKeyStoredHashed(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.cfg.Exec(`INSERT INTO users(email,display_name,password_hash,role,created_at)
		VALUES('noc@example.net','NOC','unusable','admin',0)`)
	u, err := scanUser(store.cfg.QueryRow(`SELECT ` + userCols + ` FROM users`).Scan)
	if err != nil {
		t.Fatal(err)
	}
	k, err := store.CreateAPIKey(u, "ansible", "admin", 0)
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	store.cfg.QueryRow(`SELECT key_hash FROM api_keys WHERE id=?`, k.ID).Scan(&stored)
	if stored != hashToken(k.Token) {
		t.Errorf("stored %q, want the hash of the key", stored)
	}
	files, _ := filepath.Glob(filepath.Join(dir, "config.db*"))
	if len(files) == 0 {
		t.Fatal("no database file found")
	}
	secret := []byte(k.Token[apiKeyHintLen:])
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(b, secret) {
			t.Errorf("%s holds the key in clear", filepath.Base(f))
		}
	}
	list, _ := store.APIKeys(0)
	if len(list) != 1 || list[0].Token != "" || list[0].Hint != k.Token[:apiKeyHintLen] {
		t.Errorf("the list must show the hint and never the key: %+v", list)
	}
}

// A key reads private targets on the public endpoints, as a session does.
// One that no longer works reads what a visitor reads.
func TestAPIKeyReadsPrivateTargets(t *testing.T) {
	store, mux, mkUser := keyFixture(t)
	k, err := store.CreateAPIKey(mkUser("noc@example.net", "viewer"), "grafana", "viewer", 0)
	if err != nil {
		t.Fatal(err)
	}
	cat, _ := store.CreateCategory("c", "C", "C", true)
	priv, err := store.CreateTarget(&Target{CategoryID: cat, Slug: "priv", Title: "Private one", Host: "192.0.2.1",
		Proto: "icmp", IntervalS: 60, Packets: 10, SpacingMs: 100, TimeoutMs: 1000, Public: false, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	series := "/api/v1/series?target=" + strconv.FormatInt(priv, 10) + "&from=-3h"
	if w := keyCall(mux, "GET", "/api/v1/tree", "", k.Token); !strings.Contains(w.Body.String(), "Private one") {
		t.Error("the tree hides the private target from a key")
	}
	if w := keyCall(mux, "GET", series, "", k.Token); w.Code != 200 {
		t.Errorf("series of a private target with a key: HTTP %d, want 200", w.Code)
	}
	store.RevokeAPIKey(k.ID, 0)
	if w := keyCall(mux, "GET", "/api/v1/tree", "", k.Token); strings.Contains(w.Body.String(), "Private one") {
		t.Error("the tree shows the private target to a revoked key")
	}
	if w := keyCall(mux, "GET", series, "", k.Token); w.Code != 404 {
		t.Errorf("series of a private target with a revoked key: HTTP %d, want 404", w.Code)
	}
}

// Keys are made by a signed-in account, never by a key or by the instance
// token, and each account reaches only its own unless it is an admin.
func TestAPIKeyManagement(t *testing.T) {
	store, mux, mkUser := keyFixture(t)
	editor, viewer, admin := mkUser("editor@example.net", "editor"),
		mkUser("viewer@example.net", "viewer"), mkUser("admin@example.net", "admin")
	session := func(u *User) func(method, path, body string) *httptest.ResponseRecorder {
		sess, csrf, err := store.OpenSession(u.ID, "192.0.2.1", "test")
		if err != nil {
			t.Fatal(err)
		}
		return func(method, path, body string) *httptest.ResponseRecorder {
			r := httptest.NewRequest(method, path, strings.NewReader(body))
			r.AddCookie(&http.Cookie{Name: sessionCookie, Value: sess})
			r.Header.Set("X-CSRF-Token", csrf)
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			return w
		}
	}
	asEditor, asViewer, asAdmin := session(editor), session(viewer), session(admin)

	w := asEditor("POST", "/api/v1/admin/apikeys", `{"name":"deploy","role":"editor","days":30}`)
	var created APIKey
	if err := json.Unmarshal(w.Body.Bytes(), &created); err != nil || w.Code != 200 ||
		!strings.HasPrefix(created.Token, "ssk_") || created.Role != "editor" || created.ExpiresAt == 0 {
		t.Fatalf("creating a key: HTTP %d %s", w.Code, w.Body)
	}
	tok, editorKey := created.Token, strconv.FormatInt(created.ID, 10)
	if _, err := store.CreateAPIKey(viewer, "scraper", "viewer", 0); err != nil {
		t.Fatal(err)
	}

	if w := keyCall(mux, "POST", "/api/v1/admin/apikeys", `{"name":"child"}`, tok); w.Code != 403 {
		t.Errorf("a key creating a key: HTTP %d, want 403", w.Code)
	}
	if w := keyCall(mux, "POST", "/api/v1/admin/apikeys", `{"name":"orphan"}`, "secret"); w.Code != 403 {
		t.Errorf("the admin token creating a key: HTTP %d, want 403", w.Code)
	}

	if body := asViewer("GET", "/api/v1/admin/apikeys", "").Body.String(); strings.Contains(body, "deploy") ||
		!strings.Contains(body, "scraper") {
		t.Errorf("a viewer must list its own keys only: %s", body)
	}
	if body := asAdmin("GET", "/api/v1/admin/apikeys", "").Body.String(); !strings.Contains(body, "deploy") ||
		!strings.Contains(body, "scraper") || strings.Contains(body, "key_hash") || strings.Contains(body, tok) {
		t.Errorf("an admin lists every key, and never a secret: %s", body)
	}
	if w := asViewer("DELETE", "/api/v1/admin/apikeys/"+editorKey, ""); w.Code != 404 {
		t.Errorf("a viewer revoking someone else's key: HTTP %d, want 404", w.Code)
	}
	if w := keyCall(mux, "GET", "/api/v1/auth/me", "", tok); w.Code != 200 {
		t.Fatalf("the key should still work: HTTP %d", w.Code)
	}
	if w := asAdmin("DELETE", "/api/v1/admin/apikeys/"+editorKey, ""); w.Code != 204 {
		t.Errorf("an admin revoking a key: HTTP %d, want 204", w.Code)
	}
	if w := keyCall(mux, "GET", "/api/v1/auth/me", "", tok); w.Code != 401 {
		t.Errorf("a revoked key: HTTP %d, want 401", w.Code)
	}
	var n int
	store.cfg.QueryRow(`SELECT COUNT(*) FROM audit_log WHERE entity='apikey' AND entity_id=?
		AND action IN ('apikey_create','apikey_revoke')`, editorKey+" (deploy)").Scan(&n)
	if n != 2 {
		t.Errorf("%d audit entries for the key's creation and revocation, want 2", n)
	}
}

// cheapHash is a hash verifyPassword accepts, at the lowest iteration count it
// allows, so that a test signs in without paying for 210 000.
func cheapHash(pw string) string {
	salt := []byte("0123456789abcdef")
	return "pbkdf2$sha256$1000$" + base64.RawStdEncoding.EncodeToString(salt) + "$" +
		base64.RawStdEncoding.EncodeToString(pbkdf2SHA256([]byte(pw), salt, 1000, 32))
}

// A key cannot change its account's password. A session can, once its current
// password is checked against the sign-in budget, which an ordinary change
// does not come near.
func TestPasswordChange(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	api := &API{store: store, token: "secret", probeID: 1, limiter: newAttemptLimiter()}
	mux := http.NewServeMux()
	api.AuthRoutes(mux)
	const old, fresh = "old password 1234", "new password 5678"
	res, err := store.cfg.Exec(`INSERT INTO users(email,display_name,password_hash,role,created_at)
		VALUES('noc@example.net','NOC',?,'admin',?)`, cheapHash(old), time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()
	u, err := scanUser(store.cfg.QueryRow(`SELECT `+userCols+` FROM users WHERE id=?`, id).Scan)
	if err != nil {
		t.Fatal(err)
	}
	hash := func() string {
		var h string
		store.cfg.QueryRow(`SELECT password_hash FROM users WHERE id=?`, id).Scan(&h)
		return h
	}
	call := func(path, body string, c *http.Cookie, csrf, bearer string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", path, strings.NewReader(body))
		if c != nil {
			r.AddCookie(c)
			r.Header.Set("X-CSRF-Token", csrf)
		}
		if bearer != "" {
			r.Header.Set("Authorization", "Bearer "+bearer)
		}
		w := httptest.NewRecorder()
		mux.ServeHTTP(w, r)
		return w
	}
	login := func(pw string) (*http.Cookie, string) {
		w := call("/api/v1/auth/login", `{"email":"noc@example.net","password":"`+pw+`"}`, nil, "", "")
		var out struct {
			CSRF string `json:"csrf"`
		}
		json.Unmarshal(w.Body.Bytes(), &out)
		if w.Code != 200 || len(w.Result().Cookies()) == 0 || out.CSRF == "" {
			t.Fatalf("signing in: HTTP %d %s", w.Code, w.Body)
		}
		return w.Result().Cookies()[0], out.CSRF
	}
	change := func(c *http.Cookie, csrf, bearer, current, next string) *httptest.ResponseRecorder {
		return call("/api/v1/auth/password", `{"current":"`+current+`","new":"`+next+`"}`, c, csrf, bearer)
	}

	key, err := store.CreateAPIKey(u, "ci", "admin", 0)
	if err != nil {
		t.Fatal(err)
	}
	before := hash()
	if w := change(nil, "", key.Token, old, fresh); w.Code != 403 || !strings.Contains(w.Body.String(), "sign in") {
		t.Errorf("a key changing its account's password: HTTP %d %s, want 403", w.Code, w.Body)
	}
	if w := change(nil, "", "secret", old, fresh); w.Code != 400 ||
		!strings.Contains(w.Body.String(), "the API token has no password") {
		t.Errorf("the admin token changing a password: HTTP %d %s, want 400", w.Code, w.Body)
	}
	if hash() != before {
		t.Fatal("the password was changed without a session")
	}

	// A sign-in, a mistyped change, the change, and the sign-in it asks for:
	// four attempts of the eight.
	c, csrf := login(old)
	if w := change(c, csrf, "", "not the password", fresh); w.Code != 403 ||
		!strings.Contains(w.Body.String(), "current password is incorrect") {
		t.Errorf("a wrong current password: HTTP %d %s, want 403", w.Code, w.Body)
	}
	if w := change(c, csrf, "", old, fresh); w.Code != 200 || !strings.Contains(w.Body.String(), `"reauth":true`) {
		t.Fatalf("a session changing its password: HTTP %d %s", w.Code, w.Body)
	}
	r := httptest.NewRequest("GET", "/api/v1/auth/me", nil)
	r.AddCookie(c)
	w := httptest.NewRecorder()
	mux.ServeHTTP(w, r)
	if w.Code != 401 {
		t.Errorf("the session survived the password change: HTTP %d", w.Code)
	}
	if w := keyCall(mux, "GET", "/api/v1/auth/me", "", key.Token); w.Code != 200 {
		t.Errorf("a password change revoked the account's key: HTTP %d", w.Code)
	}
	c, csrf = login(fresh)

	for api.limiter.allow("192.0.2.1") {
	}
	before = hash()
	if w := change(c, csrf, "", fresh, "third password 9012"); w.Code != 429 {
		t.Errorf("a password change past the attempt budget: HTTP %d %s, want 429", w.Code, w.Body)
	}
	if hash() != before {
		t.Error("the password was changed past the attempt budget")
	}
}

// A live stream opened with a credential shows private targets, and stops
// showing them at the next tick once the credential no longer works.
func TestLiveStreamRechecksItsCaller(t *testing.T) {
	saved := liveEvery
	liveEvery = 10 * time.Millisecond
	t.Cleanup(func() { liveEvery = saved })
	// The public set is cached per process under a counter every store
	// starts at zero: another test's set must not stand in for this one's.
	publicIDs.Lock()
	publicIDs.ok = false
	publicIDs.Unlock()

	store, mux, mkUser := keyFixture(t)
	u := mkUser("noc@example.net", "viewer")
	cat, _ := store.CreateCategory("c", "C", "C", true)
	ids := map[bool]int64{}
	for _, public := range []bool{true, false} {
		id, err := store.CreateTarget(&Target{CategoryID: cat, Slug: "t" + strconv.FormatBool(public), Title: "T",
			Host: "192.0.2.1", Proto: "icmp", IntervalS: 60, Packets: 10, SpacingMs: 100, TimeoutMs: 1000,
			Public: public, Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		store.mxw.Exec(`INSERT INTO live(target_id,probe_id,ts,med_us,p95_us,loss_pct) VALUES(?,1,?,1000,1000,0)`,
			id, time.Now().Unix())
		ids[public] = id
	}
	shows := func(event string, public bool) bool {
		return strings.Contains(event, `"target_id":`+strconv.FormatInt(ids[public], 10)+`,`)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	stream := func(set func(*http.Request)) (next func() string, stop func()) {
		req, _ := http.NewRequest("GET", srv.URL+"/api/v1/live", nil)
		set(req)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		// Runs before srv.Close, which would otherwise wait on this stream
		// forever when the test fails halfway.
		t.Cleanup(func() { resp.Body.Close() })
		if resp.StatusCode != 200 {
			t.Fatalf("opening the live stream: HTTP %d", resp.StatusCode)
		}
		sc := bufio.NewScanner(resp.Body)
		return func() string {
			for sc.Scan() {
				if ev, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
					return ev
				}
			}
			t.Fatal("the live stream ended")
			return ""
		}, func() { resp.Body.Close() }
	}
	bearer := func(tok string) func(*http.Request) {
		return func(r *http.Request) { r.Header.Set("Authorization", "Bearer "+tok) }
	}

	next, stop := stream(func(*http.Request) {})
	for i := 0; i < 3; i++ {
		if ev := next(); shows(ev, false) || !shows(ev, true) {
			t.Fatalf("an anonymous stream: %s", ev)
		}
	}
	stop()
	next, stop = stream(bearer("secret"))
	for i := 0; i < 3; i++ {
		if ev := next(); !shows(ev, false) {
			t.Fatalf("a stream opened with the admin token lost the private target: %s", ev)
		}
	}
	stop()

	key, err := store.CreateAPIKey(u, "grafana", "viewer", 0)
	if err != nil {
		t.Fatal(err)
	}
	sess, _, err := store.OpenSession(u.ID, "192.0.2.1", "test")
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []struct {
		what string
		set  func(*http.Request)
		end  func()
	}{
		{"a key", bearer(key.Token), func() { store.RevokeAPIKey(key.ID, 0) }},
		{"a session", func(r *http.Request) { r.AddCookie(&http.Cookie{Name: sessionCookie, Value: sess}) },
			func() { store.CloseSession(sess) }},
	} {
		next, stop := stream(c.set)
		if ev := next(); !shows(ev, false) {
			t.Fatalf("a stream opened with %s: %s", c.what, ev)
		}
		c.end()
		dropped := false
		for i := 0; i < 200 && !dropped; i++ {
			dropped = !shows(next(), false)
		}
		if !dropped {
			t.Fatalf("a stream opened with %s kept the private target after it stopped working", c.what)
		}
		for i := 0; i < 3; i++ {
			if ev := next(); shows(ev, false) || !shows(ev, true) {
				t.Errorf("a stream opened with %s, once it stopped working: %s", c.what, ev)
			}
		}
		stop()
	}
}

// Keys were added to the bearer side of need() and authenticated(). A session
// is handled as before, including when the request also carries a bearer
// that is not a credential.
func TestSessionBesideABearer(t *testing.T) {
	store, mux, mkUser := keyFixture(t)
	admin, viewer := mkUser("admin@example.net", "admin"), mkUser("viewer@example.net", "viewer")
	cat, _ := store.CreateCategory("c", "C", "C", true)
	priv, err := store.CreateTarget(&Target{CategoryID: cat, Slug: "priv", Title: "Private one", Host: "192.0.2.1",
		Proto: "icmp", IntervalS: 60, Packets: 10, SpacingMs: 100, TimeoutMs: 1000, Public: false, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	session := func(u *User) (string, string) {
		tok, csrf, err := store.OpenSession(u.ID, "192.0.2.1", "test")
		if err != nil {
			t.Fatal(err)
		}
		return tok, csrf
	}
	adminTok, adminCSRF := session(admin)
	viewerTok, _ := session(viewer)
	series := "/api/v1/series?target=" + strconv.FormatInt(priv, 10) + "&from=-3h"
	target := "/api/v1/admin/targets/" + strconv.FormatInt(priv, 10)

	for _, b := range []string{"", "not-a-credential", apiKeyPrefix + strings.Repeat("0", 64)} {
		call := func(method, path, body, cookie, csrf string) *httptest.ResponseRecorder {
			r := httptest.NewRequest(method, path, strings.NewReader(body))
			if cookie != "" {
				r.AddCookie(&http.Cookie{Name: sessionCookie, Value: cookie})
			}
			if csrf != "" {
				r.Header.Set("X-CSRF-Token", csrf)
			}
			if b != "" {
				r.Header.Set("Authorization", "Bearer "+b)
			}
			w := httptest.NewRecorder()
			mux.ServeHTTP(w, r)
			return w
		}
		for _, c := range []struct {
			what               string
			method, path, body string
			cookie, csrf       string
			code               int
			want               string
		}{
			{"no credential", "GET", "/api/v1/admin/targets", "", "", "", 401, "authentication required"},
			{"an unknown session", "GET", "/api/v1/admin/targets", "", "stale", "", 401, "session expired"},
			{"an admin session", "GET", "/api/v1/admin/targets", "", adminTok, "", 200, "Private one"},
			{"a write without CSRF", "PATCH", target, `{"title":"Private one"}`, adminTok, "", 403, "invalid CSRF token"},
			{"a write with CSRF", "PATCH", target, `{"title":"Private one"}`, adminTok, adminCSRF, 200, ""},
			{"a viewer session on an admin route", "GET", "/api/v1/admin/targets", "", viewerTok, "", 403, "insufficient privileges"},
			{"a session reading a private series", "GET", series, "", viewerTok, "", 200, ""},
			{"a visitor reading a private series", "GET", series, "", "", "", 404, ""},
		} {
			w := call(c.method, c.path, c.body, c.cookie, c.csrf)
			if w.Code != c.code || !strings.Contains(w.Body.String(), c.want) {
				t.Errorf("%s, bearer %q: HTTP %d %s, want %d", c.what, b, w.Code, w.Body, c.code)
			}
		}
		if w := call("GET", "/api/v1/tree", "", viewerTok, ""); !strings.Contains(w.Body.String(), "Private one") {
			t.Errorf("a session reading the tree, bearer %q: the private target is hidden", b)
		}
	}
}

// audit_log gained two columns for keys. On a database from before them they
// are added, old entries read as they did, and an entry without a key says
// nothing of one.
func TestAuditOnDatabaseFromBeforeKeys(t *testing.T) {
	dir := t.TempDir()
	db, err := sql.Open("sqlite", dsn(filepath.Join(dir, "config.db")))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE audit_log (id INTEGER PRIMARY KEY, ts INTEGER NOT NULL, user_id INTEGER,
		email TEXT, action TEXT NOT NULL, entity TEXT, entity_id TEXT, ip TEXT);
		INSERT INTO audit_log(ts,user_id,email,action,entity,entity_id,ip)
		VALUES(1,1,'old@example.net','login','user','1','192.0.2.1')`); err != nil {
		t.Fatal(err)
	}
	db.Close()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.Audit(&User{ID: 2, Email: "noc@example.net"}, "192.0.2.2", "share_create", "target", "session")
	store.Audit(&User{ID: 2, Email: "noc@example.net", KeyID: 3, KeyName: "ci"}, "192.0.2.2", "share_create", "target", "key")
	store.Audit(nil, "cli", "probe_create", "probe", "cli")

	mux := http.NewServeMux()
	(&API{store: store, token: "secret", probeID: 1}).AuthRoutes(mux)
	w := keyCall(mux, "GET", "/api/v1/admin/audit", "", "secret")
	var got []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || len(got) != 4 {
		t.Fatalf("audit list: HTTP %d %s", w.Code, w.Body)
	}
	for _, e := range got {
		fields := make([]string, 0, len(e))
		for f := range e {
			fields = append(fields, f)
		}
		sort.Strings(fields)
		want := "action,email,entity,entity_id,ip,ts"
		if e["entity_id"] == "key" {
			want = "action,email,entity,entity_id,ip,key_name,ts"
			if e["key_name"] != "ci" || e["email"] != "noc@example.net" {
				t.Errorf("the entry made with a key: %v", e)
			}
		}
		if strings.Join(fields, ",") != want {
			t.Errorf("entry %v has fields %v, want %s", e["entity_id"], fields, want)
		}
	}
}

// A mistyped subcommand is refused before loadConfig, which would otherwise
// write a configuration with a fresh admin token.
func TestAPIKeyCLIRefusesUnknownSubcommand(t *testing.T) {
	cfg := filepath.Join(t.TempDir(), "config.json")
	for _, args := range [][]string{nil, {"lsit", "-config", cfg}} {
		if err := cmdAPIKey(args); err == nil || !strings.Contains(err.Error(), "usage") {
			t.Errorf("apikey %q: %v, want the usage", args, err)
		}
	}
	if _, err := os.Stat(cfg); !os.IsNotExist(err) {
		t.Errorf("a mistyped subcommand wrote %s", cfg)
	}
}
