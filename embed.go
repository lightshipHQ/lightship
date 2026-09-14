// Package lightship embeds the assets the binary must carry. Migrations live at the repository root
// so they are reviewable next to the schema documentation, and are embedded here because a
// //go:embed path cannot escape its own directory.
package lightship

import "embed"

//go:embed migrations/*.sql
var Migrations embed.FS
