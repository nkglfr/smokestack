package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// What people actually paste: a tab in front, a trailing space, a full URL,
// brackets around an IPv6 address, an invisible character from a web page.
func TestNormalizeHost(t *testing.T) {
	ok := []struct{ in, want string }{
		{"\t192.0.2.1", "192.0.2.1"},
		{"  192.0.2.1  ", "192.0.2.1"},
		{"192.0.2.1\n", "192.0.2.1"},
		{"\u00a0192.0.2.1", "192.0.2.1"}, // non-breaking space
		{"192.0.2.1\u200b", "192.0.2.1"}, // zero-width space
		{"[2001:db8::1]", "2001:db8::1"},
		{"2001:DB8::0001", "2001:db8::1"}, // canonical form
		{"https://example.net/path?x=1", "example.net"},
		{"EXAMPLE.NET.", "example.net"},
		{"ntp1.jussieu.fr ", "ntp1.jussieu.fr"},
		{"user@example.net", "example.net"},
		{"my_host.internal", "my_host.internal"},
	}
	for _, c := range ok {
		got, err := normalizeHost(c.in)
		if err != nil || got != c.want {
			t.Errorf("normalizeHost(%q) = %q, %v — expected %q", c.in, got, err, c.want)
		}
	}
	bad := []string{"", "   ", "1.1.1.1 8.8.8.8", "ping 192.0.2.1", "exemple..net",
		"-example.net", "exa mple.net,", "192.0.2.1;rm -rf /"}
	for _, in := range bad {
		if got, err := normalizeHost(in); err == nil {
			t.Errorf("normalizeHost(%q) should have been refused, got %q", in, got)
		}
	}
}

// The cleanup happens on creation and on update, so the API and the
// back-office both benefit, and a pasted "host:port" fills the port field.
func TestTargetHostCleanedOnSave(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cat, _ := store.CreateCategory("c", "C", "C", true)
	id, err := store.CreateTarget(&Target{CategoryID: cat, Slug: "a", Title: "  Spaced  ",
		Host: "\t 192.0.2.10 ", Proto: "icmp", IntervalS: 60, Packets: 10, SpacingMs: 100,
		TimeoutMs: 1000, Public: true, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	got, _ := store.TargetByID(id)
	if got.Host != "192.0.2.10" || got.Title != "Spaced" {
		t.Errorf("stored host %q, title %q", got.Host, got.Title)
	}
	got.Host = "  example.net:8443 "
	got.Proto = "tcp"
	if err := store.UpdateTarget(got); err != nil {
		t.Fatal(err)
	}
	after, _ := store.TargetByID(id)
	if after.Host != "example.net" || after.Port != 8443 {
		t.Errorf("host %q port %d — expected example.net and 8443", after.Host, after.Port)
	}
	// An impossible host is refused rather than measured forever.
	after.Host = "ping 192.0.2.1"
	if err := store.UpdateTarget(after); err == nil {
		t.Error("a host with a space should be refused")
	}
}

// A pasted URL must give its host, not the scheme: the host used to be
// split from its port before the scheme was removed.
func TestPastedURLKeepsTheHost(t *testing.T) {
	for _, c := range []struct {
		in, host string
		port     int
	}{
		{"https://example.net/page?a=1", "example.net", 0},
		{"http://example.net:8080/x", "example.net", 8080},
		{"example.net:8443", "example.net", 8443},
		{"[2001:db8::1]:443", "2001:db8::1", 443},
		{"\t https://EXAMPLE.net. ", "example.net", 0},
	} {
		tg := &Target{Host: c.in, Proto: "tcp", Port: 0, IntervalS: 60, Packets: 10,
			SpacingMs: 100, TimeoutMs: 1000}
		if c.port == 0 {
			tg.Port = 443 // a TCP target needs a port; the host is what we check
		}
		if err := checkTarget(tg); err != nil {
			t.Errorf("%q refused: %v", c.in, err)
			continue
		}
		if tg.Host != c.host {
			t.Errorf("%q gave the host %q, expected %q", c.in, tg.Host, c.host)
		}
		if c.port != 0 && tg.Port != c.port {
			t.Errorf("%q gave the port %d, expected %d", c.in, tg.Port, c.port)
		}
	}
}

// A TCP target without a port must say what to do, not repeat Go's
// "missing port in address".
func TestTCPTargetWithoutPort(t *testing.T) {
	p := &Prober{res: newResolver(), failing: map[int64]string{}, running: map[int64]bool{}}
	_, msg := p.runTCP(&Target{Title: "X", Host: "192.0.2.1", Proto: "tcp", Port: 0,
		Packets: 1, SpacingMs: 100, TimeoutMs: 200})
	if !strings.Contains(msg, "no port") || !strings.Contains(msg, "443") {
		t.Errorf("unhelpful message: %q", msg)
	}
	// An HTTP status is never looked at: a service answering 403 is up.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(403)
	}))
	defer srv.Close()
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(srv.URL, "http://"))
	n, _ := strconv.Atoi(port)
	rtts, msg := p.runTCP(&Target{Title: "403", Host: host, Proto: "tcp", Port: n,
		Packets: 3, SpacingMs: 50, TimeoutMs: 1000})
	if msg != "" || len(rtts) != 3 {
		t.Errorf("a service answering 403 must be measured: %d samples, %q", len(rtts), msg)
	}
}
