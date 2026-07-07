// Package migrations embeds the golang-migrate SQL files into the binary so the
// startup runner needs no filesystem layout at runtime — the Docker image
// carries the schema with the code that expects it.
package migrations

import "embed"

// FS holds every NNNN_*.{up,down}.sql migration.
//
//go:embed *.sql
var FS embed.FS
