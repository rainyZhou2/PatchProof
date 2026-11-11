// Package postgresmigrations embeds the PostgreSQL schema migrations for the
// metadata store.
package postgresmigrations

import "embed"

// FS contains the migration SQL files, one file per schema version.
//
//go:embed *.sql
var FS embed.FS
