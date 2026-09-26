package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Appairage en deux temps, pour qu'aucune instance ne se retrouve
// appairee sans qu'un humain l'ait voulu des deux cotes :
//
//  1. A saisit l'URL publique de B, une justification obligatoire et,
//     s'il le souhaite, une note privee adressee a l'administrateur de B.
//  2. B voit la demande dans son back-office : AS, empreinte, motif,
//     note privee, et si A accepte que l'appairage soit affiche.
//  3. B accepte ou refuse, avec une reponse privee facultative. En cas
//     d'acceptation, les deux cotes s'approuvent mutuellement.
//
// Affichage public : un appairage n'apparait sur la page publique que si
// les deux administrateurs l'ont accepte. Chacun peut ensuite le masquer
// de son cote ; aucun ne peut l'afficher sans l'accord de l'autre.
//
// La justification et la note privee ne sortent jamais de l'API
// d'administration : elles ne figurent sur aucune page publique.

const pairingSchema = `
CREATE TABLE IF NOT EXISTS fed_pairing (
  id          INTEGER PRIMARY KEY,
  direction   TEXT NOT NULL CHECK (direction IN ('in','out')),
  token       TEXT NOT NULL,
  asn         TEXT NOT NULL,
  org         TEXT NOT NULL,
  url         TEXT NOT NULL,
  pubkey      TEXT NOT NULL,
  fingerprint TEXT NOT NULL,
  noc_email   TEXT,
  anchors     TEXT NOT NULL DEFAULT '[]',
  message     TEXT,
  state       TEXT NOT NULL DEFAULT 'pending',
  created_at  INTEGER NOT NULL,
  decided_at  INTEGER,
  decided_by  TEXT,
  error       TEXT
);
CREATE INDEX IF NOT EXISTS idx_pairing_state ON fed_pairing(state, direction);
CREATE UNIQUE INDEX IF NOT EXISTS idx_pairing_token ON fed_pairing(direction, token);
`

const (
	minJustification = 20
	maxNoteLength    = 2000
)

type PairingRequest struct {
	ID            int64    `json:"id"`
	Direction     string   `json:"direction"`
	Token         string   `json:"token"`
	ASN           string   `json:"asn"`
	Org           string   `json:"org"`
	URL           string   `json:"url"`
	Fingerprint   string   `json:"fingerprint"`
	NOCEmail      string   `json:"noc_email"`
	Anchors       []string `json:"anchors"`
	Justification string   `json:"justification"`
	PrivateNote   string   `json:"private_note"`
	ReplyNote     string   `json:"reply_note"`
	ContactName   string   `json:"contact_name"`
	PublicListing bool     `json:"public_listing"`
	PeerPublic    bool     `json:"peer_public"`
	State         string   `json:"state"`
	CreatedAt     int64    `json:"created_at"`
	DecidedAt     *int64   `json:"decided_at"`
	DecidedBy     string   `json:"decided_by"`
	Error         string   `json:"error"`
	pubkey        string
}

func fingerprintOf(pub []byte) string {
	sum := sha256.Sum256(pub)
	h := hex.EncodeToString(sum[:8])
	var parts []string
	for i := 0; i < len(h); i += 4 {
		parts = append(parts, h[i:i+4])
	}
	return strings.Join(parts, "-")
}

// baseOf accepte indifferemment l'URL de la page d'appairage ou la
// racine de l'instance.
func baseOf(u string) string {
	u = strings.TrimSpace(u)
	u = strings.TrimSuffix(u, "/")
	u = strings.TrimSuffix(u, "/pairing")
	return strings.TrimSuffix(u, "/")
}

