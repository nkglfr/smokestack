package main

import (
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// Disponibilite. Definition retenue : la proportion des passes de mesure
// ou la cible a repondu au moins une fois. Ce n'est pas un taux de perte
// de paquets, et les deux ne racontent pas la meme chose — une cible qui
// perd un paquet sur cinq en permanence est disponible a 100 % avec une
// perte de 20 %, et c'est bien ainsi qu'un operateur la percoit.
//
// Ce n'est pas non plus un SLA. Un SLA se mesure depuis le reseau du
// client, sur son trafic reel, avec des exclusions contractuelles ; ici
// on mesure des paquets partis d'un seul point de l'Internet. L'interface
// le dit a cote du chiffre plutot que de laisser croire le contraire.

// AvailWindow est la disponibilite sur une fenetre.
type AvailWindow struct {
	// Label est la duree demandee, telle qu'on la nomme dans l'interface.
	Label string `json:"label"`
	Since int64  `json:"since"`
	// Passes et Down comptent les passes de mesure, pas les paquets.
	Passes int64 `json:"passes"`
	Down   int64 `json:"down"`
	// Pct est nil quand aucune passe n'est comptee sur la fenetre : le
	// chiffre est alors inconnu, ce qui n'est pas la meme chose que zero.
	Pct *float64 `json:"pct"`
	// Partial signale une fenetre dont les mesures ne commencent pas au
	// debut : instance recemment installee, cible recemment creee, ou
	// periode anterieure a la mise a jour qui a introduit le comptage.
	Partial bool  `json:"partial"`
	First   int64 `json:"first,omitempty"`
	// Excluded compte les passes retirees du calcul parce qu'elles
	// tombaient dans une maintenance programmee qui coupait la mesure.
	Excluded int64 `json:"excluded,omitempty"`
}

var availWindows = []struct {
	label string
	secs  int64
}{
	{"24h", 86400},
	{"7d", 7 * 86400},
	{"30d", 30 * 86400},
	{"1y", 365 * 86400},
}

// Availability renvoie une fenetre par duree standard. Une seule requete
// par fenetre, sur la table la plus grossiere qui couvre la periode : la
// disponibilite ne demande pas de resolution fine, seulement des totaux,
// et roll_1d repond pour un an en quelques lignes.
func (s *Store) Availability(targetID, probeID int64, now time.Time) []AvailWindow {
	out := make([]AvailWindow, 0, len(availWindows))
	end := now.Unix()
	for _, w := range availWindows {
		out = append(out, s.availWindow(targetID, probeID, w.label, end-w.secs, end))
	}
	return out
}

func (s *Store) availWindow(targetID, probeID int64, label string, from, to int64) AvailWindow {
	a := AvailWindow{Label: label, Since: from}
	table := availTable(to - from)
	var passes, down, first int64
	err := s.mx.QueryRow(fmt.Sprintf(
		`SELECT COALESCE(SUM(passes),0), COALESCE(SUM(down),0),
		        COALESCE(MIN(CASE WHEN passes>0 THEN bucket END),0)
		   FROM %s WHERE target_id=? AND probe_id=? AND bucket>=? AND bucket<?`, table),
		targetID, probeID, from, to).Scan(&passes, &down, &first)
	if err != nil || passes == 0 {
		return a
	}
	// Les fenetres de maintenance qui coupent la mesure sortent du calcul.
	// Une cible arretee volontairement ne compte ni comme disponible ni
	// comme indisponible : elle ne compte pas, sinon une maintenance
	// annoncee degraderait le chiffre exactement comme une panne.
	for _, w := range s.maintenanceRanges(targetID, from, to) {
		var mp, md int64
		if s.mx.QueryRow(fmt.Sprintf(
			`SELECT COALESCE(SUM(passes),0), COALESCE(SUM(down),0) FROM %s
			  WHERE target_id=? AND probe_id=? AND bucket>=? AND bucket<?`, table),
			targetID, probeID, maxInt64(w[0], from), minInt64(w[1], to)).Scan(&mp, &md) != nil {
			continue
		}
		passes -= mp
		down -= md
		a.Excluded += mp
	}
	if passes <= 0 {
		// Toute la fenetre etait en maintenance : le chiffre est inconnu,
		// pas nul, et l'interface le dit.
		a.Excluded, a.Passes, a.Down = 0, 0, 0
		return a
	}
	a.Passes, a.Down, a.First = passes, down, first
	pct := float64(passes-down) * 100 / float64(passes)
	a.Pct = &pct
	// Une fenetre dont la premiere mesure arrive nettement apres son debut
	// est partielle. Le seuil est large a dessein : un decalage d'un
	// bucket n'est pas une lacune, et une cible creee la semaine derniere
	// ne doit pas afficher une disponibilite annuelle comme si elle
	// couvrait l'annee.
	a.Partial = first > from+availSlack(to-from)
	return a
}

// availTable : la disponibilite n'agrege que des compteurs, donc on prend
// la table la plus grossiere disponible pour la fenetre. samples ne garde
// que quarante-huit heures, roll_1m un an ; au-dela il faut roll_1h.
func availTable(span int64) string {
	switch {
	case span <= 2*3600:
		return "samples"
	case span <= 2*86400:
		return "roll_1m"
	case span <= 90*86400:
		return "roll_1h"
	}
	return "roll_1d"
}

// Cache court, parce que le chiffre est sur une page publique et qu'une
// minute de retard sur une disponibilite annuelle n'interesse personne,
// alors que quatre agregations par visiteur, si.
type availCacheT struct {
	mu    sync.Mutex
	items map[string]availEntry
}

type availEntry struct {
	data map[string]any
	exp  time.Time
}

var availCache = &availCacheT{items: map[string]availEntry{}}

func (c *availCacheT) get(k string) map[string]any {
	c.mu.Lock()
	defer c.mu.Unlock()
	if e, ok := c.items[k]; ok && time.Now().Before(e.exp) {
		return e.data
	}
	delete(c.items, k)
	return nil
}

func (c *availCacheT) put(k string, v map[string]any, ttl time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	// Borne dure : une instance avec beaucoup de cibles ne doit pas voir
	// ce cache grossir sans fin. Au-dela, on vide plutot que de gerer une
	// eviction fine pour des entrees qui expirent de toute facon.
	if len(c.items) > 2048 {
		c.items = map[string]availEntry{}
	}
	c.items[k] = availEntry{data: v, exp: time.Now().Add(ttl)}
}

// availability sert le chiffre sur les pages publiques, avec la meme
// verification de visibilite que les series : une cible privee ne rend
// pas sa disponibilite a un visiteur qui ne la voit pas.
func (a *API) availability(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	targetID, err := strconv.ParseInt(q.Get("target"), 10, 64)
	if err != nil {
		writeErr(w, 400, "missing target parameter")
		return
	}
	// A figure the operator does not publish does not leave by this door
	// either: hiding it in the browser would leave the API serving it to
	// whoever knows the address. The back-office still reads it, since it
	// is where publishing is decided.
	if !a.store.Site().PublicAvailability && !a.authenticated(r) {
		// Whether the figure is published is an instance setting an operator
		// can change, so this 404 must not outlive the change in a cache.
		cachePrivate(w)
		writeErr(w, 404, "availability is not published")
		return
	}
	probeID, probePriv := a.resolveProbe(r, a.probeID)
	shared, hasShare := a.shareGrant(r)
	t, terr := a.store.TargetByID(targetID)
	if terr != nil ||
		(!t.Public && !a.authenticated(r) && !(hasShare && shared == targetID)) {
		cachePrivate(w)
		writeErr(w, 404, "target not found")
		return
	}
	// Same rule as a series: the figure reads the same for everyone allowed
	// to see it, so the target's visibility decides and no Vary is needed.
	// This endpoint set no directive at all, which left a private target's
	// availability cacheable by heuristic.
	//
	// Unless ?probe= resolved through a privilege, which is the one case
	// where the figure does not read the same for everyone. With no Vary
	// here, this URL has one shared cache entry, and the authenticated
	// answer must not be what fills it.
	if t.Public && !probePriv {
		w.Header().Set("Cache-Control", "public, max-age=60")
	} else {
		cachePrivate(w)
	}
	key := fmt.Sprintf("avail|%d|%d", targetID, probeID)
	if v := availCache.get(key); v != nil {
		writeJSON(w, v)
		return
	}
	out := map[string]any{
		"target_id": targetID,
		"windows":   a.store.Availability(targetID, probeID, time.Now()),
		// Dit avec le chiffre, pas dans une note de bas de page : le
		// malentendu qu'on veut eviter est precisement celui-la.
		"disclaimer": "measured from a single vantage point; not a contractual SLA",
	}
	availCache.put(key, out, 60*time.Second)
	writeJSON(w, out)
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func minInt64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func availSlack(span int64) int64 {
	switch {
	case span <= 86400:
		return 600
	case span <= 30*86400:
		return 2 * 3600
	}
	return 2 * 86400
}
