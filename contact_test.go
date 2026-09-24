package main

import (
	"crypto/sha256"
	"strconv"
	"testing"
	"time"
)

func TestCheckContact(t *testing.T) {
	ok := func(name, email, subject, body, hp string) (*ContactMessage, error) {
		return checkContact(name, email, subject, body, hp)
	}
	if _, err := ok("Loïc", "loic@example.net", "Hello", "A real question about your network", ""); err != nil {
		t.Errorf("a valid message must pass: %v", err)
	}
	for _, c := range []struct{ name, why, n, e, s, b, hp string }{
		{"honeypot", "a filled hidden field is a bot", "A", "a@b.fr", "", "long enough message", "http://spam"},
		{"no name", "", "", "a@b.fr", "", "long enough message", ""},
		{"bad email", "", "A", "not-an-address", "", "long enough message", ""},
		{"too short", "", "A", "a@b.fr", "", "hi", ""},
		{"header injection", "", "A\r\nBcc: victim@example.net", "a@b.fr", "", "long enough message", ""},
	} {
		if _, err := ok(c.n, c.e, c.s, c.b, c.hp); err == nil {
			t.Errorf("%s: should have been refused", c.name)
		}
	}
	long := make([]byte, contactMaxBody+1)
	for i := range long {
		long[i] = 'a'
	}
	if _, err := ok("A", "a@b.fr", "", string(long), ""); err == nil {
		t.Error("an oversized message should be refused")
	}
}

func TestContactRateLimit(t *testing.T) {
	contactLimiter.hits = nil
	for i := 0; i < contactPerHour; i++ {
		if !contactAllowed("192.0.2.1") {
			t.Fatalf("message %d should be allowed", i+1)
		}
	}
	if contactAllowed("192.0.2.1") {
		t.Error("beyond the hourly quota, the message must be refused")
	}
	if !contactAllowed("192.0.2.2") {
		t.Error("another sender must not be affected")
	}
}

// The built-in robot check: a signed challenge, real work to solve, and
// refusals that say why. No third party is involved.
func TestCaptcha(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	a := &API{store: store}
	secret := a.captchaSecret()
	if len(secret) < 32 {
		t.Fatal("a secret should have been generated")
	}
	if string(a.captchaSecret()) != string(secret) {
		t.Error("the secret must be stable across calls")
	}
	salt, ts := "abc123", time.Now().Unix()
	sig := captchaSign(salt, ts, secret)

	// Solving it: find a nonce with enough leading zero bits.
	nonce := ""
	for i := 0; i < 5_000_000; i++ {
		n := strconv.Itoa(i)
		sum := sha256.Sum256([]byte(salt + n))
		if leadingZeroBits(sum[:]) >= captchaBits {
			nonce = n
			break
		}
	}
	if nonce == "" {
		t.Fatal("no solution found")
	}
	if err := a.checkCaptcha(salt, sig, nonce, ts); err != nil {
		t.Errorf("a solved challenge must pass: %v", err)
	}
	for _, c := range []struct {
		name, salt, sig, nonce string
		ts                     int64
	}{
		{"no work done", salt, sig, "0", ts},
		{"forged signature", salt, "deadbeef", nonce, ts},
		{"expired", salt, captchaSign(salt, ts-captchaTTL-60, secret), nonce, ts - captchaTTL - 60},
		{"nothing supplied", "", "", "", ts},
	} {
		if err := a.checkCaptcha(c.salt, c.sig, c.nonce, c.ts); err == nil {
			t.Errorf("%s should have been refused", c.name)
		}
	}
	if leadingZeroBits([]byte{0x00, 0x00, 0x3f}) != 18 {
		t.Errorf("leadingZeroBits: %d", leadingZeroBits([]byte{0x00, 0x00, 0x3f}))
	}
}

// Older instances only had the contact_form switch: their behaviour must not
// change when the mode setting appears.
func TestContactModeCompatibility(t *testing.T) {
	if m := ContactModeOf(Site{ContactForm: true}); m != "form" {
		t.Errorf("an older instance with the form on: %q", m)
	}
	if m := ContactModeOf(Site{ContactForm: false}); m != "off" {
		t.Errorf("an older instance with the form off: %q", m)
	}
	if m := ContactModeOf(Site{ContactForm: true, ContactMode: "links"}); m != "links" {
		t.Errorf("an explicit mode wins: %q", m)
	}
	if m := ContactModeOf(Site{ContactMode: "nonsense", ContactForm: true}); m != "form" {
		t.Errorf("an unknown mode falls back: %q", m)
	}
}
