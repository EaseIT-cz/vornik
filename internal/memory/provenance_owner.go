package memory

import (
	"os"
	"syscall"
)

// ownedByDaemon reports whether the file is owned by the daemon's user. A
// platform whose FileInfo carries no Stat_t refuses (the daemon ships for
// linux), so a daemon-only clone is unusable there rather than unchecked.
func ownedByDaemon(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && int(st.Uid) == os.Geteuid()
}
