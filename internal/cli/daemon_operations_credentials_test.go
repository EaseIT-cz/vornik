package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// GitHub #581 D3 (enterprise-packaging-design.md, amendment 2026-10-29): on a
// managed image the idle gate reads its address and key from the instance's
// own config file through this command, so no shell parses YAML and no key
// reaches an argv.

func writeOpsConfig(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "vornik.yaml")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

const managedOpsConfig = `hosting:
  mode: managed
  operations_address: 10.0.0.5:8081
  previous_admin_api_key: sk-old
api:
  api_keys: [sk-old, sk-new]
admin:
  allowed_keys: [sk-old, sk-new]
`

func runOpsCreds(t *testing.T, path string, terminal bool) (string, error) {
	t.Helper()
	prev := stdoutIsTerminal
	stdoutIsTerminal = func(io.Writer) bool { return terminal }
	t.Cleanup(func() { stdoutIsTerminal = prev })
	var out bytes.Buffer
	daemonOpsCredsCmd.SetOut(&out)
	err := runDaemonOperationsCredentials(daemonOpsCredsCmd, path)
	return out.String(), err
}

func TestDaemonOperationsCredentials581_PrintsURLCurrentAndPrevious(t *testing.T) {
	out, err := runOpsCreds(t, writeOpsConfig(t, managedOpsConfig), false)
	if err != nil {
		t.Fatalf("err = %v", err)
	}
	want := "url=http://10.0.0.5:8081\nkey=sk-new\nprevious=sk-old\n"
	if out != want {
		t.Errorf("output = %q; want %q", out, want)
	}
}

func TestDaemonOperationsCredentials581_RefusesATerminal(t *testing.T) {
	out, err := runOpsCreds(t, writeOpsConfig(t, managedOpsConfig), true)
	if err == nil || strings.Contains(out, "sk-") {
		t.Errorf("printed to a terminal: out %q, err %v", out, err)
	}
}

func TestDaemonOperationsCredentials581_RefusesWhatItCannotUse(t *testing.T) {
	for name, body := range map[string]string{
		"a self host":      "api:\n  api_keys: [k]\nadmin:\n  allowed_keys: [k]\n",
		"no address":       "hosting:\n  mode: managed\napi:\n  api_keys: [k]\nadmin:\n  allowed_keys: [k]\n",
		"no current key":   "hosting:\n  mode: managed\n  operations_address: 10.0.0.5:8081\n",
		"two current keys": "hosting:\n  mode: managed\n  operations_address: 10.0.0.5:8081\napi:\n  api_keys: [a, b]\nadmin:\n  allowed_keys: [a, b]\n",
	} {
		t.Run(name, func(t *testing.T) {
			if out, err := runOpsCreds(t, writeOpsConfig(t, body), false); err == nil {
				t.Errorf("err = nil, out %q; want a refusal", out)
			}
		})
	}
	if _, err := runOpsCreds(t, filepath.Join(t.TempDir(), "absent.yaml"), false); err == nil {
		t.Error("an absent file was not refused")
	}
}
