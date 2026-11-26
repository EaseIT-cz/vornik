// Package termcheck answers one question: is this file an interactive
// terminal?
//
// It asks the kernel for the descriptor's terminal attributes, not the file
// mode. A mode test (os.ModeCharDevice) also matches /dev/null, so a program
// run with stdin at /dev/null took itself for interactive, prompted and read
// EOF (GitHub #865: the EULA acceptance under managed-upgrade).
package termcheck

import "os"

// IsTerminal reports whether f is a terminal. A nil file is not.
func IsTerminal(f *os.File) bool {
	if f == nil {
		return false
	}
	return isTerminal(f.Fd())
}
