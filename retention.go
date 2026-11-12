package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"
)

// Retention per resolution, as an instance setting (#115).
//
// The cascade's `keep` field was the only answer to "how long is this kept",
// compiled in, and the only setting near it was keep_days on a target, which
// cuts every tier at the same date. So an instance could not drop old
// 1-minute data while keeping the hourly data, and an instance with the disk
// for it could not keep 1-minute data beyond a year.
//
// That choice stops being a detail once an instance measures from several
// places. With fifty probes over twelve hundred targets, roll_1m is most of
// the database — about 3.2e10 of 3.9e10 rows after a year — because every
// probe writes its own row per minute per target. A year of history from
// every probe is affordable at one point per hour and not at one per minute,
// and until now there was no way to say so.

// RetentionConfig holds one value per tier, in days. 0 means unlimited.
//
// Days rather than seconds because this is an operator's decision, written in
// the unit they think in, and because a tier kept for hours is not a choice
// anyone needs: the floors below are already above that.
type RetentionConfig struct {
	Samples int `json:"samples"`
	Roll1m  int `json:"roll_1m"`
	Roll5m  int `json:"roll_5m"`
	Roll1h  int `json:"roll_1h"`
	Roll1d  int `json:"roll_1d"`
}

// defaultRetention is today's compiled-in cascade, to the day. An upgrade
// therefore deletes nothing: the setting starts by describing what the
// instance was already doing.
func defaultRetention() RetentionConfig {
	return RetentionConfig{Samples: 2, Roll1m: 365, Roll5m: 1095}
}

// retentionFloors is the shortest each tier may be kept, in days, and it is
// not a matter of taste: RollupTick rebuilds each tier from the one below it
// over a fixed window.
//
//	windows := []int64{2 * 3600, 6 * 3600, 3 * 86400, 3 * 86400}
//
// A source kept for less than the window that reads it is purged before it
// has been rolled up, which loses measurements rather than merely shortening
// history. The first two windows are hours, so one day clears them with room
// to spare; the next two are three days and set the floor exactly. roll_1d is
// last and nothing rolls up from it, so it only has to be a day.
//
// Expressed by field order, matching retentionTiers below.
var retentionFloors = [5]int{1, 1, 3, 3, 1}

// retentionTiers names the tables in cascade order, so that this file and the
// cascade cannot drift into disagreeing about which value belongs to which
// table.
var retentionTiers = [5]string{"samples", "roll_1m", "roll_5m", "roll_1h", "roll_1d"}

// Days returns the five values in cascade order.
func (c RetentionConfig) Days() [5]int {
	return [5]int{c.Samples, c.Roll1m, c.Roll5m, c.Roll1h, c.Roll1d}
}

// withDays is the inverse, for a caller that worked in the array form.
func retentionFromDays(d [5]int) RetentionConfig {
	return RetentionConfig{Samples: d[0], Roll1m: d[1], Roll5m: d[2], Roll1h: d[3], Roll1d: d[4]}
}

// effectiveDays treats 0 as unlimited, so the ordering rule below can compare
// tiers with plain arithmetic instead of special-casing at every step.
const unlimitedDays = 1 << 30

func effectiveDays(v int) int {
	if v == 0 {
		return unlimitedDays
	}
	return v
}

// Validate refuses a configuration rather than accepting one that would lose
// measurements, and says which value is wrong and why. The caller keeps the
// previous configuration on an error: a half-applied retention is worse than
// a rejected one, because the damage is done by the next purge.
func (c RetentionConfig) Validate() error {
	d := c.Days()
	for i, v := range d {
		if v < 0 {
			return fmt.Errorf("%s: %d days is not a duration", retentionTiers[i], v)
		}
		if v == 0 {
			continue // unlimited, which no floor can be shorter than
		}
		if v < retentionFloors[i] {
			return fmt.Errorf(
				"%s: %d day(s) is shorter than the %d day(s) the next rollup reads "+
					"from it, so the rows would be deleted before being rolled up",
				retentionTiers[i], v, retentionFloors[i])
		}
	}
	// A coarser tier kept for less time than a finer one inverts the point of
	// the cascade. It is not a hole — tierFrom only ever walks coarser, so
	// nothing vanishes from the display — it is paying more to keep the
	// expensive detail longer than the cheap summary. Every row in roll_1m
	// stands for sixty in samples, so this is almost always a transposed
	// pair of numbers, and refusing it costs an operator one error message
	// where accepting it costs them disk for months.
	for i := 1; i < len(d); i++ {
		if effectiveDays(d[i]) < effectiveDays(d[i-1]) {
			return fmt.Errorf(
				"%s (%s) cannot be kept for less time than %s (%s): the coarser "+
					"tier is what answers the older windows",
				retentionTiers[i], humanDays(d[i]),
				retentionTiers[i-1], humanDays(d[i-1]))
		}
	}
	return nil
}

