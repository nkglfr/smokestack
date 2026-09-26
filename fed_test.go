package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Deux instances de federation pretes a se parler, chacune avec sa propre
// cle et son propre AS.
func newFed(t *testing.T, asn, org, base string) *Federation {
	t.Helper()
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(store.Close)
	f, err := NewFederation(store, t.TempDir(), FedConfig{
		Enabled: true, ASN: asn, Org: org, BaseURL: base,
		Anchors: []string{"192.0.2.10"},
	})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

// signedFactory rend une fonction qui refabrique a l'identique la meme
// requete signee : de quoi tester un rejeu, vers nous ou vers un autre.
func signedFactory(priv ed25519.PrivateKey, asn, audience, method, path string,
	payload any) func() *http.Request {
	body, _ := json.Marshal(payload)
	ts := time.Now().Unix()
	nonce := randomHex(12)
	sig := base64.StdEncoding.EncodeToString(
		ed25519.Sign(priv, canonical(method, audience, path, ts, nonce, body)))
	return func() *http.Request {
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		req.Header.Set("X-Fed-ASN", asn)
		req.Header.Set("X-Fed-Audience", audience)
		req.Header.Set("X-Fed-Timestamp", fmt.Sprint(ts))
		req.Header.Set("X-Fed-Nonce", nonce)
		req.Header.Set("X-Fed-Signature", sig)
		return req
	}
}

// signedReq fabrique une requete signee comme un pair le ferait, avec la
// cle donnee, a destination de l'audience donnee.
func signedReq(priv ed25519.PrivateKey, asn, audience, method, path string,
	payload any) *http.Request {
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	ts := time.Now().Unix()
	nonce := randomHex(12)
	sig := ed25519.Sign(priv, canonical(method, audience, path, ts, nonce, body))
	req.Header.Set("X-Fed-ASN", asn)
	req.Header.Set("X-Fed-Audience", audience)
	req.Header.Set("X-Fed-Timestamp", fmt.Sprint(ts))
	req.Header.Set("X-Fed-Nonce", nonce)
	req.Header.Set("X-Fed-Signature", base64.StdEncoding.EncodeToString(sig))
	return req
}

// profileServer sert un profil de federation, comme une vraie instance.
func profileServer(t *testing.T, prof FedProfile) *httptest.Server {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/fed/profile" {
			http.NotFound(w, r)
			return
		}
		json.NewEncoder(w).Encode(prof)
	}))
	t.Cleanup(s.Close)
	return s
}

// ------------------------------------------------------ usurpation d'AS

// Une demande d'appairage signee avec la cle qu'elle transporte ne suffit
// pas : l'instance situee a l'URL annoncee doit publier cette meme cle et
// ce meme AS. Sans cela, n'importe qui se presente comme AS3215.
func TestPairingRefusesUnbackedIdentity(t *testing.T) {
	victim := newFed(t, "AS64500", "Victim", "https://victim.example.net")

	// Un attaquant fabrique sa propre cle et annonce l'URL d'un tiers, qui
	// publie une autre cle et un autre AS.
	attackerPub, attackerPriv, _ := ed25519.GenerateKey(nil)
	realPub, _, _ := ed25519.GenerateKey(nil)
	real := profileServer(t, FedProfile{
		ASN:    "AS3215",
		PubKey: base64.StdEncoding.EncodeToString(realPub),
	})
	// Le client de la federation refuse les adresses privees, et un
	// serveur de test en est une : on ouvre les deux garde-fous pour ce
	// test, qui porte sur la liaison identite / URL.
	victim.client = &http.Client{Timeout: 5 * time.Second}
	fedAllowPrivateForTests = true
	t.Cleanup(func() { fedAllowPrivateForTests = false })

	payload := pairingPayload{
		Token: randomHex(8), ASN: "AS3215", Org: "Orange", URL: real.URL,
		PubKey:        base64.StdEncoding.EncodeToString(attackerPub),
		Justification: strings.Repeat("we peer at France-IX ", 3),
	}
	req := signedReq(attackerPriv, "AS3215", "AS64500", "POST",
		"/api/v1/fed/pairing/request", payload)
	if _, err := victim.HandlePairingRequest(req); err == nil {
		t.Fatal("a request whose announced instance publishes another key must be refused")
	} else if !strings.Contains(err.Error(), "does not publish the key") {
		t.Fatalf("unexpected reason: %v", err)
	}

	// Meme URL, mais qui publie bien la cle de l'attaquant et son propre
	// AS : la demande est enregistree, en attente d'un humain.
	ok := profileServer(t, FedProfile{
		ASN:    "AS64999",
		PubKey: base64.StdEncoding.EncodeToString(attackerPub),
	})
	payload.ASN, payload.URL = "AS64999", ok.URL
	req = signedReq(attackerPriv, "AS64999", "AS64500", "POST",
		"/api/v1/fed/pairing/request", payload)
	p, err := victim.HandlePairingRequest(req)
	if err != nil {
		t.Fatalf("a consistent request must be accepted as pending: %v", err)
	}
	if p.State != "pending" {
		t.Errorf("state = %q, expected pending", p.State)
	}
}

