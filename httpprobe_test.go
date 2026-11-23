package main

import (
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func httpTarget(srv *httptest.Server, path string, packets int) *Target {
	u := srv.URL // http://127.0.0.1:PORT
	host, port, _ := net.SplitHostPort(strings.TrimPrefix(u, "http://"))
	p, _ := strconv.Atoi(port)
	return &Target{Host: host, Proto: "http", Port: p, HTTPPath: path,
		HTTPScheme: "http", Packets: packets, SpacingMs: 1, TimeoutMs: 1000,
		IntervalS: 60}
}

func TestHTTPTimesTheFirstByte(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	p := &Prober{res: newResolver()}
	rtts, errStr := p.runHTTP(httpTarget(srv, "/health", 3))
	if errStr != "" || len(rtts) != 3 {
		t.Fatalf("got %d measurements and %q, want 3 and no error", len(rtts), errStr)
	}
}

// The point of timing the first byte rather than the download: a slow body
// must not move the figure. A handler that answers at once and then dribbles
// for half a second has to read as fast, because the path is fast.
func TestHTTPIgnoresHowLongTheBodyTakes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "4")
		w.WriteHeader(200)
		w.(http.Flusher).Flush()
		time.Sleep(400 * time.Millisecond)
		w.Write([]byte("slow"))
	}))
	defer srv.Close()

	p := &Prober{res: newResolver()}
	rtts, errStr := p.runHTTP(httpTarget(srv, "/", 1))
	if errStr != "" || len(rtts) != 1 {
		t.Fatalf("got %d measurements and %q", len(rtts), errStr)
	}
	if rtts[0] > 300_000 {
		t.Errorf("%.0f µs: the body's 400 ms leaked into the measurement, which is the "+
			"whole thing this probe is built not to do", rtts[0])
	}
}

// Every packet of a burst must measure the same quantity. With a connection
// reused, the second request would skip resolution, connection and handshake,
// and the spread across the packets would be the difference between two
// different measurements rather than the variability of one.
func TestHTTPDoesNotReuseTheConnection(t *testing.T) {
	var mu struct {
		conns map[string]bool
	}
	mu.conns = map[string]bool{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.conns[r.RemoteAddr] = true
		w.Write([]byte("ok"))
	}))
	defer srv.Close()

	p := &Prober{res: newResolver()}
	if _, errStr := p.runHTTP(httpTarget(srv, "/", 3)); errStr != "" {
		t.Fatalf("the pass failed: %s", errStr)
	}
	if len(mu.conns) < 3 {
		t.Errorf("three packets arrived on %d connection(s): keep-alives are on, so packets "+
			"2 and 3 skipped the handshake and measured something else", len(mu.conns))
	}
}

func TestHTTPStatusHandling(t *testing.T) {
	cases := []struct {
		code    int
		measure bool
		says    string
	}{
		{200, true, ""},
		{204, true, ""},
		{301, true, ""}, // a redirect is an answer; the chain is not followed
		{302, true, ""},
		{404, false, "404"},
		{500, false, "500"},
		{503, false, "503"},
	}
	for _, c := range cases {
		t.Run(fmt.Sprint(c.code), func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if c.code >= 300 && c.code < 400 {
					w.Header().Set("Location", "/elsewhere")
				}
				w.WriteHeader(c.code)
			}))
			defer srv.Close()

			p := &Prober{res: newResolver()}
			rtts, errStr := p.runHTTP(httpTarget(srv, "/", 1))
			if c.measure {
				if len(rtts) != 1 || errStr != "" {
					t.Errorf("%d: got %d measurements and %q, want 1 and no error",
						c.code, len(rtts), errStr)
				}
				return
			}
			if len(rtts) != 0 {
				t.Errorf("%d: measured %d round trips from a server not serving the path",
					c.code, len(rtts))
			}
			if !strings.Contains(errStr, c.says) {
				t.Errorf("%d: the reason is %q and does not name the status", c.code, errStr)
			}
		})
	}
}

