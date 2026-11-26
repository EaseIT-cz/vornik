package registry

import (
	"strings"
	"testing"
)

// GitHub #920 (agent-administered design part 24 §24.3 B1): the load rules.
func TestValidateHandOver_920(t *testing.T) {
	const src = "hermes--prague-venue-research--scout"
	ok := &BrokerHandOver{Workflow: "hermes--prague-venues--enrich", Documents: map[string]string{"venues": "artifacts/out/enrich.md"}}
	for _, tc := range []struct {
		name string
		id   string
		h    *BrokerHandOver
		want string
	}{
		{"none", src, nil, ""},
		{"valid", src, ok, ""},
		{"operator workflow", "research", ok, "agent workflows only"},
		{"other namespace", src, &BrokerHandOver{Workflow: "scout--x--y"}, "of the namespace"},
		{"itself", src, &BrokerHandOver{Workflow: src}, "itself"},
		{"bad property", src, &BrokerHandOver{Workflow: ok.Workflow, Documents: map[string]string{"Venues": "artifacts/out/a.md"}}, "property name"},
		{"path outside out", src, &BrokerHandOver{Workflow: ok.Workflow, Documents: map[string]string{"v": "artifacts/in/a.md"}}, "directly under artifacts/out/"},
		{"nested path", src, &BrokerHandOver{Workflow: ok.Workflow, Documents: map[string]string{"v": "artifacts/out/x/a.md"}}, "directly under artifacts/out/"},
		{"too many", src, &BrokerHandOver{Workflow: ok.Workflow, Documents: map[string]string{"a": "artifacts/out/a", "b": "artifacts/out/b", "c": "artifacts/out/c"}}, "at most"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateHandOver(tc.id, tc.h)
			if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

// Part 24 §24.6 S1: the shared-memory flag is for agent projects other
// than home.
func TestValidateSharedMemory_920(t *testing.T) {
	for _, tc := range []struct {
		id   string
		want string
	}{
		{"hermes--venues", ""},
		{"assistant", "only an agent project"},
		{"hermes--home", "home project holds the shared memory"},
	} {
		p := &Project{ID: tc.id, Memory: ProjectMemory{SharedWithNamespace: true}}
		err := p.validateSharedMemory("p.yaml")
		if tc.want == "" && err != nil || tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("%s: err = %v, want %q", tc.id, err, tc.want)
		}
	}
	if got := (&Project{ID: "hermes--venues", Memory: ProjectMemory{SharedWithNamespace: true}}).MemoryProject(); got != "hermes--home" {
		t.Errorf("shared MemoryProject = %q", got)
	}
	if got := (&Project{ID: "hermes--mail"}).MemoryProject(); got != "hermes--mail" {
		t.Errorf("sealed MemoryProject = %q", got)
	}
}
