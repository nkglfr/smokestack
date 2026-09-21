package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Quatre roles hierarchiques. Le role master est le seul a pouvoir
// gerer les comptes ; il peut y en avoir plusieurs, et le dernier
// master actif ne peut etre ni supprime ni retrograde, pour eviter de
// se verrouiller dehors.
const (
	RoleViewer = 1
	RoleEditor = 2
	RoleAdmin  = 3
	RoleMaster = 4
)

var roleLevels = map[string]int{
	"viewer": RoleViewer, "editor": RoleEditor,
	"admin": RoleAdmin, "master": RoleMaster,
}

const (
	sessionCookie = "smokestack_session"
	sessionTTL    = 12 * time.Hour
	pbkdfIters    = 210000
)

type User struct {
	ID          int64  `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
	Locale      string `json:"locale"`
	Disabled    bool   `json:"disabled"`
	CreatedAt   int64  `json:"created_at"`
	LastLoginAt *int64 `json:"last_login_at"`
}

func (u *User) Level() int { return roleLevels[u.Role] }

// ------------------------------------------------------ mots de passe

// pbkdf2SHA256 evite une dependance a golang.org/x/crypto : la
// construction tient en quinze lignes et reste la recommandation
// OWASP a condition d'y mettre assez d'iterations.
func pbkdf2SHA256(password, salt []byte, iter, keyLen int) []byte {
	out := make([]byte, 0, keyLen)
	var block [4]byte
	for i := 1; len(out) < keyLen; i++ {
		binary.BigEndian.PutUint32(block[:], uint32(i))
		mac := hmac.New(sha256.New, password)
		mac.Write(salt)
		mac.Write(block[:])
		u := mac.Sum(nil)
		t := make([]byte, len(u))
		copy(t, u)
		for n := 1; n < iter; n++ {
			mac = hmac.New(sha256.New, password)
			mac.Write(u)
			u = mac.Sum(nil)
			for j := range t {
				t[j] ^= u[j]
			}
		}
		out = append(out, t...)
	}
	return out[:keyLen]
}

func hashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := pbkdf2SHA256([]byte(pw), salt, pbkdfIters, 32)
	return fmt.Sprintf("pbkdf2$sha256$%d$%s$%s", pbkdfIters,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(key)), nil
}

func verifyPassword(stored, pw string) bool {
	parts := strings.Split(stored, "$")
	if len(parts) != 5 || parts[0] != "pbkdf2" || parts[1] != "sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[2])
	if err != nil || iter < 1000 {
		return false
	}
	salt, err1 := base64.RawStdEncoding.DecodeString(parts[3])
	want, err2 := base64.RawStdEncoding.DecodeString(parts[4])
	if err1 != nil || err2 != nil {
		return false
	}
	got := pbkdf2SHA256([]byte(pw), salt, iter, len(want))
	return subtle.ConstantTimeCompare(got, want) == 1
}

func validPassword(pw string) error {
	if len(pw) < 12 {
		return fmt.Errorf("the password must be at least 12 characters long")
	}
	return nil
}

// -------------------------------------------------------- utilisateurs

func (s *Store) CountUsers() int {
	var n int
	s.cfg.QueryRow(`SELECT COUNT(*) FROM users`).Scan(&n)
	return n
}

const userCols = `id,email,display_name,role,locale,disabled,created_at,last_login_at`

func scanUser(scan func(...any) error) (*User, error) {
	u := &User{}
	var dis int
	if err := scan(&u.ID, &u.Email, &u.DisplayName, &u.Role, &u.Locale,
		&dis, &u.CreatedAt, &u.LastLoginAt); err != nil {
		return nil, err
	}
	u.Disabled = dis == 1
	return u, nil
}

func (s *Store) Users() ([]*User, error) {
	rows, err := s.cfg.Query(`SELECT ` + userCols + ` FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*User{}
	for rows.Next() {
		u, err := scanUser(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (s *Store) CreateUser(email, name, pw, role string) (*User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" || !strings.Contains(email, "@") {
		return nil, fmt.Errorf("invalid email address")
	}
	if _, ok := roleLevels[role]; !ok {
		return nil, fmt.Errorf("role inconnu: %s", role)
	}
	if err := validPassword(pw); err != nil {
		return nil, err
	}
	h, err := hashPassword(pw)
	if err != nil {
		return nil, err
	}
	if name == "" {
		name = email
	}
	res, err := s.cfg.Exec(
		`INSERT INTO users(email,display_name,password_hash,role,created_at)
		 VALUES(?,?,?,?,?)`, email, name, h, role, time.Now().Unix())
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, fmt.Errorf("this email address is already in use")
		}
		return nil, err
	}
	id, _ := res.LastInsertId()
	row := s.cfg.QueryRow(`SELECT `+userCols+` FROM users WHERE id=?`, id)
	return scanUser(row.Scan)
}

