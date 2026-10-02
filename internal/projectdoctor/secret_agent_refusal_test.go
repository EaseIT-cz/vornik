package projectdoctor

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"vornik.io/vornik/internal/registry"
)

// Design review round 3: projectdoctor is a second writer of secret values
// into the process environment. It must refuse agent projects, or a value
// entered on the phone could also land in plaintext secrets/*.env.
func TestSetSecret_RefusesAgentProjects(t *testing.T) {
	proj := &registry.Project{ID: "hermes--finance", Permissions: registry.ProjectPermissions{Secrets: []string{"hermes/FIO"}}}
	w := &recordingSecretWriter{}
	d := New(Deps{Registry: fakeResolver{proj: proj}, SecretWriter: w})
	err := d.SetSecret("hermes--finance", "hermes/FIO", "v")
	if err == nil {
		t.Fatal("SetSecret accepted an agent project")
	}
	if !strings.Contains(err.Error(), "approver device") {
		t.Fatalf("refusal does not say where the credential goes: %v", err)
	}
	if len(w.calls) != 0 {
		t.Fatal("the writer was called")
	}
}

// Plan review f329 finding 1 (design §13): drive the real env writer for an
// agent project, then assert no agent-shaped credential exists in the process
// environment or in any file under the secrets dir. This is the tree-wide
// negative for this writer; the other writers (fix-it, the project-setup
// page, integrations.Save) carry their own refusal tests.
func TestAgentCredential_NeverReachesTheFlatEnvironment(t *testing.T) {
	dir := t.TempDir()
	// Snapshot first, so an inherited variable can never false-fail the
	// check: only what this test's act adds is judged (review 8932, minor).
	before := map[string]bool{}
	for _, kv := range os.Environ() {
		before[kv] = true
	}
	proj := &registry.Project{ID: "hermes--finance", Permissions: registry.ProjectPermissions{Secrets: []string{"hermes/FIO"}}}
	d := New(Deps{Registry: fakeResolver{proj: proj}, SecretWriter: NewEnvSecrets(dir)})
	_ = d.SetSecret("hermes--finance", "hermes/FIO", "AGENT-CANARY-VALUE")

	shape := regexp.MustCompile(`^[a-z][a-z0-9]{1,15}/[A-Z][A-Z0-9_]{0,63}=`)
	for _, kv := range os.Environ() {
		if before[kv] {
			continue
		}
		if shape.MatchString(kv) || strings.Contains(kv, "AGENT-CANARY-VALUE") {
			t.Fatalf("agent credential in the process environment: %q", strings.SplitN(kv, "=", 2)[0])
		}
	}
	examined := 0
	_ = filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		examined++
		b, _ := os.ReadFile(path)
		if strings.Contains(string(b), "AGENT-CANARY-VALUE") || strings.Contains(string(b), "hermes/FIO") {
			t.Fatalf("agent credential written to %s", path)
		}
		return nil
	})
	t.Logf("examined %d secrets file(s) and %d environment entries", examined, len(os.Environ()))
}
