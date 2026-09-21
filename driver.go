package main

// Le driver SQLite est importé uniquement pour son effet de bord
// (enregistrement sous le nom "sqlite"). Tout le reste du code
// n'utilise que database/sql, ce qui garde la dépendance isolée ici.
import _ "modernc.org/sqlite"
