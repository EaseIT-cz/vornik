// Package instanceid holds the one rule for a managed instance's id (GitHub
// #566; mcp-server-authentication-design.md §16.2 R3). The provision command,
// the config and the OAuth state constructor all use it, so no producer can
// put a "." in an id: the EaseIT OAuth router splits a routed state at its
// first ".".
package instanceid

import "regexp"

// Pattern is an instance id: lowercase letters, digits and inner hyphens, at
// most 64 characters.
var Pattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$`)
