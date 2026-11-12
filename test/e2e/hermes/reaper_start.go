//go:build e2e_hermes

package hermes

import (
	"os"
	"os/exec"
	"sync"
	"syscall"
)

// ensureReaper and reaperOnce are lane-only (the e2e_hermes tag): only the
// lane starts containers and daemons (GitHub #694; reaper.go holds the
// untagged, unit-tested parts).

var reaperOnce sync.Once

// ensureReaper starts this binary's reaper once, before the first container
// or daemon. It runs in its own session (Setsid), so a signal to the run's
// process group does not reach it; a cgroup kill does (lane design).
func ensureReaper() error {
	var err error
	reaperOnce.Do(func() {
		if err = os.WriteFile(reaperPIDs, nil, 0o600); err != nil {
			return
		}
		pid := os.Getpid()
		cmd := exec.Command("bash", "-c", reaperScript(pid, procStartTime(pid), runSuffix, reaperPIDs, 5, 20))
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err = cmd.Start(); err != nil {
			return
		}
		_ = cmd.Process.Release()
	})
	return err
}
