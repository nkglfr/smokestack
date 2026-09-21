package main

import (
	"archive/zip"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Mise a jour de l'application.
//
// Disposition sur disque (creee par install.sh) :
//
//	/opt/smokestack/releases/1.2.0/smokestack
//	/opt/smokestack/releases/1.3.0/smokestack
//	/opt/smokestack/current  -> releases/1.3.0
//	/opt/smokestack/previous -> releases/1.2.0
//
// Un paquet est un ZIP contenant le binaire "smokestack", un manifeste
// et la signature Ed25519 du manifeste. Le manifeste porte l'empreinte
// SHA-256 du binaire : signer le manifeste revient a signer le binaire.
//
// Etapes : verification (signature, plateforme, empreinte) -> extraction
// dans releases/<version> -> autotest du nouveau binaire -> sauvegarde de
// config.db -> bascule atomique du lien current -> redemarrage en place.
// Si la nouvelle version echoue trois fois a demarrer, l'ancienne est
// restauree automatiquement.

const (
	maxPackageBytes = 200 << 20
	pendingFile     = "update-pending.json"
	keepReleases    = 3
)

type UpdateConfig struct {
	Enabled       bool   `json:"enabled"`
	ManifestURL   string `json:"manifest_url"`
	AutoCheck     bool   `json:"auto_check"`
	AutoApply     bool   `json:"auto_apply"`
	CheckHours    int    `json:"check_interval_hours"`
	AllowUnsigned bool   `json:"allow_unsigned"`
	TrustedKeys   string `json:"trusted_keys_file"`
}

func defaultUpdateConfig() UpdateConfig {
	return UpdateConfig{
		Enabled: true, AutoCheck: true, AutoApply: false, CheckHours: 6,
		ManifestURL: RepoURL + "/releases/latest/download/latest.json",
		TrustedKeys: "/etc/smokestack/release-keys.pub",
	}
}

type Manifest struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
	SHA256  string `json:"sha256"`
	BuiltAt string `json:"built_at"`
	Notes   string `json:"notes,omitempty"`
}

type Staged struct {
	Manifest
	Signed  bool   `json:"signed"`
	KeyID   string `json:"key_id,omitempty"`
	Newer   bool   `json:"newer"`
	Current string `json:"current"`
	dir     string
}

type RemoteAsset struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

type RemoteRelease struct {
	Version     string                 `json:"version"`
	PublishedAt string                 `json:"published_at"`
	NotesURL    string                 `json:"notes_url"`
	Assets      map[string]RemoteAsset `json:"assets"`
	CheckedAt   int64                  `json:"checked_at,omitempty"`
	Newer       bool                   `json:"newer,omitempty"`
}

type HistoryEntry struct {
	TS     int64  `json:"ts"`
	Action string `json:"action"`
	From   string `json:"from"`
	To     string `json:"to"`
	By     string `json:"by"`
}

type pending struct {
	Previous string `json:"previous"` // lien previous avant la mise a jour
	From     string `json:"from"`
	To       string `json:"to"`
	At       int64  `json:"at"`
	Attempts int    `json:"attempts"`
}

type Updater struct {
	cfg     UpdateConfig
	dataDir string
	store   *Store // nil en ligne de commande
	root    string // racine geree (/opt/smokestack) ou "" si non geree
	reason  string // pourquoi la mise a jour en place est indisponible
	keys    map[string]ed25519.PublicKey

	mu        sync.Mutex
	staged    *Staged
	available *RemoteRelease
	busy      bool

	Restart chan struct{}
}

func NewUpdater(cfg UpdateConfig, dataDir string, store *Store) *Updater {
	u := &Updater{cfg: cfg, dataDir: dataDir, store: store,
		keys: map[string]ed25519.PublicKey{}, Restart: make(chan struct{}, 1)}
	u.loadKeys()
	u.detectLayout()
	return u
}

// ---------------------------------------------------------------- cles

func keyID(pub ed25519.PublicKey) string {
	h := sha256.Sum256(pub)
	return hex.EncodeToString(h[:4])
}

