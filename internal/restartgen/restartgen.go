// Package restartgen is the persisted restart generation (GitHub #799,
// operations-API design §6.13): one integer next to the daemon's
// configuration file, bumped on every restart-pending mark and read once at
// start, before the configuration is parsed.
package restartgen

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

// FileName is the counter's file, in the configuration file's directory.
const FileName = "restart-generation"

// File is the counter. Safe for concurrent use within one process; only the
// running daemon writes it.
type File struct {
	path string
	mu   sync.Mutex
}

// New returns the counter beside configPath.
func New(configPath string) *File {
	return &File{path: filepath.Join(filepath.Dir(configPath), FileName)}
}

// Read returns the count. A missing file is 0; a file that does not parse is
// (0, false), so callers report it as unknown, never as 0.
func (f *File) Read() (int64, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.read()
}

func (f *File) read() (int64, bool) {
	b, err := os.ReadFile(f.path)
	if os.IsNotExist(err) {
		return 0, true
	}
	if err != nil {
		return 0, false
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil || n < 0 {
		return 0, false
	}
	return n, true
}

// Bump adds one: read, increment, then a temporary file, fsync and rename,
// under one mutex. A counter that does not parse is restarted from 0 + 1.
func (f *File) Bump() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, _ := f.read()
	tmp, err := os.CreateTemp(filepath.Dir(f.path), FileName+".tmp-*")
	if err != nil {
		return fmt.Errorf("restart generation: %w", err)
	}
	name := tmp.Name()
	defer func() { _ = os.Remove(name) }()
	if _, err := fmt.Fprintf(tmp, "%d\n", n+1); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("restart generation: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("restart generation: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("restart generation: %w", err)
	}
	if err := os.Rename(name, f.path); err != nil {
		return fmt.Errorf("restart generation: %w", err)
	}
	return nil
}
