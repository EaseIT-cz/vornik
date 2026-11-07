// Package text holds system-step handlers that work on text (GitHub #633).
package text

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"vornik.io/vornik/internal/executor"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/readability"
)

// ReportName is the OUTPUT artifact the handler stores.
const ReportName = "readability.md"

// maxInputBytes bounds what one file may be: a larger file is reported as not
// examined rather than read (design §6.3). It bounds the measured part of a
// diff the same way.
const maxInputBytes = 1 << 20

// ArtifactGetter loads artifact metadata.
type ArtifactGetter interface {
	Get(ctx context.Context, id string) (*persistence.Artifact, error)
}

// ArtifactStore reads artifact bodies and stores the report. The production
// store runs its secret scan on every text artifact it stores.
type ArtifactStore interface {
	Open(ctx context.Context, artifactID string) (io.ReadCloser, error)
	Store(ctx context.Context, projectID, executionID, taskID, name, sourcePath string) (*persistence.Artifact, error)
}

// ReadabilityHandler is the text.readability system step: it measures the
// text a review is about with internal/readability and stores the report.
//
// What it measures, in order:
//   - the task's staged input artifacts (the companion review workflows);
//   - otherwise the added lines of prose files in the previous step's diff
//     (github-review, where the step follows forge.fetch_diff).
//
// It makes no network call and starts no process.
type ReadabilityHandler struct {
	artifacts ArtifactGetter
	store     ArtifactStore
}

// NewReadabilityHandler builds the handler.
func NewReadabilityHandler(artifacts ArtifactGetter, store ArtifactStore) *ReadabilityHandler {
	return &ReadabilityHandler{artifacts: artifacts, store: store}
}

// Name is the handler's identifier in workflow YAML.
func (h *ReadabilityHandler) Name() string { return "text.readability" }

type taskInputs struct {
	Context struct {
		InputArtifactIDs []string `json:"inputArtifactIDs"`
	} `json:"context"`
}

// Execute measures, stores the report and returns it as the step's message.
func (h *ReadabilityHandler) Execute(ctx context.Context, in executor.SystemStepInput) (executor.SystemStepResult, error) {
	if h == nil || h.artifacts == nil || h.store == nil {
		return executor.SystemStepResult{}, errors.New("text.readability: handler is missing its artifact dependencies")
	}
	if in.Task == nil {
		return executor.SystemStepResult{}, errors.New("text.readability: task is nil")
	}
	var payload taskInputs
	if len(in.Task.Payload) > 0 {
		_ = json.Unmarshal(in.Task.Payload, &payload)
	}

	var (
		files []readability.File
		skip  []readability.NotExamined
		err   error
	)
	if ids := payload.Context.InputArtifactIDs; len(ids) > 0 {
		files, skip, err = h.measureArtifacts(ctx, in.Task.ProjectID, ids)
		if err != nil {
			return executor.SystemStepResult{}, err
		}
	} else {
		files, skip = measureDiff(diffFrom(in.PrevResult))
	}

	report := readability.Render(files, skip)
	art, err := h.storeReport(ctx, in, report)
	if err != nil {
		return executor.SystemStepResult{}, fmt.Errorf("text.readability: store report: %w", err)
	}
	out, err := json.Marshal(map[string]any{
		"message":            report,
		"artifact_id":        art.ID,
		"files_examined":     len(files),
		"files_not_examined": len(skip),
	})
	if err != nil {
		return executor.SystemStepResult{}, fmt.Errorf("text.readability: marshal result: %w", err)
	}
	return executor.SystemStepResult{Result: out}, nil
}

// measureArtifacts measures each staged input that is text and not too large.
// An artifact of another project is refused outright (the #351 rule
// rag.extract applies: inputArtifactIDs is caller-written).
func (h *ReadabilityHandler) measureArtifacts(ctx context.Context, projectID string, ids []string) ([]readability.File, []readability.NotExamined, error) {
	var (
		files []readability.File
		skip  []readability.NotExamined
	)
	for _, id := range ids {
		art, err := h.artifacts.Get(ctx, id)
		if err != nil || art == nil || art.ProjectID != projectID {
			return nil, nil, fmt.Errorf("text.readability: artifact %s is missing or belongs to another project; refused", id)
		}
		if !isText(art) {
			skip = append(skip, readability.NotExamined{Name: art.Name, Reason: "not a text file"})
			continue
		}
		body, tooLarge, err := h.read(ctx, id)
		if err != nil {
			skip = append(skip, readability.NotExamined{Name: art.Name, Reason: "could not be read"})
			continue
		}
		if tooLarge {
			skip = append(skip, readability.NotExamined{Name: art.Name, Reason: "larger than 1 MiB"})
			continue
		}
		files = append(files, readability.File{Name: art.Name, Report: readability.Measure(body)})
	}
	return files, skip, nil
}

