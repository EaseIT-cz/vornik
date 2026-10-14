package artifacts

import (
	"fmt"
	"os"
	"syscall"
)

// OpenRegularNoFollow opens path for reading without following a symlink in
// its final component and without blocking, and refuses anything that is
// not a regular file (GitHub #359, 15-artifact-store.md §13.3 H3).
//
// O_NOFOLLOW makes a final-component symlink fail with ELOOP instead of
// being followed; O_NONBLOCK makes a FIFO open return at once instead of
// waiting for a writer; the Fstat check runs on the opened descriptor, so
// the type checked is the type read. Intermediate components are resolved
// normally: callers of Store/StoreInput name daemon-owned files. This is
// final-component protection only, NOT confinement: never pass it a path
// any component of which an agent can influence. Paths an agent can change
// are opened beneath a pinned os.Root by the executor's harvest and reach
// the store as a descriptor (StoreFile), not through this function.
func OpenRegularNoFollow(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		_ = f.Close()
		return nil, fmt.Errorf("%s: not a regular file (%s)", path, info.Mode().Type())
	}
	return f, nil
}
