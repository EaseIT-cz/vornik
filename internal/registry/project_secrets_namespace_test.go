package registry

import (
	"strings"
	"testing"
)

// Agent-administered Vornik design §8.1: the namespace rule on the secret
// allowlist itself.
func TestProjectSecrets_NamespaceRule(t *testing.T) {
	refuse := []*Project{
		{ID: "hermes--finance", Permissions: ProjectPermissions{Secrets: []string{"GITHUB_TOKEN"}}}, // flat, from an agent project
		{ID: "hermes--finance", Permissions: ProjectPermissions{Secrets: []string{"codex/X"}}},      // foreign namespace
		{ID: "assistant", Permissions: ProjectPermissions{Secrets: []string{"hermes/X"}}},           // operator reaching an agent namespace
	}
	for _, p := range refuse {
		if err := validateProjectSecretsNamespace(p); err == nil {
			t.Errorf("%s with %v: accepted", p.ID, p.Permissions.Secrets)
		}
	}
	allow := []*Project{
		{ID: "hermes--finance", Permissions: ProjectPermissions{Secrets: []string{"hermes/FIO"}}},
		{ID: "assistant", Permissions: ProjectPermissions{Secrets: []string{"GITHUB_TOKEN", "my.token-v2"}}},
	}
	for _, p := range allow {
		if err := validateProjectSecretsNamespace(p); err != nil {
			t.Errorf("%s: %v", p.ID, err)
		}
	}
}

// Review 20261002-a6af finding 2: "--" in an ID is reserved for agent
// namespaces (design §5), so an operator project named my--finance is read
// as namespace "my". That must surface at load as a refusal that names the
// reservation, never as flat secrets that silently stop resolving.
func TestProjectSecrets_ReservedSeparatorIsNamedAtLoad(t *testing.T) {
	p := &Project{ID: "my--finance", Permissions: ProjectPermissions{Secrets: []string{"GITHUB_TOKEN"}}}
	err := validateProjectSecretsNamespace(p)
	if err == nil {
		t.Fatal("accepted flat secrets on an ID in agent namespace my")
	}
	if !strings.Contains(err.Error(), `"--"`) || !strings.Contains(err.Error(), "reserved") {
		t.Fatalf("refusal does not explain the reserved separator: %v", err)
	}
}
