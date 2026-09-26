package main

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/smtp"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// La federation relie des instances exploitees par des operateurs
// differents. Le modele de confiance est volontairement minimal :
// chaque instance a une paire de cles Ed25519, on appaire deux
// instances en comparant les empreintes hors bande, et toutes les
// requetes inter-instances sont signees au niveau du message plutot
// qu'au niveau du transport. Signer le message plutot que d'utiliser
// du mTLS permet de passer derriere n'importe quel reverse proxy ou
// terminaison TLS sans plomberie de certificats.

const fedSchema = `
CREATE TABLE IF NOT EXISTS fed_identity (
  id      INTEGER PRIMARY KEY CHECK (id = 1),
  seed    TEXT NOT NULL,
  created INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS fed_peers (
  id           INTEGER PRIMARY KEY,
  asn          TEXT NOT NULL,
  org          TEXT NOT NULL,
  url          TEXT NOT NULL UNIQUE,
  pubkey       TEXT NOT NULL,
  fingerprint  TEXT NOT NULL,
  noc_email    TEXT,
  noc_phone    TEXT,
  anchors      TEXT NOT NULL DEFAULT '[]',
  state        TEXT NOT NULL DEFAULT 'pending',
  last_seen_at INTEGER,
  last_error   TEXT,
  created_at   INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_fed_peers_asn ON fed_peers(asn);
CREATE TABLE IF NOT EXISTS fed_reports (
  from_asn   TEXT NOT NULL,
  to_asn     TEXT NOT NULL,
  anchor     TEXT NOT NULL,
  window_end INTEGER NOT NULL,
  med_ms     REAL NOT NULL,
  p95_ms     REAL NOT NULL,
  loss_pct   REAL NOT NULL,
  PRIMARY KEY (from_asn, to_asn, anchor, window_end)
) WITHOUT ROWID;
CREATE INDEX IF NOT EXISTS idx_fed_reports_win ON fed_reports(window_end);
CREATE TABLE IF NOT EXISTS fed_incidents (
  id            TEXT PRIMARY KEY,
  opened_at     INTEGER NOT NULL,
  closed_at     INTEGER,
  observer_asn  TEXT NOT NULL,
  suspect_asn   TEXT NOT NULL,
  target        TEXT NOT NULL,
  severity      TEXT NOT NULL DEFAULT 'warning',
  detail        TEXT,
  notice_due_at INTEGER,
  notified_at   INTEGER,
  ack_at        INTEGER,
  ack_by        TEXT
);
CREATE TABLE IF NOT EXISTS fed_corroborations (
  incident_id  TEXT NOT NULL,
  observer_asn TEXT NOT NULL,
  ts           INTEGER NOT NULL,
  med_ms       REAL,
  loss_pct     REAL,
  detail       TEXT,
  PRIMARY KEY (incident_id, observer_asn)
) WITHOUT ROWID;
`

// Seuils de declenchement. Trois observateurs independants avant toute
// notification sortante, et un preavis laisse a l'AS mis en cause pour
// acquitter avant que son NOC soit sollicite.
const (
	fedMinCorroborations = 3
	fedNoticeDelay       = 5 * time.Minute
	fedSuppressWindow    = 6 * time.Hour
	fedLossThreshold     = 5.0
	fedClockSkew         = 300
)

type NotifyConfig struct {
	SMTPHost   string `json:"smtp_host"`
	SMTPPort   int    `json:"smtp_port"`
	SMTPUser   string `json:"smtp_user"`
	SMTPPass   string `json:"smtp_pass"`
	From       string `json:"from"`
	WebhookURL string `json:"webhook_url"`
	Enabled    bool   `json:"enabled"`
}

type FedProfile struct {
	ASN       string   `json:"asn"`
	Org       string   `json:"org"`
	URL       string   `json:"url"`
	PubKey    string   `json:"pubkey"`
	Fprint    string   `json:"fingerprint"`
	NOCEmail  string   `json:"noc_email"`
	NOCPhone  string   `json:"noc_phone,omitempty"`
	Anchors   []string `json:"anchors"`
	Software  string   `json:"software"`
	PublicURL string   `json:"public_url"`
}

type Peer struct {
	ID         int64    `json:"id"`
	ASN        string   `json:"asn"`
	Org        string   `json:"org"`
	URL        string   `json:"url"`
	Fprint     string   `json:"fingerprint"`
	NOCEmail   string   `json:"noc_email"`
	Anchors    []string `json:"anchors"`
	State      string   `json:"state"`
	LastSeenAt int64    `json:"last_seen_at"`
	LastError  string   `json:"last_error"`
	Public     bool     `json:"public"`
	Consent    bool     `json:"peer_consent"`
	TrustedAt  int64    `json:"trusted_at"`
	pubkey     ed25519.PublicKey
}

// Enabled reports whether this instance takes part in the federation, which
// decides whether the public pages about it are worth showing.
func (f *Federation) Enabled() bool { return f != nil && f.enabled }

type Federation struct {
	store   *Store
	dataDir string
	enabled bool
	asn     string
	org     string
	baseURL string
	anchors []string

	priv ed25519.PrivateKey
	pub  ed25519.PublicKey

	mu     sync.Mutex
	nonces map[string]int64
	client *http.Client
	asnSvc *ASNService
}