func (f *Federation) initPairing() error {
	if _, err := f.store.cfg.Exec(pairingSchema); err != nil {
		return err
	}
	addColumn(f.store.cfg, "fed_pairing", "justification TEXT")
	addColumn(f.store.cfg, "fed_pairing", "private_note TEXT")
	addColumn(f.store.cfg, "fed_pairing", "reply_note TEXT")
	addColumn(f.store.cfg, "fed_pairing", "contact_name TEXT")
	addColumn(f.store.cfg, "fed_pairing", "public_listing INTEGER NOT NULL DEFAULT 0")
	addColumn(f.store.cfg, "fed_pairing", "peer_public INTEGER NOT NULL DEFAULT 0")
	return nil
}

const pairCols = `id,direction,token,asn,org,url,pubkey,fingerprint,
                  COALESCE(noc_email,''),anchors,
                  COALESCE(justification,COALESCE(message,'')),
                  COALESCE(private_note,''),COALESCE(reply_note,''),
                  COALESCE(contact_name,''),public_listing,peer_public,state,
                  created_at,decided_at,COALESCE(decided_by,''),COALESCE(error,'')`

func scanPairing(scan func(...any) error) (*PairingRequest, error) {
	p := &PairingRequest{}
	var anchors string
	var pub, peerPub int
	if err := scan(&p.ID, &p.Direction, &p.Token, &p.ASN, &p.Org, &p.URL,
		&p.pubkey, &p.Fingerprint, &p.NOCEmail, &anchors, &p.Justification,
		&p.PrivateNote, &p.ReplyNote, &p.ContactName, &pub, &peerPub,
		&p.State, &p.CreatedAt, &p.DecidedAt, &p.DecidedBy, &p.Error); err != nil {
		return nil, err
	}
	p.PublicListing, p.PeerPublic = pub == 1, peerPub == 1
	json.Unmarshal([]byte(anchors), &p.Anchors)
	return p, nil
}