func (h *ReadabilityHandler) read(ctx context.Context, id string) (string, bool, error) {
	rc, err := h.store.Open(ctx, id)
	if err != nil {
		return "", false, err
	}
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(io.LimitReader(rc, maxInputBytes+1))
	if err != nil {
		return "", false, err
	}
	if len(b) > maxInputBytes {
		return "", true, nil
	}
	return string(b), false, nil
}

var textExts = map[string]bool{".md": true, ".markdown": true, ".txt": true, ".rst": true, ".adoc": true}

// isText accepts a text MIME type or, when the type is missing or generic,
// a prose file extension.
func isText(a *persistence.Artifact) bool {
	if a.MimeType != nil && strings.HasPrefix(*a.MimeType, "text/") {
		return true
	}
	return textExts[strings.ToLower(filepath.Ext(a.Name))]
}

func (h *ReadabilityHandler) storeReport(ctx context.Context, in executor.SystemStepInput, report string) (*persistence.Artifact, error) {
	f, err := os.CreateTemp("", "vornik-readability-*.md")
	if err != nil {
		return nil, err
	}
	tmp := f.Name()
	defer func() { _ = os.Remove(tmp) }()
	if _, err := f.WriteString(report); err != nil {
		_ = f.Close()
		return nil, err
	}
	if err := f.Close(); err != nil {
		return nil, err
	}
	execID := ""
	if in.Execution != nil {
		execID = in.Execution.ID
	}
	art, err := h.store.Store(ctx, in.Task.ProjectID, execID, in.Task.ID, ReportName, tmp)
	if err != nil {
		return nil, err
	}
	if art == nil {
		return nil, errors.New("store returned no artifact")
	}
	return art, nil
}

// diffFrom reads forge.fetch_diff's "diff" field from the previous result.
func diffFrom(prev json.RawMessage) string {
	if len(prev) == 0 {
		return ""
	}
	var r struct {
		Diff string `json:"diff"`
	}
	if json.Unmarshal(prev, &r) != nil {
		return ""
	}
	return r.Diff
}

var hunkRE = regexp.MustCompile(`^@@ -\d+(?:,\d+)? \+(\d+)(?:,\d+)? @@`)

// measureDiff measures the added lines of each prose file in a unified diff,
// each at its new-file line number; context and removed lines are never
// measured. The lines are measured as they are, never padded out to their
// numbers: a pull request author controls every hunk header, so the work must
// grow with the diff's bytes (at most maxInputBytes are read), not with the
// line numbers it claims (security review of 75a87da36 and c8954a027).
func measureDiff(diff string) ([]readability.File, []readability.NotExamined) {
	if diff == "" {
		return nil, nil
	}
	var skip []readability.NotExamined
	if len(diff) > maxInputBytes {
		cut := strings.LastIndexByte(diff[:maxInputBytes], '\n')
		if cut < 0 {
			cut = maxInputBytes
		}
		diff = diff[:cut]
		skip = append(skip, readability.NotExamined{Name: "diff", Reason: "diff beyond 1 MiB not examined"})
	}
	var (
		files []readability.File
		path  string
		lines []readability.Line
		next  int
	)
	flush := func() {
		if path != "" && len(lines) > 0 {
			files = append(files, readability.File{Name: path, Report: readability.MeasureLines(lines)})
		}
	}
	for _, l := range strings.Split(diff, "\n") {
		switch {
		case strings.HasPrefix(l, "+++ "):
			flush()
			path, lines, next = "", nil, 0
			p := strings.TrimPrefix(strings.TrimPrefix(l, "+++ "), "b/")
			if textExts[strings.ToLower(filepath.Ext(p))] {
				path = p
			}
		case strings.HasPrefix(l, "@@"):
			if m := hunkRE.FindStringSubmatch(l); m != nil {
				next, _ = strconv.Atoi(m[1])
			}
		case strings.HasPrefix(l, "---") || strings.HasPrefix(l, "diff ") || strings.HasPrefix(l, "\\"):
		case strings.HasPrefix(l, "+"):
			if path != "" && next > 0 {
				lines = append(lines, readability.Line{N: next, Text: l[1:]})
			}
			next++
		case strings.HasPrefix(l, "-"):
		default:
			next++
		}
	}
	flush()
	return files, skip
}
