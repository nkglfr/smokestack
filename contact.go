package main

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/mail"
	"net/smtp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Publishing the operator's address on a public page feeds spam lists. The
// contact form keeps it private: visitors write from the site, messages land
// in the back-office, and a notification goes out only if SMTP is already
// configured for the NOC alerts. Replies are sent by the operator from their
// own mail client, so no mail ever leaves the instance on a visitor's behalf.

type ContactMessage struct {
	ID      int64  `json:"id"`
	TS      int64  `json:"ts"`
	Name    string `json:"name"`
	Email   string `json:"email"`
	Subject string `json:"subject"`
	Body    string `json:"body"`
	Lang    string `json:"lang"`
	IP      string `json:"ip"`
	Read    bool   `json:"read"`
}

const (
	contactMaxBody = 4000
	contactMinBody = 10
	contactPerHour = 3 // per IP
)

func contactSchema(s *Store) {
	s.cfg.Exec(`CREATE TABLE IF NOT EXISTS contact_messages(
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		ts INTEGER NOT NULL, name TEXT NOT NULL, email TEXT NOT NULL,
		subject TEXT NOT NULL DEFAULT '', body TEXT NOT NULL,
		lang TEXT NOT NULL DEFAULT '', ip TEXT NOT NULL DEFAULT '',
		read INTEGER NOT NULL DEFAULT 0)`)
	s.cfg.Exec(`CREATE INDEX IF NOT EXISTS idx_contact_ts ON contact_messages(ts DESC)`)
}

// contactLimiter keeps a short memory of senders: a handful of messages per
// hour and per address is plenty for a contact form.
var contactLimiter struct {
	sync.Mutex
	hits map[string][]int64
}

func contactAllowed(ip string) bool {
	now := time.Now().Unix()
	contactLimiter.Lock()
	defer contactLimiter.Unlock()
	if contactLimiter.hits == nil {
		contactLimiter.hits = map[string][]int64{}
	}
	kept := contactLimiter.hits[ip][:0]
	for _, t := range contactLimiter.hits[ip] {
		if now-t < 3600 {
			kept = append(kept, t)
		}
	}
	contactLimiter.hits[ip] = kept
	if len(kept) >= contactPerHour {
		return false
	}
	contactLimiter.hits[ip] = append(kept, now)
	return true
}

// checkContact validates what a visitor typed and returns the message to
// store, or the reason to refuse it.
func checkContact(name, email, subject, body, honeypot string) (*ContactMessage, error) {
	if strings.TrimSpace(honeypot) != "" {
		// Hidden field filled: a bot. Refused without saying why.
		return nil, fmt.Errorf("message refused")
	}
	name = strings.TrimSpace(name)
	subject = strings.TrimSpace(subject)
	body = strings.TrimSpace(body)
	email = strings.TrimSpace(email)
	if name == "" || len(name) > 120 {
		return nil, fmt.Errorf("a name is required")
	}
	if _, err := mail.ParseAddress(email); err != nil {
		return nil, fmt.Errorf("this email address is not valid")
	}
	if len(body) < contactMinBody {
		return nil, fmt.Errorf("the message is too short")
	}
	if len(body) > contactMaxBody || len(subject) > 200 {
		return nil, fmt.Errorf("the message is too long")
	}
	// Header injection through the fields echoed in the notification.
	for _, v := range []string{name, email, subject} {
		if strings.ContainsAny(v, "\r\n") {
			return nil, fmt.Errorf("message refused")
		}
	}
	return &ContactMessage{Name: name, Email: email, Subject: subject, Body: body}, nil
}

