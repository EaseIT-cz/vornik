package executor

// GitHub #578 (tool-credential-carryover design, "Amendment 2026-10-31
// (#578)"): a captured credential is sealed at rest in task_credentials, and
// a read refuses any value that is not sealed for that row. Found at: review
// (#571's design), observed at production level as plaintext rows at rest.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/secretstore"
)

// memCredRepo is a plain in-memory TaskCredentialRepository: what it holds is
// exactly what would be at rest.
type memCredRepo struct{ rows []*persistence.TaskCredential }

func (m *memCredRepo) Upsert(_ context.Context, c *persistence.TaskCredential) error {
	cp := *c
	m.rows = append(m.rows, &cp)
	return nil
}

func (m *memCredRepo) ListByTaskLatestExecution(_ context.Context, taskID string) ([]*persistence.TaskCredential, error) {
	var out []*persistence.TaskCredential
	for _, r := range m.rows {
		if r.TaskID == taskID {
			cp := *r
			out = append(out, &cp)
		}
	}
	return out, nil
}

// fakeOperatorSealer seals reversibly, binding the label, like the store.
type fakeOperatorSealer struct{}

func (fakeOperatorSealer) SealOperator(label string, plain []byte) (string, error) {
	return secretstore.SealedPrefix + label + "|" + string(plain), nil
}

func (fakeOperatorSealer) OpenOperator(label, sealed string) ([]byte, error) {
	raw, ok := strings.CutPrefix(sealed, secretstore.SealedPrefix)
	if !ok {
		return nil, secretstore.ErrNotSealed
	}
	l, v, ok := strings.Cut(raw, "|")
	if !ok || l != label {
		return nil, errors.New("does not open")
	}
	return []byte(v), nil
}

func sealedRepo578(inner persistence.TaskCredentialRepository) *SealedTaskCredentials {
	return NewSealedTaskCredentials(inner, func() (OperatorSealer, error) { return fakeOperatorSealer{}, nil }, nil)
}

func TestSealedTaskCredentials578_StoresSealedReadsPlain(t *testing.T) {
	inner := &memCredRepo{}
	r := sealedRepo578(inner)
	c := &persistence.TaskCredential{TaskID: "t1", ExecutionID: "e1", Tool: "mcp__pagedrop__pagedrop_publish", Label: "viewing password", Value: "hunter2-xY9pQ", ArtifactURL: "https://v/p/1"}
	if err := r.Upsert(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if got := inner.rows[0].Value; !strings.HasPrefix(got, secretstore.SealedPrefix) {
		t.Fatalf("stored value is not sealed: %q", got)
	}
	if c.Value != "hunter2-xY9pQ" {
		t.Errorf("Upsert must not change the caller's struct, got %q", c.Value)
	}
	list, err := r.ListByTaskLatestExecution(context.Background(), "t1")
	if err != nil || len(list) != 1 || list[0].Value != "hunter2-xY9pQ" {
		t.Fatalf("read back %v %v, want the plaintext", list, err)
	}
}

func TestSealedTaskCredentials578_RefusesWhatIsNotSealedForTheRow(t *testing.T) {
	inner := &memCredRepo{}
	r := sealedRepo578(inner)
	good := &persistence.TaskCredential{TaskID: "t1", ExecutionID: "e1", Tool: "x", Label: "p", Value: "keep-me"}
	if err := r.Upsert(context.Background(), good); err != nil {
		t.Fatal(err)
	}
	// A plaintext row (written before #578), and a value sealed for another
	// row copied here: neither is ever returned.
	inner.rows = append(inner.rows,
		&persistence.TaskCredential{TaskID: "t1", ExecutionID: "e1", Tool: "y", Label: "p", Value: "plaintext-at-rest"},
		&persistence.TaskCredential{TaskID: "t1", ExecutionID: "e1", Tool: "z", Label: "p", Value: inner.rows[0].Value},
	)
	list, err := r.ListByTaskLatestExecution(context.Background(), "t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Value != "keep-me" {
		t.Fatalf("got %+v, want only the row sealed for itself", list)
	}
}

func TestTaskCredentialLabel578_IsInjective(t *testing.T) {
	a := TaskCredentialLabel(&persistence.TaskCredential{TaskID: "t/1", ExecutionID: "e", Tool: "x", ArtifactURL: ""})
	b := TaskCredentialLabel(&persistence.TaskCredential{TaskID: "t", ExecutionID: "1/e", Tool: "x", ArtifactURL: ""})
	if a == b {
		t.Fatalf("two rows share a label: %q", a)
	}
}

// B1a: with no detector the strong-pattern check cannot run, so nothing is
// captured (review 2474 F2: it used to capture anything).
func TestCaptureToolCredential578_NoDetectorNoCapture(t *testing.T) {
	repo := &fakeCredRepo{}
	e := newCredExecutor(t, repo)
	e.secretsDetector = nil
	got := e.captureToolCredential(context.Background(), &persistence.Task{ID: "t1"}, &persistence.Execution{ID: "e1"},
		"mcp__pagedrop__pagedrop_publish", `{"password":"sk-proj1234567890abcdefghijklmnopqrstuv"}`)
	if got != "" || len(repo.creds) != 0 {
		t.Fatalf("captured %q (%d rows) without a detector", got, len(repo.creds))
	}
}

// B2: the captured value never reaches the audit row; a value that was not
// captured is left to the audit's own scanning.
func TestPersistToolAudit578_CarriedValueRedactedInTheAudit(t *testing.T) {
	cases := []struct {
		name, password string
		carried        bool
	}{
		{"carried", "hunter2-xY9pQ", true},
		{"refused strong value", "sk-proj1234567890abcdefghijklmnopqrstuv", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ar := &stubAuditRepoY{}
			repo := &fakeCredRepo{}
			e := newCredExecutor(t, repo)
			e.auditRepo = ar
			out := `{"password":"` + tc.password + `","url":"https://v/p/1"}`
			result, _ := json.Marshal(map[string]any{"toolAudit": []map[string]any{{"tool": "mcp__pagedrop__pagedrop_publish", "input": "{}", "output": out}}})
			e.persistToolAuditFromResult(context.Background(), &persistence.Task{ID: "t1", ProjectID: "p"}, &persistence.Execution{ID: "e1"}, "s1", result)
			if len(ar.entries) != 1 {
				t.Fatalf("want 1 audit row, got %d", len(ar.entries))
			}
			got := ar.entries[0].ToolOutput
			if tc.carried {
				if strings.Contains(got, tc.password) || !strings.Contains(got, "[REDACTED:carried_credential]") {
					t.Errorf("audit row holds the carried value: %q", got)
				}
				if len(repo.creds) != 1 {
					t.Errorf("credential not captured")
				}
			} else if !strings.Contains(got, tc.password) || len(repo.creds) != 0 {
				t.Errorf("a refused value must be left to the audit's scan, uncaptured: %q (%d)", got, len(repo.creds))
			}
		})
	}
}

