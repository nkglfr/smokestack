package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Sharing one target with someone who has no account: a customer, a peer's
// NOC, a provider handling a ticket. The link gives read access to that one
// target and nothing else, expires, and can be revoked.
//
// The token is kept hashed, like a session: a copy of the database does not
// hand over working links. It is shown once, when created.

type ShareLink struct {
	ID        int64  `json:"id"`
	TargetID  int64  `json:"target_id"`
	Title     string `json:"title,omitempty"`
	Note      string `json:"note"`
	CreatedAt int64  `json:"created_at"`
	CreatedBy string `json:"created_by"`
	ExpiresAt int64  `json:"expires_at"` // 0 = no expiry
	LastUsed  int64  `json:"last_used,omitempty"`
	Uses      int64  `json:"uses"`
	Token     string `json:"token,omitempty"` // only just after creation
	URL       string `json:"url,omitempty"`   // idem
}

func shareSchema(s *Store) {
	s.cfg.Exec(`CREATE TABLE IF NOT EXISTS share_links(
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		target_id INTEGER NOT NULL,
		token_hash TEXT NOT NULL UNIQUE,
		note TEXT NOT NULL DEFAULT '',
		created_at INTEGER NOT NULL,
		created_by TEXT NOT NULL DEFAULT '',
		expires_at INTEGER NOT NULL DEFAULT 0,
		last_used INTEGER NOT NULL DEFAULT 0,
		uses INTEGER NOT NULL DEFAULT 0)`)
	s.cfg.Exec(`CREATE INDEX IF NOT EXISTS idx_share_target ON share_links(target_id)`)
}

// CreateShare returns the token once; only its hash is stored.
func (s *Store) CreateShare(targetID int64, days int, note, by string) (*ShareLink, error) {
	if _, err := s.TargetByID(targetID); err != nil {
		return nil, fmt.Errorf("target not found")
	}
	if days < 0 || days > 3650 {
		return nil, fmt.Errorf("the number of days must be between 0 (no expiry) and 3650")
	}
	tok := randomHex(24)
	now := time.Now().Unix()
	var exp int64
	if days > 0 {
		exp = now + int64(days)*86400
	}
	res, err := s.cfg.Exec(`INSERT INTO share_links(target_id,token_hash,note,created_at,created_by,expires_at)
	                        VALUES(?,?,?,?,?,?)`, targetID, hashToken(tok), strings.TrimSpace(note), now, by, exp)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &ShareLink{ID: id, TargetID: targetID, Note: note, CreatedAt: now,
		CreatedBy: by, ExpiresAt: exp, Token: tok}, nil
}

// ShareTarget resolves a token to the target it opens, refusing an expired
// one, and counts the use.
func (s *Store) ShareTarget(tok string) (int64, bool) {
	if len(tok) < 16 {
		return 0, false
	}
	var id, targetID, exp int64
	err := s.cfg.QueryRow(`SELECT id,target_id,expires_at FROM share_links WHERE token_hash=?`,
		hashToken(tok)).Scan(&id, &targetID, &exp)
	if err != nil {
		return 0, false
	}
	now := time.Now().Unix()
	if exp > 0 && now > exp {
		return 0, false
	}
	s.cfg.Exec(`UPDATE share_links SET last_used=?, uses=uses+1 WHERE id=?`, now, id)
	return targetID, true
}

func (s *Store) Shares() ([]ShareLink, error) {
	rows, err := s.cfg.Query(`SELECT s.id,s.target_id,s.note,s.created_at,s.created_by,
	                          s.expires_at,s.last_used,s.uses,COALESCE(t.title,'')
	                          FROM share_links s LEFT JOIN targets t ON t.id=s.target_id
	                          ORDER BY s.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []ShareLink{}
	for rows.Next() {
		var l ShareLink
		if err := rows.Scan(&l.ID, &l.TargetID, &l.Note, &l.CreatedAt, &l.CreatedBy,
			&l.ExpiresAt, &l.LastUsed, &l.Uses, &l.Title); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *Store) RevokeShare(id int64) error {
	_, err := s.cfg.Exec(`DELETE FROM share_links WHERE id=?`, id)
	return err
}

// shareGrant returns the target a request is allowed to read through a share
// token, if any. It is the only way an unauthenticated caller reaches a
// private target, and it opens exactly one.
func (a *API) shareGrant(r *http.Request) (int64, bool) {
	tok := r.URL.Query().Get("share")
	if tok == "" {
		return 0, false
	}
	return a.store.ShareTarget(tok)
}

func (a *API) ShareRoutes(mux *http.ServeMux) {
	shareSchema(a.store)
	mux.HandleFunc("POST /api/v1/admin/targets/{id}/share", a.need(RoleEditor, a.shareCreate))
	mux.HandleFunc("GET /api/v1/admin/shares", a.need(RoleEditor, a.shareList))
	mux.HandleFunc("DELETE /api/v1/admin/shares/{id}", a.need(RoleEditor, a.shareRevoke))
}

func (a *API) shareCreate(w http.ResponseWriter, r *http.Request, u *User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, 400, "invalid identifier")
		return
	}
	var in struct {
		Days *int   `json:"days"` // absent = 30, 0 = no expiry
		Note string `json:"note"`
	}
	json.NewDecoder(r.Body).Decode(&in)
	days := 30
	if in.Days != nil {
		days = *in.Days
	}
	link, err := a.store.CreateShare(id, days, in.Note, u.Email)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	a.store.Audit(u, clientIP(r), "share_create", "target", strconv.FormatInt(id, 10))
	link.URL = strings.TrimSuffix(a.baseURL(r), "/") + "/s/" + link.Token
	writeJSON(w, link)
}

func (a *API) shareList(w http.ResponseWriter, r *http.Request, u *User) {
	list, err := a.store.Shares()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, list)
}

func (a *API) shareRevoke(w http.ResponseWriter, r *http.Request, u *User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, 400, "invalid identifier")
		return
	}
	if err := a.store.RevokeShare(id); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	a.store.Audit(u, clientIP(r), "share_revoke", "share", strconv.FormatInt(id, 10))
	w.WriteHeader(http.StatusNoContent)
}
