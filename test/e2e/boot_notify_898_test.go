//go:build e2e_http
// +build e2e_http

package e2e_test

// GitHub #898: a boot load that blocks (a project file that is a named pipe)
// left the daemon alive and answering nothing, and the quickstart unit was
// Type=simple, so systemd reported it `active (running)` with no bound:
// TimeoutStartSec covered only ExecStartPre.
//
// Found at: e2e-lane (while writing the #879 test,
// TestBootReload879_AHungBootLoadHangsTheBoot).
//
// The fix makes the shipped unit Type=notify, so systemd counts the unit
// started only on the daemon's READY=1 and TimeoutStartSec bounds the whole
// boot (quickstart-host-daemon-install design, change 2026-11-24). This test
// plays systemd's half of that contract against the real binary: it reads
// the unit the quickstart installs, hands the daemon a NOTIFY_SOCKET, and
// checks that READY=1 does not arrive while the boot load is blocked and does
// arrive, with /healthz answering, once the load completes. systemd itself is
// not run: a user manager on the lane host is production's.

import (
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestBoot898_TheUnitWaitsForReadyAndAHungBootNeverSendsIt fails on a unit
// that is not Type=notify, because then no READY=1 bounds the boot.
func TestBoot898_TheUnitWaitsForReadyAndAHungBootNeverSendsIt(t *testing.T) {
	unitPath := filepath.Join(repoRoot(), "deployments", "podman", "systemd", "vornik.service")
	unit, err := os.ReadFile(unitPath)
	if err != nil {
		t.Fatal(err)
	}
	if typ := unitServiceType(string(unit)); typ != "notify" {
		t.Fatalf("%s is Type=%s: systemd counts the unit started when the process forks, so "+
			"TimeoutStartSec never bounds a hung boot and a daemon that answers nothing shows "+
			"active (running). It must be Type=notify (the daemon sends READY=1 once it serves).", unitPath, typ)
	}

	// The notify socket systemd would hand the daemon. A short directory: a
	// unix socket path is limited to 108 bytes.
	sockDir, err := os.MkdirTemp("", "n898")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sockDir) })
	sockPath := filepath.Join(sockDir, "notify")
	notify, err := net.ListenUnixgram("unixgram", &net.UnixAddr{Name: sockPath, Net: "unixgram"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = notify.Close() })
	messages := make(chan string, 8)
	go func() {
		buf := make([]byte, 4096)
		for {
			n, _, err := notify.ReadFromUnix(buf)
			if err != nil {
				close(messages)
				return
			}
			messages <- string(buf[:n])
		}
	}()

	const hung = "e2e-898-hung"
	d := layout879(t)
	d.env = []string{"NOTIFY_SOCKET=" + sockPath}
	h := d.startWithHungProject(t, hung)

	// The boot is blocked in the read. For 5 s: alive, and no READY=1, so
	// systemd would still be counting TimeoutStartSec.
	select {
	case msg := <-messages:
		t.Fatalf("the daemon notified %q while its boot load was blocked", msg)
	case <-d.exited:
		t.Fatalf("the daemon exited while its boot load was blocked:\n%s", d.log())
	case <-time.After(5 * time.Second):
	}

	h.release(t)
	select {
	case msg, ok := <-messages:
		if !ok || !strings.Contains(msg, "READY=1") {
			t.Fatalf("after the load completed the daemon notified %q, not READY=1:\n%s", msg, d.log())
		}
	case <-d.exited:
		t.Fatalf("the daemon exited after its boot load completed:\n%s", d.log())
	case <-time.After(120 * time.Second):
		t.Fatalf("no READY=1 within 120 s of the load completing:\n%s", d.log())
	}

	// READY=1 means serving: the listener is bound before it is sent, so
	// /healthz answers now, without a wait loop.
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get(d.base + "/healthz")
	if err != nil {
		t.Fatalf("/healthz after READY=1: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz after READY=1: HTTP %d", resp.StatusCode)
	}
	if !d.hasProject(t, hung) {
		t.Fatalf("after READY=1: project %s is not loaded", hung)
	}
}

// unitServiceType returns the Type= of a unit's [Service] section, or
// "simple" (systemd's default) when it names none.
func unitServiceType(unit string) string {
	inService := false
	for _, line := range strings.Split(unit, "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			inService = line == "[Service]"
			continue
		}
		if v, ok := strings.CutPrefix(line, "Type="); ok && inService {
			return strings.TrimSpace(v)
		}
	}
	return "simple"
}