func (f *Federation) Pairings() ([]*PairingRequest, error) {
	rows, err := f.store.cfg.Query(`SELECT ` + pairCols +
		` FROM fed_pairing ORDER BY created_at DESC LIMIT 100`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*PairingRequest{}
	for rows.Next() {
		p, err := scanPairing(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (f *Federation) pairingByID(id int64) (*PairingRequest, error) {
	row := f.store.cfg.QueryRow(`SELECT `+pairCols+` FROM fed_pairing WHERE id=?`, id)
	return scanPairing(row.Scan)
}

// ---------------------------------------------------------- validation

func checkJustification(s string) (string, error) {
	s = strings.TrimSpace(s)
	n := utf8.RuneCountInString(s)
	if n < minJustification {
		return "", fmt.Errorf("the justification must be at least %d characters long",
			minJustification)
	}
	if n > maxNoteLength {
		return "", fmt.Errorf("the justification exceeds %d characters", maxNoteLength)
	}
	return s, nil
}

func checkNote(s string) (string, error) {
	s = strings.TrimSpace(s)
	if utf8.RuneCountInString(s) > maxNoteLength {
		return "", fmt.Errorf("the note exceeds %d characters", maxNoteLength)
	}
	return s, nil
}

// ------------------------------------------------------ cote demandeur

type PairingInput struct {
	URL           string `json:"url"`
	Justification string `json:"justification"`
	PrivateNote   string `json:"private_note"`
	ContactName   string `json:"contact_name"`
	PublicListing bool   `json:"public_listing"`
	// Fingerprint : si l'operateur connait deja l'empreinte de son
	// homologue, elle est verifiee avant l'envoi de la demande.
	Fingerprint string `json:"fingerprint"`
}

type pairingPayload struct {
	Token         string   `json:"token"`
	ASN           string   `json:"asn"`
	Org           string   `json:"org"`
	URL           string   `json:"url"`
	PubKey        string   `json:"pubkey"`
	NOCEmail      string   `json:"noc_email"`
	Anchors       []string `json:"anchors"`
	Justification string   `json:"justification"`
	PrivateNote   string   `json:"private_note"`
	ContactName   string   `json:"contact_name"`
	PublicListing bool     `json:"public_listing"`
}

func (f *Federation) RequestPairing(in PairingInput) (*PairingRequest, error) {
	base, err := safeFedURL(in.URL)
	if err != nil {
		return nil, err
	}
	just, err := checkJustification(in.Justification)
	if err != nil {
		return nil, err
	}
	note, err := checkNote(in.PrivateNote)
	if err != nil {
		return nil, err
	}
	me := f.Profile()
	if me.ASN == "" {
		return nil, fmt.Errorf("set your AS number before pairing")
	}
	if base == me.URL {
		return nil, fmt.Errorf("this is the URL of this instance")
	}

	// Lecture du profil public du distant : on obtient sa cle et son
	// empreinte, que l'operateur compare avant d'aller plus loin.
	resp, err := f.client.Get(base + "/api/v1/fed/profile")
	if err != nil {
		return nil, fmt.Errorf("instance unreachable: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("profil distant: HTTP %d", resp.StatusCode)
	}
	var prof FedProfile
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<18)).Decode(&prof); err != nil {
		return nil, err
	}
	raw, err := base64.StdEncoding.DecodeString(prof.PubKey)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("invalid remote public key")
	}
	remoteASN, err := normASN(prof.ASN)
	if err != nil {
		return nil, fmt.Errorf("the remote instance announces no usable AS number")
	}
	prof.ASN = remoteASN
	if prof.ASN == me.ASN {
		return nil, fmt.Errorf("the remote instance announces the same AS as ours")
	}
	if in.Fingerprint != "" && !sameFingerprint(in.Fingerprint, fingerprintOf(raw)) {
		return nil, fmt.Errorf("that instance publishes the fingerprint %s, "+
			"not the one you entered", fingerprintOf(raw))
	}
	if err := f.refuseKeyChange(base, prof.ASN, prof.PubKey); err != nil {
		return nil, err
	}
	prof.Anchors, _ = cleanAnchors(prof.Anchors)
	prof.Org, prof.NOCEmail = oneLine(prof.Org, 120), oneLine(prof.NOCEmail, 200)

	token := randomHex(16)
	anchors, _ := json.Marshal(prof.Anchors)
	res, err := f.store.cfg.Exec(
		`INSERT INTO fed_pairing(direction,token,asn,org,url,pubkey,fingerprint,
		                         noc_email,anchors,justification,private_note,
		                         contact_name,public_listing,state,created_at)
		 VALUES('out',?,?,?,?,?,?,?,?,?,?,?,?,'pending',?)`,
		token, prof.ASN, prof.Org, base, prof.PubKey, fingerprintOf(raw),
		prof.NOCEmail, string(anchors), just, note, in.ContactName,
		b2i(in.PublicListing), time.Now().Unix())
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()

	payload := pairingPayload{
		Token: token, ASN: me.ASN, Org: me.Org, URL: me.URL,
		PubKey: me.PubKey, NOCEmail: me.NOCEmail, Anchors: me.Anchors,
		Justification: just, PrivateNote: note,
		ContactName: in.ContactName, PublicListing: in.PublicListing,
	}
	if err := f.postUnsigned(prof.ASN, base+"/api/v1/fed/pairing/request", payload); err != nil {
		f.store.cfg.Exec(
			`UPDATE fed_pairing SET state='failed', error=? WHERE id=?`,
			err.Error(), id)
		return nil, fmt.Errorf("sending the request: %w", err)
	}
	return f.pairingByID(id)
}

// postUnsigned signe avec notre cle sans supposer de relation etablie :
// le destinataire verifie contre la cle annoncee dans le corps.
func (f *Federation) postUnsigned(audience, url string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	idx := strings.Index(url, "/api/")
	if idx < 0 {
		return fmt.Errorf("invalid API URL")
	}
	f.sign(req, audience, url[idx:], body)
	resp, err := f.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	msg, _ := io.ReadAll(io.LimitReader(resp.Body, 2048))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}

// ------------------------------------------------------ cote sollicite

func (f *Federation) HandlePairingRequest(r *http.Request) (*PairingRequest, error) {
	h, body, err := f.readSigned(r)
	if err != nil {
		return nil, err
	}
	var in pairingPayload
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, err
	}
	if in.Token == "" || in.ASN == "" || in.URL == "" || in.PubKey == "" {
		return nil, fmt.Errorf("incomplete request")
	}
	just, err := checkJustification(in.Justification)
	if err != nil {
		return nil, err
	}
	note, err := checkNote(in.PrivateNote)
	if err != nil {
		return nil, err
	}
	asn, err := normASN(in.ASN)
	if err != nil {
		return nil, err
	}
	in.ASN = asn
	sigASN, err := normASN(h.asn)
	if err != nil || sigASN != in.ASN {
		return nil, fmt.Errorf("the signing AS does not match the body")
	}
	base, err := safeFedURL(in.URL)
	if err != nil {
		return nil, fmt.Errorf("announced URL: %w", err)
	}
	in.URL = base
	raw, err := base64.StdEncoding.DecodeString(in.PubKey)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("invalid public key")
	}
	if err := f.checkSig(ed25519.PublicKey(raw), r, h, body); err != nil {
		return nil, err
	}
	if in.ASN == f.Profile().ASN {
		return nil, fmt.Errorf("same AS as ours")
	}
	// La signature ne prouve que la possession de la cle transportee : a
	// elle seule, elle laisse n'importe qui se presenter comme
	// « AS3215 Orange ». On exige donc que l'instance situee a l'URL
	// annoncee publie bien cette cle et cet AS. L'operateur garde la
	// comparaison d'empreinte hors bande, rendue obligatoire a
	// l'acceptation.
	if err := f.confirmAnnouncedIdentity(in.URL, in.ASN, in.PubKey); err != nil {
		return nil, err
	}
	if err := f.refuseKeyChange(in.URL, in.ASN, in.PubKey); err != nil {
		return nil, err
	}
	in.Org, in.NOCEmail = oneLine(in.Org, 120), oneLine(in.NOCEmail, 200)
	in.ContactName = oneLine(in.ContactName, 120)
	in.Anchors, _ = cleanAnchors(in.Anchors)

	anchors, _ := json.Marshal(in.Anchors)
	_, err = f.store.cfg.Exec(
		`INSERT INTO fed_pairing(direction,token,asn,org,url,pubkey,fingerprint,
		                         noc_email,anchors,justification,private_note,
		                         contact_name,public_listing,state,created_at)
		 VALUES('in',?,?,?,?,?,?,?,?,?,?,?,?,'pending',?)
		 ON CONFLICT(direction,token) DO UPDATE SET
		   created_at=excluded.created_at, justification=excluded.justification,
		   private_note=excluded.private_note, anchors=excluded.anchors,
		   public_listing=excluded.public_listing, state='pending'`,
		in.Token, in.ASN, in.Org, in.URL, in.PubKey, fingerprintOf(raw),
		in.NOCEmail, string(anchors), just, note, in.ContactName,
		b2i(in.PublicListing), time.Now().Unix())
	if err != nil {
		return nil, err
	}
	log.Printf("pairing request received from %s (%s), fingerprint %s",
		in.ASN, in.Org, fingerprintOf(raw))
	row := f.store.cfg.QueryRow(`SELECT `+pairCols+
		` FROM fed_pairing WHERE direction='in' AND token=?`, in.Token)
	return scanPairing(row.Scan)
}

