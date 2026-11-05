// Package migrations embeds the forward-only goose SQL migrations so every
// binary carries the exact schema it was built against.
package migrations

import "embed"

// FS holds the migration files.
//
//go:embed *.sql
var FS embed.FS