// Accepter exige que l'operateur saisisse l'empreinte lue hors bande.
func TestPairingAcceptNeedsFingerprint(t *testing.T) {
	f := newFed(t, "AS64500", "Us", "https://us.example.net")
	pub, _, _ := ed25519.GenerateKey(nil)
	res, err := f.store.cfg.Exec(
		`INSERT INTO fed_pairing(direction,token,asn,org,url,pubkey,fingerprint,
		                         anchors,justification,state,created_at)
		 VALUES('in','tok','AS64999','Them','https://them.example.net',?,?,'[]',
		        'a justification long enough','pending',?)`,
		base64.StdEncoding.EncodeToString(pub), fingerprintOf(pub), time.Now().Unix())
	if err != nil {
		t.Fatal(err)
	}
	id, _ := res.LastInsertId()

	if err := f.DecidePairing(id, DecisionInput{Accept: true}, "a@b.c"); err == nil {
		t.Fatal("accepting without the fingerprint must be refused")
	}
	if err := f.DecidePairing(id, DecisionInput{Accept: true,
		Fingerprint: "0000-0000-0000-0000"}, "a@b.c"); err == nil {
		t.Fatal("a wrong fingerprint must be refused")
	}
	// La bonne empreinte, recopiee sans les tirets, passe.
	fp := strings.ReplaceAll(fingerprintOf(pub), "-", "")
	if err := f.DecidePairing(id, DecisionInput{Accept: true, Fingerprint: fp},
		"a@b.c"); err != nil {
		t.Fatalf("the right fingerprint must be accepted: %v", err)
	}
	peer, err := f.PeerByASN("AS64999")
	if err != nil || peer.State != "trusted" {
		t.Fatalf("the peer should now be trusted: %v", err)
	}
}

// ------------------------------------------- remplacement de cle d'un pair

// Une « nouvelle demande » d'un pair deja approuve ne remplace pas sa
// cle : sinon l'attaquant prend sa place en conservant l'etat trusted.
func TestTrustedPeerKeyCannotBeReplaced(t *testing.T) {
	f := newFed(t, "AS64500", "Us", "https://us.example.net")
	good, _, _ := ed25519.GenerateKey(nil)
	url := "https://them.example.net"
	f.store.cfg.Exec(
		`INSERT INTO fed_peers(asn,org,url,pubkey,fingerprint,anchors,state,created_at)
		 VALUES('AS64999','Them',?,?,?,'[]','trusted',?)`,
		url, base64.StdEncoding.EncodeToString(good), fingerprintOf(good),
		time.Now().Unix())

	evil, _, _ := ed25519.GenerateKey(nil)
	p := &PairingRequest{ASN: "AS64999", Org: "Them", URL: url,
		pubkey: base64.StdEncoding.EncodeToString(evil), Fingerprint: fingerprintOf(evil)}
	if err := f.upsertTrustedPeer(p, true, true); err == nil {
		t.Fatal("replacing the key of a trusted peer must be refused")
	}
	// La cle en base n'a pas bouge.
	var stored string
	f.store.cfg.QueryRow(`SELECT pubkey FROM fed_peers WHERE url=?`, url).Scan(&stored)
	if stored != base64.StdEncoding.EncodeToString(good) {
		t.Error("the stored key was modified")
	}
	// Un nouvel appairage avec la meme cle reste possible.
	p.pubkey = base64.StdEncoding.EncodeToString(good)
	if err := f.upsertTrustedPeer(p, true, true); err != nil {
		t.Errorf("the same key must still be accepted: %v", err)
	}
}

