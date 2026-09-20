// Package migrations carries the schema as embedded SQL files.
//
// They are embedded so that the binary is self-contained: a container that
// starts cannot be missing its migrations, and there is no second artefact to
// keep in step with the image.
package migrations

import "embed"

// FS holds every migration, named NNNN_lower_snake_case.sql.
//
//go:embed *.sql
var FS embed.FS
