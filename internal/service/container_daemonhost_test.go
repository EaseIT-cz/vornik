package service

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Plan P6 amendment F2: a containerised daemon's UID means nothing on the
// host, so capabilities says when the daemon runs in one. Control:
// inContainer.
func TestInContainer(t *testing.T) {
	statOnly := func(present string) func(string) (os.FileInfo, error) {
		return func(p string) (os.FileInfo, error) {
			if p == present {
				return nil, nil
			}
			return nil, fs.ErrNotExist
		}
	}
	for present, want := range map[string]bool{"/run/.containerenv": true, "/.dockerenv": true, "/elsewhere": false} {
		if got := inContainer(statOnly(present)); got != want {
			t.Errorf("with %s present: %v, want %v", present, got, want)
		}
	}
	c := &Container{ConfigPath: "/cfg/config.yaml"}
	if h := c.daemonHost(); h.UID != os.Geteuid() || h.StoreKeyPath != c.storeKeyPath() {
		t.Fatalf("daemonHost: %+v", h)
	}
}

// GitHub #75 (T6): a relative --config made store_key_path relative, so
// vornikctl probed it against its own cwd. The daemon reports an absolute path.
func TestDaemonHost_StoreKeyPathIsAbsolute(t *testing.T) {
	c := &Container{ConfigPath: "cfg/config.yaml"}
	h := c.daemonHost()
	if !filepath.IsAbs(h.StoreKeyPath) || !strings.HasSuffix(h.StoreKeyPath, filepath.Join("cfg", "secrets", "store.key")) {
		t.Fatalf("store key path: %q", h.StoreKeyPath)
	}
}