// scrubbingAuditRepo is a stored audit table with the realtime row already
// written (the agent streams each tool call with the same audit_id; the batch
// insert is ON CONFLICT (id) DO NOTHING, so the realtime row wins).
type scrubbingAuditRepo struct {
	stubAuditRepoY
	stored map[string]string // id -> tool_output
}

func (s *scrubbingAuditRepo) Log(ctx context.Context, e *persistence.ToolAuditEntry) error {
	if _, ok := s.stored[e.ID]; ok {
		return nil // DO NOTHING
	}
	s.stored[e.ID] = e.ToolOutput
	return s.stubAuditRepoY.Log(ctx, e)
}

func (s *scrubbingAuditRepo) ReplaceOutputValue(_ context.Context, _, _, value, replacement string) (int64, error) {
	var n int64
	for id, out := range s.stored {
		if strings.Contains(out, value) {
			s.stored[id] = strings.ReplaceAll(out, value, replacement)
			n++
		}
	}
	return n, nil
}

// Commit security review (2026-10-31) of fa42db10d: the realtime row won the
// race, so redacting only the batch row left the plaintext stored; and a
// value with JSON-escaped characters was not matched in the stored output.
func TestPersistToolAudit578_ScrubsTheRowThatWonTheRace(t *testing.T) {
	for _, pw := range []string{"hunter2-xY9pQ", `pw"with\quote<tag>`} {
		enc, _ := json.Marshal(map[string]string{"password": pw, "url": "https://v/p/1"})
		out := string(enc)
		ar := &scrubbingAuditRepo{stored: map[string]string{"ta_live": out}}
		repo := &fakeCredRepo{}
		e := newCredExecutor(t, repo)
		e.auditRepo = ar
		result, _ := json.Marshal(map[string]any{"toolAudit": []map[string]any{{"audit_id": "ta_live", "tool": "mcp__pagedrop__pagedrop_publish", "input": "{}", "output": out}}})
		e.persistToolAuditFromResult(context.Background(), &persistence.Task{ID: "t1", ProjectID: "p"}, &persistence.Execution{ID: "e1"}, "s1", result)
		if len(repo.creds) != 1 {
			t.Fatalf("%q: not captured", pw)
		}
		stored := ar.stored["ta_live"]
		for _, form := range CarriedCredentialForms(pw) {
			if strings.Contains(stored, form) {
				t.Errorf("%q: the stored row still holds %q: %s", pw, form, stored)
			}
		}
		if !strings.Contains(stored, "[REDACTED:carried_credential]") {
			t.Errorf("%q: no marker in the stored row: %s", pw, stored)
		}
	}
}

// A Python MCP server's json.dumps writes non-ASCII as \uXXXX by default
// (ensure_ascii), so that form is scrubbed too (review b0cf, declined claim
// that Go writes every tool output).
func TestCarriedCredentialForms578_CoversASCIIEscapes(t *testing.T) {
	bs := string(rune(92)) // a backslash, built so no escape sequence is written here
	value := "p" + string(rune(0xe4)) + "ss" + string(rune(0x1f600))
	want := "p" + bs + "u00e4ss" + bs + "ud83d" + bs + "ude00"
	forms := CarriedCredentialForms(value)
	for _, f := range forms {
		if f == want {
			return
		}
	}
	t.Fatalf("no ASCII-escaped form %q among %q", want, forms)
}
