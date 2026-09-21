// Package headway carries the assets the binary has to ship with. It holds no
// logic: it exists because //go:embed cannot reach outside its own directory,
// and PROJECT.md §5 puts migrations/ at the repository root where a reader
// looks for them rather than buried inside a package.
package headway

import "embed"

// Migrations holds the SQL migration files, applied at startup by
// internal/store. Embedding them means the image needs no migration tool and
// no bind mount, which is what makes Definition of Done #8 — docker compose up
// on a clean machine, no manual SQL — achievable.
//
//go:embed migrations/*.sql
var Migrations embed.FS
