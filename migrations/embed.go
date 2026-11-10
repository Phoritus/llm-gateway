// Package migrations embeds the SQL migration files so the binary can bring
// its own schema up at startup without shipping a separate migration tool or
// mounting the repository into the container.
package migrations

import "embed"

// FS holds every versioned migration in this directory.
//
//go:embed *.sql
var FS embed.FS