// UseASNService donne a la federation de quoi verifier, quand
// l'information est en cache, qu'une ancre appartient bien a l'AS qui la
// declare.
func (f *Federation) UseASNService(s *ASNService) {
	if f != nil {
		f.asnSvc = s
	}
}

// ------------------------------------------------------------- identite

func NewFederation(store *Store, dataDir string, cfg FedConfig) (*Federation, error) {
	if _, err := store.cfg.Exec(fedSchema); err != nil {
		return nil, fmt.Errorf("schema federation: %w", err)
	}
	f := &Federation{
		store: store, dataDir: dataDir, enabled: cfg.Enabled,
		asn: cfg.ASN, org: cfg.Org, baseURL: strings.TrimSuffix(cfg.BaseURL, "/"),
		anchors: cfg.Anchors,
		nonces:  map[string]int64{},
		client:  fedClient(20 * time.Second),
	}
	if err := f.loadKey(); err != nil {
		return nil, err
	}
	// Colonnes ajoutees apres la premiere version : l'affichage public
	// d'un pair exige le consentement des deux cotes.
	addColumn(store.cfg, "fed_peers", "public INTEGER NOT NULL DEFAULT 0")
	addColumn(store.cfg, "fed_peers", "peer_consent INTEGER NOT NULL DEFAULT 0")
	addColumn(store.cfg, "fed_peers", "trusted_at INTEGER")
	if err := f.initPairing(); err != nil {
		return nil, err
	}
	return f, nil
}

func (f *Federation) loadKey() error {
	path := filepath.Join(f.dataDir, "fed.key")
	if b, err := os.ReadFile(path); err == nil {
		seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
		if err == nil && len(seed) == ed25519.SeedSize {
			f.priv = ed25519.NewKeyFromSeed(seed)
			f.pub = f.priv.Public().(ed25519.PublicKey)
			return nil
		}
	}
	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return err
	}
	f.priv = ed25519.NewKeyFromSeed(seed)
	f.pub = f.priv.Public().(ed25519.PublicKey)
	enc := base64.StdEncoding.EncodeToString(seed)
	if err := os.WriteFile(path, []byte(enc), 0o600); err != nil {
		return err
	}
	log.Printf("federation identity created, fingerprint %s", f.Fingerprint())
	return nil
}

// Fingerprint est ce que deux operateurs comparent hors bande — sur la
// liste d'un point d'echange, par telephone, dans un canal de peering.
func (f *Federation) Fingerprint() string {
	sum := sha256.Sum256(f.pub)
	h := hex.EncodeToString(sum[:8])
	var parts []string
	for i := 0; i < len(h); i += 4 {
		parts = append(parts, h[i:i+4])
	}
	return strings.Join(parts, "-")
}

func (f *Federation) Profile() FedProfile {
	site := f.store.Site()
	asn := f.asn
	if asn == "" {
		asn = site.ASN
	}
	org := f.org
	if org == "" {
		org = site.Org
	}
	return FedProfile{
		ASN: asn, Org: org, URL: f.baseURL,
		PubKey:   base64.StdEncoding.EncodeToString(f.pub),
		Fprint:   f.Fingerprint(),
		NOCEmail: site.NOCEmail,
		Anchors:  f.anchors,
		Software: "smokestack/" + Version,
	}
}

// -------------------------------------------------------- signatures

// canonical inclut l'audience — le numero d'AS du destinataire — pour
// qu'une requete signee a l'intention d'une instance ne puisse pas etre
// rejouee telle quelle vers une autre.
func canonical(method, audience, path string, ts int64, nonce string, body []byte) []byte {
	sum := sha256.Sum256(body)
	return []byte(strings.Join([]string{
		fedSigVersion, method, audience, path, strconv.FormatInt(ts, 10), nonce,
		hex.EncodeToString(sum[:]),
	}, "\n"))
}

// fedSigVersion est dans la chaine signee : un changement de format ne
// peut pas etre confondu avec l'ancien.
const fedSigVersion = "smokestack-fed-1"

func (f *Federation) sign(req *http.Request, audience, path string, body []byte) {
	ts := time.Now().Unix()
	nb := make([]byte, 12)
	rand.Read(nb)
	nonce := hex.EncodeToString(nb)
	sig := ed25519.Sign(f.priv, canonical(req.Method, audience, path, ts, nonce, body))
	req.Header.Set("X-Fed-ASN", f.Profile().ASN)
	req.Header.Set("X-Fed-Audience", audience)
	req.Header.Set("X-Fed-Timestamp", strconv.FormatInt(ts, 10))
	req.Header.Set("X-Fed-Nonce", nonce)
	req.Header.Set("X-Fed-Signature", base64.StdEncoding.EncodeToString(sig))
	req.Header.Set("Content-Type", "application/json")
}

type sigHeaders struct {
	asn      string
	audience string
	ts       int64
	nonce    string
	sig      []byte
}