func (s *Store) masterCount(excludeID int64) int {
	var n int
	s.cfg.QueryRow(
		`SELECT COUNT(*) FROM users WHERE role='master' AND disabled=0 AND id<>?`,
		excludeID).Scan(&n)
	return n
}

func (s *Store) UpdateUser(id int64, name, role, pw *string, disabled *bool) error {
	cur := s.cfg.QueryRow(`SELECT `+userCols+` FROM users WHERE id=?`, id)
	u, err := scanUser(cur.Scan)
	if err != nil {
		return fmt.Errorf("user not found")
	}
	// Garde-fou : on ne retire jamais le dernier master actif.
	losingMaster := (role != nil && *role != "master") ||
		(disabled != nil && *disabled)
	if u.Role == "master" && losingMaster && s.masterCount(id) == 0 {
		return fmt.Errorf("at least one active master account must remain")
	}
	if name != nil && *name != "" {
		s.cfg.Exec(`UPDATE users SET display_name=? WHERE id=?`, *name, id)
	}
	if role != nil {
		if _, ok := roleLevels[*role]; !ok {
			return fmt.Errorf("role inconnu: %s", *role)
		}
		s.cfg.Exec(`UPDATE users SET role=? WHERE id=?`, *role, id)
	}
	if disabled != nil {
		s.cfg.Exec(`UPDATE users SET disabled=? WHERE id=?`, b2i(*disabled), id)
		if *disabled {
			s.cfg.Exec(`DELETE FROM sessions WHERE user_id=?`, id)
		}
	}
	if pw != nil {
		if err := validPassword(*pw); err != nil {
			return err
		}
		h, err := hashPassword(*pw)
		if err != nil {
			return err
		}
		s.cfg.Exec(`UPDATE users SET password_hash=? WHERE id=?`, h, id)
		// Un changement de mot de passe invalide les sessions ouvertes.
		s.cfg.Exec(`DELETE FROM sessions WHERE user_id=?`, id)
	}
	return nil
}

func (s *Store) DeleteUser(id int64) error {
	row := s.cfg.QueryRow(`SELECT `+userCols+` FROM users WHERE id=?`, id)
	u, err := scanUser(row.Scan)
	if err != nil {
		return fmt.Errorf("user not found")
	}
	if u.Role == "master" && s.masterCount(id) == 0 {
		return fmt.Errorf("at least one active master account must remain")
	}
	_, err = s.cfg.Exec(`DELETE FROM users WHERE id=?`, id)
	return err
}

func (s *Store) Audit(u *User, ip, action, entity, entityID string) {
	var uid *int64
	var email *string
	if u != nil {
		uid = &u.ID
		email = &u.Email
	}
	s.cfg.Exec(
		`INSERT INTO audit_log(ts,user_id,email,action,entity,entity_id,ip)
		 VALUES(?,?,?,?,?,?,?)`,
		time.Now().Unix(), uid, email, action, entity, entityID, ip)
}

// ------------------------------------------------------------ sessions

func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (s *Store) OpenSession(userID int64, ip, ua string) (token, csrf string, err error) {
	token, csrf = randomHex(32), randomHex(16)
	now := time.Now()
	_, err = s.cfg.Exec(
		`INSERT INTO sessions(token_hash,user_id,csrf,ip,user_agent,created_at,expires_at)
		 VALUES(?,?,?,?,?,?,?)`,
		hashToken(token), userID, csrf, ip, ua, now.Unix(),
		now.Add(sessionTTL).Unix())
	return
}

