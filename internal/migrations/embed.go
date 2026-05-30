// Package migrations holds the embedded goose SQL migrations for Substrate.
package migrations

import "embed"

// FS holds every migration file, embedded into the binary.
//
//go:embed *.sql
var FS embed.FS