// -------------------------------------------------- signature et rejeu

// Une requete signee a l'intention d'une instance ne doit pas etre
// acceptee par une autre, et un nonce ne sert qu'une fois.
func TestSignatureAudienceAndReplay(t *testing.T) {
	a := newFed(t, "AS64500", "A", "https://a.example.net")
	b := newFed(t, "AS64501", "B", "https://b.example.net")
	pub, priv, _ := ed25519.GenerateKey(nil)
	for _, f := range []*Federation{a, b} {
		f.store.cfg.Exec(
			`INSERT INTO fed_peers(asn,org,url,pubkey,fingerprint,anchors,state,created_at)
			 VALUES('AS64999','Them','https://them.example.net',?,'x','[]','trusted',?)`,
			base64.StdEncoding.EncodeToString(pub), time.Now().Unix())
	}

	mk := signedFactory(priv, "AS64999", "AS64500", "POST", "/api/v1/fed/report",
		map[string]any{"reports": []any{}})
	// Rejouee vers B, la meme requete est refusee.
	if _, _, err := b.verify(mk()); err == nil {
		t.Error("a request addressed to AS64500 must not be accepted by AS64501")
	}
	if _, _, err := a.verify(mk()); err != nil {
		t.Fatalf("the right recipient must accept it: %v", err)
	}
	// Rejouee vers A, elle est refusee : le nonce a servi.
	if _, _, err := a.verify(mk()); err == nil {
		t.Error("replaying the same nonce must be refused")
	}
}

// Le cache de nonces ne doit pas se remplir avant la verification de
// signature, sinon un inconnu le remplit — et peut bruler a l'avance le
// nonce d'un pair.
func TestNonceRecordedOnlyAfterValidSignature(t *testing.T) {
	f := newFed(t, "AS64500", "A", "https://a.example.net")
	pub, priv, _ := ed25519.GenerateKey(nil)
	f.store.cfg.Exec(
		`INSERT INTO fed_peers(asn,org,url,pubkey,fingerprint,anchors,state,created_at)
		 VALUES('AS64999','Them','https://them.example.net',?,'x','[]','trusted',?)`,
		base64.StdEncoding.EncodeToString(pub), time.Now().Unix())

	mkGood := signedFactory(priv, "AS64999", "AS64500", "POST", "/api/v1/fed/report",
		map[string]any{"reports": []any{}})
	nonce := mkGood().Header.Get("X-Fed-Nonce")

	// Un tiers presente le meme nonce avec une signature invalide.
	bad := signedReq(priv, "AS64999", "AS64500", "POST", "/api/v1/fed/report",
		map[string]any{"reports": []any{}})
	bad.Header.Set("X-Fed-Nonce", nonce)
	if _, _, err := f.verify(bad); err == nil {
		t.Fatal("an invalid signature must be refused")
	}
	if n := len(f.nonces); n != 0 {
		t.Errorf("the nonce cache must stay empty after a refused request, holds %d", n)
	}
	// Le pair legitime peut encore utiliser son nonce.
	if _, _, err := f.verify(mkGood()); err != nil {
		t.Errorf("the legitimate request must still be accepted: %v", err)
	}
}

// ------------------------------------------------------ contenu injecte

