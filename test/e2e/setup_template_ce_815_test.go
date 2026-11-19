//go:build e2e_http
// +build e2e_http

package e2e_test

// GitHub #815: on a Community Edition install with auth on, the setup
// guide's step 2 ("Choose what it is for") answered every card with "Could
// not create it: This is an Enterprise Edition feature". The template routes
// gated on requireAdminGate, which answers 501 when the admin surface is not
// wired, and Community never wires it.
//
// Found at: production (operator observation), compared as e2e-lane.
// Test at: e2e-lane. The lane's own daemon runs with auth off, where every
// gate admits every caller, so this test boots a second daemon from the
// same Community binary with auth on and one admin-class key, and sends
// step 2's exact request (internal/ui/templates/setup.html).

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"vornik.io/vornik/internal/testutil/testenv"
)

const (
	ce815AdminKey = "sk-e2e-815-admin"
	ce815PlainKey = "sk-e2e-815-plain"
)

// bootCommunityAuthDaemon starts a second daemon from the lane's Community
// binary with auth on, ce815AdminKey admin-class and ce815PlainKey a plain
// static key. It returns the base URL and the config tree (GitHub #815, #816).
func bootCommunityAuthDaemon(t *testing.T) (string, string) {
	t.Helper()
	work := t.TempDir()
	configsTree := filepath.Join(work, "configs")
	cfgFile := filepath.Join(work, "vornik.yaml")
	port := freePort()
	if err := layoutConfigTree(configsTree, cfgFile, filepath.Join(work, "vornik.db"), work, port); err != nil {
		t.Fatal(err)
	}
	cfg, err := os.ReadFile(cfgFile)
	if err != nil {
		t.Fatal(err)
	}
	authOn := fmt.Sprintf("api:\n  auth_enabled: true\n  api_keys: [%q, %q]\nadmin:\n  enabled: true\n  allowed_keys: [%q]\n", ce815AdminKey, ce815PlainKey, ce815AdminKey)
	patched := strings.Replace(string(cfg), "api:\n  auth_enabled: false\n", authOn, 1)
	if patched == string(cfg) {
		t.Fatal("the lane's config no longer carries the auth block this test replaces")
	}
	if err := os.WriteFile(cfgFile, []byte(patched), 0o600); err != nil {
		t.Fatal(err)
	}

	log, err := os.Create(filepath.Join(work, "daemon.log"))
	if err != nil {
		t.Fatal(err)
	}
	d := exec.Command(e2eBin)
	d.Dir = work
	d.Env = testenv.Daemon(filepath.Join(work, "data"), "VORNIK_CONFIG="+cfgFile, "VORNIK_CONFIGS_DIR="+configsTree)
	d.Stdout, d.Stderr = log, log
	if err := d.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = d.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { _, _ = d.Process.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			_ = d.Process.Kill()
			<-done
		}
		_ = log.Close()
	})
	base := fmt.Sprintf("http://127.0.0.1:%d", port)
	// 120 s, not the lane's 40: this is a second daemon booting beside the
	// lane's own, and under the 2026-11-18 gate (a bench running alongside)
	// its boot was still in progress, with no error, when 40 s ran out.
	if err := waitHealthy(base, 120*time.Second); err != nil {
		b, _ := os.ReadFile(filepath.Join(work, "daemon.log"))
		t.Fatalf("the auth-on daemon never became healthy: %v\n%s", err, b)
	}

	return base, configsTree
}

func TestSetupStep2CreatesProjectOnCommunityWithAuth815(t *testing.T) {
	base, configsTree := bootCommunityAuthDaemon(t)
	client := &http.Client{Timeout: 10 * time.Second} // a hung daemon fails, not stalls
	// Step 2's request, as setup.html sends it.
	post := func(key, pid string) (int, string) {
		body := fmt.Sprintf(`{"slug":%q,"parameters":{"projectId":[%q],"displayName":[%q]}}`, e2eTemplateSlug, pid, "E2E 815")
		req, _ := http.NewRequest(http.MethodPost, base+"/api/v1/projects/from-template", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+key)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(resp.Body)
		return resp.StatusCode, string(b)
	}
	if code, body := post(ce815AdminKey, "ce-815"); code != http.StatusCreated {
		t.Fatalf("an admin-class key on Community: step 2 answered %d %s, want 201", code, body)
	}
	if _, err := os.Stat(filepath.Join(configsTree, "projects", "ce-815.yaml")); err != nil {
		t.Fatalf("the project file was not written: %v", err)
	}
	if code, body := post(ce815PlainKey, "ce-815-plain"); code != http.StatusForbidden || !strings.Contains(body, "OPERATOR_CAPABILITY_REQUIRED") {
		t.Fatalf("a key that is not admin-class: %d %s, want 403 OPERATOR_CAPABILITY_REQUIRED", code, body)
	}

	// The gallery list beside it: the same gate.
	req, _ := http.NewRequest(http.MethodGet, base+"/api/v1/project-templates", nil)
	req.Header.Set("Authorization", "Bearer "+ce815AdminKey)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("listing templates with an admin-class key on Community: %d, want 200", resp.StatusCode)
	}
}
