package service

import (
	"io/fs"
	"os"
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
