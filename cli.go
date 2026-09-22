package main

import (
	"archive/zip"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const usage = `smokestack %s — latency monitoring

Usage:
  smokestack [-config FILE] [-ip IP] [-port PORT]   run the service (default)
                                               (also SMOKESTACK_LISTEN_IP / SMOKESTACK_LISTEN_PORT)
  smokestack probe [-config FILE]               run the isolated probe (probe.mode "external")
  smokestack version                            print version
  smokestack selftest                           check the binary is sound
  smokestack user add -email E -name N [-role master|admin|editor|viewer] [-password P]
  smokestack update [-force] PACKAGE.zip        install a release package
  smokestack rollback                           return to the previous release
  smokestack release keygen -out DIR            create a release signing key pair
  smokestack release pack -binary B -version V -key K -out OUT.zip [-os O -arch A]
  smokestack release latest -version V -base-url URL PKG.zip...   print latest.json

Every command accepts -config (default /etc/smokestack/config.json).
`

// runCLI renvoie true si une sous-commande a ete traitee.
func runCLI(args []string) bool {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return false
	}
	var err error
	switch args[0] {
	case "serve":
		return false
	case "version":
		fmt.Printf("smokestack %s %s-%s %s\n", Version, runtime.GOOS, runtime.GOARCH, BuildDate)
	case "selftest":
		err = cmdSelftest()
	case "probe":
		err = runProbe(args[1:])
	case "user":
		err = cmdUser(args[1:])
	case "update":
		err = cmdUpdate(args[1:])
	case "rollback":
		err = cmdRollback(args[1:])
	case "release":
		err = cmdRelease(args[1:])
	case "help", "-h", "--help":
		fmt.Printf(usage, Version)
	default:
		fmt.Fprintf(os.Stderr, usage, Version)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	return true
}

func cliConfig(fsn *flag.FlagSet) *string {
	return fsn.String("config", "/etc/smokestack/config.json", "configuration file")
}

// cmdSelftest verifie ce dont le service a besoin pour demarrer : ses
// ressources embarquees, la langue de reference et le pilote SQLite.
// Le module de mise a jour l'execute sur tout nouveau binaire avant de
// l'activer.
func cmdSelftest() error {
	sub, err := fs.Sub(webFS, "web")
	if err != nil {
		return err
	}
	for _, f := range []string{"index.html", "admin.html", "app.css", "i18n.js"} {
		if _, err := fs.Stat(sub, f); err != nil {
			return fmt.Errorf("ressource manquante : %s", f)
		}
	}
	tmp, err := os.MkdirTemp("", "smokestack-selftest-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	if _, err := NewI18n(sub, tmp); err != nil {
		return err
	}
	db, err := sql.Open("sqlite", filepath.Join(tmp, "t.db"))
	if err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE TABLE t(x INTEGER)`); err != nil {
		db.Close()
		return fmt.Errorf("SQLite driver: %w", err)
	}
	db.Close()
	sk := NewSketch()
	sk.Add(1000)
	if UnmarshalSketch(sk.MarshalBinary()).Count() != 1 {
		return fmt.Errorf("sketch: round trip mismatch")
	}
	return json.NewEncoder(os.Stdout).Encode(map[string]any{
		"ok": true, "version": Version, "platform": runtime.GOOS + "-" + runtime.GOARCH,
	})
}

func cmdUser(args []string) error {
	if len(args) == 0 || args[0] != "add" {
		return fmt.Errorf("usage: smokestack user add -email E -name N [-role R] [-password P]")
	}
	fsn := flag.NewFlagSet("user add", flag.ExitOnError)
	cfgPath := cliConfig(fsn)
	email := fsn.String("email", "", "email")
	name := fsn.String("name", "", "display name")
	role := fsn.String("role", "master", "master|admin|editor|viewer")
	pass := fsn.String("password", "", "password (generated if empty)")
	fsn.Parse(args[1:])
	if *email == "" {
		return fmt.Errorf("-email is required")
	}
	if *name == "" {
		*name = strings.Split(*email, "@")[0]
	}
	generated := *pass == ""
	if generated {
		b := make([]byte, 15)
		rand.Read(b)
		*pass = base64.RawURLEncoding.EncodeToString(b)
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	store, err := OpenStore(cfg.DataDir)
	if err != nil {
		return err
	}
	defer store.Close()
	u, err := store.CreateUser(*email, *name, *pass, *role)
	if err != nil {
		return err
	}
	store.Audit(nil, "cli", "user_create", "user", fmt.Sprint(u.ID))
	os.Remove(filepath.Join(cfg.DataDir, "setup-code"))
	fmt.Printf("account created: %s (%s)\n", u.Email, u.Role)
	if generated {
		fmt.Printf("password: %s\n", *pass)
	}
	return nil
}

func restartService() {
	if _, err := exec.LookPath("systemctl"); err == nil {
		if exec.Command("systemctl", "is-active", "--quiet", "smokestack").Run() == nil {
			if exec.Command("systemctl", "restart", "smokestack").Run() == nil {
				fmt.Println("service restarted")
				return
			}
		}
	}
	fmt.Println("restart the service to run the new version")
}

func cmdUpdate(args []string) error {
	fsn := flag.NewFlagSet("update", flag.ExitOnError)
	cfgPath := cliConfig(fsn)
	force := fsn.Bool("force", false, "allow installing an older or equal version")
	fsn.Parse(args)
	if fsn.NArg() != 1 {
		return fmt.Errorf("usage: smokestack update [-force] PACKAGE.zip")
	}
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	store, _ := OpenStore(cfg.DataDir)
	if store != nil {
		defer store.Close()
	}
	u := NewUpdater(cfg.Update, cfg.DataDir, store)
	st, err := u.Stage(fsn.Arg(0), false)
	if err != nil {
		return err
	}
	fmt.Printf("verified: %s (signed: %v %s)\n", st.Version, st.Signed, st.KeyID)
	if _, err := u.Apply(*force, "cli"); err != nil {
		return err
	}
	fmt.Printf("active version: %s\n", st.Version)
	restartService()
	return nil
}

func cmdRollback(args []string) error {
	fsn := flag.NewFlagSet("rollback", flag.ExitOnError)
	cfgPath := cliConfig(fsn)
	fsn.Parse(args)
	cfg, err := loadConfig(*cfgPath)
	if err != nil {
		return err
	}
	store, _ := OpenStore(cfg.DataDir)
	if store != nil {
		defer store.Close()
	}
	v, err := NewUpdater(cfg.Update, cfg.DataDir, store).Rollback("cli")
	if err != nil {
		return err
	}
	fmt.Printf("active version: %s\n", v)
	restartService()
	return nil
}

// --------------------------------------------------------------- release

func cmdRelease(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: smokestack release keygen|pack|latest ...")
	}
	switch args[0] {
	case "keygen":
		return releaseKeygen(args[1:])
	case "pack":
		return releasePack(args[1:])
	case "latest":
		return releaseLatest(args[1:])
	}
	return fmt.Errorf("unknown release command %q", args[0])
}

func releaseKeygen(args []string) error {
	fsn := flag.NewFlagSet("release keygen", flag.ExitOnError)
	out := fsn.String("out", ".", "output directory")
	fsn.Parse(args)
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return err
	}
	keyPath := filepath.Join(*out, "release.key")
	if _, err := os.Stat(keyPath); err == nil {
		return fmt.Errorf("%s already exists, refusing to overwrite", keyPath)
	}
	if err := os.WriteFile(keyPath,
		[]byte("ed25519-private:"+base64.StdEncoding.EncodeToString(priv.Seed())+"\n"), 0o600); err != nil {
		return err
	}
	line := "ed25519:" + base64.StdEncoding.EncodeToString(pub)
	os.WriteFile(filepath.Join(*out, "release.key.pub"), []byte(line+"\n"), 0o644)
	fmt.Printf("private key: %s   (keep it secret: CI secret SMOKESTACK_RELEASE_KEY)\n", keyPath)
	fmt.Printf("public key (id %s), append to release.pub:\n%s\n", keyID(pub), line)
	return nil
}

func loadPrivateKey(path string) (ed25519.PrivateKey, error) {
	var raw string
	if v := os.Getenv("SMOKESTACK_RELEASE_KEY"); v != "" && path == "" {
		raw = v
	} else {
		b, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		raw = string(b)
	}
	raw = strings.TrimPrefix(strings.TrimSpace(raw), "ed25519-private:")
	seed, err := base64.StdEncoding.DecodeString(raw)
	if err != nil || len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("invalid private key")
	}
	return ed25519.NewKeyFromSeed(seed), nil
}

func releasePack(args []string) error {
	fsn := flag.NewFlagSet("release pack", flag.ExitOnError)
	bin := fsn.String("binary", "", "compiled binary")
	ver := fsn.String("version", "", "version (e.g. 1.2.0)")
	osName := fsn.String("os", runtime.GOOS, "target OS")
	arch := fsn.String("arch", runtime.GOARCH, "target architecture")
	key := fsn.String("key", "", "private key file (or env SMOKESTACK_RELEASE_KEY)")
	unsigned := fsn.Bool("unsigned", false, "build an unsigned package (test only)")
	notes := fsn.String("notes", "", "release notes")
	out := fsn.String("out", "", "output zip")
	fsn.Parse(args)
	*ver = strings.TrimPrefix(*ver, "v")
	if *bin == "" || *ver == "" || *out == "" {
		return fmt.Errorf("-binary, -version and -out are required")
	}
	data, err := os.ReadFile(*bin)
	if err != nil {
		return err
	}
	sum := sha256.Sum256(data)
	m := Manifest{Name: "smokestack", Version: *ver, OS: *osName, Arch: *arch,
		SHA256: hex.EncodeToString(sum[:]), BuiltAt: time.Now().UTC().Format(time.RFC3339), Notes: *notes}
	mraw, _ := json.MarshalIndent(m, "", "  ")

	var sig []byte
	if !*unsigned {
		priv, err := loadPrivateKey(*key)
		if err != nil {
			return fmt.Errorf("signing key: %w (use -unsigned for a test package)", err)
		}
		sig = ed25519.Sign(priv, mraw)
	}

	f, err := os.Create(*out)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(f)
	add := func(name string, mode os.FileMode, content []byte) error {
		h := &zip.FileHeader{Name: name, Method: zip.Deflate, Modified: time.Now()}
		h.SetMode(mode)
		w, err := zw.CreateHeader(h)
		if err != nil {
			return err
		}
		_, err = w.Write(content)
		return err
	}
	if err := add("smokestack", 0o755, data); err != nil {
		return err
	}
	add("manifest.json", 0o644, mraw)
	if sig != nil {
		add("manifest.sig", 0o644, []byte(base64.StdEncoding.EncodeToString(sig)+"\n"))
	}
	if err := zw.Close(); err != nil {
		return err
	}
	f.Close()
	zb, _ := os.ReadFile(*out)
	zs := sha256.Sum256(zb)
	fmt.Printf("%s  %s  (%s-%s %s, signed: %v)\n", hex.EncodeToString(zs[:]), *out, *osName, *arch, *ver, sig != nil)
	return nil
}

// releaseLatest produit latest.json, le document que les instances
// consultent pour savoir si une nouvelle version existe.
func releaseLatest(args []string) error {
	fsn := flag.NewFlagSet("release latest", flag.ExitOnError)
	ver := fsn.String("version", "", "version")
	base := fsn.String("base-url", "", "download base URL")
	notes := fsn.String("notes-url", "", "release notes URL")
	fsn.Parse(args)
	*ver = strings.TrimPrefix(*ver, "v")
	if *ver == "" || *base == "" || fsn.NArg() == 0 {
		return fmt.Errorf("usage: release latest -version V -base-url URL PKG.zip...")
	}
	rel := RemoteRelease{Version: *ver, NotesURL: *notes,
		PublishedAt: time.Now().UTC().Format(time.RFC3339), Assets: map[string]RemoteAsset{}}
	for _, p := range fsn.Args() {
		zr, err := zip.OpenReader(p)
		if err != nil {
			return err
		}
		mf := zipEntry(&zr.Reader, "manifest.json")
		if mf == nil {
			zr.Close()
			return fmt.Errorf("%s: manifest.json missing", p)
		}
		raw, _ := readSmall(mf, 64<<10)
		zr.Close()
		var m Manifest
		if err := json.Unmarshal(raw, &m); err != nil {
			return err
		}
		fh, err := os.Open(p)
		if err != nil {
			return err
		}
		h := sha256.New()
		n, _ := io.Copy(h, fh)
		fh.Close()
		rel.Assets[m.OS+"-"+m.Arch] = RemoteAsset{
			URL:    strings.TrimSuffix(*base, "/") + "/" + filepath.Base(p),
			SHA256: hex.EncodeToString(h.Sum(nil)), Size: n,
		}
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(rel)
}
