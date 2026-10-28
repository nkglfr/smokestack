package main

// Remote probe enrolment.
//
// A probe running on another machine reaches the instance over HTTP and
// authenticates with a token. The verification side of that already existed
// in ingest(): it reads probes.token_hash, compares in constant time and
// refuses an empty hash. What was missing was any way to issue a token, so
// the remote mode could not be used at all — ProbeID() inserts a probe but
// never writes token_hash, and nothing else ever did.
//
// Only the hash is stored, so a token is shown once and cannot be recovered
// afterwards. That is the same contract as a share link, and for the same
// reason: a stored credential is a credential that leaks with the backup.

import (
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"
)

// A probe as the back-office sees it. Token is filled on the one response
// that issues it and is empty everywhere else; token_hash never leaves the
// database at all, under any field name.
type Probe struct {
	ID         int64  `json:"id"`
	Slug       string `json:"slug"`
	Name       string `json:"name"`
	Location   string `json:"location"`
	Local      bool   `json:"local"`
	Enabled    bool   `json:"enabled"`
	LastSeenAt int64  `json:"last_seen_at"`
	HasToken   bool   `json:"has_token"`
	Token      string `json:"token,omitempty"`

	// Where it measures from. SeenAddress is observed when the probe
	// connects; Address is what the operator stated and wins when set.
	// Address is the repair for the two cases observation gets wrong: a probe
	// behind NAT, and an instance behind a reverse proxy, where what arrives
	// is the proxy rather than the probe.
	Address     string `json:"address"`
	SeenAddress string `json:"seen_address"`
	// Derived, never stored: the address actually used, and the AS it falls
	// in. The AS is resolved from the lookup service's cache at read time, so
	// it follows a renumbering without a column to keep in step.
	From string `json:"from,omitempty"`
	ASN  string `json:"asn,omitempty"`
}

// EffectiveAddress is the stated address when there is one, the observed
// address otherwise. Everything that needs to know where a probe measures
// from asks this rather than picking a column, so the precedence is written
// once.
func (p Probe) EffectiveAddress() string {
	if a := strings.TrimSpace(p.Address); a != "" {
		return a
	}
	return strings.TrimSpace(p.SeenAddress)
}

// The slug identifies the probe in the ingest body and will end up in URLs
// and in the interface. Constraining it here keeps every later use plain,
// rather than leaving each one to guess what a slug can contain.
const probeSlugMax = 39

func validProbeSlug(s string) error {
	if len(s) < 2 || len(s) > probeSlugMax {
		return fmt.Errorf("the slug must be between 2 and %d characters", probeSlugMax)
	}
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		case r == '-' && i > 0 && i < len(s)-1:
		default:
			return fmt.Errorf("the slug takes lowercase letters, digits and inner hyphens only")
		}
	}
	return nil
}

