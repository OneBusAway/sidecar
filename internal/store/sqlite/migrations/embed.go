// Package migrations holds the embedded goose migration files for SQLite.
package migrations

import "embed"

// FS holds every migration, embedded so the binary needs no files on disk.
//
//go:embed *.sql
var FS embed.FS
