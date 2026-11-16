package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
)

// Les interfaces sont traduites par des fichiers JSON, un par langue.
// L'anglais est la langue de reference et il est obligatoire : sans
// en.json, l'instance refuse de demarrer. Toute cle absente d'une autre
// langue retombe sur l'anglais, ce qui garantit qu'aucun ecran n'affiche
// jamais une cle brute.
//
// Sources, dans l'ordre (la derniere l'emporte) :
//   - les fichiers embarques dans le binaire (web/i18n/*.json) ;
//   - <data_dir>/i18n/*.json, pour ajouter ou corriger une langue sans
//     recompiler.
//
// Les fichiers acceptent des objets imbriques, aplatis en cles pointees :
//   {"home": {"faults": "Faults"}}  ->  "home.faults"
// Les metadonnees vivent sous "_meta" : name (nom natif), dir (ltr/rtl),
// flag (code pays a deux lettres, dont l'interface tire le drapeau).

const baseLang = "en"

var (
	langCodeRE = regexp.MustCompile(`^[a-z]{2,3}(-[A-Z]{2})?$`)
	countryRE  = regexp.MustCompile(`^[A-Z]{2}$`)
	varRE      = regexp.MustCompile(`\{[a-z_]+\}`)
)

type LangInfo struct {
	Code     string   `json:"code"`
	Name     string   `json:"name"`
	Dir      string   `json:"dir"`
	Flag     string   `json:"flag"`
	Coverage float64  `json:"coverage"`
	Missing  int      `json:"missing"`
	Source   string   `json:"source"`
	Warnings []string `json:"warnings,omitempty"`
}

type I18n struct {
	mu    sync.RWMutex
	dicts map[string]map[string]string
	infos map[string]*LangInfo
	rev   string
	dirs  []string
	embed fs.FS
}

// rev summarises what is loaded. It changes when a translation changes —
// including through a file dropped into <data_dir>/i18n/ and a reload from
// the back-office — and does not change when the same content is loaded
// again, so an address that names it stays valid across a restart.
func dictsRev(dicts map[string]map[string]string) string {
	h := sha256.New()
	codes := make([]string, 0, len(dicts))
	for c := range dicts {
		codes = append(codes, c)
	}
	sort.Strings(codes)
	for _, c := range codes {
		d := dicts[c]
		keys := make([]string, 0, len(d))
		for k := range d {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Fprintf(h, "%s\x00", c)
		for _, k := range keys {
			fmt.Fprintf(h, "%s\x00%s\x00", k, d[k])
		}
	}
	return hex.EncodeToString(h.Sum(nil)[:6])
}

func flatten(prefix string, in map[string]any, out map[string]string) {
	for k, v := range in {
		key := k
		if prefix != "" {
			key = prefix + "." + k
		}
		switch t := v.(type) {
		case string:
			out[key] = t
		case map[string]any:
			flatten(key, t, out)
		}
	}
}

func parseLang(b []byte) (map[string]string, error) {
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	out := map[string]string{}
	flatten("", raw, out)
	return out, nil
}

func NewI18n(embedded fs.FS, dataDir string) (*I18n, error) {
	i := &I18n{embed: embedded, dirs: []string{filepath.Join(dataDir, "i18n")}}
	return i, i.Reload()
}

func (i *I18n) Reload() error {
	dicts := map[string]map[string]string{}
	sources := map[string]string{}

	load := func(name string, b []byte, src string) {
		code := strings.TrimSuffix(filepath.Base(name), ".json")
		if !langCodeRE.MatchString(code) {
			log.Printf("i18n: file ignored (invalid code): %s", name)
			return
		}
		d, err := parseLang(b)
		if err != nil {
			log.Printf("i18n: %s illisible: %v", name, err)
			return
		}
		if dicts[code] == nil {
			dicts[code] = map[string]string{}
		}
		for k, v := range d {
			dicts[code][k] = v
		}
		sources[code] = src
	}

	if i.embed != nil {
		entries, _ := fs.ReadDir(i.embed, "i18n")
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			if b, err := fs.ReadFile(i.embed, "i18n/"+e.Name()); err == nil {
				load(e.Name(), b, "embedded")
			}
		}
	}
	for _, dir := range i.dirs {
		files, _ := filepath.Glob(filepath.Join(dir, "*.json"))
		for _, f := range files {
			if b, err := os.ReadFile(f); err == nil {
				load(f, b, "override")
			}
		}
	}

	base, ok := dicts[baseLang]
	if !ok {
		return fmt.Errorf("reference language file %s.json not found: "+
			"English is mandatory", baseLang)
	}

	infos := map[string]*LangInfo{}
	total := 0
	for k := range base {
		if !strings.HasPrefix(k, "_meta.") {
			total++
		}
	}
	for code, d := range dicts {
		info := &LangInfo{Code: code, Name: d["_meta.name"], Dir: d["_meta.dir"],
			Flag: strings.ToUpper(d["_meta.flag"]), Source: sources[code]}
		if info.Name == "" {
			info.Name = code
		}
		if info.Dir != "rtl" {
			info.Dir = "ltr"
		}
		// The flag is the country the language declares, as a two-letter
		// code; the interface derives the emoji from it. Anything else is
		// dropped rather than published as a flag.
		if !countryRE.MatchString(info.Flag) {
			info.Flag = ""
		}
		have := 0
		for k, ref := range base {
			if strings.HasPrefix(k, "_meta.") {
				continue
			}
			tr, ok := d[k]
			if !ok || tr == "" {
				continue
			}
			have++
			// Une variable {n} presente en anglais mais oubliee dans la
			// traduction produirait un texte faux : on le signale.
			if !sameVars(ref, tr) {
				info.Warnings = append(info.Warnings,
					fmt.Sprintf("%s: placeholders differ from English", k))
			}
		}
		for k := range d {
			if _, ok := base[k]; !ok && !strings.HasPrefix(k, "_meta.") {
				info.Warnings = append(info.Warnings,
					fmt.Sprintf("%s: key unknown in the reference", k))
			}
		}
		sort.Strings(info.Warnings)
		info.Missing = total - have
		if total > 0 {
			info.Coverage = float64(have) / float64(total)
		}
		infos[code] = info
		if len(info.Warnings) > 0 {
			log.Printf("i18n %s: %d warning(s)", code, len(info.Warnings))
		}
	}

	i.mu.Lock()
	i.dicts, i.infos, i.rev = dicts, infos, dictsRev(dicts)
	i.mu.Unlock()
	log.Printf("i18n: %d language(s) loaded, reference %s (%d keys)",
		len(dicts), baseLang, total)
	return nil
}