// Probes lists every probe, local one included, for the back-office.
func (s *Store) Probes() ([]Probe, error) {
	rows, err := s.cfg.Query(
		`SELECT id, slug, name, COALESCE(location,''), local, enabled,
		        COALESCE(last_seen_at,0), COALESCE(token_hash,'') <> '',
		        COALESCE(address,''), COALESCE(seen_address,'')
		   FROM probes ORDER BY local DESC, slug`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Probe{}
	for rows.Next() {
		var p Probe
		var local, enabled, hasTok int
		if err := rows.Scan(&p.ID, &p.Slug, &p.Name, &p.Location,
			&local, &enabled, &p.LastSeenAt, &hasTok, &p.Address, &p.SeenAddress); err != nil {
			return nil, err
		}
		p.Local, p.Enabled, p.HasToken = local == 1, enabled == 1, hasTok == 1
		p.From = p.EffectiveAddress()
		out = append(out, p)
	}
	return out, rows.Err()
}

// CreateProbe registers a probe that runs elsewhere. It is created without a
// token and therefore cannot send anything until one is issued, which keeps
// the two decisions — this probe exists, this probe may write — separate.
func (s *Store) CreateProbe(slug, name, location string) (*Probe, error) {
	slug = strings.ToLower(strings.TrimSpace(slug))
	if err := validProbeSlug(slug); err != nil {
		return nil, err
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = slug
	}
	res, err := s.cfg.Exec(
		`INSERT INTO probes(slug,name,location,local,enabled) VALUES(?,?,?,0,1)`,
		slug, name, strings.TrimSpace(location))
	if err != nil {
		return nil, fmt.Errorf("a probe with this slug already exists")
	}
	id, _ := res.LastInsertId()
	return &Probe{ID: id, Slug: slug, Name: name,
		Location: strings.TrimSpace(location), Enabled: true}, nil
}

// IssueProbeToken mints a token for a probe and stores only its hash. The
// token is returned once; a second call replaces it, which is how a leaked
// token is rotated.
//
// It refuses the local probe on purpose. The local probe writes through the
// Unix socket, which is reachable only by a process on the host; giving it
// an HTTP credential as well would widen the surface for nothing.
func (s *Store) IssueProbeToken(id int64) (string, error) {
	var local int
	if err := s.cfg.QueryRow(`SELECT local FROM probes WHERE id=?`, id).Scan(&local); err != nil {
		if err == sql.ErrNoRows {
			return "", fmt.Errorf("unknown probe")
		}
		return "", err
	}
	if local == 1 {
		return "", fmt.Errorf("the local probe writes through the Unix socket and takes no token")
	}
	tok := randomHex(24)
	if _, err := s.cfg.Exec(`UPDATE probes SET token_hash=? WHERE id=?`, hashToken(tok), id); err != nil {
		return "", err
	}
	return tok, nil
}

// RevokeProbeToken withdraws the credential. Rotation alone is not enough:
// a token that leaked from a decommissioned machine has to be removable
// without inventing a replacement nobody will use. ingest() refuses an
// empty hash, so this takes effect on the next request.
func (s *Store) RevokeProbeToken(id int64) error {
	res, err := s.cfg.Exec(`UPDATE probes SET token_hash=NULL WHERE id=? AND local=0`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("unknown probe")
	}
	return nil
}

// SetProbeEnabled stops or resumes a probe. ingest() selects on enabled=1,
// so a disabled probe is refused at the door while its measurements stay.
//
// There is deliberately no delete. The rollup tables in metrics.db are keyed
// on probe_id and live in a different database, so removing the row here
// would orphan every measurement the probe ever sent, silently and with no
// way to attribute them again.
func (s *Store) SetProbeEnabled(id int64, on bool) error {
	v := 0
	if on {
		v = 1
	}
	res, err := s.cfg.Exec(`UPDATE probes SET enabled=? WHERE id=? AND local=0`, v, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("unknown probe")
	}
	return nil
}

// ProbeTargetIDs is the set of targets a probe may measure. It is consulted
// on the probe's own hot path, so it returns a set rather than a list.
//
// A probe with no assignment gets an empty set, and an empty set means an
// empty target list. That is the safe way round: a probe enrolled today
// measures nothing until someone says what it should measure, rather than
// everything until someone says otherwise.
func (s *Store) ProbeTargetIDs(probeID int64) (map[int64]bool, error) {
	rows, err := s.cfg.Query(`SELECT target_id FROM probe_targets WHERE probe_id=?`, probeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// SetProbeTargets replaces a probe's assignment with exactly this set. It is
// a replacement rather than an add, because the operator edits a list and
// expects what they left to be what the probe gets; reconciling additions and
// removals at the caller would be the same work done less reliably.
//
// It refuses the local probe. That probe measures everything by construction
// and is never read from this table, so an assignment for it would be a row
// that looks meaningful and changes nothing.
func (s *Store) SetProbeTargets(probeID int64, ids []int64) error {
	var local int
	if err := s.cfg.QueryRow(`SELECT local FROM probes WHERE id=?`, probeID).Scan(&local); err != nil {
		if err == sql.ErrNoRows {
			return fmt.Errorf("unknown probe")
		}
		return err
	}
	if local == 1 {
		return fmt.Errorf("the local probe measures every target and takes no assignment")
	}
	tx, err := s.cfg.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM probe_targets WHERE probe_id=?`, probeID); err != nil {
		return err
	}
	ins, err := tx.Prepare(`INSERT INTO probe_targets(probe_id,target_id) VALUES(?,?)
	                        ON CONFLICT(probe_id,target_id) DO NOTHING`)
	if err != nil {
		return err
	}
	defer ins.Close()
	for _, id := range ids {
		// A target that does not exist would be a row nothing ever reads and
		// a silent typo in an operator's list, so it is refused rather than
		// stored.
		var n int
		if err := tx.QueryRow(`SELECT COUNT(*) FROM targets WHERE id=?`, id).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("no target with id %d", id)
		}
		if _, err := ins.Exec(probeID, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// PrimaryTargets is the set of targets this probe decides for: the ones whose
// primary it is, plus — when it is the local probe — every target that has
// chosen nobody. That second half is what keeps an instance with no remote
// probe behaving as it always did.
func (s *Store) PrimaryTargets(probeID int64) (map[int64]bool, error) {
	var local int
	if err := s.cfg.QueryRow(`SELECT local FROM probes WHERE id=?`, probeID).Scan(&local); err != nil && err != sql.ErrNoRows {
		return nil, err
	}
	q := `SELECT id FROM targets WHERE primary_probe_id=?`
	if local == 1 {
		q = `SELECT id FROM targets WHERE primary_probe_id=? OR primary_probe_id=0`
	}
	rows, err := s.cfg.Query(q, probeID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// TargetPrimaries maps every target to the probe that decides it, with the
// local probe's own id substituted for the zero that means "nobody chose".
// Callers then have one rule instead of two.
func (s *Store) TargetPrimaries(localProbeID int64) (map[int64]int64, error) {
	rows, err := s.cfg.Query(`SELECT id, primary_probe_id FROM targets`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]int64{}
	for rows.Next() {
		var id, pid int64
		if err := rows.Scan(&id, &pid); err != nil {
			return nil, err
		}
		if pid == 0 {
			pid = localProbeID
		}
		out[id] = pid
	}
	return out, rows.Err()
}

// TargetWatchers maps every target to the probes that measure it. The local
// probe measures everything and is in no assignment row, so it is added to
// every target rather than left out of all of them.
func (s *Store) TargetWatchers(localProbeID int64) (map[int64][]int64, error) {
	rows, err := s.cfg.Query(`SELECT pt.target_id, pt.probe_id FROM probe_targets pt
	                          JOIN probes p ON p.id = pt.probe_id
	                          WHERE p.enabled = 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64][]int64{}
	for rows.Next() {
		var tid, pid int64
		if err := rows.Scan(&tid, &pid); err != nil {
			return nil, err
		}
		out[tid] = append(out[tid], pid)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	trows, err := s.cfg.Query(`SELECT id FROM targets`)
	if err != nil {
		return nil, err
	}
	defer trows.Close()
	for trows.Next() {
		var tid int64
		if err := trows.Scan(&tid); err != nil {
			return nil, err
		}
		out[tid] = append(out[tid], localProbeID)
	}
	return out, trows.Err()
}

// EnabledProbeIDs lists the probes worth asking for an opinion.
func (s *Store) EnabledProbeIDs() ([]int64, error) {
	rows, err := s.cfg.Query(`SELECT id FROM probes WHERE enabled=1 ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// SetTargetPrimaryProbe chooses which probe decides a target's status. Zero
// hands it back to the local probe.
//
// The chosen probe must already be assigned this target. Pointing a target at
// a probe that is not allowed to measure it would produce a target whose
// status nobody can ever report, and the operator would have no way of seeing
// why from the page that sets it.
func (s *Store) SetTargetPrimaryProbe(targetID, probeID int64) error {
	if probeID != 0 {
		var local, enabled int
		err := s.cfg.QueryRow(`SELECT local, enabled FROM probes WHERE id=?`, probeID).Scan(&local, &enabled)
		if err == sql.ErrNoRows {
			return fmt.Errorf("unknown probe")
		}
		if err != nil {
			return err
		}
		if local == 1 {
			return fmt.Errorf("the local probe is chosen by leaving the primary unset")
		}
		if enabled != 1 {
			return fmt.Errorf("that probe is disabled")
		}
		var n int
		if err := s.cfg.QueryRow(
			`SELECT COUNT(*) FROM probe_targets WHERE probe_id=? AND target_id=?`,
			probeID, targetID).Scan(&n); err != nil {
			return err
		}
		if n == 0 {
			return fmt.Errorf("that probe is not assigned this target, so it cannot be its primary")
		}
	}
	res, err := s.cfg.Exec(`UPDATE targets SET primary_probe_id=? WHERE id=?`, probeID, targetID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("unknown target")
	}
	return nil
}

// SeenProbeFrom records that a probe just spoke to us, and from where. It is
// the network path's version of TouchProbe: the local probe reaches the
// service through a Unix socket and has no remote address worth recording, so
// its own path is left exactly as it was.
//
// The address is written on every call rather than only when it changes,
// because the row is being updated for last_seen_at regardless and a
// comparison would cost a read to save nothing.
func (s *Store) SeenProbeFrom(id int64, addr string) {
	s.cfg.Exec(`UPDATE probes SET last_seen_at=?, seen_address=? WHERE id=?`,
		time.Now().Unix(), strings.TrimSpace(addr), id)
}

// SetProbeAddress states where a probe measures from, overriding what the
// service observes. An empty value clears the override and hands the question
// back to observation.
func (s *Store) SetProbeAddress(id int64, addr string) error {
	addr = strings.TrimSpace(addr)
	if addr != "" && net.ParseIP(addr) == nil {
		return fmt.Errorf("%q is not an IP address", addr)
	}
	res, err := s.cfg.Exec(`UPDATE probes SET address=? WHERE id=?`, addr, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("unknown probe")
	}
	return nil
}

// ProbeByToken resolves a probe from the token it presented. The lookup is on
// the hash rather than on the token, exactly as a share link resolves: what
// the database holds is not the secret, so comparing on it gives away nothing
// worth timing.
//
// A disabled probe resolves to nothing, which is what makes disabling it stop
// the measurements rather than merely hide it. So does the local probe, which
// has no token and must stay reachable through the socket alone.
func (s *Store) ProbeByToken(tok string) (int64, bool) {
	if len(tok) < 16 {
		return 0, false
	}
	var id int64
	err := s.cfg.QueryRow(
		`SELECT id FROM probes WHERE token_hash=? AND enabled=1 AND local=0`,
		hashToken(tok)).Scan(&id)
	if err != nil {
		return 0, false
	}
	return id, true
}

// ------------------------------------------------------------------ API

func (a *API) probesGet(w http.ResponseWriter, r *http.Request, u *User) {
	list, err := a.store.Probes()
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	// The AS is read from the lookup service's cache rather than stored, so a
	// probe that is renumbered is right again on its own. A miss schedules
	// the lookup and leaves the field empty for now; nothing here waits on
	// the network while an operator is looking at a page.
	if a.asn != nil {
		for i := range list {
			addr := list[i].From
			if addr == "" {
				continue
			}
			if as, ok := a.asn.ASNOfIP(addr); ok {
				list[i].ASN = as
			} else {
				go a.asn.RefreshIPASN(addr)
			}
		}
	}
	writeJSON(w, map[string]any{"probes": list})
}

func (a *API) probesPost(w http.ResponseWriter, r *http.Request, u *User) {
	var in struct {
		Slug     string `json:"slug"`
		Name     string `json:"name"`
		Location string `json:"location"`
		Address  string `json:"address"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	// Checked before the probe exists rather than after: a malformed address
	// should not leave a half-made probe behind for the operator to clean up.
	if addr := strings.TrimSpace(in.Address); addr != "" && net.ParseIP(addr) == nil {
		writeErr(w, 400, fmt.Sprintf("%q is not an IP address", addr))
		return
	}
	p, err := a.store.CreateProbe(in.Slug, in.Name, in.Location)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if strings.TrimSpace(in.Address) != "" {
		if err := a.store.SetProbeAddress(p.ID, in.Address); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		p.Address = strings.TrimSpace(in.Address)
	}
	a.store.Audit(u, clientIP(r), "probe_create", "probe", p.Slug)
	writeJSON(w, p)
}

func (a *API) probeTokenPost(w http.ResponseWriter, r *http.Request, u *User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, 400, "bad probe id")
		return
	}
	tok, err := a.store.IssueProbeToken(id)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	a.store.Audit(u, clientIP(r), "probe_token_issue", "probe", strconv.FormatInt(id, 10))
	writeJSON(w, map[string]any{"token": tok})
}

func (a *API) probeTokenDelete(w http.ResponseWriter, r *http.Request, u *User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, 400, "bad probe id")
		return
	}
	if err := a.store.RevokeProbeToken(id); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	a.store.Audit(u, clientIP(r), "probe_token_revoke", "probe", strconv.FormatInt(id, 10))
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) probeTargetsGet(w http.ResponseWriter, r *http.Request, u *User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, 400, "bad probe id")
		return
	}
	set, err := a.store.ProbeTargetIDs(id)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	ids := make([]int64, 0, len(set))
	for tid := range set {
		ids = append(ids, tid)
	}
	sort.Slice(ids, func(i, j int) bool { return ids[i] < ids[j] })
	writeJSON(w, map[string]any{"targets": ids})
}

