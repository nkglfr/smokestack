package main

import (
	"crypto/rand"
	"log"
	"os"
	"path/filepath"
	"strings"
)

// Code d'installation : tant qu'aucun compte n'existe, la creation du
// compte master exige ce code, affiche dans le journal et ecrit dans un
// fichier lisible uniquement par le service. Il disparait des que le
// premier compte est cree.

const setupAlphabet = "ABCDEFGHJKLMNPQRSTUVWXYZ23456789" // sans 0/O ni 1/I

func newSetupCode() string {
	b := make([]byte, 12)
	rand.Read(b)
	var sb strings.Builder
	for i, c := range b {
		if i > 0 && i%4 == 0 {
			sb.WriteByte('-')
		}
		sb.WriteByte(setupAlphabet[int(c)%len(setupAlphabet)])
	}
	return sb.String()
}

func (a *API) initSetupCode(dataDir string) {
	if a.store.CountUsers() > 0 {
		return
	}
	a.setupFile = filepath.Join(dataDir, "setup-code")
	if b, err := os.ReadFile(a.setupFile); err == nil && len(strings.TrimSpace(string(b))) >= 12 {
		a.setupCode = strings.TrimSpace(string(b))
	} else {
		a.setupCode = newSetupCode()
		if err := os.WriteFile(a.setupFile, []byte(a.setupCode+"\n"), 0o600); err != nil {
			log.Printf("setup-code: %v", err)
		}
	}
	log.Printf("aucun compte : ouvrez /admin et saisissez le code d'installation %s "+
		"(aussi dans %s), ou lancez 'smokestack user add'", a.setupCode, a.setupFile)
}

func (a *API) clearSetupCode() {
	a.setupMu.Lock()
	defer a.setupMu.Unlock()
	a.setupCode = ""
	if a.setupFile != "" {
		os.Remove(a.setupFile)
	}
}