// readSigned extrait le corps et les en-tetes de signature sans encore
// savoir a quelle cle publique se referer : l'appairage a besoin de
// verifier une signature dont la cle arrive dans le corps meme.
func (f *Federation) readSigned(r *http.Request) (sigHeaders, []byte, error) {
	var h sigHeaders
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		return h, nil, err
	}
	h.asn = r.Header.Get("X-Fed-ASN")
	h.audience = r.Header.Get("X-Fed-Audience")
	h.ts, _ = strconv.ParseInt(r.Header.Get("X-Fed-Timestamp"), 10, 64)
	h.nonce = r.Header.Get("X-Fed-Nonce")
	sigB64 := r.Header.Get("X-Fed-Signature")
	if h.asn == "" || h.nonce == "" || sigB64 == "" {
		return h, body, fmt.Errorf("missing signature headers")
	}
	if len(h.nonce) > 64 {
		return h, body, fmt.Errorf("oversized nonce")
	}
	// L'audience doit etre nous. Sans cela, une requete signee vers une
	// instance est acceptee par toutes les autres.
	me, err := normASN(f.Profile().ASN)
	if err != nil {
		return h, body, fmt.Errorf("this instance has no AS number set")
	}
	aud, err := normASN(h.audience)
	if err != nil || aud != me {
		return h, body, fmt.Errorf("request not addressed to this instance")
	}
	now := time.Now().Unix()
	if h.ts < now-fedClockSkew || h.ts > now+fedClockSkew {
		return h, body, fmt.Errorf("timestamp outside tolerance")
	}
	sig, err := base64.StdEncoding.DecodeString(sigB64)
	if err != nil {
		return h, body, err
	}
	h.sig = sig
	return h, body, nil
}

func (f *Federation) checkSig(pub ed25519.PublicKey, r *http.Request,
	h sigHeaders, body []byte) error {
	if len(pub) != ed25519.PublicKeySize {
		return fmt.Errorf("missing public key")
	}
	if !ed25519.Verify(pub, canonical(r.Method, h.audience, r.URL.Path,
		h.ts, h.nonce, body), h.sig) {
		return fmt.Errorf("invalid signature")
	}
	// Le nonce n'est retenu qu'une fois la signature verifiee : sinon
	// n'importe qui remplit le cache avec des requetes non signees, et
	// peut meme « bruler » a l'avance le nonce d'un pair.
	return f.useNonce(h.asn, h.nonce, h.ts)
}

// useNonce refuse un rejeu et borne le cache. La cle inclut l'AS pour que
// deux pairs ne puissent pas s'invalider mutuellement.
func (f *Federation) useNonce(asn, nonce string, ts int64) error {
	now := time.Now().Unix()
	f.mu.Lock()
	defer f.mu.Unlock()
	for k, v := range f.nonces {
		if v < now-fedClockSkew*2 {
			delete(f.nonces, k)
		}
	}
	if len(f.nonces) >= maxNonceCache {
		// Plein malgre le nettoyage : on repart a vide plutot que de
		// grossir sans fin. La fenetre de rejeu reste bornee par
		// l'horodatage, deja verifie.
		f.nonces = map[string]int64{}
		log.Printf("federation: nonce cache full, cleared")
	}
	key := asn + "/" + nonce
	if _, seen := f.nonces[key]; seen {
		return fmt.Errorf("nonce already seen")
	}
	f.nonces[key] = ts
	return nil
}

// verify authentifie une requete entrante d'un pair deja approuve.
func (f *Federation) verify(r *http.Request) (*Peer, []byte, error) {
	h, body, err := f.readSigned(r)
	if err != nil {
		return nil, nil, err
	}
	peer, err := f.PeerByASN(h.asn)
	if err != nil {
		return nil, nil, fmt.Errorf("pair inconnu: %s", h.asn)
	}
	if peer.State != "trusted" {
		return nil, nil, fmt.Errorf("peer not approved")
	}
	if err := f.checkSig(peer.pubkey, r, h, body); err != nil {
		return nil, nil, err
	}
	return peer, body, nil
}

// ------------------------------------------------------------- pairs

func scanPeer(scan func(...any) error) (*Peer, error) {
	p := &Peer{}
	var anchors, pubkey string
	var lastSeen, trustedAt *int64
	var lastErr *string
	var pub, consent int
	if err := scan(&p.ID, &p.ASN, &p.Org, &p.URL, &pubkey, &p.Fprint,
		&p.NOCEmail, &anchors, &p.State, &lastSeen, &lastErr,
		&pub, &consent, &trustedAt); err != nil {
		return nil, err
	}
	if lastSeen != nil {
		p.LastSeenAt = *lastSeen
	}
	if trustedAt != nil {
		p.TrustedAt = *trustedAt
	}
	if lastErr != nil {
		p.LastError = *lastErr
	}
	p.Public, p.Consent = pub == 1, consent == 1
	json.Unmarshal([]byte(anchors), &p.Anchors)
	if raw, err := base64.StdEncoding.DecodeString(pubkey); err == nil &&
		len(raw) == ed25519.PublicKeySize {
		p.pubkey = ed25519.PublicKey(raw)
	}
	return p, nil
}

const peerCols = `id,asn,org,url,pubkey,fingerprint,COALESCE(noc_email,''),
                  anchors,state,last_seen_at,last_error,
                  public,peer_consent,trusted_at`

func (f *Federation) PeerByASN(asn string) (*Peer, error) {
	row := f.store.cfg.QueryRow(
		`SELECT `+peerCols+` FROM fed_peers WHERE asn=? ORDER BY id LIMIT 1`, asn)
	return scanPeer(row.Scan)
}