func (a *API) probeTargetsPut(w http.ResponseWriter, r *http.Request, u *User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, 400, "bad probe id")
		return
	}
	var in struct {
		Targets []int64 `json:"targets"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := a.store.SetProbeTargets(id, in.Targets); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	a.store.Audit(u, clientIP(r), "probe_targets", "probe", strconv.FormatInt(id, 10))
	w.WriteHeader(http.StatusNoContent)
}

// targetPrimaryPut chooses the probe that decides a target's status. It is a
// route of its own rather than a field on the target patch: it is a statement
// about probes, it is validated against the assignment table, and burying it
// among thirty measurement parameters would make it invisible.
func (a *API) targetPrimaryPut(w http.ResponseWriter, r *http.Request, u *User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, 400, "bad target id")
		return
	}
	var in struct {
		ProbeID int64 `json:"probe_id"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := a.store.SetTargetPrimaryProbe(id, in.ProbeID); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	a.store.Audit(u, clientIP(r), "target_primary_probe", "target", strconv.FormatInt(id, 10))
	w.WriteHeader(http.StatusNoContent)
}

func (a *API) probesPatch(w http.ResponseWriter, r *http.Request, u *User) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, 400, "bad probe id")
		return
	}
	var in struct {
		Enabled *bool   `json:"enabled"`
		Address *string `json:"address"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if in.Enabled == nil && in.Address == nil {
		writeErr(w, 400, "nothing to change")
		return
	}
	// A pointer rather than a string, so that clearing the override is
	// expressible: an absent field changes nothing, an empty one hands the
	// question back to what the service observes.
	if in.Address != nil {
		if err := a.store.SetProbeAddress(id, *in.Address); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		a.store.Audit(u, clientIP(r), "probe_address", "probe", strconv.FormatInt(id, 10))
	}
	if in.Enabled != nil {
		if err := a.store.SetProbeEnabled(id, *in.Enabled); err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		action := "probe_disable"
		if *in.Enabled {
			action = "probe_enable"
		}
		a.store.Audit(u, clientIP(r), action, "probe", strconv.FormatInt(id, 10))
	}
	w.WriteHeader(http.StatusNoContent)
}

// ------------------------------------------------------------------ CLI

// An operator on the host should not have to open the back-office to enrol a
// probe, if only because the first probe is often added while setting the
// machine up. These subcommands sit under "probe" beside the one that runs
// it, and go through exactly the same store calls as the API.
func cmdProbe(args []string) error {
	const usage = "usage: smokestack probe list|add|token|revoke [flags]"
	if len(args) == 0 {
		return fmt.Errorf("%s", usage)
	}
	fsn := flag.NewFlagSet("probe "+args[0], flag.ExitOnError)
	cfgPath := cliConfig(fsn)
	slug := fsn.String("slug", "", "probe slug")
	name := fsn.String("name", "", "display name")
	location := fsn.String("location", "", "where it measures from")
	address := fsn.String("address", "", "its public IP, when the observed one would be wrong (NAT, reverse proxy)")
	targets := fsn.String("targets", "", "comma-separated target ids this probe measures (targets only)")
	withToken := fsn.Bool("token", false, "issue a token straight away (add only)")
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

	// Resolving the slug here rather than in each branch keeps the three
	// commands that need an existing probe from repeating the lookup, and
	// gives all of them the same error for a slug nobody registered.
	bySlug := func() (int64, error) {
		if *slug == "" {
			return 0, fmt.Errorf("-slug is required")
		}
		var id int64
		err := store.cfg.QueryRow(`SELECT id FROM probes WHERE slug=?`, strings.TrimSpace(*slug)).Scan(&id)
		if err == sql.ErrNoRows {
			return 0, fmt.Errorf("no probe with slug %q", *slug)
		}
		return id, err
	}

	switch args[0] {
	case "list":
		list, err := store.Probes()
		if err != nil {
			return err
		}
		w := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(w, "SLUG\tNAME\tLOCATION\tFROM\tKIND\tSTATE\tTOKEN")
		for _, p := range list {
			kind, state, tok := "remote", "disabled", "none"
			if p.Local {
				kind = "local"
			}
			if p.Enabled {
				state = "enabled"
			}
			if p.HasToken {
				tok = "set"
			} else if p.Local {
				tok = "n/a"
			}
			from := p.From
			if from == "" {
				from = "—"
			} else if p.Address != "" {
				from += " (stated)"
			}
			fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", p.Slug, p.Name, p.Location, from, kind, state, tok)
		}
		return w.Flush()

	case "add":
		p, err := store.CreateProbe(*slug, *name, *location)
		if err != nil {
			return err
		}
		store.Audit(nil, "cli", "probe_create", "probe", p.Slug)
		if strings.TrimSpace(*address) != "" {
			if err := store.SetProbeAddress(p.ID, *address); err != nil {
				return err
			}
		}
		fmt.Printf("probe registered: %s\n", p.Slug)
		if !*withToken {
			fmt.Printf("it cannot send anything yet: run `smokestack probe token -slug %s` to issue one\n", p.Slug)
			return nil
		}
		tok, err := store.IssueProbeToken(p.ID)
		if err != nil {
			return err
		}
		store.Audit(nil, "cli", "probe_token_issue", "probe", strconv.FormatInt(p.ID, 10))
		printProbeToken(tok)
		return nil

	case "token":
		id, err := bySlug()
		if err != nil {
			return err
		}
		tok, err := store.IssueProbeToken(id)
		if err != nil {
			return err
		}
		store.Audit(nil, "cli", "probe_token_issue", "probe", strconv.FormatInt(id, 10))
		printProbeToken(tok)
		return nil

	case "targets":
		id, err := bySlug()
		if err != nil {
			return err
		}
		// An empty -targets clears the assignment, which is the same thing
		// the API means by an empty list: this probe measures nothing.
		ids := []int64{}
		for _, part := range strings.Split(*targets, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			n, err := strconv.ParseInt(part, 10, 64)
			if err != nil {
				return fmt.Errorf("%q is not a target id", part)
			}
			ids = append(ids, n)
		}
		if err := store.SetProbeTargets(id, ids); err != nil {
			return err
		}
		store.Audit(nil, "cli", "probe_targets", "probe", strconv.FormatInt(id, 10))
		if len(ids) == 0 {
			fmt.Printf("%s now measures nothing\n", *slug)
			return nil
		}
		fmt.Printf("%s now measures %d target(s)\n", *slug, len(ids))
		return nil

	case "revoke":
		id, err := bySlug()
		if err != nil {
			return err
		}
		if err := store.RevokeProbeToken(id); err != nil {
			return err
		}
		store.Audit(nil, "cli", "probe_token_revoke", "probe", strconv.FormatInt(id, 10))
		fmt.Printf("token revoked: %s can no longer send measurements\n", *slug)
		return nil
	}
	return fmt.Errorf("%s", usage)
}

// The token goes to stdout alone on its line, so that piping the command into
// a file or a secret store captures the token and nothing else. Everything
// said about it goes to stderr for the same reason.
func printProbeToken(tok string) {
	fmt.Fprintln(os.Stderr, "token issued. It is shown once and only its hash is stored, so copy it now:")
	fmt.Println(tok)
	fmt.Fprintln(os.Stderr, "set it as probe.token in the probe's configuration file.")
}