// Un pair ne declare que ses propres mesures, vers un membre connu, sur
// une fenetre passee, avec des valeurs plausibles.
func TestAcceptReportsFiltersInjectedValues(t *testing.T) {
	f := newFed(t, "AS64500", "Us", "https://us.example.net")
	pub, _, _ := ed25519.GenerateKey(nil)
	f.store.cfg.Exec(
		`INSERT INTO fed_peers(asn,org,url,pubkey,fingerprint,anchors,state,created_at)
		 VALUES('AS64999','Them','https://them.example.net',?,'x','["192.0.2.10"]','trusted',?)`,
		base64.StdEncoding.EncodeToString(pub), time.Now().Unix())

	now := time.Now().Unix()
	in := []anchorReport{
		{FromASN: "AS64999", ToASN: "AS64500", Anchor: "192.0.2.10", WinEnd: now - 300,
			MedMs: 12, P95Ms: 20, LossPct: 3},
		// Une autre AS que la sienne.
		{FromASN: "AS64321", ToASN: "AS64500", Anchor: "192.0.2.10", WinEnd: now - 300},
		// Un AS qui n'est pas membre ici.
		{FromASN: "AS64999", ToASN: "AS65123", Anchor: "192.0.2.10", WinEnd: now - 300},
		// Une fenetre dans le futur, qui resterait affichee indefiniment.
		{FromASN: "AS64999", ToASN: "AS64500", Anchor: "192.0.2.10",
			WinEnd: now + 86400*365},
		// Une ancre en espace prive.
		{FromASN: "AS64999", ToASN: "AS64500", Anchor: "192.168.1.1", WinEnd: now - 300},
		// Des valeurs hors bornes.
		{FromASN: "AS64999", ToASN: "AS64500", Anchor: "192.0.2.10", WinEnd: now - 600,
			MedMs: -5, LossPct: 4000},
	}
	out := f.acceptReports("AS64999", in)
	if len(out) != 2 {
		t.Fatalf("2 reports expected, got %d: %+v", len(out), out)
	}
	if out[1].LossPct != 100 || out[1].MedMs != 0 {
		t.Errorf("values should be clamped, got %+v", out[1])
	}
}

// Une ancre annoncee par un pair ne doit pas faire pinguer une adresse
// privee ou de bouclage par toute la federation.
func TestAnchorValidation(t *testing.T) {
	bad := []string{"192.168.1.1", "10.0.0.5", "127.0.0.1", "169.254.1.1",
		"100.64.0.1", "::1", "fe80::1", "", "not a host", "http://x.example.net",
		"239.1.1.1"}
	for _, h := range bad {
		if _, err := checkAnchor(h); err == nil {
			t.Errorf("%q should have been refused", h)
		}
	}
	for _, h := range []string{"192.0.2.10", "2001:db8::1", "anchor.example.net"} {
		if _, err := checkAnchor(h); err != nil {
			t.Errorf("%q should have been accepted: %v", h, err)
		}
	}
	// Le nombre d'ancres est borne et les doublons ecartes.
	many := make([]string, 0, 20)
	for i := 0; i < 20; i++ {
		many = append(many, fmt.Sprintf("192.0.2.%d", i+1))
	}
	many = append(many, "192.0.2.1", "192.168.0.1")
	ok, rejected := cleanAnchors(many)
	if len(ok) != maxAnchors {
		t.Errorf("%d anchors kept, expected %d", len(ok), maxAnchors)
	}
	if len(rejected) == 0 {
		t.Error("the refused anchors should be reported")
	}
}

// Une URL de pair en javascript: ne doit jamais entrer en base, donc
// jamais finir dans un lien de la page publique.
func TestPeerURLValidation(t *testing.T) {
	for _, u := range []string{
		"javascript:alert(1)", "javascript:alert(1)//https://x.example.net",
		"data:text/html,<script>", "file:///etc/passwd", "",
		"https://user:pass@x.example.net", "http://127.0.0.1:8080",
		"http://localhost", "https://192.168.1.10", "https://[::1]",
		"https://x.example.net\nX-Injected: 1",
	} {
		if got, err := safeFedURL(u); err == nil {
			t.Errorf("%q should have been refused, got %q", u, got)
		}
	}
	got, err := safeFedURL("https://smoke.example.net/pairing/")
	if err != nil || got != "https://smoke.example.net" {
		t.Errorf("got %q, %v", got, err)
	}
}

// -------------------------------------------------- alertes forgeables

// Nous ne sollicitons jamais un NOC sur la seule parole de nos pairs :
// il faut que nos propres mesures corroborent.
func TestNOCNotRaisedWithoutOurOwnMeasurement(t *testing.T) {
	f := newFed(t, "AS64500", "Us", "https://us.example.net")
	id := newIncidentID()
	f.saveIncident(Incident{ID: id, OpenedAt: time.Now().Unix() - 600,
		ObserverASN: "AS64901", SuspectASN: "AS64999", Target: "192.0.2.10",
		Severity: "warning", Detail: "loss 30%"})
	// Trois pairs complices corroborent, nous non.
	for _, asn := range []string{"AS64901", "AS64902", "AS64903"} {
		f.addCorroboration(id, asn, 10, 30, "loss 30%")
	}
	f.store.cfg.Exec(`UPDATE fed_incidents SET notice_due_at=? WHERE id=?`,
		time.Now().Unix()-60, id)

	f.notifyDue()
	var notified *int64
	f.store.cfg.QueryRow(`SELECT notified_at FROM fed_incidents WHERE id=?`, id).
		Scan(&notified)
	if notified != nil {
		t.Error("no NOC may be notified without our own corroboration")
	}
	if f.weCorroborate(id) {
		t.Error("weCorroborate should be false here")
	}
	f.addCorroboration(id, "AS64500", 10, 30, "loss 30%")
	if !f.weCorroborate(id) {
		t.Error("weCorroborate should be true once we measured it")
	}
}

