package harnessconfig

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

// statFile is Apply's default stat: a missing file is a state, not an
// error; a symlink is refused, because the rename would replace the link,
// not the file the harness reads.
func statFile(path string) (fileState, error) {
	fi, err := os.Lstat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return fileState{}, nil
	}
	if err != nil {
		return fileState{}, err
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return fileState{}, errors.New(path + " is a symlink; edit the file it points to by hand, or replace the link with the file")
	}
	if !fi.Mode().IsRegular() {
		return fileState{}, errors.New(path + " is not a regular file")
	}
	st := fileState{exists: true, size: fi.Size(), mod: fi.ModTime().UnixNano(), mode: fi.Mode()}
	if sys, ok := fi.Sys().(*syscall.Stat_t); ok {
		st.ino = sys.Ino
	}
	return st, nil
}
