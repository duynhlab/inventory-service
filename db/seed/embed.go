// Package seed embeds DEV-ONLY demo seed data, applied exclusively by the
// service's `seed` subcommand (via pkg/migratex-style direct exec). It is kept
// OUT of the versioned schema-migration chain in db/migrations/sql so that the
// `migrate` subcommand — which runs in every environment, including production —
// never inserts demo inventory.
package seed

import "embed"

// FS holds the demo seed up-migrations (NNNNNN_*.up.sql) under sql/.
//
//go:embed sql/*.sql
var FS embed.FS
