package main

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"
)

// Regles de surete communes a toute la federation. Le principe : rien de
// ce qui arrive d'un pair n'est publie, envoye par courriel ou utilise
// comme cible de mesure sans avoir traverse ce fichier.
//
//   - tout texte libre est ramene a une ligne imprimable et bornee, pour
//     qu'il ne puisse ni injecter d'en-tete de courriel ni deborder d'une
//     page ;
//   - toute URL de pair est validee, donc jamais un javascript: dans un
//     lien de la page publique ;
//   - toute sortie HTTP vers un pair passe par un client qui refuse de se
//     connecter a une adresse privee, ce qui ferme le SSRF ;
//   - toute ancre annoncee est verifiee avant d'etre pinguee.

const (
	maxFedText    = 400 // detail d'incident, note d'acquittement
	maxAnchors    = 8
	maxReports    = 200
	fedMaxFuture  = 300 // 5 min de tolerance d'horloge sur une fenetre
	fedMaxPast    = 26 * 3600
	maxNonceCache = 20000
)

// oneLine ramene un texte a une seule ligne imprimable, bornee en
// longueur. Les retours a la ligne sont la faille : ils transforment un
// sujet de courriel en en-tetes supplementaires.
func oneLine(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		switch {
		case r == '\n' || r == '\r' || r == '\t':
			return ' '
		case r == 0x7f || unicode.IsControl(r):
			return -1
		case !utf8.ValidRune(r):
			return -1
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if max > 0 && utf8.RuneCountInString(s) > max {
		r := []rune(s)
		s = strings.TrimSpace(string(r[:max])) + "…"
	}
	return s
}

// fedText est ce que l'on accepte d'un pair comme texte libre.
func fedText(s string) string { return oneLine(s, maxFedText) }

// mailHeader refuse un en-tete de courriel qui contient un retour a la
// ligne : c'est la seule chose qui compte pour l'injection d'en-tetes.
func mailHeader(s string) string {
	return oneLine(s, 200)
}

var addrRe = regexp.MustCompile(`^[^\s<>",;:\\@]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}$`)

// mailAddress valide une adresse destinataire avant de la donner a un
// serveur SMTP. Sans cela, un pair choisit ses propres en-tetes.
func mailAddress(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s != oneLine(s, 0) {
		return "", fmt.Errorf("invalid address")
	}
	if !addrRe.MatchString(s) {
		return "", fmt.Errorf("invalid address: %q", s)
	}
	return s, nil
}

var asnRe = regexp.MustCompile(`^(?i)AS[0-9]{1,10}$`)

// normASN ramene un numero d'AS a la forme ASnnnn, et refuse tout le
// reste : il sert de cle dans plusieurs tables et de champ d'audience.
func normASN(s string) (string, error) {
	s = strings.ToUpper(strings.TrimSpace(s))
	if s == "" {
		return "", fmt.Errorf("missing AS number")
	}
	if !strings.HasPrefix(s, "AS") {
		s = "AS" + s
	}
	if !asnRe.MatchString(s) {
		return "", fmt.Errorf("invalid AS number: %q", s)
	}
	return s, nil
}

// ------------------------------------------------------------ URL de pair

// fedAllowPrivateForTests ouvre les adresses privees aux tests, qui font
// tourner de vraies instances sur la boucle locale. Rien ne le met a vrai
// hors d'un fichier _test.go.
var fedAllowPrivateForTests bool

// safeFedURL valide l'URL annoncee par un pair. Elle finit dans un lien
// de la page publique et comme destination de requetes sortantes : les
// deux exigent un schema connu et un hote nomme.
func safeFedURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", fmt.Errorf("missing URL")
	}
	if raw != oneLine(raw, 0) {
		return "", fmt.Errorf("invalid URL")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid URL")
	}
	if u.Scheme != "https" && u.Scheme != "http" {
		return "", fmt.Errorf("the URL must start with https://")
	}
	if u.User != nil {
		return "", fmt.Errorf("the URL must not carry credentials")
	}
	host := u.Hostname()
	if host == "" {
		return "", fmt.Errorf("the URL has no host")
	}
	// Un hote litteral en espace privee n'a rien a faire dans un annuaire
	// public, et servirait a faire sonner une adresse interne.
	if !fedAllowPrivateForTests {
		if ip := net.ParseIP(host); ip != nil && !isPublicIP(ip) {
			return "", fmt.Errorf("the URL points at a private address")
		}
		if strings.EqualFold(host, "localhost") ||
			strings.HasSuffix(strings.ToLower(host), ".localhost") {
			return "", fmt.Errorf("the URL points at this machine")
		}
	}
	out := u.Scheme + "://" + u.Host
	if p := strings.TrimSuffix(u.Path, "/"); p != "" && p != "/pairing" {
		out += p
	}
	return out, nil
}