// Un incident recu est renormalise : identifiant verifie, AS connu, texte
// ramene a une ligne, dates et preavis recalcules localement.
func TestIncomingIncidentIsNormalised(t *testing.T) {
	f := newFed(t, "AS64500", "Us", "https://us.example.net")
	pub, priv, _ := ed25519.GenerateKey(nil)
	f.store.cfg.Exec(
		`INSERT INTO fed_peers(asn,org,url,pubkey,fingerprint,anchors,state,created_at)
		 VALUES('AS64999','Them','https://them.example.net',?,'x','["192.0.2.10"]','trusted',?)`,
		base64.StdEncoding.EncodeToString(pub), time.Now().Unix())
	api := &API{store: f.store, fed: f}

	ack := time.Now().Unix()
	call := func(i Incident) *httptest.ResponseRecorder {
		req := signedReq(priv, "AS64999", "AS64500", "POST", "/api/v1/fed/incident", i)
		w := httptest.NewRecorder()
		api.fedIncident(w, req)
		return w
	}

	// Un AS mis en cause qui n'est pas membre : refuse.
	if w := call(Incident{ID: newIncidentID(), SuspectASN: "AS65123",
		Target: "192.0.2.10"}); w.Code != 400 {
		t.Errorf("naming a non-member AS should be refused, got %d", w.Code)
	}
	// Un identifiant fabrique : refuse.
	if w := call(Incident{ID: "../../etc/passwd", SuspectASN: "AS64999",
		Target: "192.0.2.10"}); w.Code != 400 {
		t.Errorf("an invalid identifier should be refused, got %d", w.Code)
	}
	// Une cible en espace prive : refusee.
	if w := call(Incident{ID: newIncidentID(), SuspectASN: "AS64999",
		Target: "192.168.1.1"}); w.Code != 400 {
		t.Errorf("a private target should be refused, got %d", w.Code)
	}

	id := newIncidentID()
	w := call(Incident{ID: id, SuspectASN: "as64999", Target: "192.0.2.10",
		OpenedAt: time.Now().Unix() + 86400*365, AckAt: &ack, AckBy: "them",
		Severity: "critical",
		Detail:   "line one\r\nSubject: injected\r\n" + strings.Repeat("x", 900),
		LossPct:  5000, MedMs: -12})
	if w.Code != 200 {
		t.Fatalf("a consistent incident should be accepted, got %d: %s", w.Code, w.Body)
	}
	var detail, severity, suspect string
	var opened int64
	var ackAt, due *int64
	f.store.cfg.QueryRow(
		`SELECT detail,severity,suspect_asn,opened_at,ack_at,notice_due_at
		   FROM fed_incidents WHERE id=?`, id).
		Scan(&detail, &severity, &suspect, &opened, &ackAt, &due)
	if strings.ContainsAny(detail, "\r\n") {
		t.Error("the detail must not keep line breaks")
	}
	if len([]rune(detail)) > maxFedText+1 {
		t.Errorf("the detail must be bounded, %d runes", len([]rune(detail)))
	}
	if severity != "warning" {
		t.Errorf("severity = %q, must be decided locally", severity)
	}
	if suspect != "AS64999" {
		t.Errorf("suspect = %q, should be normalised", suspect)
	}
	if opened > time.Now().Unix()+5 {
		t.Error("a date in the future must be brought back to now")
	}
	if ackAt != nil {
		t.Error("the sender must not be able to pre-acknowledge its own incident")
	}
	if due == nil || *due <= opened {
		t.Error("the notice delay must be recomputed locally")
	}
	// Le texte libre d'un pair ne ressort pas sous notre nom sur l'API
	// publique.
	pw := httptest.NewRecorder()
	api.fedIncidents(pw, httptest.NewRequest("GET", "/api/v1/fed/incidents", nil))
	if strings.Contains(pw.Body.String(), "line one") {
		t.Error("a peer's free text must not be republished on the public API")
	}
}