func (f *Federation) Peers() ([]*Peer, error) {
	rows, err := f.store.cfg.Query(`SELECT ` + peerCols + ` FROM fed_peers ORDER BY asn`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*Peer{}
	for rows.Next() {
		p, err := scanPeer(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (f *Federation) trustedPeers() []*Peer {
	all, err := f.Peers()
	if err != nil {
		return nil
	}
	var out []*Peer
	for _, p := range all {
		if p.State == "trusted" && p.pubkey != nil {
			out = append(out, p)
		}
	}
	return out
}

// AddPeer va chercher le profil d'une instance distante et l'enregistre
// en attente. L'operateur compare ensuite l'empreinte retournee avec
// celle que son homologue lui a communiquee, puis approuve.
func (f *Federation) AddPeer(rawURL string) (*Peer, error) {
	url, err := safeFedURL(rawURL)
	if err != nil {
		return nil, err
	}
	resp, err := f.client.Get(url + "/api/v1/fed/profile")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("profil distant: HTTP %d", resp.StatusCode)
	}
	var prof FedProfile
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<18)).Decode(&prof); err != nil {
		return nil, err
	}
	if prof.ASN == "" || prof.PubKey == "" {
		return nil, fmt.Errorf("incomplete remote profile (missing AS or key)")
	}
	asn, err := normASN(prof.ASN)
	if err != nil {
		return nil, err
	}
	prof.ASN = asn
	raw, err := base64.StdEncoding.DecodeString(prof.PubKey)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return nil, fmt.Errorf("invalid remote public key")
	}
	if err := f.refuseKeyChange(url, prof.ASN, prof.PubKey); err != nil {
		return nil, err
	}
	prof.Anchors, _ = cleanAnchors(prof.Anchors)
	prof.Org = oneLine(prof.Org, 120)
	prof.NOCEmail = oneLine(prof.NOCEmail, 200)
	sum := sha256.Sum256(raw)
	h := hex.EncodeToString(sum[:8])
	var parts []string
	for i := 0; i < len(h); i += 4 {
		parts = append(parts, h[i:i+4])
	}
	fprint := strings.Join(parts, "-")

	anchors, _ := json.Marshal(prof.Anchors)
	_, err = f.store.cfg.Exec(
		`INSERT INTO fed_peers(asn,org,url,pubkey,fingerprint,noc_email,anchors,
		                       state,created_at)
		 VALUES(?,?,?,?,?,?,?,'pending',?)
		 ON CONFLICT(url) DO UPDATE SET
		   asn=excluded.asn, org=excluded.org, pubkey=excluded.pubkey,
		   fingerprint=excluded.fingerprint, noc_email=excluded.noc_email,
		   anchors=excluded.anchors`,
		prof.ASN, prof.Org, url, prof.PubKey, fprint, prof.NOCEmail,
		string(anchors), time.Now().Unix())
	if err != nil {
		return nil, err
	}
	row := f.store.cfg.QueryRow(`SELECT `+peerCols+` FROM fed_peers WHERE url=?`, url)
	return scanPeer(row.Scan)
}

// refuseKeyChange interdit qu'une demande entrante — ou une relecture de
// profil — remplace la cle d'un pair deja approuve. Sans ce garde-fou,
// accepter une fausse « nouvelle demande » d'un pair connu donne sa place
// a l'attaquant, en conservant l'etat trusted. Le changement de cle reste
// possible, mais par une action explicite ou l'operateur saisit la
// nouvelle empreinte.
func (f *Federation) refuseKeyChange(url, asn, pubkey string) error {
	rows, err := f.store.cfg.Query(
		`SELECT url,asn,pubkey,fingerprint,state FROM fed_peers
		  WHERE url=? OR asn=?`, url, asn)
	if err != nil {
		return nil
	}
	defer rows.Close()
	for rows.Next() {
		var u, a, pk, fp, st string
		if rows.Scan(&u, &a, &pk, &fp, &st) != nil {
			continue
		}
		if st != "trusted" || pk == pubkey {
			continue
		}
		return fmt.Errorf("%s is already paired with a different key "+
			"(fingerprint %s): if that instance really rotated its key, "+
			"rotate it here explicitly", a, fp)
	}
	return rows.Err()
}

// RotatePeerKey remplace la cle d'un pair approuve, en exigeant que
// l'operateur saisisse l'empreinte lue hors bande. C'est le seul chemin
// vers un changement de cle.
func (f *Federation) RotatePeerKey(id int64, confirm string) error {
	row := f.store.cfg.QueryRow(`SELECT `+peerCols+` FROM fed_peers WHERE id=?`, id)
	p, err := scanPeer(row.Scan)
	if err != nil {
		return fmt.Errorf("peer not found")
	}
	resp, err := f.client.Get(p.URL + "/api/v1/fed/profile")
	if err != nil {
		return fmt.Errorf("instance unreachable: %w", err)
	}
	defer resp.Body.Close()
	var prof FedProfile
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<18)).Decode(&prof); err != nil {
		return err
	}
	raw, err := base64.StdEncoding.DecodeString(prof.PubKey)
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return fmt.Errorf("invalid remote public key")
	}
	fp := fingerprintOf(raw)
	if !sameFingerprint(confirm, fp) {
		return fmt.Errorf("the fingerprint you entered does not match the one "+
			"this instance now publishes (%s)", fp)
	}
	_, err = f.store.cfg.Exec(
		`UPDATE fed_peers SET pubkey=?, fingerprint=? WHERE id=?`,
		prof.PubKey, fp, id)
	if err == nil {
		log.Printf("federation: key of %s rotated to fingerprint %s", p.ASN, fp)
	}
	return err
}