func humanDays(v int) string {
	if v == 0 {
		return "unlimited"
	}
	if v == 1 {
		return "1 day"
	}
	return fmt.Sprintf("%d days", v)
}

// RetentionConfig reads the instance setting, falling back to the compiled
// defaults. A stored value that no longer validates — written by an older
// version, or edited in the database by hand — is discarded whole rather than
// repaired field by field, because a repaired retention is a number nobody
// chose.
func (s *Store) RetentionConfig() RetentionConfig {
	def := defaultRetention()
	raw := s.Setting("retention", "")
	if raw == "" {
		return def
	}
	var v RetentionConfig
	if json.Unmarshal([]byte(raw), &v) != nil {
		return def
	}
	if v.Validate() != nil {
		return def
	}
	return v
}

// SetRetention validates and stores. It does not purge: the next purge tick
// applies it, which is also what makes the preview below honest about when
// the deletion happens.
func (s *Store) SetRetention(c RetentionConfig) error {
	if err := c.Validate(); err != nil {
		return err
	}
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return s.SetSetting("retention", string(b))
}

// RetentionChange is what a given tier would lose, for the back-office to
// show before anything is saved.
type RetentionChange struct {
	Table string `json:"table"`
	From  string `json:"from"`
	To    string `json:"to"`
	// Rows that the next purge would delete under the new value and would
	// not have deleted under the old one.
	Rows int64 `json:"rows"`
	// Oldest kept, as an epoch, under the new value. 0 when unlimited.
	CutAt int64 `json:"cut_at,omitempty"`
	// Set when this tier stops being able to rebuild the one above it over
	// history. Shortening a fine tier deletes rows now and takes away the
	// only source RollupBackfill has for that period, for ever.
	BreaksBackfillFor string `json:"breaks_backfill_for,omitempty"`
}

// RetentionPreview says what saving `next` would delete, so that an operator
// sees the cost before paying it. Deletion is the one thing in this program
// that cannot be undone.
func (s *Store) RetentionPreview(next RetentionConfig, now int64) ([]RetentionChange, error) {
	if err := next.Validate(); err != nil {
		return nil, err
	}
	cur := s.RetentionConfig().Days()
	nd := next.Days()
	out := []RetentionChange{}
	for i := range nd {
		// Unchanged, or loosened: nothing is deleted that was not already.
		if nd[i] == cur[i] || nd[i] == 0 {
			continue
		}
		if cur[i] != 0 && nd[i] >= cur[i] {
			continue
		}
		cut := now - int64(nd[i])*86400
		var rows int64
		q := fmt.Sprintf(`SELECT COUNT(*) FROM %s WHERE bucket < ?`, retentionTiers[i])
		args := []any{cut}
		if cur[i] != 0 {
			// Only what the old value was still keeping counts as a loss.
			q += ` AND bucket >= ?`
			args = append(args, now-int64(cur[i])*86400)
		}
		if err := s.mx.QueryRow(q, args...).Scan(&rows); err != nil {
			return nil, err
		}
		ch := RetentionChange{
			Table: retentionTiers[i],
			From:  humanDays(cur[i]),
			To:    humanDays(nd[i]),
			Rows:  rows,
			CutAt: cut,
		}
		if i+1 < len(nd) {
			ch.BreaksBackfillFor = retentionTiers[i+1]
		}
		out = append(out, ch)
	}
	return out, nil
}

// String is for the command line, where the five values are read at a glance.
func (c RetentionConfig) String() string {
	d := c.Days()
	parts := make([]string, 0, len(d))
	for i, v := range d {
		parts = append(parts, fmt.Sprintf("%s=%s", retentionTiers[i], humanDays(v)))
	}
	return strings.Join(parts, "  ")
}

