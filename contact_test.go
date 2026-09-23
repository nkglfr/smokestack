package main

import "testing"

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
