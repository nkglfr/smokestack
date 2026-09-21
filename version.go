package main

import (
	_ "embed"
	"net/http"
	"runtime"
)

// Version et date de construction sont injectees a la compilation :
//
//	go build -ldflags "-X main.Version=1.2.0 -X main.BuildDate=2026-09-21"
var (
	Version   = "dev"
	BuildDate = ""
)

// Adresses du projet, ecrites en dur dans le binaire : le lien vers le
// site officiel figure dans le pied de page de toutes les instances et ne
// se configure pas depuis le back-office. Elles sont fixees a la
// compilation (make OFFICIAL_URL=... REPO_URL=...) ; RepoURL sert aussi
// de source aux mises a jour automatiques. `make release` refuse de
// publier tant qu'elles contiennent CHANGE-ME.
var (
	OfficialURL = "https://smokestack.CHANGE-ME"
	RepoURL     = "https://github.com/CHANGE-ME/smokestack"
)

// Cles publiques de publication, une par ligne ("ed25519:<base64>").
// Seuls les paquets signes par l'une d'elles sont installables, sauf
// autorisation explicite des paquets non signes.
//
//go:embed release.pub
var embeddedReleaseKeys string

func (a *API) versionGet(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, map[string]any{
		"version": Version, "build_date": BuildDate,
		"official_url": OfficialURL, "repo_url": RepoURL,
		"platform": runtime.GOOS + "-" + runtime.GOARCH,
	})
}
