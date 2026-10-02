package service

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/projectdoctor"
	"vornik.io/vornik/internal/registry"
)

// The fix-it doctor's secret form reaches projectdoctor.SetSecret through
// fixitSecretSetter; an agent project must be refused on this path too, so a
// future second writer cannot bypass the guard unnoticed (design §8.1).
func TestFixitSecretSetter_RefusesAgentProjects(t *testing.T) {
	proj := &registry.Project{ID: "hermes--finance"}
	proj.Permissions.Secrets = []string{"hermes/FIO"}
	writer := &memorySecretWriter{}
	doctor := projectdoctor.New(projectdoctor.Deps{Registry: fakeProjectResolver{proj: proj}, SecretWriter: writer})
	if err := (fixitSecretSetter{doctor: doctor}).Set(context.Background(), "hermes--finance", "hermes/FIO", "v"); err == nil {
		t.Fatal("fix-it path accepted an agent project")
	}
	if len(writer.set) != 0 {
		t.Fatalf("writer called: %v", writer.set)
	}
}