// confirmAnnouncedIdentity verifie que l'instance situee a l'URL annoncee
// publie bien la cle et l'AS de la demande. Un attaquant qui pretend
// etre un autre AS doit donc au minimum controler une instance a l'URL
// qu'il annonce ; il ne peut plus emprunter le nom d'un reseau connu en
// pointant vers son propre serveur.
func (f *Federation) confirmAnnouncedIdentity(base, asn, pubkey string) error {
	resp, err := f.client.Get(base + "/api/v1/fed/profile")
	if err != nil {
		return fmt.Errorf("the announced instance (%s) is unreachable, "+
			"so its identity cannot be confirmed: %w", base, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("the announced instance (%s) answers HTTP %d "+
			"on its federation profile", base, resp.StatusCode)
	}
	var prof FedProfile
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<18)).Decode(&prof); err != nil {
		return fmt.Errorf("unreadable profile at %s", base)
	}
	remote, err := normASN(prof.ASN)
	if err != nil {
		return fmt.Errorf("the instance at %s announces no usable AS number", base)
	}
	if remote != asn {
		return fmt.Errorf("the instance at %s announces %s, but the request "+
			"claims to come from %s", base, remote, asn)
	}
	if prof.PubKey != pubkey {
		return fmt.Errorf("the instance at %s does not publish the key that "+
			"signed this request", base)
	}
	return nil
}

