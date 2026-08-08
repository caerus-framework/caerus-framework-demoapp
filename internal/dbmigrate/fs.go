// Package dbmigrate embeds golang-migrate SQL next to this file.
//
// Go's //go:embed cannot use ".." — so the SQL lives under this package
// directory, not at the module root. Paths in docs still say "migrations/"
// conceptually; on disk they are internal/dbmigrate/migrations/.
package dbmigrate

import "embed"

//go:embed migrations
var Migrations embed.FS
