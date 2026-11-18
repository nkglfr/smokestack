package main

import "testing"

// Every column a target is stored with comes back unchanged through each read
// that shares targetCols and scanTargets, after a create and after an update.
// A column added to the list in one place and not another would shift every
// field after it, and most of those fields are read by nothing that fails
// loudly.
func TestTargetColumnsRoundTrip(t *testing.T) {
	store, err := OpenStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	cat, err := store.CreateCategory("c", "C", "C", true)
	if err != nil {
		t.Fatal(err)
	}
	other, err := store.CreateCategory("d", "D", "D", true)
	if err != nil {
		t.Fatal(err)
	}

	read := func(t *testing.T, id int64, label string) map[string]*Target {
		t.Helper()
		got := map[string]*Target{}
		byID, err := store.TargetByID(id)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		got["TargetByID"] = byID
		active, err := store.ActiveTargets()
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		for _, x := range active {
			if x.ID == id {
				got["ActiveTargets"] = x
			}
		}
		cats, err := store.Tree(false)
		if err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		for _, c := range cats {
			for _, x := range c.Targets {
				if x.ID == id {
					got["Tree"] = x
				}
			}
		}
		return got
	}
	check := func(t *testing.T, id int64, want Target, label string) {
		t.Helper()
		want.ID = id
		got := read(t, id, label)
		for _, via := range []string{"TargetByID", "ActiveTargets", "Tree"} {
			if got[via] == nil {
				t.Errorf("%s: %s does not return the target", label, via)
			} else if *got[via] != want {
				t.Errorf("%s: %s returned\n%+v\nwant\n%+v", label, via, *got[via], want)
			}
		}
	}

	for _, c := range []struct {
		name          string
		create, patch Target
	}{
		{"tcp", Target{CategoryID: cat, Slug: "svc", Title: "Service", Host: "svc.example.net",
			Proto: "tcp", IntervalS: 120, Packets: 7, SpacingMs: 300, TimeoutMs: 1500,
			Public: true, Enabled: true, Family: 6, Port: 8443, PinIP: "2001:db8::10",
			AlertsOff: true, TraceHours: 12, HideHost: true, KeepDays: 90,
			LossWarn: 3, LossCrit: 12, LatFactor: 2.5, CertOff: true},
			Target{CategoryID: other, Slug: "svc2", Title: "Service 2", Host: "192.0.2.80",
				Proto: "icmp", IntervalS: 300, Packets: 11, SpacingMs: 400, TimeoutMs: 2500,
				Public: false, Enabled: true, Family: 4, Port: 0, PinIP: "",
				AlertsOff: false, TraceHours: 48, HideHost: false, KeepDays: 365,
				LossWarn: 5, LossCrit: 25, LatFactor: 3, CertOff: false}},
		{"dns", Target{CategoryID: cat, Slug: "res", Title: "Resolver", Host: "192.0.2.53",
			Proto: "dns", IntervalS: 60, Packets: 10, SpacingMs: 200, TimeoutMs: 2000,
			Public: true, Enabled: true, Family: 4, Port: 5353, DNSName: "example.org.", DNSType: "SOA",
			TraceHours: 6, KeepDays: 30, LossWarn: 1, LossCrit: 2, LatFactor: 1.5},
			Target{CategoryID: cat, Slug: "res", Title: "Resolver", Host: "192.0.2.54",
				Proto: "dns", IntervalS: 60, Packets: 10, SpacingMs: 200, TimeoutMs: 2000,
				Public: true, Enabled: true, Family: 4, Port: 53, DNSName: "example.net.", DNSType: "MX",
				TraceHours: 6, KeepDays: 30, LossWarn: 1, LossCrit: 2, LatFactor: 1.5}},
	} {
		t.Run(c.name, func(t *testing.T) {
			made := c.create
			id, err := store.CreateTarget(&made)
			if err != nil {
				t.Fatal(err)
			}
			// Set through its own route, which validates probe assignments
			// this test has no use for.
			if _, err := store.cfg.Exec(`UPDATE targets SET primary_probe_id=7 WHERE id=?`, id); err != nil {
				t.Fatal(err)
			}
			want := c.create
			want.PrimaryProbeID = 7
			check(t, id, want, "created")

			upd := c.patch
			upd.ID, upd.PrimaryProbeID = id, 7
			if err := store.UpdateTarget(&upd); err != nil {
				t.Fatal(err)
			}
			want = c.patch
			want.PrimaryProbeID = 7
			check(t, id, want, "updated")

			if err := store.ArchiveTarget(id); err != nil {
				t.Fatal(err)
			}
			arch, err := store.ArchivedTargets()
			if err != nil {
				t.Fatal(err)
			}
			var got *Target
			for _, x := range arch {
				if x.ID == id {
					got = x
				}
			}
			if got == nil {
				t.Fatal("ArchivedTargets does not return the archived target")
			}
			if got.ArchivedAt == 0 || got.Enabled || got.Public || !got.AlertsOff {
				t.Errorf("archived as %+v", *got)
			}
			want.ID = id
			want.Slug, want.Title, want.ArchivedAt = got.Slug, got.Title, got.ArchivedAt
			want.Enabled, want.Public, want.AlertsOff = false, false, true
			if *got != want {
				t.Errorf("ArchivedTargets returned\n%+v\nwant\n%+v", *got, want)
			}
		})
	}
}