type pairingDecision struct {
	Token         string     `json:"token"`
	Decision      string     `json:"decision"`
	ReplyNote     string     `json:"reply_note"`
	PublicListing bool       `json:"public_listing"`
	Profile       FedProfile `json:"profile"`
}

type DecisionInput struct {
	Accept        bool   `json:"accept"`
	Reply         string `json:"reply"`
	PublicListing bool   `json:"public_listing"`
	// Fingerprint : l'empreinte telle que l'operateur l'a recue hors
	// bande. Obligatoire pour accepter — c'est la seule chose qui lie la
	// cle a un interlocuteur reel, et rien ne l'imposait.
	Fingerprint string `json:"fingerprint"`
}

func (f *Federation) DecidePairing(id int64, in DecisionInput, by string) error {
	p, err := f.pairingByID(id)
	if err != nil {
		return fmt.Errorf("request not found")
	}
	if p.Direction != "in" {
		return fmt.Errorf("only incoming requests can be decided here")
	}
	if p.State != "pending" {
		return fmt.Errorf("request already handled (%s)", p.State)
	}
	reply, err := checkNote(in.Reply)
	if err != nil {
		return err
	}
	now := time.Now().Unix()
	if in.Accept {
		if !sameFingerprint(in.Fingerprint, p.Fingerprint) {
			return fmt.Errorf("to accept, confirm the fingerprint you received "+
				"out of band from that operator — this request carries %s",
				p.Fingerprint)
		}
		// Le demandeur a donne son accord (ou non) dans sa demande ;
		// nous donnons le notre maintenant.
		if err := f.upsertTrustedPeer(p, p.PublicListing, in.PublicListing); err != nil {
			return err
		}
	}
	decision := "rejected"
	if in.Accept {
		decision = "accepted"
	}
	f.store.cfg.Exec(
		`UPDATE fed_pairing SET state=?, decided_at=?, decided_by=?,
		        reply_note=?, peer_public=? WHERE id=?`,
		decision, now, by, reply, b2i(in.PublicListing), id)

	payload := pairingDecision{
		Token: p.Token, Decision: decision, ReplyNote: reply,
		PublicListing: in.PublicListing, Profile: f.Profile(),
	}
	payload.Profile.NOCPhone = ""
	if err := f.postUnsigned(p.ASN, p.URL+"/api/v1/fed/pairing/response", payload); err != nil {
		f.store.cfg.Exec(`UPDATE fed_pairing SET error=? WHERE id=?`, err.Error(), id)
		log.Printf("pairing response to %s: %v", p.URL, err)
	}
	return nil
}

