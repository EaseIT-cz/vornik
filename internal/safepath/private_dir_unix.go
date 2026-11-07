//go:build unix

package safepath

import (
	"fmt"
	"io/fs"
	"os"
	"syscall"
)

// notPrivate returns why fi is not private to the current user, or "".
func notPrivate(fi fs.FileInfo) string {
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Sprintf("is writable by group or others (mode %o)", fi.Mode().Perm())
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || int(st.Uid) != os.Getuid() {
		return "is owned by another user"
	}
	return ""
}
