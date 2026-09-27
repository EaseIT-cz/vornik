package agentbench

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Independent grading (benchmark LLD §12.24): acceptance suites written from
// each task's specification, never shown to the agents.

// AcceptanceOutcome is the grade of one task repeat.
type AcceptanceOutcome string

// The acceptance outcomes. Only passed, failed, timeout and does_not_compile
// count as graded; error is a harness fault and not_graded means no suite.
const (
	AcceptancePassed         AcceptanceOutcome = "passed"
	AcceptanceFailed         AcceptanceOutcome = "failed"           // an assertion failed
	AcceptanceTimeout        AcceptanceOutcome = "timeout"          // go test's own -timeout: the code hangs
	AcceptanceDoesNotCompile AcceptanceOutcome = "does_not_compile" // package missing or not buildable against the pinned API
	AcceptanceError          AcceptanceOutcome = "error"            // harness fault; never a model failure
	AcceptanceNotGraded      AcceptanceOutcome = "not_graded"       // the task has no suite
)

// AcceptanceResult is one repeat's grade. Output is truncated to 4 KB.
type AcceptanceResult struct {
	Outcome AcceptanceOutcome `json:"outcome"`
	Output  string            `json:"output,omitempty"`
}

// AcceptanceGrader grades the produced package with the task's suite. The
// podman implementation lives in internal/cli: only vornikctl spawns
// processes, and agent-written code never runs on the host.
type AcceptanceGrader interface {
	Grade(ctx context.Context, spec TaskSpec) AcceptanceResult
}

// AcceptanceDir resolves the task's suite directory against the task-set
// directory. "" when the task has no suite. A path that escapes is refused.
func (t TaskSpec) AcceptanceDir() (string, error) {
	if t.Acceptance == "" {
		return "", nil
	}
	clean := filepath.Clean(t.Acceptance)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("task %q acceptance %q must stay inside the task-set directory", t.ID, t.Acceptance)
	}
	return filepath.Join(t.attachmentBase, clean), nil
}

// AcceptanceSetDigest hashes every suite in the set together with which tasks
// have one, order-independently. A set with no suites has its own digest, so
// an ungraded run never silently compares with a graded one (§12.24).
func AcceptanceSetDigest(tasks []TaskSpec) (string, error) {
	type entry struct{ id, body string }
	var entries []entry
	for _, t := range tasks {
		dir, err := t.AcceptanceDir()
		if err != nil {
			return "", err
		}
		if dir == "" {
			entries = append(entries, entry{t.ID, "-"})
			continue
		}
		files, err := filepath.Glob(filepath.Join(dir, "*.go"))
		if err != nil {
			return "", err
		}
		sort.Strings(files)
		var b strings.Builder
		for _, f := range files {
			data, err := os.ReadFile(f)
			if err != nil {
				return "", fmt.Errorf("read acceptance suite: %w", err)
			}
			fmt.Fprintf(&b, "%d:%s%d:%s", len(filepath.Base(f)), filepath.Base(f), len(data), data)
		}
		entries = append(entries, entry{t.ID, b.String()})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].id < entries[j].id })
	h := sha256.New()
	for _, e := range entries {
		_, _ = fmt.Fprintf(h, "%d:%s%d:%s", len(e.id), e.id, len(e.body), e.body)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
