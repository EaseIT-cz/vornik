// Package opencodecompanion embeds the opencode companion bundle so `vornikctl
// companion connect opencode` installs the bundle this repository tests (LLD
// 29 B1). Every file under this directory except this one, its test and the
// skill generator is part of the bundle.
package opencodecompanion

import "embed"

// Bundle is the bundle tree, rooted at this directory.
//
//go:embed plugin skills scripts README.md LICENSE
var Bundle embed.FS
