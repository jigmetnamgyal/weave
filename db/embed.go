// Package db embeds the SQL migrations so they travel with any binary that
// needs them.
//
// Embedding rather than reading from disk means a migration job cannot be run
// against a directory that has drifted from the binary, and it lets a future
// deployment ship migrations inside the image it is migrating for.
package db

import "embed"

// MigrationsFS holds the ordered migrations in db/migrations.
//
//go:embed migrations/*.sql
var MigrationsFS embed.FS

// MigrationsDir is the path to the migrations inside MigrationsFS.
const MigrationsDir = "migrations"
