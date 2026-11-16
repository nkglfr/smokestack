package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

// certServer runs a TLS listener presenting a certificate built here, so the
// tests cover the real handshake rather than a mocked one.
func certServer(t *testing.T, names []string, notBefore, notAfter time.Time) (host string, port int) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: names[0]},
		Issuer:       pkix.Name{CommonName: "Test CA"},
		NotBefore:    notBefore,
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	// A name that is an address belongs in the IP SAN: put it in the DNS
	// names and verification stops on the name, never reaching the chain,
	// which is not what a real server with a self-signed certificate does.
	for _, n := range names {
		if ip := net.ParseIP(n); ip != nil {
			tpl.IPAddresses = append(tpl.IPAddresses, ip)
		} else {
			tpl.DNSNames = append(tpl.DNSNames, n)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{
		Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			if tc, ok := c.(*tls.Conn); ok {
				tc.Handshake()
			}
			c.Close()
		}
	}()
	h, p, _ := net.SplitHostPort(ln.Addr().String())
	n, _ := strconv.Atoi(p)
	return h, n
}

// What the inspection reports: the dates, the names, and the reason when a
// certificate would be refused by a client.
func TestInspectCert(t *testing.T) {
	now := time.Now()

	// A self-signed certificate: the chain cannot be verified, which is a
	// real problem and must be named rather than swallowed.
	h, p := certServer(t, []string{"127.0.0.1"}, now.Add(-time.Hour), now.Add(40*24*time.Hour))
	st := inspectCert(h, p, 4, 3*time.Second)
	if st.NotAfter == 0 {
		t.Fatalf("the certificate should have been read anyway: %+v", st)
	}
	if st.DaysLeft < 38 || st.DaysLeft > 40 {
		t.Errorf("days left = %d, expected about 39", st.DaysLeft)
	}
	if st.Problem == "" {
		t.Error("a self-signed certificate must be reported as refused")
	}
	if !strings.Contains(st.Problem, "chain") && !strings.Contains(st.Problem, "refused") {
		t.Errorf("the reason should be actionable: %q", st.Problem)
	}

	// Expired.
	h2, p2 := certServer(t, []string{"127.0.0.1"}, now.Add(-48*time.Hour), now.Add(-time.Hour))
	if st := inspectCert(h2, p2, 4, 3*time.Second); st.DaysLeft > 0 {
		t.Errorf("an expired certificate has no days left: %d", st.DaysLeft)
	}

	// Nothing listening at all.
	if st := inspectCert("127.0.0.1", 1, 4, time.Second); st.Problem == "" {
		t.Error("a port with no TLS must be reported")
	}

	// Whole days, rounded towards the operator's interest.
	if d := daysLeft(now.Add(12*time.Hour), now); d != 0 {
		t.Errorf("twelve hours left is zero days, got %d", d)
	}
	if d := daysLeft(now.Add(-time.Hour), now); d >= 0 {
		t.Errorf("an expired certificate is negative, got %d", d)
	}
}