type sessionInfo struct {
	User *User
	CSRF string
}

func (s *Store) Session(token string) (*sessionInfo, error) {
	row := s.cfg.QueryRow(
		`SELECT s.csrf, u.id,u.email,u.display_name,u.role,u.locale,
		        u.disabled,u.created_at,u.last_login_at
		   FROM sessions s JOIN users u ON u.id=s.user_id
		  WHERE s.token_hash=? AND s.expires_at>?`,
		hashToken(token), time.Now().Unix())
	var csrf string
	u := &User{}
	var dis int
	if err := row.Scan(&csrf, &u.ID, &u.Email, &u.DisplayName, &u.Role,
		&u.Locale, &dis, &u.CreatedAt, &u.LastLoginAt); err != nil {
		return nil, err
	}
	if dis == 1 {
		return nil, sql.ErrNoRows
	}
	u.Disabled = false
	return &sessionInfo{User: u, CSRF: csrf}, nil
}

func (s *Store) CloseSession(token string) {
	s.cfg.Exec(`DELETE FROM sessions WHERE token_hash=?`, hashToken(token))
}

func (s *Store) PurgeSessions() {
	s.cfg.Exec(`DELETE FROM sessions WHERE expires_at < ?`, time.Now().Unix())
}

// --------------------------------------------------- limitation d'essais

type attemptLimiter struct {
	mu   sync.Mutex
	hits map[string][]int64
}

func newAttemptLimiter() *attemptLimiter {
	return &attemptLimiter{hits: map[string][]int64{}}
}

// allow autorise 8 tentatives par tranche de 10 minutes et par IP.
func (l *attemptLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now().Unix()
	var kept []int64
	for _, ts := range l.hits[key] {
		if ts > now-600 {
			kept = append(kept, ts)
		}
	}
	if len(kept) >= 8 {
		l.hits[key] = kept
		return false
	}
	l.hits[key] = append(kept, now)
	return true
}

// clientIP ne croit l'en-tete X-Forwarded-For que si la connexion vient
// d'un reverse proxy local : expose directement, un client pourrait sinon
// forger cet en-tete pour contourner la limitation des tentatives.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
		if xf := r.Header.Get("X-Forwarded-For"); xf != "" {
			parts := strings.Split(xf, ",")
			return strings.TrimSpace(parts[len(parts)-1])
		}
	}
	return host
}

// ------------------------------------------------------- garde d'acces

// need construit un gestionnaire protege. Deux modes d'authentification
// coexistent : le cookie de session pour l'interface, et le jeton
// d'administration en Bearer pour l'automatisation, qui vaut master.
// Le CSRF n'est exige que pour le mode cookie.
func (a *API) need(level int, h func(http.ResponseWriter, *http.Request, *User)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		bearer := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		if bearer != "" && a.token != "" &&
			subtle.ConstantTimeCompare([]byte(bearer), []byte(a.token)) == 1 {
			h(w, r, &User{ID: 0, Email: "api-token", DisplayName: "jeton API",
				Role: "master"})
			return
		}
		c, err := r.Cookie(sessionCookie)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, "authentication required")
			return
		}
		sess, err := a.store.Session(c.Value)
		if err != nil {
			clearSessionCookie(w)
			writeErr(w, http.StatusUnauthorized, "session expired")
			return
		}
		if r.Method != "GET" && r.Method != "HEAD" {
			if r.Header.Get("X-CSRF-Token") != sess.CSRF {
				writeErr(w, http.StatusForbidden, "invalid CSRF token")
				return
			}
		}
		if sess.User.Level() < level {
			writeErr(w, http.StatusForbidden, "insufficient privileges")
			return
		}
		h(w, r, sess.User)
	}
}

// auth conserve la signature utilisee par les routes existantes.
func (a *API) auth(h http.HandlerFunc) http.HandlerFunc {
	return a.need(RoleAdmin, func(w http.ResponseWriter, r *http.Request, _ *User) {
		h(w, r)
	})
}

