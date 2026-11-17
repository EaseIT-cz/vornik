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

// The acceptance outcomes. passed, failed, timeout, does_not_compile and
// pipeline_failed count as graded; error is a harness fault and not_graded
// means no suite.
const (
	AcceptancePassed         AcceptanceOutcome = "passed"
	AcceptanceFailed         AcceptanceOutcome = "failed"           // an assertion failed
	AcceptanceTimeout        AcceptanceOutcome = "timeout"          // go test's own -timeout: the code hangs
	AcceptanceDoesNotCompile AcceptanceOutcome = "does_not_compile" // package missing or not buildable against the pinned API
	AcceptanceError          AcceptanceOutcome = "error"            // harness fault; never a model failure
	AcceptanceNotGraded      AcceptanceOutcome = "not_graded"       // the task has no suite
	// AcceptancePipelineFailed: the repeat failed and its package never
	// reached HEAD, so nothing was graded; a model failure (§12.32, #661).
	AcceptancePipelineFailed AcceptanceOutcome = "pipeline_failed"
)

// AcceptanceResult is one repeat's grade. Output is truncated to 4 KB.
type AcceptanceResult struct {
	Outcome AcceptanceOutcome `json:"outcome"`
	Output  string            `json:"output,omitempty"`
	// FeedbackOutput is the grader's output before the 4 KB journal bound,
	// capped at 64 KB, for the bare baseline's second try only (GitHub #650:
	// Aider's protocol shows the model the test output). Never journaled.
	FeedbackOutput string `json:"-"`
}

// acceptanceOutputLimit is the 4 KB bound on an outcome's output (§12.24).
const acceptanceOutputLimit = 4096

// truncateOutput bounds an outcome's output to acceptanceOutputLimit bytes.
func truncateOutput(s string) string {
	if len(s) <= acceptanceOutputLimit {
		return s
	}
	return s[:acceptanceOutputLimit] + "\n[truncated]"
}

// AcceptanceGrader grades the produced package with the task's suite. The
// podman implementation lives in internal/cli: only vornikctl spawns
// processes, and agent-written code never runs on the host.
type AcceptanceGrader interface {
	Grade(ctx context.Context, spec TaskSpec) AcceptanceResult
}

// AcceptanceDir resolves the task's suite directory against the task-set
// directory. "" when the task has no suite. A path that escapes is refused.
func (t TaskSpec) AcceptanceDir() (string, error) { return t.taskSetDir("acceptance", t.Acceptance) }

// SeedDir resolves the task's seed directory the same way (GitHub #650). ""
// when the task has no seed.
func (t TaskSpec) SeedDir() (string, error) { return t.taskSetDir("seed", t.Seed) }

// taskSetDir resolves a directory a task names relative to its task-set
// file, refusing one that escapes it.
func (t TaskSpec) taskSetDir(field, rel string) (string, error) {
	if rel == "" {
		return "", nil
	}
	clean := filepath.Clean(rel)
	if filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("task %q %s %q must stay inside the task-set directory", t.ID, field, rel)
	}
	return filepath.Join(t.attachmentBase, clean), nil
}

// SuiteFiles are a suite directory's test files in the languages the grader
// runs, sorted: Go and, since GitHub #650, Python. Only these are hashed, so a
// Go-only suite keeps the digest it had before Python was graded.
func SuiteFiles(dir string) ([]string, error) {
	var files []string
	for _, pattern := range []string{"*.go", "*.py"} {
		m, err := filepath.Glob(filepath.Join(dir, pattern))
		if err != nil {
			return nil, err
		}
		files = append(files, m...)
	}
	sort.Strings(files)
	return files, nil
}

// TaskBody is what the task-set digest hashes for one task: its workflow and
// prompt, as before, and the contents of its seed files when it has a seed,
// so a changed stub refuses comparison as a changed prompt does (GitHub #650).
// A task without a seed keeps its body, and so its set keeps its digest.
func TaskBody(t TaskSpec) (string, error) {
	body := t.Workflow + "\x00" + t.Prompt
	dir, err := t.SeedDir()
	if err != nil || dir == "" {
		return body, err
	}
	files, err := SeedFiles(dir)
	if err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString(body + "\x00seed:")
	for _, rel := range files {
		data, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			return "", fmt.Errorf("read seed of %q: %w", t.ID, err)
		}
		fmt.Fprintf(&b, "%d:%s%d:%s", len(rel), rel, len(data), data)
	}
	return b.String(), nil
}

// SeedFiles lists a seed directory's regular files as slash-separated paths
// relative to it, sorted.
func SeedFiles(dir string) ([]string, error) {
	var out []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("seed %s holds a symlink (%s); a seed is plain files", dir, p)
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		out = append(out, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
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
		files, err := SuiteFiles(dir)
		if err != nil {
			return "", err
		}
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
