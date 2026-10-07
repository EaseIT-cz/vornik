package service

import (
	"os"
	"path/filepath"

	"vornik.io/vornik/internal/api"
)

// containerMarkers are the files a container runtime leaves at the root:
// podman's /run/.containerenv, docker's /.dockerenv.
var containerMarkers = []string{"/run/.containerenv", "/.dockerenv"}

// inContainer reports whether any marker exists. stat is os.Stat in
// production and a fake in tests.
func inContainer(stat func(string) (os.FileInfo, error)) bool {
	for _, m := range containerMarkers {
		if _, err := stat(m); err == nil {
			return true
		}
	}
	return false
}

// daemonHost is what capabilities shows an admin-class caller (plan P6.4,
// amendment F1): vornikctl agent connect compares the UID and tries to read
// the store key's path. Inside a container the UID is not the host's, which
// the containerised flag says (amendment F2).
func (c *Container) daemonHost() api.DaemonHost {
	// Absolute, resolved against the daemon's cwd (where its own store opens
	// it): a relative --config would otherwise be probed against vornikctl's
	// cwd (GitHub #75, T6).
	key := c.storeKeyPath()
	if abs, err := filepath.Abs(key); err == nil {
		key = abs
	}
	return api.DaemonHost{UID: os.Geteuid(), Containerized: inContainer(os.Stat), StoreKeyPath: key}
}