func parseKeyLines(text string, into map[string]ed25519.PublicKey) {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "ed25519:")
		raw, err := base64.StdEncoding.DecodeString(line)
		if err != nil || len(raw) != ed25519.PublicKeySize {
			log.Printf("update: public key ignored (invalid format)")
			continue
		}
		pub := ed25519.PublicKey(raw)
		into[keyID(pub)] = pub
	}
}

func (u *Updater) loadKeys() {
	parseKeyLines(embeddedReleaseKeys, u.keys)
	if u.cfg.TrustedKeys != "" {
		if b, err := os.ReadFile(u.cfg.TrustedKeys); err == nil {
			parseKeyLines(string(b), u.keys)
		}
	}
}

func (u *Updater) KeyIDs() []string {
	out := []string{}
	for id := range u.keys {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// ----------------------------------------------------------- disposition

func (u *Updater) detectLayout() {
	if !u.cfg.Enabled {
		u.reason = "updates are disabled in the configuration"
		return
	}
	exe, err := os.Executable()
	if err != nil {
		u.reason = "unknown binary location"
		return
	}
	if real, err := filepath.EvalSymlinks(exe); err == nil {
		exe = real
	}
	relDir := filepath.Dir(exe)
	releases := filepath.Dir(relDir)
	if filepath.Base(releases) != "releases" {
		u.reason = "unmanaged installation (binary outside <root>/releases/<version>/): " +
			"use install.sh to enable in-place updates"
		return
	}
	root := filepath.Dir(releases)
	if _, err := os.Lstat(filepath.Join(root, "current")); err != nil {
		u.reason = "link " + filepath.Join(root, "current") + " missing"
		return
	}
	probe := filepath.Join(releases, ".write-test")
	if err := os.WriteFile(probe, nil, 0o600); err != nil {
		u.reason = "the service cannot write to " + releases
		return
	}
	os.Remove(probe)
	u.root = root
}

func (u *Updater) Managed() bool { return u.root != "" }

func (u *Updater) link(name string) string {
	t, err := os.Readlink(filepath.Join(u.root, name))
	if err != nil {
		return ""
	}
	return filepath.Base(t)
}

// swapLink remplace un lien symbolique de facon atomique (rename).
func swapLink(path, target string) error {
	tmp := path + ".tmp"
	os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// ---------------------------------------------------------------- versions

// parseVersion lit "1.2.3", "v1.2.3" ou "1.2.3-rc1". "dev" vaut 0.0.0.
func parseVersion(v string) (nums [3]int, pre string) {
	v = strings.TrimPrefix(strings.TrimSpace(v), "v")
	if i := strings.IndexAny(v, "-+"); i >= 0 {
		pre, v = v[i+1:], v[:i]
	}
	for i, p := range strings.SplitN(v, ".", 3) {
		n, _ := strconv.Atoi(p)
		nums[i] = n
	}
	return
}

// compareVersions renvoie -1, 0 ou 1.
func compareVersions(a, b string) int {
	na, pa := parseVersion(a)
	nb, pb := parseVersion(b)
	for i := 0; i < 3; i++ {
		if na[i] != nb[i] {
			if na[i] < nb[i] {
				return -1
			}
			return 1
		}
	}
	switch {
	case pa == pb:
		return 0
	case pa == "": // une version finale passe devant sa pre-version
		return 1
	case pb == "":
		return -1
	case pa < pb:
		return -1
	}
	return 1
}

func validVersion(v string) bool {
	v = strings.TrimPrefix(v, "v")
	if v == "" || len(v) > 40 || v[0] < '0' || v[0] > '9' {
		return false
	}
	for _, c := range v {
		if !(c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' ||
			c == '.' || c == '-' || c == '+') {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------- paquet

func zipEntry(zr *zip.Reader, name string) *zip.File {
	for _, f := range zr.File {
		// Accepte la racine du ZIP ou un unique dossier englobant.
		clean := filepath.ToSlash(filepath.Clean(f.Name))
		if clean == name || (strings.Count(clean, "/") == 1 && strings.HasSuffix(clean, "/"+name)) {
			return f
		}
	}
	return nil
}

func readSmall(f *zip.File, max int64) ([]byte, error) {
	if f.UncompressedSize64 > uint64(max) {
		return nil, fmt.Errorf("%s too large", f.Name)
	}
	rc, err := f.Open()
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(io.LimitReader(rc, max))
}

// Verify controle un paquet sans rien installer.
func (u *Updater) Verify(zipPath string, requireSigned bool) (*Staged, *zip.ReadCloser, error) {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return nil, nil, fmt.Errorf("unreadable ZIP: %w", err)
	}
	fail := func(e error) (*Staged, *zip.ReadCloser, error) { zr.Close(); return nil, nil, e }

	mf, bin := zipEntry(&zr.Reader, "manifest.json"), zipEntry(&zr.Reader, "smokestack")
	if mf == nil || bin == nil {
		return fail(errors.New("invalid package: manifest.json and smokestack are required"))
	}
	raw, err := readSmall(mf, 64<<10)
	if err != nil {
		return fail(err)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return fail(fmt.Errorf("unreadable manifest: %w", err))
	}
	if m.Name != "smokestack" || !validVersion(m.Version) || len(m.SHA256) != 64 {
		return fail(errors.New("incomplete or invalid manifest"))
	}
	if m.OS != runtime.GOOS || m.Arch != runtime.GOARCH {
		return fail(fmt.Errorf("package for %s-%s, this server is %s-%s",
			m.OS, m.Arch, runtime.GOOS, runtime.GOARCH))
	}
	if bin.UncompressedSize64 > maxPackageBytes {
		return fail(errors.New("binary too large"))
	}

	st := &Staged{Manifest: m, Current: Version, Newer: compareVersions(m.Version, Version) > 0}
	if sf := zipEntry(&zr.Reader, "manifest.sig"); sf != nil {
		sigRaw, err := readSmall(sf, 4096)
		if err == nil {
			sig, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(sigRaw)))
			if err == nil {
				for id, k := range u.keys {
					if ed25519.Verify(k, raw, sig) {
						st.Signed, st.KeyID = true, id
						break
					}
				}
			}
		}
	}
	if !st.Signed && (requireSigned || !u.cfg.AllowUnsigned) {
		if len(u.keys) == 0 {
			return fail(errors.New("no trusted release key is configured: " +
				"add the public key to /etc/smokestack/release-keys.pub (see DEPLOY.md)"))
		}
		return fail(errors.New("missing or unknown signature: package rejected"))
	}
	return st, zr, nil
}

// Stage verifie, extrait et autoteste un paquet. Il ne bascule rien.
func (u *Updater) Stage(zipPath string, requireSigned bool) (*Staged, error) {
	if !u.Managed() {
		return nil, errors.New(u.reason)
	}
	u.mu.Lock()
	if u.busy {
		u.mu.Unlock()
		return nil, errors.New("an update is already in progress")
	}
	u.busy = true
	u.mu.Unlock()
	defer func() { u.mu.Lock(); u.busy = false; u.mu.Unlock() }()

	st, zr, err := u.Verify(zipPath, requireSigned)
	if err != nil {
		return nil, err
	}
	defer zr.Close()

	if st.Version == u.link("current") {
		return nil, fmt.Errorf("version %s is already active", st.Version)
	}
	releases := filepath.Join(u.root, "releases")
	staging, err := os.MkdirTemp(releases, ".staging-")
	if err != nil {
		return nil, err
	}
	cleanup := func() { os.RemoveAll(staging) }

	bin := zipEntry(&zr.Reader, "smokestack")
	rc, err := bin.Open()
	if err != nil {
		cleanup()
		return nil, err
	}
	dst := filepath.Join(staging, "smokestack")
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		rc.Close()
		cleanup()
		return nil, err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(out, h), io.LimitReader(rc, maxPackageBytes))
	rc.Close()
	out.Close()
	if err != nil {
		cleanup()
		return nil, err
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != strings.ToLower(st.SHA256) {
		cleanup()
		return nil, errors.New("binary SHA-256 differs from the manifest: package tampered with")
	}
	if b, err := json.MarshalIndent(st.Manifest, "", "  "); err == nil {
		os.WriteFile(filepath.Join(staging, "manifest.json"), b, 0o644)
	}

	// Autotest : le nouveau binaire doit demarrer, charger ses ressources
	// et annoncer la version du manifeste.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	res, err := exec.CommandContext(ctx, dst, "selftest").Output()
	if err != nil {
		cleanup()
		return nil, fmt.Errorf("self-test of the new binary failed: %v", err)
	}
	var self struct {
		OK      bool   `json:"ok"`
		Version string `json:"version"`
	}
	if json.Unmarshal(res, &self) != nil || !self.OK || self.Version != st.Version {
		cleanup()
		return nil, fmt.Errorf("self-test: reported version %q, expected %q", self.Version, st.Version)
	}

	final := filepath.Join(releases, st.Version)
	if st.Version != u.link("current") {
		os.RemoveAll(final)
	}
	if err := os.Rename(staging, final); err != nil {
		cleanup()
		return nil, err
	}
	st.dir = final
	u.mu.Lock()
	u.staged = st
	u.mu.Unlock()
	log.Printf("update: version %s staged (signed: %v, key %s)", st.Version, st.Signed, st.KeyID)
	return st, nil
}

// Apply active la version preparee et demande le redemarrage.
func (u *Updater) Apply(force bool, by string) (*Staged, error) {
	u.mu.Lock()
	st := u.staged
	u.mu.Unlock()
	if st == nil {
		return nil, errors.New("no version staged: upload a package first")
	}
	if !st.Newer && !force {
		return nil, fmt.Errorf("version %s is not newer than %s: "+
			"tick \"force\" to go back to an older version", st.Version, Version)
	}
	from, oldPrev := u.link("current"), u.link("previous")
	if err := u.backupConfig(); err != nil {
		log.Printf("update: could not back up config.db (%v), continuing", err)
	}
	if from != "" {
		swapLink(filepath.Join(u.root, "previous"), filepath.Join("releases", from))
	}
	if err := swapLink(filepath.Join(u.root, "current"), filepath.Join("releases", st.Version)); err != nil {
		return nil, fmt.Errorf("switch failed: %w", err)
	}
	u.writePending(pending{Previous: oldPrev, From: from, To: st.Version, At: time.Now().Unix()})
	u.record("update", from, st.Version, by)
	u.prune()
	u.mu.Lock()
	u.staged = nil
	u.mu.Unlock()
	log.Printf("update: %s -> %s, restarting", from, st.Version)
	u.signalRestart()
	return st, nil
}

// Rollback revient a la version precedente.
func (u *Updater) Rollback(by string) (string, error) {
	if !u.Managed() {
		return "", errors.New(u.reason)
	}
	prev, cur := u.link("previous"), u.link("current")
	if prev == "" || prev == cur {
		return "", errors.New("no previous version available")
	}
	if _, err := os.Stat(filepath.Join(u.root, "releases", prev, "smokestack")); err != nil {
		return "", fmt.Errorf("version %s not found on disk", prev)
	}
	if err := swapLink(filepath.Join(u.root, "current"), filepath.Join("releases", prev)); err != nil {
		return "", err
	}
	swapLink(filepath.Join(u.root, "previous"), filepath.Join("releases", cur))
	os.Remove(filepath.Join(u.dataDir, pendingFile))
	u.record("rollback", cur, prev, by)
	log.Printf("rollback: %s -> %s, restarting", cur, prev)
	u.signalRestart()
	return prev, nil
}

func (u *Updater) signalRestart() {
	select {
	case u.Restart <- struct{}{}:
	default:
	}
}

// CurrentBinary est le chemin a executer apres une bascule.
func (u *Updater) CurrentBinary() string {
	return filepath.Join(u.root, "current", "smokestack")
}

func (u *Updater) backupConfig() error {
	if u.store == nil {
		return nil
	}
	dir := filepath.Join(u.dataDir, "backups")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	name := filepath.Join(dir, fmt.Sprintf("config-%s-%d.db", Version, time.Now().Unix()))
	_, err := u.store.cfg.Exec(`VACUUM INTO ?`, name)
	// On ne garde que les 5 sauvegardes les plus recentes.
	files, _ := filepath.Glob(filepath.Join(dir, "config-*.db"))
	sort.Strings(files)
	for len(files) > 5 {
		os.Remove(files[0])
		files = files[1:]
	}
	return err
}

func (u *Updater) prune() {
	keep := map[string]bool{u.link("current"): true, u.link("previous"): true}
	entries, _ := os.ReadDir(filepath.Join(u.root, "releases"))
	var vers []string
	for _, e := range entries {
		if e.IsDir() && !strings.HasPrefix(e.Name(), ".") {
			vers = append(vers, e.Name())
		}
	}
	sort.Slice(vers, func(i, j int) bool { return compareVersions(vers[i], vers[j]) > 0 })
	kept := 0
	for _, v := range vers {
		if keep[v] {
			continue
		}
		if kept < keepReleases {
			kept++
			continue
		}
		os.RemoveAll(filepath.Join(u.root, "releases", v))
	}
}

// ------------------------------------------------- confirmation / secours

func (u *Updater) writePending(p pending) {
	b, _ := json.Marshal(p)
	os.WriteFile(filepath.Join(u.dataDir, pendingFile), b, 0o640)
}

// CheckPending s'execute tout au debut du demarrage. Apres une mise a
// jour, chaque demarrage incremente un compteur ; au-dela de trois
// tentatives, la version precedente est restauree. Renvoie true si un
// retour arriere a ete fait et qu'il faut relancer le processus.
func (u *Updater) CheckPending() bool {
	path := filepath.Join(u.dataDir, pendingFile)
	b, err := os.ReadFile(path)
	if err != nil || !u.Managed() {
		return false
	}
	var p pending
	if json.Unmarshal(b, &p) != nil {
		os.Remove(path)
		return false
	}
	p.Attempts++
	if p.Attempts > 3 && p.From != "" {
		log.Printf("update: version %s failed %d times, going back to %s",
			p.To, p.Attempts-1, p.From)
		swapLink(filepath.Join(u.root, "current"), filepath.Join("releases", p.From))
		// Le lien previous retrouve sa valeur d'avant la mise a jour :
		// proposer de « revenir » a la version defaillante n'aurait pas de sens.
		if p.Previous != "" && p.Previous != p.From {
			swapLink(filepath.Join(u.root, "previous"), filepath.Join("releases", p.Previous))
		} else {
			os.Remove(filepath.Join(u.root, "previous"))
		}
		b, _ := json.Marshal(p)
		os.WriteFile(filepath.Join(u.dataDir, "update-rolledback.json"), b, 0o640)
		os.Remove(path)
		return true
	}
	u.writePending(p)
	return false
}

// RecordAutoRollback inscrit dans l'historique une restauration
// automatique faite au demarrage (avant que la base soit ouverte).
func (u *Updater) RecordAutoRollback() {
	path := filepath.Join(u.dataDir, "update-rolledback.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var p pending
	if json.Unmarshal(b, &p) == nil {
		u.record("auto_rollback", p.To, p.From, "auto")
		log.Printf("history: automatic rollback %s -> %s recorded", p.To, p.From)
	}
	os.Remove(path)
}

// ConfirmAfter valide la mise a jour une fois le service stable.
func (u *Updater) ConfirmAfter(d time.Duration, stop <-chan struct{}) {
	path := filepath.Join(u.dataDir, pendingFile)
	if _, err := os.Stat(path); err != nil {
		return
	}
	select {
	case <-stop:
	case <-time.After(d):
		os.Remove(path)
		log.Printf("update to %s confirmed", Version)
	}
}

// ------------------------------------------------------------ historique

func (u *Updater) History() []HistoryEntry {
	out := []HistoryEntry{}
	if u.store != nil {
		json.Unmarshal([]byte(u.store.Setting("update_history", "[]")), &out)
	}
	return out
}

func (u *Updater) record(action, from, to, by string) {
	if u.store == nil {
		return
	}
	h := append([]HistoryEntry{{TS: time.Now().Unix(), Action: action, From: from, To: to, By: by}},
		u.History()...)
	if len(h) > 30 {
		h = h[:30]
	}
	b, _ := json.Marshal(h)
	u.store.SetSetting("update_history", string(b))
}

// -------------------------------------------------- verification distante

func (u *Updater) autoFlags() (check, apply bool) {
	check, apply = u.cfg.AutoCheck, u.cfg.AutoApply
	if u.store != nil {
		if v := u.store.Setting("update_auto_check", ""); v != "" {
			check = v == "1"
		}
		if v := u.store.Setting("update_auto_apply", ""); v != "" {
			apply = v == "1"
		}
	}
	return
}

func (u *Updater) Check() (*RemoteRelease, error) {
	if u.cfg.ManifestURL == "" || strings.Contains(u.cfg.ManifestURL, "CHANGE-ME") {
		return nil, errors.New("no update source configured (update.manifest_url)")
	}
	client := &http.Client{Timeout: 20 * time.Second}
	req, _ := http.NewRequest("GET", u.cfg.ManifestURL, nil)
	req.Header.Set("User-Agent", "smokestack/"+Version)
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("update source: HTTP %d", resp.StatusCode)
	}
	var rel RemoteRelease
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return nil, err
	}
	if !validVersion(rel.Version) {
		return nil, errors.New("update source: invalid version")
	}
	rel.CheckedAt = time.Now().Unix()
	rel.Newer = compareVersions(rel.Version, Version) > 0
	u.mu.Lock()
	u.available = &rel
	u.mu.Unlock()
	return &rel, nil
}

// fetchAndApply telecharge le paquet de la plateforme courante et
// l'installe. Les mises a jour automatiques exigent toujours une
// signature, meme si les paquets non signes sont autorises a la main.
func (u *Updater) fetchAndApply(rel *RemoteRelease, by string) error {
	asset, ok := rel.Assets[runtime.GOOS+"-"+runtime.GOARCH]
	if !ok {
		return fmt.Errorf("no %s-%s package in version %s", runtime.GOOS, runtime.GOARCH, rel.Version)
	}
	if !strings.HasPrefix(asset.URL, "https://") {
		return errors.New("non-HTTPS package URL rejected")
	}
	tmp, err := os.CreateTemp(filepath.Join(u.root, "releases"), ".download-*.zip")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Get(asset.URL)
	if err != nil {
		tmp.Close()
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		tmp.Close()
		return fmt.Errorf("telechargement : HTTP %d", resp.StatusCode)
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(tmp, h), io.LimitReader(resp.Body, maxPackageBytes))
	tmp.Close()
	if err != nil {
		return err
	}
	if asset.SHA256 != "" && hex.EncodeToString(h.Sum(nil)) != strings.ToLower(asset.SHA256) {
		return errors.New("downloaded package checksum differs from the announced one")
	}
	if _, err := u.Stage(tmp.Name(), true); err != nil {
		return err
	}
	_, err = u.Apply(false, by)
	return err
}

func (u *Updater) Loop(stop <-chan struct{}) {
	hours := u.cfg.CheckHours
	if hours <= 0 {
		hours = 6
	}
	run := func() {
		check, apply := u.autoFlags()
		if !check {
			return
		}
		rel, err := u.Check()
		if err != nil {
			log.Printf("checking for updates: %v", err)
			return
		}
		if !rel.Newer {
			return
		}
		log.Printf("new version available: %s (current %s)", rel.Version, Version)
		if apply && u.Managed() {
			if err := u.fetchAndApply(rel, "auto"); err != nil {
				log.Printf("automatic update to %s: %v", rel.Version, err)
			}
		}
	}
	select {
	case <-stop:
		return
	case <-time.After(2 * time.Minute):
	}
	run()
	t := time.NewTicker(time.Duration(hours) * time.Hour)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			run()
		}
	}
}

