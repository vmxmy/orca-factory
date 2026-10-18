// Package deploy embeds the operations assets that
// `orca-factory-driver install` writes: systemd/ goes to the user unit
// directory and ops/ to ~/.local/share/orca-factory-ops.
package deploy

import "embed"

//go:embed systemd ops
var Assets embed.FS