func (s *Store) SaveContact(m *ContactMessage) (int64, error) {
	res, err := s.cfg.Exec(`INSERT INTO contact_messages(ts,name,email,subject,body,lang,ip)
	                        VALUES(?,?,?,?,?,?,?)`,
		m.TS, m.Name, m.Email, m.Subject, m.Body, m.Lang, m.IP)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) Contacts(limit int) ([]*ContactMessage, error) {
	rows, err := s.cfg.Query(`SELECT id,ts,name,email,subject,body,lang,ip,read
	                          FROM contact_messages ORDER BY ts DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*ContactMessage
	for rows.Next() {
		m := &ContactMessage{}
		var read int
		if err := rows.Scan(&m.ID, &m.TS, &m.Name, &m.Email, &m.Subject, &m.Body,
			&m.Lang, &m.IP, &read); err != nil {
			return nil, err
		}
		m.Read = read == 1
		out = append(out, m)
	}
	return out, rows.Err()
}

func (s *Store) UnreadContacts() int {
	var n int
	s.cfg.QueryRow(`SELECT COUNT(*) FROM contact_messages WHERE read=0`).Scan(&n)
	return n
}

func (s *Store) MarkContact(id int64, read bool) error {
	_, err := s.cfg.Exec(`UPDATE contact_messages SET read=? WHERE id=?`, b2i(read), id)
	return err
}

func (s *Store) DeleteContact(id int64) error {
	_, err := s.cfg.Exec(`DELETE FROM contact_messages WHERE id=?`, id)
	return err
}

func (a *API) ContactRoutes(mux *http.ServeMux) {
	contactSchema(a.store)
	mux.HandleFunc("GET /api/v1/contact/challenge", a.contactChallenge)
	mux.HandleFunc("POST /api/v1/contact", a.contactPost)
	mux.HandleFunc("GET /api/v1/admin/messages", a.auth(a.messagesGet))
	mux.HandleFunc("PATCH /api/v1/admin/messages/{id}", a.auth(a.messagesPatch))
	mux.HandleFunc("DELETE /api/v1/admin/messages/{id}", a.auth(a.messagesDelete))
}

// A proof-of-work challenge instead of a third-party captcha: nothing to
// read, so it works in every language, nothing to click, and no visitor data
// leaves the instance. The browser looks for a nonce whose SHA-256 starts
// with enough zero bits — about a second of work, invisible to a person,
// expensive for a bot sending thousands of messages.
const (
	captchaBits = 18
	captchaTTL  = 600
)

func captchaSign(salt string, ts int64, secret []byte) string {
	m := hmac.New(sha256.New, secret)
	fmt.Fprintf(m, "%s|%d", salt, ts)
	return hex.EncodeToString(m.Sum(nil))
}

func (a *API) captchaSecret() []byte {
	s := a.store.Setting("captcha_secret", "")
	if s == "" {
		b := make([]byte, 32)
		rand.Read(b)
		s = hex.EncodeToString(b)
		a.store.SetSetting("captcha_secret", s)
	}
	return []byte(s)
}

func (a *API) contactChallenge(w http.ResponseWriter, r *http.Request) {
	b := make([]byte, 12)
	rand.Read(b)
	salt := hex.EncodeToString(b)
	ts := time.Now().Unix()
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, map[string]any{"salt": salt, "ts": ts, "bits": captchaBits,
		"sig": captchaSign(salt, ts, a.captchaSecret())})
}

// checkCaptcha verifies the signature, the age and the work itself.
func (a *API) checkCaptcha(salt, sig, nonce string, ts int64) error {
	if salt == "" || sig == "" || nonce == "" {
		return fmt.Errorf("the verification is missing")
	}
	now := time.Now().Unix()
	if ts > now+60 || now-ts > captchaTTL {
		return fmt.Errorf("the verification expired, please send again")
	}
	if !hmac.Equal([]byte(sig), []byte(captchaSign(salt, ts, a.captchaSecret()))) {
		return fmt.Errorf("invalid verification")
	}
	sum := sha256.Sum256([]byte(salt + nonce))
	if leadingZeroBits(sum[:]) < captchaBits {
		return fmt.Errorf("invalid verification")
	}
	return nil
}

func leadingZeroBits(b []byte) int {
	n := 0
	for _, x := range b {
		if x == 0 {
			n += 8
			continue
		}
		for m := byte(0x80); m > 0; m >>= 1 {
			if x&m != 0 {
				return n
			}
			n++
		}
		return n
	}
	return n
}

func (a *API) contactPost(w http.ResponseWriter, r *http.Request) {
	site := a.store.Site()
	if ContactModeOf(site) != "form" {
		writeErr(w, 404, "the contact form is disabled on this instance")
		return
	}
	var in struct {
		Name, Email, Subject, Message, Lang, Website string
		Salt, Sig, Nonce                             string
		TS                                           int64
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&in); err != nil {
		writeErr(w, 400, "invalid message")
		return
	}
	m, err := checkContact(in.Name, in.Email, in.Subject, in.Message, in.Website)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if site.Captcha {
		if err := a.checkCaptcha(in.Salt, in.Sig, in.Nonce, in.TS); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
	}
	ip := clientIP(r)
	if !contactAllowed(ip) {
		writeErr(w, 429, "too many messages from this address, try again later")
		return
	}
	m.TS, m.IP, m.Lang = time.Now().Unix(), ip, in.Lang
	if _, err := a.store.SaveContact(m); err != nil {
		writeErr(w, 500, "could not store the message")
		return
	}
	go a.notifyContact(site, m)
	writeJSON(w, map[string]any{"ok": true})
}

// notifyContact tells the operator that a message is waiting. It never
// forwards it to the visitor's address, and fails silently: the message is
// stored either way and visible in the back-office.
func (a *API) notifyContact(site Site, m *ContactMessage) {
	to := strings.TrimSpace(site.ContactNotify)
	if to == "" || a.fed == nil {
		return
	}
	cfg := a.fed.notifyConfig()
	if cfg.SMTPHost == "" || cfg.From == "" {
		return
	}
	subject := "[smokestack] Message from " + m.Name
	if m.Subject != "" {
		subject += ": " + m.Subject
	}
	body := fmt.Sprintf("A visitor wrote from %s\n\nName:  %s\nEmail: %s\n%s\n\n%s\n\n"+
		"Reply to them directly, or open the back-office: %s/admin\n",
		site.Title, m.Name, m.Email,
		func() string {
			if m.Subject != "" {
				return "Subject: " + m.Subject
			}
			return ""
		}(),
		m.Body, strings.TrimSuffix(site.URL, "/"))
	msg := []byte(fmt.Sprintf("From: %s\r\nTo: %s\r\nReply-To: %s\r\nSubject: %s\r\n"+
		"MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s",
		cfg.From, to, m.Email, subject, body))
	var auth smtp.Auth
	if cfg.SMTPUser != "" {
		auth = smtp.PlainAuth("", cfg.SMTPUser, cfg.SMTPPass, cfg.SMTPHost)
	}
	smtp.SendMail(fmt.Sprintf("%s:%d", cfg.SMTPHost, cfg.SMTPPort), auth, cfg.From, []string{to}, msg)
}

func (a *API) messagesGet(w http.ResponseWriter, r *http.Request) {
	list, err := a.store.Contacts(200)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, map[string]any{"messages": list, "unread": a.store.UnreadContacts()})
}

func (a *API) messagesPatch(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, 400, "invalid identifier")
		return
	}
	var in struct {
		Read *bool `json:"read"`
	}
	json.NewDecoder(r.Body).Decode(&in)
	read := true
	if in.Read != nil {
		read = *in.Read
	}
	if err := a.store.MarkContact(id, read); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]any{"ok": true})
}

func (a *API) messagesDelete(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, 400, "invalid identifier")
		return
	}
	if err := a.store.DeleteContact(id); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