// ------------------------------------------------------------------ routes

func (a *API) UpdateRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/version", a.versionGet)
	if a.upd == nil {
		return
	}
	mux.HandleFunc("GET /api/v1/admin/update", a.need(RoleMaster, a.updState))
	mux.HandleFunc("POST /api/v1/admin/update/upload", a.need(RoleMaster, a.updUpload))
	mux.HandleFunc("POST /api/v1/admin/update/apply", a.need(RoleMaster, a.updApply))
	mux.HandleFunc("POST /api/v1/admin/update/rollback", a.need(RoleMaster, a.updRollback))
	mux.HandleFunc("POST /api/v1/admin/update/check", a.need(RoleMaster, a.updCheck))
	mux.HandleFunc("PUT /api/v1/admin/update/settings", a.need(RoleMaster, a.updSettings))
}

func (a *API) updState(w http.ResponseWriter, r *http.Request, u *User) {
	up := a.upd
	check, apply := up.autoFlags()
	up.mu.Lock()
	staged, avail := up.staged, up.available
	up.mu.Unlock()
	writeJSON(w, map[string]any{
		"version": Version, "build_date": BuildDate,
		"platform": runtime.GOOS + "-" + runtime.GOARCH,
		"managed":  up.Managed(), "reason": up.reason, "root": up.root,
		"current": up.link("current"), "previous": up.link("previous"),
		"staged": staged, "available": avail, "history": up.History(),
		"auto_check": check, "auto_apply": apply,
		"allow_unsigned": up.cfg.AllowUnsigned, "trusted_keys": up.KeyIDs(),
		"manifest_url": up.cfg.ManifestURL,
	})
}

