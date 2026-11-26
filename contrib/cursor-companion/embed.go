// Package cursorcompanion embeds the Cursor companion plugin so `vornikctl
// companion connect cursor` installs the bundle this repository tests (LLD 28
// B8). Every file under this directory except this one and the generator is
// part of the plugin.
package cursorcompanion

import "embed"

// Bundle is the plugin tree, rooted at this directory.
//
//go:embed .cursor-plugin/plugin.json mcp.json hooks rules skills scripts README.md LICENSE
var Bundle embed.FS