// One message per threshold crossed, not one per check, and a renewal
// rearms the whole sequence.
func TestCertAlertStages(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.cfg.Exec(certSchema)
	cat, _ := store.CreateCategory("c", "C", "C", true)
	id, err := store.CreateTarget(&Target{CategoryID: cat, Slug: "svc", Title: "Service",
		Host: "example.net", Proto: "tcp", Port: 443, Family: 4, IntervalS: 60,
		Packets: 5, SpacingMs: 500, TimeoutMs: 2000, Public: true, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	tg, _ := store.TargetByID(id)

	var sent []string
	w := NewCertWatcher(store, func(subject, body string) error {
		sent = append(sent, subject)
		return nil
	})
	cfg := CertConfig{Enabled: true, WarnDays: 30}
	at := func(days int) CertState {
		return CertState{TargetID: id, CheckedAt: time.Now().Unix(),
			NotAfter: time.Now().Add(time.Duration(days) * 24 * time.Hour).Unix(),
			DaysLeft: days, Issuer: "Test CA"}
	}
	// Comfortably valid: nothing said.
	store.saveCert(id, at(60))
	w.consider(cfg, tg, at(60))
	if len(sent) != 0 {
		t.Fatalf("a valid certificate must say nothing: %v", sent)
	}
	// Crossing 30, then checked again the next day: still one message.
	store.saveCert(id, at(29))
	w.consider(cfg, tg, at(29))
	w.consider(cfg, tg, at(28))
	if len(sent) != 1 {
		t.Fatalf("one message per threshold, got %d: %v", len(sent), sent)
	}
	// Crossing 14 and 7: one each.
	w.consider(cfg, tg, at(13))
	w.consider(cfg, tg, at(6))
	if len(sent) != 3 {
		t.Fatalf("three messages expected, got %d: %v", len(sent), sent)
	}
	// Renewed: the record clears, and a future expiry is announced again.
	w.consider(cfg, tg, at(90))
	w.consider(cfg, tg, at(20))
	if len(sent) != 4 {
		t.Fatalf("a renewal must rearm the sequence, got %v", sent)
	}
	// A problem is said once, not at every check.
	bad := CertState{TargetID: id, CheckedAt: time.Now().Unix(),
		Problem: "the certificate is not valid for example.net"}
	w.consider(cfg, tg, bad)
	w.consider(cfg, tg, bad)
	if len(sent) != 5 {
		t.Fatalf("one message for a problem, got %v", sent)
	}
	if !strings.Contains(sent[4], "certificate problem") {
		t.Errorf("subject: %q", sent[4])
	}
	// A target whose alerting is off says nothing at all.
	tg.AlertsOff = true
	w.consider(cfg, tg, at(2))
	if len(sent) != 5 {
		t.Errorf("a muted target must not alert: %v", sent)
	}
}

// Only TCP targets with a port are inspected, and the operator can exclude
// one whose port does not speak TLS.
func TestCertTargets(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cat, _ := store.CreateCategory("c", "C", "C", true)
	mk := func(slug, proto string, port int, off bool) {
		if _, err := store.CreateTarget(&Target{CategoryID: cat, Slug: slug, Title: slug,
			Host: "example.net", Proto: proto, Port: port, Family: 4, IntervalS: 60,
			Packets: 5, SpacingMs: 500, TimeoutMs: 2000, Public: true, Enabled: true,
			CertOff: off}); err != nil {
			t.Fatalf("%s: %v", slug, err)
		}
	}
	mk("https", "tcp", 443, false)
	mk("bgp", "tcp", 179, true)
	mk("ping", "icmp", 0, false)
	if _, err := store.CreateTarget(&Target{CategoryID: cat, Slug: "peer-check", Title: "peer-check",
		Host: "example.net", Proto: "tcp", Port: 443, Family: 4, IntervalS: 60, Packets: 5,
		SpacingMs: 500, TimeoutMs: 2000, Enabled: true, DCCheck: "c1"}); err != nil {
		t.Fatal(err)
	}

	got := map[string]bool{}
	for _, tg := range store.certTargets() {
		got[tg.Slug] = true
	}
	if !got["https"] {
		t.Error("a TCP target with a port should be inspected")
	}
	if got["bgp"] {
		t.Error("a target excluded by the operator must be left alone")
	}
	if got["ping"] {
		t.Error("an ICMP target has no certificate")
	}
	if got["peer-check"] {
		t.Error("a target measured for a peer's double-check is not this instance's to inspect")
	}
	// The threshold is validated rather than accepted blindly.
	if err := store.SetCertConfig(CertConfig{Enabled: true, WarnDays: 0}); err == nil {
		t.Error("zero days would mean warning at expiry, which is useless")
	}
	if err := store.SetCertConfig(CertConfig{Enabled: true, WarnDays: 45}); err != nil {
		t.Errorf("45 days is reasonable: %v", err)
	}
	if c := store.CertConfig(); c.WarnDays != 45 {
		t.Errorf("the threshold was not kept: %+v", c)
	}
	if s := stageList(45); !strings.HasPrefix(s, "45") || !strings.Contains(s, "1") {
		t.Errorf("the message should list every threshold: %q", s)
	}
	_ = fmt.Sprint()
}
