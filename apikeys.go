package main

// Per-account API keys, for the scripts that drive an instance: Ansible,
// Terraform, a CI job, a Prometheus scraper.
//
// Before them automation had one credential, the admin_token of config.json.
// It acts as master, belongs to nobody, cannot be narrowed, and is withdrawn
// only by editing the configuration and restarting — so every script held
// the keys to everything, and the audit log could only say the token did it.
//
// A key belongs to an account and acts as that account, with a role chosen
// when it is made and capped twice: never above the creator's role at
// creation, never above the account's current role at each use. Demoting an
// account therefore demotes its keys, and disabling or deleting it deletes
// them, with nothing left to revoke by hand. A key is never master and cannot
// create accounts or keys, nor change its account's password: those stay
// behind an interactive sign-in, the host, or the instance token. Whatever
// else it can create — share links, probe tokens, federation peers — outlives
// its revocation, and is where to look after a leak.
//
// The secret is shown once and only its hash is stored, like a session or a
// probe token. It starts with "ssk_" so that a secret scanner or a log filter
// recognises one that leaked, and the list keeps its first characters so the
// leaked key can be found and revoked.

import (
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	apiKeyPrefix = "ssk_"
	// What the list shows of a key: the prefix and eight hex digits, 32 of
	// its 256 random bits.
	apiKeyHintLen = len(apiKeyPrefix) + 8
	// last_used is written at most this often per key: a scraper polling
	// every few seconds would otherwise turn each of its reads into a write.
	apiKeyTouchEvery = 60
)

// AUTOINCREMENT so that a revoked key's id is never handed to a later one:
// the audit log names keys by id. No CHECK on the role, unlike users: the
// set of roles a key may carry is a decision the code can revisit, and a
// constraint here could only be changed by rebuilding the table.
const apiKeySchema = `
CREATE TABLE IF NOT EXISTS api_keys (
  id         INTEGER PRIMARY KEY AUTOINCREMENT,
  user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
  name       TEXT NOT NULL,
  role       TEXT NOT NULL,
  key_hash   TEXT NOT NULL UNIQUE,
  hint       TEXT NOT NULL,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL DEFAULT 0,
  last_used  INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_api_keys_user ON api_keys(user_id);`

func migrateAPIKeys(db *sql.DB) error {
	if _, err := db.Exec(apiKeySchema); err != nil {
		return err
	}
	addColumn(db, "audit_log", "key_id INTEGER")
	addColumn(db, "audit_log", "key_name TEXT")
	return nil
}

// An API key as its owner and the admins see it. Token is filled only in the
// response that creates it; the hash never leaves the database.
type APIKey struct {
	ID     int64  `json:"id"`
	UserID int64  `json:"user_id"`
	Owner  string `json:"owner"`
	Name   string `json:"name"`
	Role   string `json:"role"`
	// What the key acts as right now: the lower of its role and its
	// account's, empty once it has expired.
	EffectiveRole string `json:"effective_role"`
	Hint          string `json:"hint"`
	CreatedAt     int64  `json:"created_at"`
	ExpiresAt     int64  `json:"expires_at"` // 0 = never
	LastUsed      int64  `json:"last_used"`
	Token         string `json:"token,omitempty"`
}

func lowerRole(a, b string) string {
	if roleLevels[a] < roleLevels[b] {
		return a
	}
	return b
}

func keyRef(id int64, name string) string {
	return fmt.Sprintf("%d (%s)", id, name)
}

// CreateAPIKey returns the key with its token, the only time the token exists
// outside the caller's hands.
func (s *Store) CreateAPIKey(owner *User, name, role string, days int) (*APIKey, error) {
	name = strings.TrimSpace(name)
	// Printed as is in the audit log and the CLI table, where a line break or
	// a tab would forge a row.
	if name == "" || utf8.RuneCountInString(name) > 64 || strings.IndexFunc(name, unicode.IsControl) >= 0 {
		return nil, fmt.Errorf("a key needs a name of at most 64 printable characters")
	}
	lvl, ok := roleLevels[role]
	if !ok || lvl > RoleAdmin {
		return nil, fmt.Errorf("the role of a key is viewer, editor or admin")
	}
	if lvl > owner.Level() {
		return nil, fmt.Errorf("a key cannot have a higher role than its account, which is %s", owner.Role)
	}
	if owner.Disabled {
		return nil, fmt.Errorf("the account %s is disabled", owner.Email)
	}
	if days < 0 || days > 3650 {
		return nil, fmt.Errorf("the number of days must be between 0 (no expiry) and 3650")
	}
	tok := apiKeyPrefix + randomHex(32)
	now := time.Now().Unix()
	k := &APIKey{UserID: owner.ID, Owner: owner.Email, Name: name, Role: role,
		EffectiveRole: role, Hint: tok[:apiKeyHintLen], CreatedAt: now, Token: tok}
	if days > 0 {
		k.ExpiresAt = now + int64(days)*86400
	}
	// SHA-256 rather than PBKDF2: a slow hash protects guessable passwords,
	// and 256 random bits are not guessable, so it would only slow every call.
	res, err := s.cfg.Exec(`INSERT INTO api_keys(user_id,name,role,key_hash,hint,created_at,expires_at)
		VALUES(?,?,?,?,?,?,?)`, owner.ID, name, role, hashToken(tok), k.Hint, now, k.ExpiresAt)
	if err != nil {
		return nil, err
	}
	k.ID, _ = res.LastInsertId()
	return k, nil
}

