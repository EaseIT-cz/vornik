// Package logring keeps a bounded, in-memory window of project-attributed
// daemon log lines, so an agent-admin key can read why its own projects'
// work or connections failed without host access (live-task-observation
// design, Change 2026-10-08 C6, GitHub #92; core, both editions).
//
// It is filled by a tap on the root logger's writer. Only lines attributed to
// a project or a task enter it, and of each only an allowlist of fields: the
// error field and every other free-text field are dropped at the tap. The
// ring does no I/O and never fails a log write.
package logring

import (
	"bytes"
	"encoding/json"
	"io"
	"sync"
	"time"
)

const (
	// DefaultMaxLines is the design's bound on lines held.
	DefaultMaxLines = 2000
	// DefaultMaxAge is the design's bound on a line's age.
	DefaultMaxAge = time.Hour
	// maxMessageBytes caps a stored message.
	maxMessageBytes = 512
	// maxFieldBytes caps every other stored string, so the ring has a hard
	// byte ceiling (review-20261008-f7c9 finding 2).
	maxFieldBytes = 128
)

// Line is one stored, attributed log line. Message is the zerolog message
// (a string literal in 1123 of 1135 call sites, counted 2026-10-08); readers
// scrub it before it leaves the daemon.
type Line struct {
	Time        time.Time `json:"time"`
	Level       string    `json:"level,omitempty"`
	Component   string    `json:"component,omitempty"`
	ProjectID   string    `json:"project,omitempty"`
	TaskID      string    `json:"task_id,omitempty"`
	ExecutionID string    `json:"execution_id,omitempty"`
	// Server is an MCP server name on component=mcp lines.
	Server string `json:"server,omitempty"`
	// HTTPStatus is a numeric status a line carried (an MCP dial refused with
	// 401, say); readers map it to a closed connection code, never text.
	HTTPStatus int    `json:"-"`
	Message    string `json:"message"`
}

// Ring is the bounded window. Safe for concurrent use.
type Ring struct {
	mu     sync.Mutex
	lines  []Line // oldest first
	max    int
	maxAge time.Duration
	now    func() time.Time
}

// New builds a ring holding at most maxLines lines no older than maxAge.
func New(maxLines int, maxAge time.Duration) *Ring {
	if maxLines <= 0 {
		maxLines = DefaultMaxLines
	}
	if maxAge <= 0 {
		maxAge = DefaultMaxAge
	}
	return &Ring{max: maxLines, maxAge: maxAge, now: time.Now}
}

// rawLine is the subset of a zerolog JSON line the ring reads. Both
// attribution spellings in use are accepted (A0.10).
type rawLine struct {
	Level       string          `json:"level"`
	Component   string          `json:"component"`
	Message     string          `json:"message"`
	Project     json.RawMessage `json:"project"`
	ProjectID   json.RawMessage `json:"project_id"`
	TaskID      json.RawMessage `json:"task_id"`
	TaskIDCamel json.RawMessage `json:"taskId"`
	ExecID      json.RawMessage `json:"execution_id"`
	ExecIDCamel json.RawMessage `json:"executionId"`
	Server      json.RawMessage `json:"server"`
	HTTPStatus  json.RawMessage `json:"http_status"`
	StatusCamel json.RawMessage `json:"httpStatus"`
}

var (
	attrProject = []byte(`"project`)
	attrTask    = []byte(`"task`)
)

// Append parses one zerolog JSON line and stores it when it is attributed.
// It never panics and never blocks on I/O.
func (r *Ring) Append(raw []byte) {
	if len(raw) == 0 || (!bytes.Contains(raw, attrProject) && !bytes.Contains(raw, attrTask)) {
		return // fast path: most lines carry no attribution
	}
	var rl rawLine
	if json.Unmarshal(bytes.TrimSpace(raw), &rl) != nil {
		return
	}
	l := Line{
		Level:       rl.Level,
		Component:   rl.Component,
		ProjectID:   firstString(rl.ProjectID, rl.Project),
		TaskID:      firstString(rl.TaskID, rl.TaskIDCamel),
		ExecutionID: firstString(rl.ExecID, rl.ExecIDCamel),
		Server:      firstString(rl.Server),
		HTTPStatus:  firstInt(rl.HTTPStatus, rl.StatusCamel),
		Message:     rl.Message,
	}
	if l.ProjectID == "" && l.TaskID == "" {
		return
	}
	l.Message = capString(l.Message, maxMessageBytes)
	for _, f := range []*string{&l.Level, &l.Component, &l.ProjectID, &l.TaskID, &l.ExecutionID, &l.Server} {
		*f = capString(*f, maxFieldBytes)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	l.Time = r.now()
	r.lines = append(r.lines, l)
	r.trimLocked(l.Time)
}

func (r *Ring) trimLocked(now time.Time) {
	drop := 0
	if over := len(r.lines) - r.max; over > 0 {
		drop = over
	}
	for drop < len(r.lines) && now.Sub(r.lines[drop].Time) > r.maxAge {
		drop++
	}
	if drop > 0 {
		// Reslice without copying, so eviction is O(1) under the mutex
		// (review-20261008-f7c9 finding 1). When append later outgrows the
		// backing array it copies only the live lines, so the evicted prefix
		// is released then: amortised O(1), memory within a constant of max.
		r.lines = r.lines[drop:]
	}
}

func capString(v string, n int) string {
	if len(v) > n {
		return v[:n]
	}
	return v
}

// Since returns, oldest first, at most limit of the newest lines at or after
// since whose project is in projects (or every line when all is true). A
// line with only a task id has no project here; a scoped reader resolves
// tasks itself (the design keeps database lookups out of the log path).
func (r *Ring) Since(projects map[string]bool, all bool, since time.Time, limit int) []Line {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.trimLocked(r.now())
	var out []Line
	for _, l := range r.lines {
		if l.Time.Before(since) {
			continue
		}
		if all || projects[l.ProjectID] {
			out = append(out, l)
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out
}

// Oldest is the time of the oldest line held (zero when empty), so a reader
// can tell "no lines" from "older than the ring".
func (r *Ring) Oldest() time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.trimLocked(r.now())
	if len(r.lines) == 0 {
		return time.Time{}
	}
	return r.lines[0].Time
}

// Tap is an io.Writer for the root logger's MultiWriter: it offers each line
// to the ring and always reports every byte written, with no error, so the
// other writers (stdout) are never affected.
func (r *Ring) Tap() io.Writer { return tap{r} }

type tap struct{ r *Ring }

func (t tap) Write(p []byte) (int, error) {
	t.r.Append(p)
	return len(p), nil
}

func firstString(vals ...json.RawMessage) string {
	for _, v := range vals {
		var s string
		if len(v) > 0 && json.Unmarshal(v, &s) == nil && s != "" {
			return s
		}
	}
	return ""
}

func firstInt(vals ...json.RawMessage) int {
	for _, v := range vals {
		var n int
		if len(v) > 0 && json.Unmarshal(v, &n) == nil && n != 0 {
			return n
		}
	}
	return 0
}