func (a *API) updUpload(w http.ResponseWriter, r *http.Request, u *User) {
	if !a.upd.Managed() {
		writeErr(w, 400, a.upd.reason)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxPackageBytes+(1<<20))
	mr, err := r.MultipartReader()
	if err != nil {
		writeErr(w, 400, "multipart upload expected")
		return
	}
	var tmpName string
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeErr(w, 400, err.Error())
			return
		}
		if part.FormName() != "package" {
			continue
		}
		tmp, err := os.CreateTemp(filepath.Join(a.upd.root, "releases"), ".upload-*.zip")
		if err != nil {
			writeErr(w, 500, err.Error())
			return
		}
		tmpName = tmp.Name()
		_, err = io.Copy(tmp, part)
		tmp.Close()
		if err != nil {
			os.Remove(tmpName)
			writeErr(w, 400, "upload interrupted or too large")
			return
		}
		break
	}
	if tmpName == "" {
		writeErr(w, 400, "missing \"package\" field")
		return
	}
	defer os.Remove(tmpName)
	st, err := a.upd.Stage(tmpName, false)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	a.store.Audit(u, clientIP(r), "update_upload", "version", st.Version)
	writeJSON(w, st)
}

func (a *API) updApply(w http.ResponseWriter, r *http.Request, u *User) {
	var in struct {
		Force bool `json:"force"`
	}
	json.NewDecoder(r.Body).Decode(&in)
	st, err := a.upd.Apply(in.Force, u.Email)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	a.store.Audit(u, clientIP(r), "update_apply", "version", st.Version)
	writeJSON(w, map[string]any{"ok": true, "version": st.Version, "restarting": true})
}

