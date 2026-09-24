package main

import (
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// The service log goes to stdout, which journald or Docker collects. That is
// fine on a server you have a shell on, and useless from a phone or when the
// person who needs the answer is not the one with SSH. So the last lines are
// also kept in memory and shown in the back-office.
//
// In memory only, and bounded: a log is not something to store in the
// database, and a full disk must never be caused by a chatty target.

const logRingSize = 500

type logRing struct {
	mu    sync.Mutex
	lines []string
	next  int
	full  bool
}

var serviceLog = &logRing{lines: make([]string, logRingSize)}

// Write receives what the standard logger writes, keeps it, and passes it on
// unchanged so journalctl and docker logs still see everything.
func (r *logRing) Write(p []byte) (int, error) {
	for _, line := range strings.Split(strings.TrimRight(string(p), "\n"), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		r.mu.Lock()
		r.lines[r.next] = line
		r.next = (r.next + 1) % logRingSize
		if r.next == 0 {
			r.full = true
		}
		r.mu.Unlock()
	}
	return len(p), nil
}

// Lines returns the kept lines, oldest first.
func (r *logRing) Lines() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, logRingSize)
	if r.full {
		out = append(out, r.lines[r.next:]...)
	}
	out = append(out, r.lines[:r.next]...)
	return out
}

// captureLog tees the standard logger into the ring buffer.
func captureLog(w io.Writer) {
	log.SetOutput(io.MultiWriter(w, serviceLog))
}

func (a *API) LogRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/admin/logs", a.need(RoleAdmin, a.logsGet))
}

func (a *API) logsGet(w http.ResponseWriter, r *http.Request, u *User) {
	lines := serviceLog.Lines()
	// The probe runs in its own process when isolated: its log is in
	// journalctl or docker logs, not here, and the page says so.
	writeJSON(w, map[string]any{
		"lines":    lines,
		"kept":     logRingSize,
		"probe":    a.probeInProcess,
		"now":      time.Now().Unix(),
		"hostname": a.store.Site().Location,
	})
}