func (f *Federation) HandlePairingResponse(r *http.Request) (string, error) {
	h, body, err := f.readSigned(r)
	if err != nil {
		return "", err
	}
	var in pairingDecision
	if err := json.Unmarshal(body, &in); err != nil {
		return "", err
	}
	if in.Token == "" {
		return "", fmt.Errorf("missing pairing token")
	}
	row := f.store.cfg.QueryRow(`SELECT `+pairCols+
		` FROM fed_pairing WHERE direction='out' AND token=?`, in.Token)
	p, err := scanPairing(row.Scan)
	if err != nil {
		return "", fmt.Errorf("no outgoing request for this token")
	}
	if p.State != "pending" {
		return p.State, nil
	}
	// La reponse doit etre signee par la cle lue dans le profil public
	// au moment de la demande : un tiers ne peut pas repondre a sa place.
	raw, err := base64.StdEncoding.DecodeString(p.pubkey)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return "", fmt.Errorf("stored key is invalid")
	}
	if sigASN, err := normASN(h.asn); err != nil || sigASN != p.ASN {
		return "", fmt.Errorf("unexpected signing AS")
	}
	if err := f.checkSig(ed25519.PublicKey(raw), r, h, body); err != nil {
		return "", err
	}
	reply, _ := checkNote(in.ReplyNote)

	now := time.Now().Unix()
	if in.Decision == "accepted" {
		if err := f.upsertTrustedPeer(p, in.PublicListing, p.PublicListing); err != nil {
			return "", err
		}
		f.store.cfg.Exec(
			`UPDATE fed_pairing SET state='accepted', decided_at=?, reply_note=?,
			        peer_public=? WHERE id=?`,
			now, reply, b2i(in.PublicListing), p.ID)
		log.Printf("pairing accepted by %s (%s)", p.ASN, p.Org)
		return "accepted", nil
	}
	f.store.cfg.Exec(
		`UPDATE fed_pairing SET state='rejected', decided_at=?, reply_note=?
		  WHERE id=?`, now, reply, p.ID)
	return "rejected", nil
}

// upsertTrustedPeer approuve un pair. peerConsent : l'autre partie
// accepte d'etre affichee publiquement ; ourChoice : nous le souhaitons.
// L'affichage public exige les deux.
func (f *Federation) upsertTrustedPeer(p *PairingRequest, peerConsent, ourChoice bool) error {
	// Dernier verrou avant d'ecrire une cle : un pair deja approuve ne
	// change pas de cle par une nouvelle demande d'appairage.
	if err := f.refuseKeyChange(p.URL, p.ASN, p.pubkey); err != nil {
		return err
	}
	anchors, _ := json.Marshal(p.Anchors)
	now := time.Now().Unix()
	_, err := f.store.cfg.Exec(
		`INSERT INTO fed_peers(asn,org,url,pubkey,fingerprint,noc_email,anchors,
		                       state,created_at,trusted_at,peer_consent,public)
		 VALUES(?,?,?,?,?,?,?,'trusted',?,?,?,?)
		 ON CONFLICT(url) DO UPDATE SET
		   asn=excluded.asn, org=excluded.org, pubkey=excluded.pubkey,
		   fingerprint=excluded.fingerprint, noc_email=excluded.noc_email,
		   anchors=excluded.anchors, state='trusted',
		   trusted_at=COALESCE(fed_peers.trusted_at, excluded.trusted_at),
		   peer_consent=excluded.peer_consent, public=excluded.public`,
		p.ASN, p.Org, p.URL, p.pubkey, p.Fingerprint, p.NOCEmail,
		string(anchors), now, now, b2i(peerConsent), b2i(peerConsent && ourChoice))
	if err != nil {
		return err
	}
	peer, err := f.PeerByASN(p.ASN)
	if err != nil {
		return err
	}
	return f.ensureAnchorTargets(peer)
}

// SetPeerPublic permet a un administrateur de masquer un pair a tout
// moment, mais de ne l'afficher que si le pair y a consenti.
func (f *Federation) SetPeerPublic(id int64, public bool) error {
	var consent int
	if err := f.store.cfg.QueryRow(
		`SELECT peer_consent FROM fed_peers WHERE id=?`, id).Scan(&consent); err != nil {
		return fmt.Errorf("peer not found")
	}
	if public && consent == 0 {
		return fmt.Errorf("this peer has not agreed to public listing")
	}
	_, err := f.store.cfg.Exec(`UPDATE fed_peers SET public=? WHERE id=?`, b2i(public), id)
	return err
}

