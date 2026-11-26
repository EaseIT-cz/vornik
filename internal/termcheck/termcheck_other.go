//go:build !linux && !darwin

package termcheck

// isTerminal answers false where no terminal ioctl is wired: a caller then
// takes the non-interactive path, which never waits on a prompt.
func isTerminal(uintptr) bool { return false }