// sameFingerprint compare deux empreintes sans se soucier des tirets ni
// de la casse : l'operateur les recopie a la main.
func sameFingerprint(a, b string) bool {
	clean := func(s string) string {
		return strings.ToLower(strings.NewReplacer("-", "", " ", "", ":", "").Replace(
			strings.TrimSpace(s)))
	}
	ca, cb := clean(a), clean(b)
	return ca != "" && ca == cb
}

// TrustPeer approuve un pair et cree les cibles de mesure vers ses
// ancres : rejoindre un pair suffit a commencer a le mesurer.
func (f *Federation) TrustPeer(id int64) error {
	if _, err := f.store.cfg.Exec(
		`UPDATE fed_peers SET state='trusted',
		        trusted_at=COALESCE(trusted_at,?) WHERE id=?`,
		time.Now().Unix(), id); err != nil {
		return err
	}
	row := f.store.cfg.QueryRow(`SELECT `+peerCols+` FROM fed_peers WHERE id=?`, id)
	p, err := scanPeer(row.Scan)
	if err != nil {
		return err
	}
	return f.ensureAnchorTargets(p)
}

func (f *Federation) ensureAnchorTargets(p *Peer) error {
	var catID int64
	err := f.store.cfg.QueryRow(
		`SELECT id FROM categories WHERE slug='federation'`).Scan(&catID)
	if err != nil {
		catID, err = f.store.CreateCategory("federation", "Fédération", "Federation", true)
		if err != nil {
			return err
		}
	}
	// Une ancre est une adresse que toute la federation va pinguer : on
	// n'accepte que des adresses publiques, en nombre borne, et on
	// signale celles que le pair ne semble pas annoncer lui-meme.
	anchors, rejected := cleanAnchors(p.Anchors)
	for _, bad := range rejected {
		log.Printf("federation: anchor %q announced by %s refused "+
			"(not a public address, or too many)", bad, p.ASN)
	}
	for _, anchor := range anchors {
		if own, known := anchorBelongsTo(f.asnSvc, anchor, p.ASN); known && !own {
			log.Printf("federation: anchor %s announced by %s is not in that AS; "+
				"measured anyway but worth checking", anchor, p.ASN)
		}
		t := &Target{
			CategoryID: catID,
			Slug:       "fed-" + strings.ToLower(p.ASN) + "-" + anchor,
			Title:      p.Org + " (" + p.ASN + ")",
			Host:       anchor, Proto: "icmp", IntervalS: 60, Packets: 20,
			SpacingMs: 500, TimeoutMs: 2000, Public: true, Enabled: true,
		}
		if _, err := f.store.CreateTarget(t); err != nil &&
			!strings.Contains(err.Error(), "UNIQUE") {
			log.Printf("anchor target %s: %v", anchor, err)
		}
	}
	return nil
}

// ------------------------------------------------- echange de matrice

type anchorReport struct {
	FromASN string `json:"from_asn"`
	ToASN   string `json:"to_asn"`
	Anchor  string `json:"anchor"`
	WinEnd  int64  `json:"window_end"`
	MedMs   float64
	P95Ms   float64
	LossPct float64
}

func (r anchorReport) MarshalJSON() ([]byte, error) {
	return json.Marshal(map[string]any{
		"from_asn": r.FromASN, "to_asn": r.ToASN, "anchor": r.Anchor,
		"window_end": r.WinEnd, "med_ms": r.MedMs,
		"p95_ms": r.P95Ms, "loss_pct": r.LossPct,
	})
}

type reportBatch struct {
	Reports []struct {
		FromASN string  `json:"from_asn"`
		ToASN   string  `json:"to_asn"`
		Anchor  string  `json:"anchor"`
		WinEnd  int64   `json:"window_end"`
		MedMs   float64 `json:"med_ms"`
		P95Ms   float64 `json:"p95_ms"`
		LossPct float64 `json:"loss_pct"`
	} `json:"reports"`
}

// localReports agrege nos mesures vers les ancres de chaque pair sur la
// derniere fenetre de cinq minutes.
func (f *Federation) localReports(win int64) []anchorReport {
	var out []anchorReport
	me := f.Profile().ASN
	for _, p := range f.trustedPeers() {
		for _, anchor := range p.Anchors {
			var tid int64
			slug := "fed-" + strings.ToLower(p.ASN) + "-" + anchor
			if err := f.store.cfg.QueryRow(
				`SELECT id FROM targets WHERE slug=?`, slug).Scan(&tid); err != nil {
				continue
			}
			var sent, lost, cnt int64
			var sum float64
			err := f.store.mx.QueryRow(
				`SELECT COALESCE(SUM(sent),0),COALESCE(SUM(lost),0),
				        COALESCE(SUM(cnt),0),COALESCE(SUM(sum_us),0)
				   FROM samples WHERE target_id=? AND bucket>?`,
				tid, win-300).Scan(&sent, &lost, &cnt, &sum)
			if err != nil || sent == 0 {
				continue
			}
			r := anchorReport{FromASN: me, ToASN: p.ASN, Anchor: anchor, WinEnd: win}
			if cnt > 0 {
				r.MedMs = sum / float64(cnt) / 1000
				r.P95Ms = r.MedMs * 1.25
			}
			r.LossPct = float64(lost) * 100 / float64(sent)
			out = append(out, r)
		}
	}
	return out
}

