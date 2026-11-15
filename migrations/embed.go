// Package migrations embeds the goose SQL migrations so the binary can apply them.
package migrations

import "embed"

// FS contains every *.sql migration at its root.
//
//go:embed *.sql
var FS embed.FS
