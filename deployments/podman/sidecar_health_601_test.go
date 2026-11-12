package podman

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// GitHub #601 item 1 (08-runtime-manager-podman.md, "Change 2026-11-11: a
// sidecar with no route out restarts itself"). Found at production: after a
// host network outage the pasta-networked scraper and gateway kept running
// with no default route, their units stayed active, and every fetch failed
// until the operator restarted them by hand. Each unit now carries a health
// check that fails without a default route and kills the container, and a
// restart policy that brings a killed container back.
// The gateway unit ships in both editions and is checked here; the scraper
// unit is Enterprise-only, so its half is sidecar_health_601_ee_test.go,
// which the CE export prunes with the unit (CE export runbook, amendment
// 2026-11-09: an Enterprise-only test is pruned by name with the files it
// reads).
func TestSidecarUnits_RestartWithoutADefaultRoute_601(t *testing.T) {
	cmd := sidecarHealthCmd(t, "vornik-gateway.service")
	if cmd == "" {
		return
	}
	cmds := []string{cmd}

	// Run the command against a routing table with and without a default
	// route (the file path swapped for a fixture).
	if _, err := exec.LookPath("sh"); err != nil {
		t.Skip("no sh on this host")
	}
	header := "Iface\tDestination\tGateway \tFlags\tRefCnt\tUse\tMetric\tMask\t\tMTU\tWindow\tIRTT\n"
	withDefault := header + "eth0\t00000000\t0100000A\t0003\t0\t0\t0\t00000000\t0\t0\t0\n" +
		"eth0\t0000000A\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n"
	withoutDefault := header + "eth0\t0000000A\t00000000\t0001\t0\t0\t0\t00FFFFFF\t0\t0\t0\n"
	for name, table := range map[string]string{"with default": withDefault, "without default": withoutDefault, "empty": header} {
		f := filepath.Join(t.TempDir(), "route")
		if err := os.WriteFile(f, []byte(table), 0o600); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(cmds[0], "/proc/net/route") {
			t.Fatalf("health command does not read /proc/net/route: %s", cmds[0])
		}
		err := exec.Command("sh", "-c", strings.ReplaceAll(cmds[0], "/proc/net/route", f)).Run()
		if healthy := err == nil; healthy != (name == "with default") {
			t.Errorf("%s: health command healthy=%v (%v)", name, healthy, err)
		}
	}
}

// sidecarHealthCmd reads contrib/systemd/<unit>, checks the #601 health and
// restart flags, and returns its --health-cmd ("" after reporting a failure).
func sidecarHealthCmd(t *testing.T, unit string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", "..", "contrib", "systemd", unit))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.ReplaceAll(string(b), "\\\n", " ")
	m := regexp.MustCompile(`--health-cmd '([^']+)'`).FindStringSubmatch(text)
	if m == nil {
		t.Errorf("%s: no --health-cmd '...' on podman run", unit)
		return ""
	}
	for _, flag := range []string{"--health-on-failure=kill", "--health-interval=60s", "--health-retries=3"} {
		if !strings.Contains(text, flag) {
			t.Errorf("%s: podman run lacks %s", unit, flag)
		}
	}
	if !regexp.MustCompile(`(?m)^Restart=(on-failure|always)$`).MatchString(text) {
		t.Errorf("%s: a killed container is not restarted (want Restart=on-failure or always)", unit)
	}
	return m[1]
}