func (a *API) updRollback(w http.ResponseWriter, r *http.Request, u *User) {
	v, err := a.upd.Rollback(u.Email)
	if err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	a.store.Audit(u, clientIP(r), "update_rollback", "version", v)
	writeJSON(w, map[string]any{"ok": true, "version": v, "restarting": true})
}

func (a *API) updCheck(w http.ResponseWriter, r *http.Request, u *User) {
	rel, err := a.upd.Check()
	if err != nil {
		writeErr(w, 502, err.Error())
		return
	}
	writeJSON(w, rel)
}

func (a *API) updSettings(w http.ResponseWriter, r *http.Request, u *User) {
	var in struct {
		AutoCheck *bool `json:"auto_check"`
		AutoApply *bool `json:"auto_apply"`
	}
	if err := json.NewDecoder(r.Body).Decode(&in); err != nil {
		writeErr(w, 400, err.Error())
		return
	}
	if in.AutoCheck != nil {
		a.store.SetSetting("update_auto_check", map[bool]string{true: "1", false: "0"}[*in.AutoCheck])
	}
	if in.AutoApply != nil {
		a.store.SetSetting("update_auto_apply", map[bool]string{true: "1", false: "0"}[*in.AutoApply])
	}
	a.store.Audit(u, clientIP(r), "update_settings", "update", "")
	a.updState(w, r, u)
}