// APIKeys lists the keys of one account, or of every account when userID is 0.
func (s *Store) APIKeys(userID int64) ([]APIKey, error) {
	rows, err := s.cfg.Query(`SELECT k.id,k.user_id,u.email,u.role,k.name,k.role,
	                                 k.hint,k.created_at,k.expires_at,k.last_used
	                            FROM api_keys k JOIN users u ON u.id=k.user_id
	                           WHERE ?=0 OR k.user_id=?
	                           ORDER BY u.email, k.created_at`, userID, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	now := time.Now().Unix()
	out := []APIKey{}
	for rows.Next() {
		var k APIKey
		var ownerRole string
		if err := rows.Scan(&k.ID, &k.UserID, &k.Owner, &ownerRole, &k.Name, &k.Role,
			&k.Hint, &k.CreatedAt, &k.ExpiresAt, &k.LastUsed); err != nil {
			return nil, err
		}
		if k.ExpiresAt == 0 || k.ExpiresAt > now {
			k.EffectiveRole = lowerRole(k.Role, ownerRole)
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// RevokeAPIKey deletes a key and returns its name. ownerID restricts it to
// that account's keys, 0 allows any; a key out of reach reads as missing.
func (s *Store) RevokeAPIKey(id, ownerID int64) (string, error) {
	var name string
	err := s.cfg.QueryRow(`DELETE FROM api_keys WHERE id=? AND (?=0 OR user_id=?) RETURNING name`,
		id, ownerID, ownerID).Scan(&name)
	return name, err
}

// APIKeyUser is the account a key acts as, with its role lowered to the key's
// when the key's is lower. Nil for an unknown, expired or revoked key and for
// a disabled account alike.
func (s *Store) APIKeyUser(tok string) *User {
	now := time.Now().Unix()
	row := s.cfg.QueryRow(
		`SELECT k.id,k.name,k.role,k.last_used,
		        u.id,u.email,u.display_name,u.role,u.locale,u.disabled,u.created_at,u.last_login_at
		   FROM api_keys k JOIN users u ON u.id=k.user_id
		  WHERE k.key_hash=? AND (k.expires_at=0 OR k.expires_at>?) AND u.disabled=0`,
		hashToken(tok), now)
	var keyID, lastUsed int64
	var keyName, keyRole string
	u, err := scanUser(func(dst ...any) error {
		return row.Scan(append([]any{&keyID, &keyName, &keyRole, &lastUsed}, dst...)...)
	})
	if err != nil {
		return nil
	}
	u.Role = lowerRole(keyRole, u.Role)
	u.KeyID, u.KeyName = keyID, keyName
	if now-lastUsed >= apiKeyTouchEvery {
		s.cfg.Exec(`UPDATE api_keys SET last_used=? WHERE id=?`, now, keyID)
	}
	return u
}

// bearerUser resolves an Authorization: Bearer credential: the instance admin
// token, which acts as master, or an API key, which acts as its account. Nil
// when there is none or it resolves to nothing, for whatever reason, so that
// a mistyped, revoked or expired key cannot be told apart.
func (a *API) bearerUser(r *http.Request) *User {
	tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
	switch {
	case tok == "":
		return nil
	case a.token != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(a.token)) == 1:
		return &User{ID: 0, Email: "api-token", DisplayName: "API token", Role: "master"}
	case strings.HasPrefix(tok, apiKeyPrefix):
		return a.store.APIKeyUser(tok)
	}
	return nil
}

// ------------------------------------------------------------------ API

// Every role may hold keys, down to viewer: a read-only scraper is the most
// common automation there is. What changes with the role is whose keys are
// in reach — an admin's covers every account's, anyone else's only their own.
func (a *API) APIKeyRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/admin/apikeys", a.need(RoleViewer, a.apiKeysList))
	mux.HandleFunc("POST /api/v1/admin/apikeys", a.need(RoleViewer, a.apiKeysCreate))
	mux.HandleFunc("DELETE /api/v1/admin/apikeys/{id}", a.need(RoleViewer, a.apiKeysRevoke))
}

func keyScope(u *User) int64 {
	if u.Level() >= RoleAdmin {
		return 0
	}
	return u.ID
}

func (a *API) apiKeysList(w http.ResponseWriter, r *http.Request, u *User) {
	list, err := a.store.APIKeys(keyScope(u))
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, list)
}

func (a *API) apiKeysCreate(w http.ResponseWriter, r *http.Request, u *User) {
	switch {
	case u.KeyID != 0:
		writeErr(w, http.StatusForbidden, "an API key cannot create API keys: sign in to the back-office")
		return
	case u.ID == 0:
		writeErr(w, http.StatusForbidden,
			"the admin token belongs to no account: sign in, or run `smokestack apikey create` on the host")
		return
	}
	var in struct {
		Name string `json:"name"`
		Role string `json:"role"`
		Days int    `json:"days"` // 0 or absent = no expiry
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if in.Role == "" {
		in.Role = "viewer"
	}
	k, err := a.store.CreateAPIKey(u, in.Name, in.Role, in.Days)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	a.store.Audit(u, clientIP(r), "apikey_create", "apikey", keyRef(k.ID, k.Name))
	writeJSON(w, k)
}

func (a *API) apiKeysRevoke(w http.ResponseWriter, r *http.Request, u *User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, 400, "invalid identifier")
		return
	}
	name, err := a.store.RevokeAPIKey(id, keyScope(u))
	if errors.Is(err, sql.ErrNoRows) {
		writeErr(w, http.StatusNotFound, "key not found")
		return
	}
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	a.store.Audit(u, clientIP(r), "apikey_revoke", "apikey", keyRef(id, name))
	w.WriteHeader(http.StatusNoContent)
}

