package persistence

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"vornik.io/vornik/internal/testutil/repowalk"
)

// TestNoSeventhRetrySpelling is the ratchet that stops the extraction being
// undone one call site at a time.
//
// Six places had independently spelled "attempt < maxAttempts", and one of them
// (executor.taskWillRetry) documented itself as a hand-copied mirror kept in
// sync by comment. That is how the drift happened, and a seventh spelling would
// restart it — silently, because a budget-only check still compiles, still
// passes its own tests, and only misbehaves on the narrow class of failures
// that must not retry.
//
// Design https://docs.vornik.io §5.
func TestNoSeventhRetrySpelling(t *testing.T) {
	root := repoRootFromPersistence(t)

	// The shape being banned: a comparison of an attempt counter against a
	// max-attempts counter, in the packages that own retry decisions.
	// The max operand may be selector-qualified (task.MaxAttempts): the
	// unqualified-only pattern never saw the one spelling it exempted (#334
	// item 6, 2026-10-14).
	pattern := regexp.MustCompile(`(?i)\battempt\s*<\s*[\w.]*max\w*attempts?\b`)

	// Known, deliberate exceptions — each is NOT a retry decision. Each entry
	// must still hold a match: an allowance whose spelling the pattern no
	// longer sees is an allowance nobody is checking (#334 item 6, CI
	// pipeline design amendment 2026-10-14). The predicate itself
	// (internal/persistence/retry_predicate.go) was listed here too, but it
	// lies outside the walked packages, so the entry could never be read.
	allowed := map[string]string{
		// The delegated-child retry-storm guard fakes budget exhaustion. It
		// predates the predicate, reaches the same end state, and replacing it
		// is a behaviour change with its own measurement (see the note there).
		"internal/executor/workflow.go": "delegated-child retry-storm guard: drains the budget deliberately",
	}
	seenAllowed := map[string]bool{}
	files := 0

	for _, dir := range []string{"internal/scheduler", "internal/executor"} {
		walkErr := filepath.Walk(filepath.Join(root, dir), func(path string, info os.FileInfo, err error) error {
			if err == nil && info.IsDir() && repowalk.SkipDir(filepath.Join(root, dir), path, info.Name()) {
				return filepath.SkipDir
			}
			if err != nil || info.IsDir() || !strings.HasSuffix(path, ".go") {
				return err
			}
			if strings.HasSuffix(path, "_test.go") {
				return nil
			}
			files++
			rel, _ := filepath.Rel(root, path)
			rel = filepath.ToSlash(rel)
			_, isAllowed := allowed[rel]
			body, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			for i, line := range strings.Split(string(body), "\n") {
				// Comments describing the old shape are fine; code is not.
				trimmed := strings.TrimSpace(line)
				if strings.HasPrefix(trimmed, "//") {
					continue
				}
				if pattern.MatchString(line) && isAllowed {
					seenAllowed[rel] = true
					continue
				}
				if pattern.MatchString(line) {
					t.Errorf("%s:%d spells the retry-budget check by hand:\n    %s\n\n"+
						"Call persistence.TaskShouldRetry(attempt, maxAttempts, class) instead. Six\n"+
						"hand-copied spellings is how the executor's notification gating drifted out\n"+
						"of sync with the scheduler's actual decision; a seventh restarts that.\n"+
						"If this genuinely is not a retry decision, add it to `allowed` with a reason.",
						rel, i+1, trimmed)
				}
			}
			return nil
		})
		if walkErr != nil {
			t.Fatalf("walk %s: %v", dir, walkErr)
		}
	}
	t.Logf("retry-spelling ratchet: examined %d non-test Go files; %d of %d allowance(s) matched", files, len(seenAllowed), len(allowed))
	if files < 50 {
		t.Errorf("retry-spelling ratchet: examined %d non-test Go files of internal/scheduler and internal/executor (floor 50); a ratchet that looked at nothing has not passed", files)
	}
	for rel, why := range allowed {
		if !seenAllowed[rel] {
			t.Errorf("allowed entry %s (%s) matches no line: the pattern no longer sees the spelling it exempts, or the code moved; fix the pattern or drop the entry", rel, why)
		}
	}
}

// repoRootFromPersistence walks up to the module root.
func repoRootFromPersistence(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, statErr := os.Stat(filepath.Join(dir, "go.mod")); statErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("go.mod not found walking up from cwd")
		}
		dir = parent
	}
}
