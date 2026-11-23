package main

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// HTTP probe: time to first byte.
//
// What is timed is the request going out and the response's headers coming
// back — not the transfer of the body. That choice is the whole design, so it
// is worth why rather than what.
//
// A full download, which is what SmokePing's HTTP probes measure, is
// dominated by the weight of the page and the bandwidth to it rather than by
// the path. Two megabytes over ten megabits take 1.6 seconds whatever the
// round trip is. Three things follow, and all three break something this
// program is built to do: the figure stops being comparable with its own past
// the day the page grows, so a year of history becomes unreadable; the smoke
// band, which everywhere else here is the spread across the packets of one
// pass, would measure the variance of bandwidth instead; and a burst of ten
// full downloads every ten seconds, per target and per probe, makes a probe
// into a load generator against somebody else's server.
//
// Time to first byte keeps an HTTP target on the same axis as an ICMP one,
// which is what lets a page put them side by side. What it cannot see is a
// server that answers its first byte quickly and then stalls; measuring that
// is a second figure rather than this one, and it is not here.

const (
	httpDefaultPath = "/"
	// A hard ceiling on the headers alone. A server that sends an unbounded
	// header stream would otherwise hold the probe for its whole timeout and
	// read as latency rather than as the fault it is.
	httpMaxHeaderBytes = 64 << 10
)

// runHTTP times one request per packet of the pass.
func (p *Prober) runHTTP(t *Target) ([]float64, string) {
	timeout := time.Duration(t.TimeoutMs) * time.Millisecond
	spacing := time.Duration(t.SpacingMs) * time.Millisecond

	target, err := httpURL(t)
	if err != nil {
		return nil, err.Error()
	}
	// Resolved once per pass, as every other protocol here does: a name
	// looked up inside each request's timeout makes a hesitating resolver
	// read as loss on every named target at once.
	ip, err := p.targetIP(t)
	if err != nil {
		return nil, err.Error()
	}
	addr := net.JoinHostPort(ip.String(), target.Port())

	client := httpProbeClient(addr, timeout)
	defer client.CloseIdleConnections()

	var out []float64
	var lastErr string
	for i := 0; i < t.Packets; i++ {
		rtt, why := httpOnce(client, target, timeout)
		if why != "" {
			lastErr = why
		} else {
			out = append(out, rtt)
		}
		if i < t.Packets-1 {
			time.Sleep(spacing)
		}
	}
	if len(out) == 0 {
		return nil, lastErr
	}
	return out, ""
}

// httpOnce is one measurement: out to the first byte of the response and no
// further.
func httpOnce(client *http.Client, u *url.URL, timeout time.Duration) (float64, string) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return 0, err.Error()
	}
	// Identifies the probe without saying anything about who runs it.
	req.Header.Set("User-Agent", "smokestack/"+Version)
	// Asking for no compression keeps the measurement about the server and
	// the path: whether it chose to gzip changes how soon the first byte
	// leaves, and that is its decision rather than a property of the link.
	req.Header.Set("Accept-Encoding", "identity")

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, httpProbeError(err, u)
	}
	// Do returns once the response headers are in, which is the measurement.
	// The body is not read: closing without reading drops the connection,
	// which is what keeps this from being a download.
	rtt := float64(time.Since(start).Microseconds())
	resp.Body.Close()

	// A redirect is an answer. Following it would time a second request, to
	// possibly another host, and call the sum one latency — and it would
	// raise the question of whether to follow https to http, which is not a
	// question a latency measurement should be answering.
	if resp.StatusCode >= 400 {
		return 0, fmt.Sprintf("the server answered %s: reachable, and not serving this path",
			resp.Status)
	}
	return rtt, ""
}

// httpProbeClient dials the address this pass resolved while leaving the URL
// its name, so the Host header and the TLS server name stay what the operator
// typed. Keep-alives are off on purpose: with a connection reused, the second
// packet of a burst would skip resolution, connection and handshake and
// measure a different quantity from the first, and the spread across the
// packets — the smoke band — would be the difference between those two
// quantities rather than the variability of anything.
func httpProbeClient(addr string, timeout time.Duration) *http.Client {
	return &http.Client{
		// Redirects are answers here, so the chain stops at the first one.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
		Transport: &http.Transport{
			DisableKeepAlives:      true,
			DisableCompression:     true,
			MaxResponseHeaderBytes: httpMaxHeaderBytes,
			// TLS is verified, and TestHTTPRefusesAnUntrustedCertificate
			// holds that there: a latency figure from a connection that
			// accepted any certificate measures a different path from the
			// one a visitor takes.
			//
			// The 1.2 floor is not separately tested, and cannot be with
			// what this probe offers: isolating a version refusal from a
			// certificate refusal would mean trusting the test server's
			// authority, which means a trust-roots option this must not
			// have. A test that asserted the refusal without isolating it
			// would pass whether the floor were set or not.
			TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
			TLSHandshakeTimeout: timeout,
			DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				d := net.Dialer{Timeout: timeout}
				return d.DialContext(ctx, network, addr)
			},
		},
	}
}

// httpURL builds the address to request from the target's fields. The scheme
// comes from http_scheme when the operator stated one, and otherwise from the
// port, the same way the response-time breakdown decides whether to expect a
// handshake.
func httpURL(t *Target) (*url.URL, error) {
	host := hostOnly(t.Host)
	if host == "" {
		return nil, fmt.Errorf("this target has no host to request")
	}
	port := t.Port
	scheme := strings.ToLower(strings.TrimSpace(t.HTTPScheme))
	switch scheme {
	case "http", "https":
	case "":
		if port == 443 || tlsPorts[port] {
			scheme = "https"
		} else {
			scheme = "http"
		}
	default:
		return nil, fmt.Errorf("unknown scheme %q: use http or https, or leave it to the port", scheme)
	}
	if port <= 0 {
		if scheme == "https" {
			port = 443
		} else {
			port = 80
		}
	}
	path := strings.TrimSpace(t.HTTPPath)
	if path == "" {
		path = httpDefaultPath
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	u, err := url.Parse(scheme + "://" + net.JoinHostPort(host, strconv.Itoa(port)) + path)
	if err != nil {
		return nil, fmt.Errorf("the path %q does not make a usable address: %v", path, err)
	}
	return u, nil
}

// httpProbeError says which half failed, because "timeout" on its own does
// not distinguish a server that never answered from a certificate that was
// refused.
func httpProbeError(err error, u *url.URL) string {
	s := err.Error()
	switch {
	case strings.Contains(s, "x509") || strings.Contains(s, "certificate"):
		return "the TLS certificate was refused: " + certReason(s)
	case strings.Contains(s, "connection refused"):
		return "nothing is listening on " + u.Host
	case strings.Contains(s, "context deadline exceeded"),
		strings.Contains(s, "Client.Timeout"):
		return "no first byte before the timeout"
	case strings.Contains(s, "no such host"):
		return "the name could not be resolved"
	}
	// Anything else, stripped of the URL the caller already knows.
	return strings.TrimSpace(strings.ReplaceAll(s, `Get "`+u.String()+`": `, ""))
}

// certReason keeps the part of a TLS error an operator can act on.
func certReason(s string) string {
	if i := strings.LastIndex(s, ": "); i > 0 && i+2 < len(s) {
		return s[i+2:]
	}
	return s
}
