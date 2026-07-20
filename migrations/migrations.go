// Package migrations embeds the goose SQL migrations so they ship inside the single
// binary and run in-process at startup (spec §2; rollback posture in DECISIONS D15).
package migrations

import "embed"

//go:embed *.sql
var FS embed.FS
