// Package migrations embeds the forward-only SQL migration files so the
// server binary carries its schema with no filesystem dependency.
package migrations

import "embed"

// FS contains every *.sql migration, applied in lexical file order.
//
//go:embed *.sql
var FS embed.FS