func (f *Federation) postSigned(peer *Peer, path string, payload any) error {
	body, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	req, err := http.NewRequest("POST", peer.URL+path, bytes.NewReader(body))
	if err != nil {
		return err
	}
	f.sign(req, peer.ASN, path, body)
	resp, err := f.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	if resp.StatusCode >= 300 {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	return nil
}

func (f *Federation) pushReports() {
	win := time.Now().Unix() / 300 * 300
	reports := f.localReports(win)
	if len(reports) == 0 {
		return
	}
	f.storeReports(reports)
	for _, p := range f.trustedPeers() {
		err := f.postSigned(p, "/api/v1/fed/report",
			map[string]any{"reports": reports})
		if err != nil {
			f.store.cfg.Exec(`UPDATE fed_peers SET last_error=? WHERE id=?`,
				err.Error(), p.ID)
			continue
		}
		f.store.cfg.Exec(
			`UPDATE fed_peers SET last_seen_at=?, last_error=NULL WHERE id=?`,
			time.Now().Unix(), p.ID)
	}
}

// acceptReports filtre ce qu'un pair nous declare avant tout
// enregistrement : il ne peut parler que de ses propres mesures, vers un
// membre connu, sur une fenetre qui n'est pas dans le futur, avec des
// valeurs plausibles.
func (f *Federation) acceptReports(from string, in []anchorReport) []anchorReport {
	now := time.Now().Unix()
	known := map[string]bool{f.Profile().ASN: true}
	for _, p := range f.trustedPeers() {
		known[p.ASN] = true
	}
	var out []anchorReport
	for i, r := range in {
		if i >= maxReports {
			break
		}
		fromASN, err := normASN(r.FromASN)
		if err != nil || fromASN != from {
			continue // un pair ne declare que ses propres mesures
		}
		toASN, err := normASN(r.ToASN)
		if err != nil || !known[toASN] {
			continue
		}
		anchor, err := checkAnchor(r.Anchor)
		if err != nil {
			continue
		}
		win, err := clampWindow(r.WinEnd, now)
		if err != nil {
			continue
		}
		out = append(out, anchorReport{
			FromASN: fromASN, ToASN: toASN, Anchor: anchor, WinEnd: win,
			MedMs:   clampFloat(r.MedMs, 0, 60000),
			P95Ms:   clampFloat(r.P95Ms, 0, 60000),
			LossPct: clampFloat(r.LossPct, 0, 100),
		})
	}
	return out
}

func (f *Federation) storeReports(reports []anchorReport) {
	for _, r := range reports {
		f.store.cfg.Exec(
			`INSERT INTO fed_reports(from_asn,to_asn,anchor,window_end,
			                         med_ms,p95_ms,loss_pct)
			 VALUES(?,?,?,?,?,?,?)
			 ON CONFLICT(from_asn,to_asn,anchor,window_end) DO UPDATE SET
			   med_ms=excluded.med_ms, p95_ms=excluded.p95_ms,
			   loss_pct=excluded.loss_pct`,
			r.FromASN, r.ToASN, r.Anchor, r.WinEnd, r.MedMs, r.P95Ms, r.LossPct)
	}
}

// ---------------------------------------------------------- incidents

type Incident struct {
	ID           string  `json:"id"`
	OpenedAt     int64   `json:"opened_at"`
	ClosedAt     *int64  `json:"closed_at"`
	ObserverASN  string  `json:"observer_asn"`
	SuspectASN   string  `json:"suspect_asn"`
	Target       string  `json:"target"`
	Severity     string  `json:"severity"`
	Detail       string  `json:"detail"`
	NoticeDueAt  *int64  `json:"notice_due_at"`
	NotifiedAt   *int64  `json:"notified_at"`
	AckAt        *int64  `json:"ack_at"`
	AckBy        string  `json:"ack_by"`
	Corroborated int     `json:"corroborated"`
	MedMs        float64 `json:"med_ms,omitempty"`
	LossPct      float64 `json:"loss_pct,omitempty"`
}

func newIncidentID() string {
	b := make([]byte, 9)
	rand.Read(b)
	return time.Now().UTC().Format("20060102T1504") + "-" + hex.EncodeToString(b)
}

// detect ouvre un incident quand une ancre federee se degrade de facon
// soutenue, puis le diffuse aux pairs pour corroboration.
func (f *Federation) detect() {
	now := time.Now().Unix()
	me := f.Profile().ASN
	for _, p := range f.trustedPeers() {
		for _, anchor := range p.Anchors {
			var tid int64
			slug := "fed-" + strings.ToLower(p.ASN) + "-" + anchor
			if err := f.store.cfg.QueryRow(
				`SELECT id FROM targets WHERE slug=?`, slug).Scan(&tid); err != nil {
				continue
			}
			var sent, lost, cnt int64
			var sum float64
			f.store.mx.QueryRow(
				`SELECT COALESCE(SUM(sent),0),COALESCE(SUM(lost),0),
				        COALESCE(SUM(cnt),0),COALESCE(SUM(sum_us),0)
				   FROM samples WHERE target_id=? AND bucket>?`,
				tid, now-600).Scan(&sent, &lost, &cnt, &sum)
			if sent < 40 {
				continue
			}
			loss := float64(lost) * 100 / float64(sent)
			if loss < fedLossThreshold {
				continue
			}
			var open int
			f.store.cfg.QueryRow(
				`SELECT COUNT(*) FROM fed_incidents
				  WHERE suspect_asn=? AND target=? AND closed_at IS NULL
				    AND opened_at > ?`,
				p.ASN, anchor, now-int64(fedSuppressWindow.Seconds())).Scan(&open)
			if open > 0 {
				continue
			}
			med := 0.0
			if cnt > 0 {
				med = sum / float64(cnt) / 1000
			}
			inc := Incident{
				ID: newIncidentID(), OpenedAt: now, ObserverASN: me,
				SuspectASN: p.ASN, Target: anchor, Severity: "warning",
				Detail: fmt.Sprintf("loss %.1f%% over 10 min, median %.2f ms", loss, med),
				MedMs:  med, LossPct: loss,
			}
			due := now + int64(fedNoticeDelay.Seconds())
			inc.NoticeDueAt = &due
			f.saveIncident(inc)
			f.addCorroboration(inc.ID, me, med, loss, inc.Detail)
			f.broadcastIncident(inc)
			log.Printf("incident %s opened towards %s (%s)", inc.ID, p.ASN, anchor)
		}
	}
}

func (f *Federation) saveIncident(i Incident) {
	f.store.cfg.Exec(
		`INSERT INTO fed_incidents(id,opened_at,observer_asn,suspect_asn,target,
		                           severity,detail,notice_due_at)
		 VALUES(?,?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO NOTHING`,
		i.ID, i.OpenedAt, i.ObserverASN, i.SuspectASN, i.Target,
		i.Severity, i.Detail, i.NoticeDueAt)
}

func (f *Federation) addCorroboration(id, asn string, med, loss float64, detail string) {
	f.store.cfg.Exec(
		`INSERT INTO fed_corroborations(incident_id,observer_asn,ts,med_ms,loss_pct,detail)
		 VALUES(?,?,?,?,?,?)
		 ON CONFLICT(incident_id,observer_asn) DO UPDATE SET
		   ts=excluded.ts, med_ms=excluded.med_ms, loss_pct=excluded.loss_pct`,
		id, asn, time.Now().Unix(), med, loss, detail)
}

func (f *Federation) broadcastIncident(i Incident) {
	for _, p := range f.trustedPeers() {
		if err := f.postSigned(p, "/api/v1/fed/incident", i); err != nil {
			log.Printf("sending incident to %s: %v", p.ASN, err)
		}
	}
}

func (f *Federation) Incidents(limit int) ([]Incident, error) {
	rows, err := f.store.cfg.Query(
		`SELECT i.id,i.opened_at,i.closed_at,i.observer_asn,i.suspect_asn,
		        i.target,i.severity,COALESCE(i.detail,''),i.notice_due_at,
		        i.notified_at,i.ack_at,COALESCE(i.ack_by,''),
		        (SELECT COUNT(*) FROM fed_corroborations c WHERE c.incident_id=i.id),
		        COALESCE((SELECT c.med_ms FROM fed_corroborations c
		                   WHERE c.incident_id=i.id AND c.observer_asn=i.observer_asn),0),
		        COALESCE((SELECT c.loss_pct FROM fed_corroborations c
		                   WHERE c.incident_id=i.id AND c.observer_asn=i.observer_asn),0)
		   FROM fed_incidents i ORDER BY i.opened_at DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Incident{}
	for rows.Next() {
		var i Incident
		if err := rows.Scan(&i.ID, &i.OpenedAt, &i.ClosedAt, &i.ObserverASN,
			&i.SuspectASN, &i.Target, &i.Severity, &i.Detail, &i.NoticeDueAt,
			&i.NotifiedAt, &i.AckAt, &i.AckBy, &i.Corroborated,
			&i.MedMs, &i.LossPct); err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// ------------------------------------------------------ alerting NOC

// notifyDue envoie les notifications dont le preavis est ecoule, si et
// seulement si le nombre d'observateurs independants atteint le seuil
// et que l'AS mis en cause n'a pas acquitte entre-temps.
func (f *Federation) notifyDue() {
	now := time.Now().Unix()
	incs, err := f.Incidents(50)
	if err != nil {
		return
	}
	for _, i := range incs {
		if i.NotifiedAt != nil || i.AckAt != nil || i.ClosedAt != nil {
			continue
		}
		if i.NoticeDueAt == nil || *i.NoticeDueAt > now {
			continue
		}
		if i.Corroborated < fedMinCorroborations {
			continue
		}
		// Nous ne sollicitons jamais un NOC sur la seule parole d'autres
		// instances : il faut que nos propres mesures corroborent. Sans
		// cette condition, des pairs complices suffisent a faire partir un
		// courriel depuis notre SMTP.
		if !f.weCorroborate(i.ID) {
			continue
		}
		// On ne notifie que les membres, qui ont consenti en rejoignant.
		peer, err := f.PeerByASN(i.SuspectASN)
		if err != nil || peer.State != "trusted" || peer.NOCEmail == "" {
			continue
		}
		if err := f.sendNOC(peer, i); err != nil {
			log.Printf("notification NOC %s: %v", peer.ASN, err)
			continue
		}
		f.store.cfg.Exec(`UPDATE fed_incidents SET notified_at=? WHERE id=?`, now, i.ID)
		log.Printf("NOC %s notified for incident %s", peer.ASN, i.ID)
	}
}

// weCorroborate dit si cette instance a elle-meme observe l'incident.
func (f *Federation) weCorroborate(id string) bool {
	var n int
	f.store.cfg.QueryRow(
		`SELECT COUNT(*) FROM fed_corroborations
		  WHERE incident_id=? AND observer_asn=?`, id, f.Profile().ASN).Scan(&n)
	return n > 0
}

func (f *Federation) notifyConfig() NotifyConfig {
	c := NotifyConfig{SMTPPort: 25}
	if raw := f.store.Setting("notify", ""); raw != "" {
		json.Unmarshal([]byte(raw), &c)
	}
	return c
}

func (f *Federation) sendNOC(peer *Peer, i Incident) error {
	cfg := f.notifyConfig()
	if !cfg.Enabled {
		return fmt.Errorf("notifications disabled")
	}
	me := f.Profile()
	rows, _ := f.store.cfg.Query(
		`SELECT observer_asn, COALESCE(loss_pct,0), COALESCE(med_ms,0)
		   FROM fed_corroborations WHERE incident_id=? ORDER BY observer_asn`, i.ID)
	var obs []string
	if rows != nil {
		for rows.Next() {
			var asn string
			var loss, med float64
			if rows.Scan(&asn, &loss, &med) == nil {
				if a, err := normASN(asn); err == nil {
					obs = append(obs, fmt.Sprintf("  %-10s loss %5.1f%%  median %6.2f ms",
						a, clampFloat(loss, 0, 100), clampFloat(med, 0, 60000)))
				}
			}
		}
		rows.Close()
	}

	body := fmt.Sprintf(`Hello,

Corroborated observation of a degradation towards %s (%s), reported by
the measurement network your AS takes part in.

Target           : %s
Opened at        : %s UTC
Observers        : %d independent networks
Initial finding  : %s

Per observer:
%s

This is an automated observation, not a diagnosis. Return paths are not
visible from our measurement points.

Full record      : %s/#incident=%s
Sender           : %s (%s) - %s

To acknowledge or report ongoing maintenance, your instance can answer on
/api/v1/fed/ack, or simply reply to this message.
`,
		oneLine(peer.ASN, 24), oneLine(peer.Org, 120), oneLine(i.Target, 120),
		time.Unix(i.OpenedAt, 0).UTC().Format("2006-01-02 15:04"),
		i.Corroborated, fedText(i.Detail), strings.Join(obs, "\n"),
		me.URL, i.ID, oneLine(me.Org, 120), oneLine(me.ASN, 24),
		oneLine(me.NOCEmail, 200))

	// Le sujet contient des valeurs venues d'un pair : un retour a la
	// ligne y ajouterait des en-tetes.
	subject := mailHeader(fmt.Sprintf("[smokestack] Degradation observed towards %s - %s",
		peer.ASN, i.Target))

	if cfg.WebhookURL != "" {
		payload, _ := json.Marshal(map[string]any{
			"incident": i, "suspect": peer.ASN, "subject": subject, "body": body,
		})
		req, err := http.NewRequest("POST", cfg.WebhookURL, bytes.NewReader(payload))
		if err == nil {
			req.Header.Set("Content-Type", "application/json")
			if resp, err := f.client.Do(req); err == nil {
				resp.Body.Close()
			}
		}
	}

	if cfg.SMTPHost == "" || cfg.From == "" {
		return nil
	}
	from, err := mailAddress(cfg.From)
	if err != nil {
		return fmt.Errorf("sender address: %w", err)
	}
	to, err := mailAddress(peer.NOCEmail)
	if err != nil {
		return fmt.Errorf("NOC address of %s: %w", peer.ASN, err)
	}
	msg := []byte(fmt.Sprintf("From: %s\r\nTo: %s\r\nSubject: %s\r\n"+
		"MIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n%s",
		from, to, subject, body))
	addr := fmt.Sprintf("%s:%d", cfg.SMTPHost, cfg.SMTPPort)
	var auth smtp.Auth
	if cfg.SMTPUser != "" {
		auth = smtp.PlainAuth("", cfg.SMTPUser, cfg.SMTPPass, cfg.SMTPHost)
	}
	return smtp.SendMail(addr, auth, from, []string{to}, msg)
}

// ------------------------------------------------------------- boucle

func (f *Federation) Loop(stop <-chan struct{}) {
	if !f.enabled {
		return
	}
	push := time.NewTicker(5 * time.Minute)
	watch := time.NewTicker(time.Minute)
	defer push.Stop()
	defer watch.Stop()
	log.Printf("federation enabled, fingerprint %s", f.Fingerprint())
	for {
		select {
		case <-stop:
			return
		case <-push.C:
			f.pushReports()
		case <-watch.C:
			f.detect()
			f.notifyDue()
		}
	}
}