// ------------------------------------------------------ points annexes

// Le mot de passe SMTP ne doit pas ressortir de l'API d'administration.
func TestIdentityHidesSMTPPassword(t *testing.T) {
	f := newFed(t, "AS64500", "Us", "https://us.example.net")
	f.store.SetSetting("notify", `{"enabled":true,"smtp_host":"mail.example.net",`+
		`"smtp_user":"noc","smtp_pass":"s3cr3t","from":"noc@example.net"}`)
	api := &API{store: f.store, fed: f}
	w := httptest.NewRecorder()
	api.fedIdentity(w, httptest.NewRequest("GET", "/api/v1/admin/fed/identity", nil))
	if strings.Contains(w.Body.String(), "s3cr3t") {
		t.Fatal("the SMTP password must never be returned")
	}
	if !strings.Contains(w.Body.String(), `"smtp_pass_set":true`) {
		t.Error("the interface still needs to know a password is set")
	}
}

// Un retour a la ligne dans un sujet de courriel ajoute des en-tetes.
func TestMailHeaderAndAddressSafety(t *testing.T) {
	got := mailHeader("Degradation towards AS1\r\nBcc: victim@example.net")
	if strings.ContainsAny(got, "\r\n") {
		t.Errorf("line breaks must be removed: %q", got)
	}
	for _, a := range []string{"noc@example.net\r\nBcc: x@y.z", "not-an-address",
		"a@b", "", "<a@b.c>", "a b@c.de"} {
		if _, err := mailAddress(a); err == nil {
			t.Errorf("%q should have been refused", a)
		}
	}
	if _, err := mailAddress(" noc@example.net "); err != nil {
		t.Errorf("a normal address should pass: %v", err)
	}
}

// Un AS mal forme ne doit pas devenir une cle de table ni une audience.
func TestNormASN(t *testing.T) {
	for in, want := range map[string]string{
		"AS64500": "AS64500", "as64500": "AS64500", "64500": "AS64500",
		" AS64500 ": "AS64500",
	} {
		if got, err := normASN(in); err != nil || got != want {
			t.Errorf("normASN(%q) = %q, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "AS", "AS-1", "ASxx", "AS64500 OR 1=1",
		"AS64500\nX: y", "3215 Orange"} {
		if got, err := normASN(in); err == nil {
			t.Errorf("normASN(%q) = %q, expected an error", in, got)
		}
	}
}

// oneLine borne et aplatit, y compris les caracteres de controle.
func TestOneLine(t *testing.T) {
	got := oneLine("a\r\nb\tc\x00d   e", 0)
	if got != "a b cd e" {
		t.Errorf("got %q", got)
	}
	if n := len([]rune(oneLine(strings.Repeat("x", 900), 100))); n != 101 {
		t.Errorf("length not bounded: %d", n)
	}
}

// Une fenetre de mesure dans le futur resterait affichee comme la plus
// recente indefiniment.
func TestClampWindow(t *testing.T) {
	now := time.Now().Unix()
	if _, err := clampWindow(now+86400, now); err == nil {
		t.Error("a window in the future must be refused")
	}
	if _, err := clampWindow(now-86400*40, now); err == nil {
		t.Error("a window that is too old must be refused")
	}
	if _, err := clampWindow(now-300, now); err != nil {
		t.Errorf("a recent window must pass: %v", err)
	}
}

// Le client de la federation refuse de se connecter a une adresse privee :
// c'est ce qui ferme le SSRF via l'URL annoncee par un pair.
func TestFedClientRefusesPrivateAddresses(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(
		func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("{}")) }))
	defer srv.Close()
	if _, err := fedClient(3 * time.Second).Get(srv.URL); err == nil {
		t.Fatal("connecting to a loopback address must be refused")
	}
	if err := fedDialControl("tcp", "169.254.169.254:80", nil); err == nil {
		t.Error("the metadata address must be refused")
	}
	if err := fedDialControl("tcp", "198.51.100.7:443", nil); err != nil {
		t.Errorf("a public address must pass: %v", err)
	}
}
