package service

// GitHub #890 (agent-benchmark design §12.37, part 3): check 3c accepts a
// promoted workflow only at the SHA-256 of its proposal's YAML. That holds
// only if the production writer puts those bytes on disk unchanged, so this
// pins it at the writer the applier uses. Found at: production, the #809
// 2x2 study, where check 3c would refuse a blackbox arm's own promotion.

import (
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
	"testing"
)

func TestPromotedFileHashIsTheProposalsHash_890(t *testing.T) {
	deployed := t.TempDir()
	if err := os.MkdirAll(filepath.Join(deployed, "workflows"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VORNIK_CONFIGS_SOURCE_DIR", "")
	proposalYAML := "---\nworkflowId: dev-pipeline\nmaxStepVisits: 4\n---\n\n# Dev pipeline\n\nrepaired body, no trailing newline"
	if _, err := newFSWorkflowWriter(deployed).Write(context.Background(), "dev-pipeline", []byte(proposalYAML)); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(deployed, "workflows", "dev-pipeline.md"))
	if err != nil {
		t.Fatal(err)
	}
	if sha256.Sum256(got) != sha256.Sum256([]byte(proposalYAML)) {
		t.Fatalf("the writer changed the promoted bytes:\nwrote %q\nwant  %q", got, proposalYAML)
	}
}
