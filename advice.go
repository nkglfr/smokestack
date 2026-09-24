package main

import (
	"fmt"
	"net/http"
	"strconv"
	"time"
)

// A target can be right and still measure badly. The three usual causes look
// nothing alike in the data, so the instance can tell them apart itself
// instead of leaving the operator to guess:
//
//   - a destination that rate-limits ICMP loses one or two packets per burst,
//     nearly every burst, with a stable latency;
//   - a path or a destination that is really down loses whole passes;
//   - a rotating name mixes machines, which is detected elsewhere.
//
// The advice is computed on demand and never applied on its own: changing
// the packet count changes the resolution of the loss figure, and changing
// the interval changes the shape of the history.

type passStats struct {
	Passes     int `json:"passes"`
	Sent       int `json:"sent"`
	Lost       int `json:"lost"`
	WithLoss   int `json:"passes_with_loss"`
	Complete   int `json:"passes_fully_lost"`
	MaxLostOne int `json:"max_lost_in_one_pass"`
}

// PassStats sums up the raw passes of a target over a window.
func (s *Store) PassStats(targetID, from, to int64) (passStats, error) {
	var st passStats
	rows, err := s.mx.Query(`SELECT sent,lost FROM samples
	                         WHERE target_id=? AND bucket>=? AND bucket<=?`, targetID, from, to)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var sent, lost int
		if err := rows.Scan(&sent, &lost); err != nil {
			return st, err
		}
		st.Passes++
		st.Sent += sent
		st.Lost += lost
		if lost > 0 {
			st.WithLoss++
		}
		if sent > 0 && lost == sent {
			st.Complete++
		}
		if lost > st.MaxLostOne {
			st.MaxLostOne = lost
		}
	}
	return st, rows.Err()
}

type suggestion struct {
	Label string         `json:"label"`
	Why   string         `json:"why"`
	Patch map[string]any `json:"patch"`
}

type advice struct {
	Verdict     string       `json:"verdict"`
	Detail      string       `json:"detail"`
	Stats       passStats    `json:"stats"`
	LossPct     float64      `json:"loss_pct"`
	Suggestions []suggestion `json:"suggestions,omitempty"`
}

// adviseTarget reads the shape of the loss, not only its amount.
func adviseTarget(t *Target, st passStats, addrCount int) advice {
	a := advice{Stats: st}
	if st.Sent > 0 {
		a.LossPct = float64(st.Lost) * 100 / float64(st.Sent)
	}
	burst := t.Packets*t.SpacingMs + t.TimeoutMs
	gentle := map[string]any{"packets": 10, "spacing_ms": 300}

	// The burst crowding its interval is worth saying whatever the loss is.
	if t.IntervalS > 0 && float64(burst) > 0.6*float64(t.IntervalS)*1000 {
		a.Suggestions = append(a.Suggestions, suggestion{
			Label: "Shorten the burst",
			Why: fmt.Sprintf("the burst lasts %.1f s of a %d s interval, leaving little room; "+
				"10 packets spaced by 300 ms measure just as well",
				float64(burst)/1000, t.IntervalS),
			Patch: gentle,
		})
	}

	switch {
	case st.Passes < 10:
		a.Verdict = "not enough data"
		a.Detail = "Come back once the target has been measured for a while."
		return a

	case st.Lost == 0:
		a.Verdict = "clean"
		a.Detail = fmt.Sprintf("No loss over %d passes. Nothing to change.", st.Passes)
		return a

	// Whole passes lost: the destination or the path was down. Nothing to
	// tune, and tuning would hide it.
	case st.Complete*2 >= st.WithLoss:
		a.Verdict = "real outages"
		a.Detail = fmt.Sprintf("%d of the %d passes with loss lost every packet: "+
			"the destination or the path was unreachable, which is a real measurement. "+
			"Changing the settings would only hide it.", st.Complete, st.WithLoss)
		if addrCount > 1 {
			a.Detail += fmt.Sprintf(" This name also answered from %d addresses, so some passes "+
				"may have gone to a server that ignores ICMP.", addrCount)
		}
		return a

	// A hard ceiling of one or two packets per burst, over many bursts, is
	// the signature of a limiter: path loss eventually takes several
	// packets at once. The share of affected bursts is not the signal — a
	// destination limiting ICMP to one per second only shows up in a tenth
	// of the bursts.
	case st.MaxLostOne <= max2(1, t.Packets/10) && st.WithLoss >= 5:
		a.Verdict = "rate limiting"
		a.Detail = fmt.Sprintf("%d of %d passes lost at most %d packet(s) out of %d, "+
			"never a whole pass: the destination answers only so many ICMP requests per second. "+
			"That is its behaviour, not your network.",
			st.WithLoss, st.Passes, st.MaxLostOne, t.Packets)
		if t.Packets > 10 || t.SpacingMs < 300 {
			a.Suggestions = append(a.Suggestions, suggestion{
				Label: "Use a gentler burst",
				Why: "fewer packets, further apart: the median and the percentiles stay as good, " +
					"and the limiter stops showing up as loss",
				Patch: gentle,
			})
		} else {
			a.Detail += " The burst is already gentle; the remaining loss is the destination's own limit."
		}
		return a

	default:
		a.Verdict = "scattered loss"
		a.Detail = fmt.Sprintf("%.2f %% loss spread over %d of %d passes, up to %d packets at once: "+
			"this looks like the path rather than a limiter. Worth keeping as it is.",
			a.LossPct, st.WithLoss, st.Passes, st.MaxLostOne)
		return a
	}
}

func max2(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func (a *API) AdviceRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/admin/targets/{id}/advice", a.auth(a.targetAdvice))
}

func (a *API) targetAdvice(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, 400, "invalid identifier")
		return
	}
	t, err := a.store.TargetByID(id)
	if err != nil {
		writeErr(w, 404, "target not found")
		return
	}
	now := time.Now().Unix()
	st, err := a.store.PassStats(id, now-24*3600, now)
	if err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	addrs := a.store.TargetAddresses(now - 24*3600)[id]
	writeJSON(w, adviseTarget(t, st, len(addrs)))
}