func setSessionCookie(w http.ResponseWriter, r *http.Request, token string) {
	secure := r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/",
		HttpOnly: true, Secure: secure, SameSite: http.SameSiteLaxMode,
		MaxAge: int(sessionTTL.Seconds()),
	})
}

func clearSessionCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: "", Path: "/",
		HttpOnly: true, MaxAge: -1,
	})
}

// ------------------------------------------------------------- routes

func (a *API) AuthRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/auth/state", a.authState)
	mux.HandleFunc("POST /api/v1/auth/setup", a.authSetup)
	mux.HandleFunc("POST /api/v1/auth/login", a.authLogin)
	mux.HandleFunc("POST /api/v1/auth/logout", a.authLogout)
	mux.HandleFunc("GET /api/v1/auth/me", a.need(RoleViewer, a.authMe))
	mux.HandleFunc("POST /api/v1/auth/password", a.need(RoleViewer, a.authPassword))

	mux.HandleFunc("GET /api/v1/admin/users", a.need(RoleMaster, a.usersList))
	mux.HandleFunc("POST /api/v1/admin/users", a.need(RoleMaster, a.usersCreate))
	mux.HandleFunc("PATCH /api/v1/admin/users/{id}", a.need(RoleMaster, a.usersPatch))
	mux.HandleFunc("DELETE /api/v1/admin/users/{id}", a.need(RoleMaster, a.usersDelete))
	mux.HandleFunc("GET /api/v1/admin/audit", a.need(RoleAdmin, a.auditList))
}

// authState dit a l'interface s'il faut afficher l'ecran d'amorcage,
// l'ecran de connexion, ou directement le back-office.
func (a *API) authState(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"setup_required": a.store.CountUsers() == 0}
	if c, err := r.Cookie(sessionCookie); err == nil {
		if sess, err := a.store.Session(c.Value); err == nil {
			out["user"] = sess.User
			out["csrf"] = sess.CSRF
		}
	}
	writeJSON(w, out)
}