// ------------------------------------------------------------- routes

func (a *API) PairingRoutes(mux *http.ServeMux) {
	if a.fed == nil {
		return
	}
	mux.HandleFunc("POST /api/v1/fed/pairing/request", a.pairingInbound)
	mux.HandleFunc("POST /api/v1/fed/pairing/response", a.pairingResponse)
	mux.HandleFunc("GET /api/v1/fed/pairing/info", a.pairingInfo)

	mux.HandleFunc("GET /api/v1/admin/fed/pairing", a.need(RoleAdmin, a.pairingList))
	mux.HandleFunc("POST /api/v1/admin/fed/pairing", a.need(RoleAdmin, a.pairingCreate))
	mux.HandleFunc("POST /api/v1/admin/fed/pairing/{id}/decide",
		a.need(RoleAdmin, a.pairingDecide))
	mux.HandleFunc("PATCH /api/v1/admin/fed/peers/{id}", a.need(RoleAdmin, a.peerPatch))
}

func (a *API) pairingInfo(w http.ResponseWriter, r *http.Request) {
	p := a.fed.Profile()
	site := a.store.Site()
	var pending int
	a.store.cfg.QueryRow(
		`SELECT COUNT(*) FROM fed_pairing WHERE direction='in' AND state='pending'`).
		Scan(&pending)
	writeJSON(w, map[string]any{
		"asn": p.ASN, "org": p.Org, "url": p.URL,
		"fingerprint": p.Fprint, "anchors": p.Anchors,
		"noc_email": site.NOCEmail, "software": p.Software,
		"open": p.ASN != "", "pending_requests": pending,
		"min_justification": minJustification,
	})
}

func (a *API) pairingInbound(w http.ResponseWriter, r *http.Request) {
	p, err := a.fed.HandlePairingRequest(r)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(http.StatusAccepted)
	json.NewEncoder(w).Encode(map[string]any{
		"state": p.State, "fingerprint": a.fed.Fingerprint(),
		"note": "request recorded, waiting for human approval",
	})
}

func (a *API) pairingResponse(w http.ResponseWriter, r *http.Request) {
	state, err := a.fed.HandlePairingResponse(r)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	writeJSON(w, map[string]any{"state": state})
}

func (a *API) pairingList(w http.ResponseWriter, r *http.Request, u *User) {
	list, err := a.fed.Pairings()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	writeJSON(w, list)
}

func (a *API) pairingCreate(w http.ResponseWriter, r *http.Request, u *User) {
	var in PairingInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if in.ContactName == "" {
		in.ContactName = u.DisplayName
	}
	p, err := a.fed.RequestPairing(in)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	a.store.Audit(u, clientIP(r), "pairing_request", "peer", p.ASN)
	writeJSON(w, map[string]any{"pairing": p})
}

func (a *API) pairingDecide(w http.ResponseWriter, r *http.Request, u *User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, 400, "invalid identifier")
		return
	}
	var in DecisionInput
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := a.fed.DecidePairing(id, in, u.Email); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	action := "pairing_reject"
	if in.Accept {
		action = "pairing_accept"
	}
	a.store.Audit(u, clientIP(r), action, "pairing", r.PathValue("id"))
	writeJSON(w, map[string]any{"ok": true})
}

func (a *API) peerPatch(w http.ResponseWriter, r *http.Request, u *User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, 400, "invalid identifier")
		return
	}
	var in struct {
		Public *bool `json:"public"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if in.Public != nil {
		if err := a.fed.SetPeerPublic(id, *in.Public); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
	}
	a.store.Audit(u, clientIP(r), "peer_update", "peer", r.PathValue("id"))
	writeJSON(w, map[string]any{"ok": true})
}