func sameVars(a, b string) bool {
	va, vb := varRE.FindAllString(a, -1), varRE.FindAllString(b, -1)
	sort.Strings(va)
	sort.Strings(vb)
	return strings.Join(va, ",") == strings.Join(vb, ",")
}

// Dict renvoie le dictionnaire complet d'une langue, complete par
// l'anglais pour toute cle manquante.
func (i *I18n) Dict(code string) (map[string]string, bool) {
	i.mu.RLock()
	defer i.mu.RUnlock()
	d, ok := i.dicts[code]
	if !ok {
		return nil, false
	}
	out := make(map[string]string, len(i.dicts[baseLang]))
	for k, v := range i.dicts[baseLang] {
		out[k] = v
	}
	for k, v := range d {
		if v != "" {
			out[k] = v
		}
	}
	return out, true
}

// Text is one string with the same English fallback as Dict, for a caller
// on the server that needs a word rather than a whole dictionary.
func (i *I18n) Text(code, key string) string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	if v := i.dicts[code][key]; v != "" {
		return v
	}
	return i.dicts[baseLang][key]
}

func (i *I18n) Languages() []LangInfo {
	i.mu.RLock()
	defer i.mu.RUnlock()
	out := make([]LangInfo, 0, len(i.infos))
	for _, v := range i.infos {
		out = append(out, *v)
	}
	sort.Slice(out, func(a, b int) bool {
		if out[a].Code == baseLang {
			return true
		}
		if out[b].Code == baseLang {
			return false
		}
		return out[a].Name < out[b].Name
	})
	return out
}

func (i *I18n) MissingKeys(code string) []string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	d := i.dicts[code]
	var out []string
	for k := range i.dicts[baseLang] {
		if strings.HasPrefix(k, "_meta.") {
			continue
		}
		if v, ok := d[k]; !ok || v == "" {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

// Rev nomme le contenu charge, pour l'adresse sous laquelle les pages
// demandent un dictionnaire.
func (i *I18n) Rev() string {
	i.mu.RLock()
	defer i.mu.RUnlock()
	return i.rev
}

func (i *I18n) Has(code string) bool {
	i.mu.RLock()
	defer i.mu.RUnlock()
	_, ok := i.dicts[code]
	return ok
}

// ------------------------------------------------------------------ routes

func (a *API) I18nRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/v1/i18n", a.i18nList)
	mux.HandleFunc("GET /api/v1/i18n/{code}", a.i18nDict)
	mux.HandleFunc("GET /api/v1/admin/i18n/{code}/missing", a.need(RoleAdmin, a.i18nMissing))
	mux.HandleFunc("POST /api/v1/admin/i18n/reload", a.need(RoleAdmin, a.i18nReload))
}

func (a *API) i18nList(w http.ResponseWriter, r *http.Request) {
	def := a.store.Site().DefaultLang
	if def == "" || !a.i18n.Has(def) {
		def = baseLang
	}
	// A kilobyte that carries the revision of the dictionaries. It must
	// never be stale: a page holding an old revision would ask for an
	// address that no longer names the current content, and lose the long
	// cache on the ten times larger payload behind it.
	w.Header().Set("Cache-Control", "no-cache")
	writeJSON(w, map[string]any{
		"base": baseLang, "default": def, "rev": a.i18n.Rev(),
		"languages": a.i18n.Languages(),
	})
}

func (a *API) i18nDict(w http.ResponseWriter, r *http.Request) {
	code := r.PathValue("code")
	d, ok := a.i18n.Dict(code)
	if !ok {
		writeErr(w, 404, "unknown language")
		return
	}
	// A dictionary gains keys with every version and can be reloaded while
	// the instance runs, so a cached copy used to be a page showing raw
	// keys such as "detail.navigator" instead of sentences. Asked for
	// under an address that names its revision it cannot go stale, and is
	// kept for a year rather than fetched again on every page.
	//
	// Without the revision, or with one that is no longer current, the
	// answer is revalidated as before: an older page keeps working.
	if r.URL.Query().Get("v") == a.i18n.Rev() {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}
	writeJSON(w, d)
}

func (a *API) i18nMissing(w http.ResponseWriter, r *http.Request, u *User) {
	code := r.PathValue("code")
	if !a.i18n.Has(code) {
		writeErr(w, 404, "unknown language")
		return
	}
	writeJSON(w, map[string]any{"code": code, "missing": a.i18n.MissingKeys(code)})
}

func (a *API) i18nReload(w http.ResponseWriter, r *http.Request, u *User) {
	if err := a.i18n.Reload(); err != nil {
		writeErr(w, 500, err.Error())
		return
	}
	a.store.Audit(u, clientIP(r), "i18n_reload", "i18n", "")
	writeJSON(w, a.i18n.Languages())
}
