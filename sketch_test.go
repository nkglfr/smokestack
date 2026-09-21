package main

import (
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func exact(v []float64, q float64) float64 {
	s := append([]float64(nil), v...)
	sort.Float64s(s)
	return s[int(q*float64(len(s)-1))]
}

func TestQuantileAndMerge(t *testing.T) {
	rnd := rand.New(rand.NewSource(1))
	var all []float64
	var parts []*Sketch
	for p := 0; p < 60; p++ {
		sk := NewSketch()
		for i := 0; i < 20; i++ {
			v := 8000 + rnd.NormFloat64()*900
			if v < 100 {
				v = 100
			}
			sk.Add(v)
			all = append(all, v)
		}
		// round-trip de serialisation a chaque etape
		parts = append(parts, UnmarshalSketch(sk.MarshalBinary()))
	}
	merged := NewSketch()
	for _, p := range parts {
		merged.Merge(p)
	}
	if merged.Count() != uint64(len(all)) {
		t.Fatalf("count %d != %d", merged.Count(), len(all))
	}
	for _, q := range []float64{0.05, 0.25, 0.5, 0.75, 0.95, 0.99} {
		got, want := merged.Quantile(q), exact(all, q)
		if rel := math.Abs(got-want) / want; rel > 0.02 {
			t.Errorf("q%.2f: %.0f vs %.0f (%.3f)", q, got, want, rel)
		}
	}
}

func TestParseTime(t *testing.T) {
	now := int64(1000000)
	if v := parseTime("now-3h", now); v == now {
		t.Error("now-3h non interprete")
	}
	if v := parseTime("", 42); v != 42 {
		t.Error("defaut non applique")
	}
	if v := parseTime("1757768460", 0); v != 1757768460 {
		t.Errorf("epoch: %d", v)
	}
	if v := parseTime("1757768460000", 0); v != 1757768460 {
		t.Errorf("ms: %d", v)
	}
}

func TestOffsetSpread(t *testing.T) {
	seen := map[int64]int{}
	for id := int64(1); id <= 200; id++ {
		seen[offsetFor(id, 60)]++
	}
	if len(seen) < 40 {
		t.Errorf("offsets mal repartis: %d valeurs distinctes", len(seen))
	}
}

func TestPasswordHashing(t *testing.T) {
	h, err := hashPassword("correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if !verifyPassword(h, "correct horse battery") {
		t.Error("le bon mot de passe est rejete")
	}
	if verifyPassword(h, "correct horse batterz") {
		t.Error("un mauvais mot de passe est accepte")
	}
	if verifyPassword("nimporte-quoi", "correct horse battery") {
		t.Error("un hash malforme est accepte")
	}
	h2, _ := hashPassword("correct horse battery")
	if h == h2 {
		t.Error("le sel n'est pas aleatoire")
	}
	if err := validPassword("court"); err == nil {
		t.Error("un mot de passe trop court est accepte")
	}
}

func TestPairingHelpers(t *testing.T) {
	cases := map[string]string{
		"https://a.fr/pairing":  "https://a.fr",
		"https://a.fr/pairing/": "https://a.fr",
		"https://a.fr/":         "https://a.fr",
		"https://a.fr":          "https://a.fr",
	}
	for in, want := range cases {
		if got := baseOf(in); got != want {
			t.Errorf("baseOf(%q) = %q, attendu %q", in, got, want)
		}
	}
	fp := fingerprintOf([]byte("0123456789abcdef0123456789abcdef"))
	if len(fp) != 19 || strings.Count(fp, "-") != 3 {
		t.Errorf("empreinte mal formatee: %q", fp)
	}
}

func TestRoleLevels(t *testing.T) {
	if (&User{Role: "viewer"}).Level() >= (&User{Role: "admin"}).Level() {
		t.Error("hierarchie des roles incorrecte")
	}
	if (&User{Role: "master"}).Level() != RoleMaster {
		t.Error("master mal classe")
	}
	if (&User{Role: "inconnu"}).Level() != 0 {
		t.Error("un role inconnu doit valoir 0")
	}
}

func TestI18nFiles(t *testing.T) {
	i, err := NewI18n(os.DirFS("web"), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range i.Languages() {
		if l.Coverage < 1 {
			t.Errorf("%s incomplet: %d cles manquantes", l.Code, l.Missing)
		}
		if len(l.Warnings) > 0 {
			t.Errorf("%s: %v", l.Code, l.Warnings)
		}
	}
	d, ok := i.Dict("fr")
	if !ok || d["home.faults"] != "Défauts constatés" {
		t.Error("dictionnaire francais mal charge")
	}
}

func TestI18nRequiresEnglish(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "i18n"), 0o755)
	os.WriteFile(filepath.Join(dir, "i18n", "fr.json"), []byte(`{"a":"b"}`), 0o644)
	if _, err := NewI18n(os.DirFS(dir), t.TempDir()); err == nil {
		t.Error("l'absence d'anglais doit empecher le demarrage")
	}
}

func TestI18nFallback(t *testing.T) {
	dir := t.TempDir()
	os.MkdirAll(filepath.Join(dir, "i18n"), 0o755)
	os.WriteFile(filepath.Join(dir, "i18n", "en.json"),
		[]byte(`{"_meta":{"name":"English"},"a":{"x":"Hello {n}","y":"World"}}`), 0o644)
	os.WriteFile(filepath.Join(dir, "i18n", "it.json"),
		[]byte(`{"_meta":{"name":"Italiano"},"a":{"x":"Ciao"}}`), 0o644)
	i, err := NewI18n(os.DirFS(dir), t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	d, _ := i.Dict("it")
	if d["a.y"] != "World" {
		t.Error("le repli sur l'anglais ne fonctionne pas")
	}
	for _, l := range i.Languages() {
		if l.Code == "it" {
			if l.Missing != 1 || len(l.Warnings) != 1 {
				t.Errorf("it: attendu 1 manquante et 1 alerte de variable, obtenu %d / %v",
					l.Missing, l.Warnings)
			}
		}
	}
}

func TestNormalizeASN(t *testing.T) {
	for in, want := range map[string]string{"AS64500": "64500", "as3215": "3215", " 15557 ": "15557"} {
		if got, err := normalizeASN(in); err != nil || got != want {
			t.Errorf("normalizeASN(%q) = %q, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "AS", "ASxyz", "0", "99999999999"} {
		if _, err := normalizeASN(bad); err == nil {
			t.Errorf("normalizeASN(%q) devrait echouer", bad)
		}
	}
}

func TestJustification(t *testing.T) {
	if _, err := checkJustification("trop court"); err == nil {
		t.Error("justification trop courte acceptee")
	}
	if _, err := checkJustification("   Nous peerons au France-IX Paris   "); err != nil {
		t.Error(err)
	}
	if _, err := checkJustification(strings.Repeat("é", 19)); err == nil {
		t.Error("le decompte doit se faire en caracteres, pas en octets")
	}
}

func TestParseTimeShorthand(t *testing.T) {
	def := int64(42)
	for _, in := range []string{"-30h", "now-30h"} {
		got := parseTime(in, def)
		if d := time.Now().Unix() - got; d < 30*3600-5 || d > 30*3600+5 {
			t.Errorf("parseTime(%q) = %d, expected now-30h", in, got)
		}
	}
	if parseTime("-3x", def) != def {
		t.Error("an invalid unit must return the default")
	}
}