// cmdRetention shows the retention, and changes it only when asked twice:
// once by naming values, once with -apply.
//
// The confirmation is not ceremony. Shortening a tier deletes rows at the
// next purge and cannot be undone, and it also takes away the only source
// RollupBackfill has for rebuilding the tier above it over that period. An
// operator who types a number should see what it costs before it costs it,
// and on a command line the only way to show them is to stop.
func cmdRetention(args []string) error {
	fsn := flag.NewFlagSet("retention", flag.ExitOnError)
	cfgPath := cliConfig(fsn)
	apply := fsn.Bool("apply", false, "save the values (without it, only the effect is shown)")
	// -1 means "not named on the command line", so that 0 keeps its meaning
	// of unlimited rather than being indistinguishable from an omitted flag.
	const unset = -1
	flags := [5]*int{
		fsn.Int("samples", unset, "days to keep raw samples (0 = unlimited)"),
		fsn.Int("roll-1m", unset, "days to keep 1-minute buckets (0 = unlimited)"),
		fsn.Int("roll-5m", unset, "days to keep 5-minute buckets (0 = unlimited)"),
		fsn.Int("roll-1h", unset, "days to keep hourly buckets (0 = unlimited)"),
		fsn.Int("roll-1d", unset, "days to keep daily buckets (0 = unlimited)"),
	}
	fsn.Parse(args)

	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	store, err := OpenStore(cfg.DataDir)
	if err != nil {
		return err
	}
	defer store.Close()

	cur := store.RetentionConfig()
	next := cur.Days()
	named := false
	for i, f := range flags {
		if *f != unset {
			next[i] = *f
			named = true
		}
	}
	if !named {
		fmt.Println(cur.String())
		fmt.Fprintln(os.Stderr,
			"\nName one or more of -samples -roll-1m -roll-5m -roll-1h -roll-1d to change it.\n"+
				"Without -apply, the effect is shown and nothing is saved.")
		return nil
	}

	want := retentionFromDays(next)
	if err := want.Validate(); err != nil {
		return err
	}
	changes, err := store.RetentionPreview(want, time.Now().Unix())
	if err != nil {
		return err
	}

	fmt.Println("from:", cur.String())
	fmt.Println("  to:", want.String())
	if len(changes) == 0 {
		fmt.Println("\nNothing is deleted by this change.")
	} else {
		fmt.Println("\nThe next purge would delete, and this cannot be undone:")
		for _, c := range changes {
			fmt.Printf("  %-8s %-10s -> %-10s  %d rows older than %s\n",
				c.Table, c.From, c.To, c.Rows,
				time.Unix(c.CutAt, 0).UTC().Format("2006-01-02 15:04 UTC"))
			if c.BreaksBackfillFor != "" {
				fmt.Printf("           and %s can no longer be rebuilt over that period, ever\n",
					c.BreaksBackfillFor)
			}
		}
	}
	if !*apply {
		fmt.Println("\nNothing was saved. Add -apply to save it.")
		return nil
	}
	if err := store.SetRetention(want); err != nil {
		return err
	}
	store.Audit(nil, "cli", "retention_set", "instance", want.String())
	fmt.Println("\nsaved")
	return nil
}

// ----------------------------------------------------------------- API

func (a *API) retentionGet(w http.ResponseWriter, r *http.Request, u *User) {
	// The floors and the defaults go with the values, so the form can refuse
	// a number before a round trip and name the same reason the server would.
	floors := map[string]int{}
	for i, t := range retentionTiers {
		floors[t] = retentionFloors[i]
	}
	writeJSON(w, map[string]any{
		"config":   a.store.RetentionConfig(),
		"defaults": defaultRetention(),
		"floors":   floors,
		"tiers":    retentionTiers,
	})
}

// retentionPreview answers what saving a configuration would delete, without
// saving it. A separate route rather than a flag on the PUT, so that the
// question "what would this cost" cannot be answered by a request that also
// does it.
func (a *API) retentionPreview(w http.ResponseWriter, r *http.Request, u *User) {
	var c RetentionConfig
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	changes, err := a.store.RetentionPreview(c, time.Now().Unix())
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	cachePrivate(w)
	writeJSON(w, map[string]any{"changes": changes})
}

func (a *API) retentionPut(w http.ResponseWriter, r *http.Request, u *User) {
	var c RetentionConfig
	if err := json.NewDecoder(r.Body).Decode(&c); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if err := a.store.SetRetention(c); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	a.store.Audit(u, clientIP(r), "retention_set", "instance", c.String())
	writeJSON(w, map[string]any{"config": a.store.RetentionConfig()})
}
