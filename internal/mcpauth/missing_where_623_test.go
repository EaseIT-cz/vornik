package mcpauth

// Change review round 2, finding 2 (review-20261102-6a74): the unresolved-
// secret error must say where the name was looked for. A namespaced name is
// read from the credential store only; another name from the environment,
// then the operator credentials (GitHub #623).

import (
	"strings"
	"testing"
)

func TestMissingWhere623(t *testing.T) {
	if got := MissingWhere("hermes/FIO"); strings.Contains(got, "environment") {
		t.Errorf("namespaced: %q mentions the environment, which is never read for it", got)
	}
	got := MissingWhere("CONTEXT7_API_KEY")
	if !strings.Contains(got, "environment") || !strings.Contains(got, "operator credentials") {
		t.Errorf("un-namespaced: %q should name both places", got)
	}
}
