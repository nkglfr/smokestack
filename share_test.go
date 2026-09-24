package main

import (
	"testing"
	"time"
)

// A share link opens exactly one target, even a private one, and stops
// working when revoked or expired. The token itself is never stored.
func TestShareLinks(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	shareSchema(store)
	cat, _ := store.CreateCategory("c", "C", "C", true)
	mk := func(slug string, public bool) int64 {
		id, err := store.CreateTarget(&Target{CategoryID: cat, Slug: slug, Title: slug,
			Host: "192.0.2.1", Proto: "icmp", IntervalS: 60, Packets: 10, SpacingMs: 100,
			TimeoutMs: 1000, Public: public, Enabled: true})
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	priv, other := mk("private-one", false), mk("another", false)

	link, err := store.CreateShare(priv, 30, "for the customer", "noc@example.net")
	if err != nil {
		t.Fatal(err)
	}
	if link.Token == "" || len(link.Token) < 32 {
		t.Fatalf("a token was expected: %q", link.Token)
	}
	// The token is not in the database, only its hash.
	var stored string
	store.cfg.QueryRow(`SELECT token_hash FROM share_links WHERE id=?`, link.ID).Scan(&stored)
	if stored == link.Token || stored == "" {
		t.Error("the token must be stored hashed, never in clear")
	}
	// It opens its target, and only that one.
	if got, ok := store.ShareTarget(link.Token); !ok || got != priv {
		t.Errorf("the link should open target %d, got %d (%v)", priv, got, ok)
	}
	if got, _ := store.ShareTarget(link.Token); got == other {
		t.Error("a link must never open another target")
	}
	// Nonsense and truncated tokens are refused.
	for _, bad := range []string{"", "short", link.Token[:20], link.Token + "a"} {
		if _, ok := store.ShareTarget(bad); ok {
			t.Errorf("token %q should be refused", bad)
		}
	}
	// Uses are counted, which is what tells an operator a link is live.
	list, _ := store.Shares()
	if len(list) != 1 || list[0].Uses == 0 || list[0].Title != "private-one" {
		t.Errorf("share list: %+v", list)
	}
	// An expired link stops working.
	store.cfg.Exec(`UPDATE share_links SET expires_at=? WHERE id=?`, time.Now().Unix()-60, link.ID)
	if _, ok := store.ShareTarget(link.Token); ok {
		t.Error("an expired link must be refused")
	}
	// A revoked link stops working.
	fresh, _ := store.CreateShare(priv, 0, "no expiry", "noc@example.net")
	if _, ok := store.ShareTarget(fresh.Token); !ok {
		t.Fatal("a link without expiry should work")
	}
	if err := store.RevokeShare(fresh.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.ShareTarget(fresh.Token); ok {
		t.Error("a revoked link must be refused")
	}
	// Sharing a target that does not exist is refused.
	if _, err := store.CreateShare(99999, 30, "", "x"); err == nil {
		t.Error("sharing an unknown target should be refused")
	}
}

// The hop count over time is what makes a topology change visible.
func TestHopSeries(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cat, _ := store.CreateCategory("c", "C", "C", true)
	id, err := store.CreateTarget(&Target{CategoryID: cat, Slug: "t", Title: "T",
		Host: "192.0.2.9", Proto: "icmp", IntervalS: 60, Packets: 10, SpacingMs: 100,
		TimeoutMs: 1000, Public: true, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	add := func(ago int64, hops int, reached bool) {
		var hs []Hop
		for i := 0; i < hops; i++ {
			hs = append(hs, Hop{TTL: i + 1, Addr: "192.0.2.1", ASN: "AS64500", Sent: 3})
		}
		if err := store.SaveTraceroute(&Traceroute{TargetID: id, ProbeID: 1, TS: now - ago,
			Kind: "reference", Family: 4, Dest: "192.0.2.9", Reached: reached, Hops: hs}); err != nil {
			t.Fatal(err)
		}
	}
	add(40*86400, 5, true) // older than the window
	add(3*86400, 5, true)
	add(2*86400, 7, true)
	add(86400, 7, false)
	pts, err := store.HopSeries(id, now-30*86400)
	if err != nil {
		t.Fatal(err)
	}
	if len(pts) != 3 {
		t.Fatalf("3 points expected inside the window, got %d", len(pts))
	}
	if pts[0].TS > pts[1].TS || pts[1].TS > pts[2].TS {
		t.Error("points must come oldest first")
	}
	if pts[0].Hops != 5 || pts[1].Hops != 7 {
		t.Errorf("hop counts: %d then %d", pts[0].Hops, pts[1].Hops)
	}
	if pts[2].Reached {
		t.Error("the last point should be marked as not reached")
	}
	if pts[0].ASPath == "" {
		t.Error("the AS path should travel with each point, for the tooltip")
	}
}