// A redirect must not be followed. Following it would time a second request,
// possibly to another host, and call the sum one latency.
func TestHTTPDoesNotFollowARedirect(t *testing.T) {
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		if r.URL.Path == "/start" {
			http.Redirect(w, r, "/end", http.StatusFound)
			return
		}
		w.Write([]byte("end"))
	}))
	defer srv.Close()

	p := &Prober{res: newResolver()}
	rtts, errStr := p.runHTTP(httpTarget(srv, "/start", 1))
	if len(rtts) != 1 || errStr != "" {
		t.Fatalf("got %d measurements and %q", len(rtts), errStr)
	}
	if hits != 1 {
		t.Errorf("the server was hit %d times: the redirect was followed, so the figure is "+
			"two requests added together", hits)
	}
}

func TestHTTPSilenceAndRefusal(t *testing.T) {
	// Nothing listening: a refusal, and it should say so rather than read as
	// a slow network.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()

	p := &Prober{res: newResolver()}
	tg := &Target{Host: "127.0.0.1", Proto: "http", Port: port, HTTPScheme: "http",
		Packets: 1, SpacingMs: 1, TimeoutMs: 300, IntervalS: 60}
	rtts, errStr := p.runHTTP(tg)
	if len(rtts) != 0 {
		t.Errorf("a closed port produced %d measurements", len(rtts))
	}
	if !strings.Contains(errStr, "listening") {
		t.Errorf("the reason is %q, which does not say nothing is listening", errStr)
	}
}

func TestHTTPTimeoutIsNotAMeasurement(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(400 * time.Millisecond)
		w.Write([]byte("late"))
	}))
	defer srv.Close()

	tg := httpTarget(srv, "/", 1)
	tg.TimeoutMs = 100
	p := &Prober{res: newResolver()}
	rtts, errStr := p.runHTTP(tg)
	if len(rtts) != 0 {
		t.Errorf("a server that answered after the timeout produced %d measurements", len(rtts))
	}
	if errStr == "" {
		t.Error("a timeout was recorded without a reason")
	}
}

func TestHTTPURLFromTheTargetsFields(t *testing.T) {
	cases := []struct {
		name string
		t    Target
		want string
		bad  bool
	}{
		{"an explicit scheme and path",
			Target{Host: "example.net", Port: 8080, HTTPScheme: "http", HTTPPath: "/health"},
			"http://example.net:8080/health", false},
		{"port 443 means https without being told",
			Target{Host: "example.net", Port: 443},
			"https://example.net:443/", false},
		{"a TLS port from the breakdown's own list",
			Target{Host: "example.net", Port: 8443},
			"https://example.net:8443/", false},
		{"no port, no scheme: plain http on 80",
			Target{Host: "example.net"},
			"http://example.net:80/", false},
		{"no port with https stated: 443",
			Target{Host: "example.net", HTTPScheme: "https"},
			"https://example.net:443/", false},
		{"a path without its leading slash",
			Target{Host: "example.net", Port: 80, HTTPPath: "health"},
			"http://example.net:80/health", false},
		{"a host given with a port keeps only the host",
			Target{Host: "example.net:9999", Port: 80},
			"http://example.net:80/", false},
		{"an unknown scheme is refused rather than guessed",
			Target{Host: "example.net", HTTPScheme: "gopher"}, "", true},
		{"no host at all", Target{Port: 80}, "", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			u, err := httpURL(&c.t)
			if c.bad {
				if err == nil {
					t.Errorf("accepted, giving %s", u)
				}
				return
			}
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			if u.String() != c.want {
				t.Errorf("got %s, want %s", u, c.want)
			}
		})
	}
}

func TestHTTPIsAKnownProtocol(t *testing.T) {
	if !validProto("http") {
		t.Fatal("http is not a known protocol, so no target could be created with it")
	}
}