// ------------------------------------------------------------------ CLI

// The admin token belongs to no account and so cannot create a key over the
// API. A provisioning run that starts from a bare host gets its first key
// here instead, through the same store calls and the same caps.
func cmdAPIKey(args []string) error {
	const usage = "usage: smokestack apikey list|create|revoke [flags]"
	// Before loadConfig, which writes a configuration when the file is missing.
	if len(args) == 0 || (args[0] != "list" && args[0] != "create" && args[0] != "revoke") {
		return fmt.Errorf("%s", usage)
	}
	fsn := flag.NewFlagSet("apikey "+args[0], flag.ExitOnError)
	cfgPath := cliConfig(fsn)
	email := fsn.String("email", "", "account the key acts as (create only)")
	name := fsn.String("name", "", "what the key is for, e.g. ansible (create only)")
	role := fsn.String("role", "viewer", "viewer|editor|admin, at most the account's role (create only)")
	days := fsn.Int("days", 0, "expire after this many days, 0 = never (create only)")
	id := fsn.Int64("id", 0, "key id, as list prints it (revoke only)")
	fsn.Parse(args[1:])

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	store, err := OpenStore(cfg.DataDir)
	if err != nil {
		return err
	}
	defer store.Close()

	switch args[0] {
	case "list":
		list, err := store.APIKeys(0)
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "ID\tOWNER\tNAME\tROLE\tACTS AS\tKEY\tEXPIRES\tLAST USED")
		day := func(ts int64, zero string) string {
			if ts == 0 {
				return zero
			}
			return time.Unix(ts, 0).UTC().Format("2006-01-02")
		}
		for _, k := range list {
			acts := k.EffectiveRole
			if acts == "" {
				acts = "nothing"
			}
			fmt.Fprintf(w, "%d\t%s\t%s\t%s\t%s\t%s…\t%s\t%s\n", k.ID, k.Owner, k.Name, k.Role, acts,
				k.Hint, day(k.ExpiresAt, "never"), day(k.LastUsed, "—"))
		}
		return w.Flush()

	case "create":
		row := store.cfg.QueryRow(`SELECT `+userCols+` FROM users WHERE email=?`,
			strings.ToLower(strings.TrimSpace(*email)))
		u, err := scanUser(row.Scan)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("no account with email %q", *email)
		}
		if err != nil {
			return err
		}
		k, err := store.CreateAPIKey(u, *name, *role, *days)
		if err != nil {
			return err
		}
		store.Audit(nil, "cli", "apikey_create", "apikey", keyRef(k.ID, k.Name))
		// The token alone on stdout, so that redirecting the command into a
		// secret store captures it and nothing else.
		fmt.Fprintf(os.Stderr, "key %d created for %s as %s. It is shown once and only its hash is stored, so copy it now:\n",
			k.ID, u.Email, k.Role)
		fmt.Println(k.Token)
		return nil

	case "revoke":
		name, err := store.RevokeAPIKey(*id, 0)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("no key with id %d", *id)
		}
		if err != nil {
			return err
		}
		store.Audit(nil, "cli", "apikey_revoke", "apikey", keyRef(*id, name))
		fmt.Printf("key %d (%s) revoked: it stops working immediately\n", *id, name)
		return nil
	}
	return fmt.Errorf("%s", usage)
}
