package main

import (
	"strings"
	"testing"
)

// Nothing validated the protocol: not checkTarget, not the API, and
// Prober.Run fell through to ICMP for anything it did not recognise. A target
// could say one thing, be stored and displayed as that thing, name it in its
// alerts, and be measured with another — every surface agreeing on the same
// wrong answer, which is why nobody noticed for months.
func TestTargetProtocolIsValidated(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cat, err := store.CreateCategory("c", "Catégorie", "Category", true)
	if err != nil {
		t.Fatal(err)
	}

	mk := func(slug, proto string, port int) error {
		_, err := store.CreateTarget(&Target{CategoryID: cat, Slug: slug, Title: slug,
			Host: "198.51.100.1", Proto: proto, Port: port, IntervalS: 60, Packets: 10,
			SpacingMs: 100, TimeoutMs: 1000, Public: true, Enabled: true})
		return err
	}

	for _, bad := range []string{"quux", "icmp6", "ICMPv6", "tcp6", "ping"} {
		if err := mk("bad-"+strings.ToLower(bad), bad, 443); err == nil {
			t.Errorf("protocol %q was accepted", bad)
		}
	}

	// Accepted, and normalised on the way in: a protocol typed with a capital
	// or a stray space is the same protocol, not a different one.
	for i, ok := range []string{"icmp", "ICMP", "  icmp  ", ""} {
		slug := "ok-icmp-" + string(rune('a'+i))
		if err := mk(slug, ok, 0); err != nil {
			t.Fatalf("protocol %q was refused: %v", ok, err)
		}
		got, err := store.TargetBySlug(slug)
		if err != nil {
			t.Fatal(err)
		}
		if got.Proto != "icmp" {
			t.Errorf("protocol %q was stored as %q, want icmp", ok, got.Proto)
		}
	}
	if err := mk("ok-tcp", "TCP", 443); err != nil {
		t.Fatalf("TCP was refused: %v", err)
	}
	got, err := store.TargetBySlug("ok-tcp")
	if err != nil {
		t.Fatal(err)
	}
	if got.Proto != "tcp" {
		t.Errorf("TCP was stored as %q", got.Proto)
	}

	// Changing an existing target has to go through the same gate, otherwise
	// the refusal is only a formality on the way in.
	got.Proto = "quux"
	if err := store.UpdateTarget(got); err == nil {
		t.Error("an existing target was updated to an unknown protocol")
	}
}

// Rows written before any of this existed. An empty protocol has always been
// measured with ICMP, so recording that changes no measurement — it only
// writes down what was already happening. Anything else is left alone, since
// quietly turning it into ICMP would repeat the defect being fixed.
func TestStoredProtocolsAreNormalisedOnOpen(t *testing.T) {
	dir := t.TempDir()
	store, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	cat, err := store.CreateCategory("c", "Catégorie", "Category", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateTarget(&Target{CategoryID: cat, Slug: "legacy", Title: "legacy",
		Host: "198.51.100.1", Proto: "icmp", IntervalS: 60, Packets: 10,
		SpacingMs: 100, TimeoutMs: 1000, Public: true, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.CreateTarget(&Target{CategoryID: cat, Slug: "odd", Title: "odd",
		Host: "198.51.100.2", Proto: "icmp", IntervalS: 60, Packets: 10,
		SpacingMs: 100, TimeoutMs: 1000, Public: true, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	// Written behind the validation, the way the API used to let anyone.
	store.cfg.Exec(`UPDATE targets SET proto='' WHERE slug='legacy'`)
	store.cfg.Exec(`UPDATE targets SET proto='QUUX' WHERE slug='odd'`)
	store.Close()

	store, err = OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	legacy, err := store.TargetBySlug("legacy")
	if err != nil {
		t.Fatal(err)
	}
	if legacy.Proto != "icmp" {
		t.Errorf("an empty protocol became %q, want icmp", legacy.Proto)
	}
	odd, err := store.TargetBySlug("odd")
	if err != nil {
		t.Fatal(err)
	}
	if odd.Proto != "quux" {
		t.Errorf("an unknown protocol became %q: it should be lowercased and left alone, "+
			"so that it surfaces as an error rather than being silently measured", odd.Proto)
	}
}

func TestKnownProtocols(t *testing.T) {
	for _, p := range knownProtos {
		if !validProto(p) {
			t.Errorf("%q is listed but refused", p)
		}
		if p != strings.ToLower(strings.TrimSpace(p)) {
			t.Errorf("%q is listed in a form checkTarget would never produce", p)
		}
	}
	for _, p := range []string{"", " ", "ICMP", "UDP", "quux"} {
		if validProto(p) {
			t.Errorf("%q is accepted and should not be", p)
		}
	}
}