// authSetup ne fonctionne que sur une base sans aucun compte : c'est
// la creation du premier master, et la fenetre se referme ensuite.
func (a *API) authSetup(w http.ResponseWriter, r *http.Request) {
	if a.store.CountUsers() > 0 {
		writeErr(w, http.StatusConflict, "an account already exists")
		return
	}
	var in struct {
		Email, Name, Password string
		SetupCode             string `json:"setup_code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	// Sans ce code, le premier visiteur d'une instance fraichement
	// exposee pourrait s'attribuer le compte master.
	if !a.limiter.allow(clientIP(r)) {
		writeErr(w, http.StatusTooManyRequests, "too many attempts, try again later")
		return
	}
	if a.setupCode == "" || subtle.ConstantTimeCompare(
		[]byte(strings.TrimSpace(in.SetupCode)), []byte(a.setupCode)) != 1 {
		writeErr(w, http.StatusForbidden,
			"invalid setup code: it is shown in the service log "+
				"and in the setup-code file of the data directory")
		return
	}
	u, err := a.store.CreateUser(in.Email, in.Name, in.Password, "master")
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	token, csrf, err := a.store.OpenSession(u.ID, clientIP(r), r.UserAgent())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	setSessionCookie(w, r, token)
	a.clearSetupCode()
	a.store.Audit(u, clientIP(r), "setup", "user", strconv.FormatInt(u.ID, 10))
	writeJSON(w, map[string]any{"user": u, "csrf": csrf})
}

func (a *API) authLogin(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r)
	if !a.limiter.allow(ip) {
		writeErr(w, http.StatusTooManyRequests,
			"too many attempts, try again in a few minutes")
		return
	}
	var in struct{ Email, Password string }
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	var id int64
	var hash string
	var disabled int
	err := a.store.cfg.QueryRow(
		`SELECT id,password_hash,disabled FROM users WHERE email=?`,
		strings.ToLower(strings.TrimSpace(in.Email))).Scan(&id, &hash, &disabled)
	// Message identique dans tous les cas : on ne revele pas si le
	// compte existe.
	if err != nil || disabled == 1 || !verifyPassword(hash, in.Password) {
		a.store.Audit(nil, ip, "login_failed", "user", in.Email)
		writeErr(w, http.StatusUnauthorized, "invalid credentials")
		return
	}
	token, csrf, err := a.store.OpenSession(id, ip, r.UserAgent())
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	a.store.cfg.Exec(`UPDATE users SET last_login_at=? WHERE id=?`,
		time.Now().Unix(), id)
	setSessionCookie(w, r, token)
	row := a.store.cfg.QueryRow(`SELECT `+userCols+` FROM users WHERE id=?`, id)
	u, _ := scanUser(row.Scan)
	a.store.Audit(u, ip, "login", "user", strconv.FormatInt(id, 10))
	writeJSON(w, map[string]any{"user": u, "csrf": csrf})
}

func (a *API) authLogout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookie); err == nil {
		a.store.CloseSession(c.Value)
	}
	clearSessionCookie(w)
	writeJSON(w, map[string]any{"ok": true})
}

func (a *API) authMe(w http.ResponseWriter, r *http.Request, u *User) {
	writeJSON(w, u)
}

func (a *API) authPassword(w http.ResponseWriter, r *http.Request, u *User) {
	var in struct{ Current, New string }
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if u.ID == 0 {
		writeErr(w, 400, "the API token has no password")
		return
	}
	var hash string
	a.store.cfg.QueryRow(`SELECT password_hash FROM users WHERE id=?`, u.ID).Scan(&hash)
	if !verifyPassword(hash, in.Current) {
		writeErr(w, 403, "current password is incorrect")
		return
	}
	if err := a.store.UpdateUser(u.ID, nil, nil, &in.New, nil); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	clearSessionCookie(w)
	a.store.Audit(u, clientIP(r), "password_change", "user",
		strconv.FormatInt(u.ID, 10))
	writeJSON(w, map[string]any{"ok": true, "reauth": true})
}

func (a *API) usersList(w http.ResponseWriter, r *http.Request, u *User) {
	list, err := a.store.Users()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, list)
}

func (a *API) usersCreate(w http.ResponseWriter, r *http.Request, actor *User) {
	var in struct{ Email, Name, Password, Role string }
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if in.Role == "" {
		in.Role = "viewer"
	}
	u, err := a.store.CreateUser(in.Email, in.Name, in.Password, in.Role)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	a.store.Audit(actor, clientIP(r), "user_create", "user",
		strconv.FormatInt(u.ID, 10))
	writeJSON(w, u)
}

func (a *API) usersPatch(w http.ResponseWriter, r *http.Request, actor *User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, 400, "invalid identifier")
		return
	}
	var in struct {
		Name     *string `json:"display_name"`
		Role     *string `json:"role"`
		Password *string `json:"password"`
		Disabled *bool   `json:"disabled"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := a.store.UpdateUser(id, in.Name, in.Role, in.Password, in.Disabled); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	a.store.Audit(actor, clientIP(r), "user_update", "user", r.PathValue("id"))
	writeJSON(w, map[string]any{"ok": true})
}

func (a *API) usersDelete(w http.ResponseWriter, r *http.Request, actor *User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, 400, "invalid identifier")
		return
	}
	if id == actor.ID {
		writeErr(w, 400, "you cannot delete your own account")
		return
	}
	if err := a.store.DeleteUser(id); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	a.store.Audit(actor, clientIP(r), "user_delete", "user", r.PathValue("id"))
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) auditList(w http.ResponseWriter, r *http.Request, u *User) {
	rows, err := a.store.cfg.Query(
		`SELECT ts,COALESCE(email,''),action,COALESCE(entity,''),
		        COALESCE(entity_id,''),COALESCE(ip,'')
		   FROM audit_log ORDER BY ts DESC LIMIT 200`)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	defer rows.Close()
	type entry struct {
		TS       int64  `json:"ts"`
		Email    string `json:"email"`
		Action   string `json:"action"`
		Entity   string `json:"entity"`
		EntityID string `json:"entity_id"`
		IP       string `json:"ip"`
	}
	out := []entry{}
	for rows.Next() {
		var e entry
		if err := rows.Scan(&e.TS, &e.Email, &e.Action, &e.Entity,
			&e.EntityID, &e.IP); err == nil {
			out = append(out, e)
		}
	}
	writeJSON(w, out)
}
