package main

import (
	"database/sql"
	"log"
	"strings"
)

// addColumn ajoute une colonne si elle n'existe pas encore. SQLite ne
// connait pas ADD COLUMN IF NOT EXISTS : on tente, et on ignore l'erreur
// de doublon. C'est ce qui permet de faire evoluer le schema d'une
// instance deja en production sans script de migration a la main.
func addColumn(db *sql.DB, table, def string) {
	_, err := db.Exec("ALTER TABLE " + table + " ADD COLUMN " + def)
	if err != nil && !strings.Contains(err.Error(), "duplicate column") {
		log.Printf("migration %s (%s): %v", table, def, err)
	}
}