// ------------------------------------------------- client HTTP sans SSRF

// fedDialControl refuse la connexion si l'adresse resolue n'est pas une
// adresse publique. Le controle a lieu apres la resolution DNS, donc un
// nom qui pointe vers 127.0.0.1 — ou qui change entre la verification et
// la connexion — est arrete quand meme.
func fedDialControl(network, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("invalid address")
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return fmt.Errorf("unresolved address")
	}
	if !isPublicIP(ip) || ip.IsMulticast() || ip.IsInterfaceLocalMulticast() ||
		ip.IsLinkLocalMulticast() {
		return fmt.Errorf("refusing to connect to a non-public address (%s)", host)
	}
	return nil
}

// fedClient est le seul client utilise vers un pair.
func fedClient(timeout time.Duration) *http.Client {
	d := &net.Dialer{Timeout: 10 * time.Second, Control: fedDialControl}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return d.DialContext(ctx, network, addr)
			},
			TLSHandshakeTimeout: 10 * time.Second,
			MaxIdleConnsPerHost: 2,
		},
		// Une redirection vers une autre instance ferait sortir la requete
		// signee de son audience : on ne suit rien.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return fmt.Errorf("redirects are not followed towards a peer")
		},
	}
}

// ---------------------------------------------------------------- ancres

var hostRe = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)

// checkAnchor valide une ancre annoncee par un pair avant qu'elle ne
// devienne une cible de mesure. Une instance ne doit pas pouvoir faire
// pinguer 192.168.1.1 — ni l'adresse d'un tiers — par tout le reseau.
func checkAnchor(h string) (string, error) {
	h = strings.ToLower(strings.TrimSpace(h))
	if h == "" {
		return "", fmt.Errorf("empty anchor")
	}
	h = strings.TrimSuffix(h, ".")
	if ip := net.ParseIP(h); ip != nil {
		if !isPublicIP(ip) || ip.IsMulticast() {
			return "", fmt.Errorf("%s is not a public address", h)
		}
		return ip.String(), nil
	}
	if !hostRe.MatchString(h) || !strings.Contains(h, ".") {
		return "", fmt.Errorf("%q is neither an address nor a host name", h)
	}
	return h, nil
}

// cleanAnchors garde les ancres utilisables, en nombre borne, et rend la
// liste de celles qui ont ete ecartees pour l'afficher a l'operateur.
func cleanAnchors(in []string) (ok []string, rejected []string) {
	seen := map[string]bool{}
	for _, a := range in {
		c, err := checkAnchor(a)
		if err != nil {
			rejected = append(rejected, oneLine(a, 80))
			continue
		}
		if seen[c] {
			continue
		}
		seen[c] = true
		if len(ok) >= maxAnchors {
			rejected = append(rejected, c)
			continue
		}
		ok = append(ok, c)
	}
	return ok, rejected
}

// anchorBelongsTo indique si l'ancre est annoncee par l'AS qui la
// declare, quand l'information est deja en cache. Sans certitude, on ne
// bloque pas : c'est un element de decision pour l'operateur, pas une
// autorisation.
func anchorBelongsTo(svc *ASNService, anchor, asn string) (bool, bool) {
	if svc == nil {
		return false, false
	}
	ip := anchor
	if net.ParseIP(anchor) == nil {
		addrs, err := net.LookupHost(anchor)
		if err != nil || len(addrs) == 0 {
			return false, false
		}
		ip = addrs[0]
	}
	got, known := svc.ASNOfIP(ip)
	if !known {
		svc.RefreshIPASN(ip)
		return false, false
	}
	want, err1 := normASN(asn)
	have, err2 := normASN(got)
	if err1 != nil || err2 != nil {
		return false, false
	}
	return have == want, true
}

// ------------------------------------------------------------- incidents

var incidentIDRe = regexp.MustCompile(`^[0-9]{8}T[0-9]{4}-[0-9a-f]{18}$`)

func checkIncidentID(s string) error {
	if !incidentIDRe.MatchString(s) {
		return fmt.Errorf("invalid incident identifier")
	}
	return nil
}

// clampFloat borne une mesure recue : une latence negative ou un taux de
// perte de 4000 % ne doivent pas atteindre une page ni un courriel.
func clampFloat(v, min, max float64) float64 {
	if v != v { // NaN
		return min
	}
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

// clampWindow refuse une fenetre dans le futur — sans quoi une mesure
// reste affichee comme « la plus recente » indefiniment — et une fenetre
// trop ancienne.
func clampWindow(win, now int64) (int64, error) {
	if win <= 0 {
		return 0, fmt.Errorf("missing window")
	}
	if win > now+fedMaxFuture {
		return 0, fmt.Errorf("window in the future")
	}
	if win < now-fedMaxPast {
		return 0, fmt.Errorf("window too old")
	}
	return win, nil
}
