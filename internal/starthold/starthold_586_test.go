package starthold

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// GitHub #586: a marker file read at process start holds the daemon from
// work (deploy idle gate design §13.1). Absent is not held; any other read
// error or content that does not parse is held, with the error as the
// reason (fail closed). The deadline counts from created_at, or from the
// process start when created_at is missing or in the future.
// Found at: review (the managed upgrade's round 3 found no way to hold a
// new version from work). Regression test at: unit.

var start586 = time.Date(2026, 11, 24, 10, 0, 0, 0, time.UTC)

func writeMarker586(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), FileName)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestStartHold586_Path(t *testing.T) {
	if got := Path("/var/lib/vornik", "/etc/vornik/config.yaml"); got != "/var/lib/vornik/start-held" {
		t.Fatalf("with a data dir: %s", got)
	}
	// Operator answer 3 (2026-11-24): beside the config file without one.
	if got := Path("", "/home/u/.config/vornik/config.yaml"); got != "/home/u/.config/vornik/start-held" {
		t.Fatalf("without a data dir: %s", got)
	}
}

func TestStartHold586_Absent_NotHeld(t *testing.T) {
	h := Read(filepath.Join(t.TempDir(), FileName), start586)
	if h.Held || h.Err != nil {
		t.Fatalf("absent marker: %+v", h)
	}
}

func TestStartHold586_Valid_HeldFromCreatedAt(t *testing.T) {
	created := start586.Add(-10 * time.Minute)
	p := writeMarker586(t, `{"hold_id":"2026.11.6-2026.11.7-1764000000","created_at":"`+created.Format(time.RFC3339)+`","reason":"managed-upgrade a->b","deadline_seconds":600}`+"\n")
	h := Read(p, start586)
	if !h.Held || h.Err != nil {
		t.Fatalf("valid marker: %+v", h)
	}
	if h.HoldID != "2026.11.6-2026.11.7-1764000000" || h.Reason != "managed-upgrade a->b" {
		t.Fatalf("fields: %+v", h)
	}
	if !h.Since.Equal(created) || h.BaseFromStart {
		t.Fatalf("deadline base = %v (from start %v), want created_at", h.Since, h.BaseFromStart)
	}
	if !h.DeadlineAt().Equal(created.Add(600 * time.Second)) {
		t.Fatalf("deadline = %v", h.DeadlineAt())
	}
	if string(h.Raw) == "" {
		t.Fatal("the raw bytes are kept for the write-back")
	}
}

func TestStartHold586_DefaultDeadline(t *testing.T) {
	p := writeMarker586(t, `{"hold_id":"x","created_at":"`+start586.Format(time.RFC3339)+`"}`)
	h := Read(p, start586)
	if !h.Held || h.Err != nil || h.Deadline != DefaultDeadline {
		t.Fatalf("no deadline_seconds: %+v", h)
	}
}

// Every malformed marker holds (fail closed) and names why.
func TestStartHold586_Malformed_HeldWithReason(t *testing.T) {
	ok := start586.Format(time.RFC3339)
	cases := map[string]struct {
		body      string
		fromStart bool
	}{
		"not JSON":          {`hold me`, true},
		"empty":             {``, true},
		"deadline too low":  {`{"hold_id":"x","created_at":"` + ok + `","deadline_seconds":59}`, false},
		"deadline too high": {`{"hold_id":"x","created_at":"` + ok + `","deadline_seconds":86401}`, false},
		"hold_id empty":     {`{"hold_id":"","created_at":"` + ok + `"}`, false},
		"hold_id bad chars": {`{"hold_id":"a b/c","created_at":"` + ok + `"}`, false},
		"hold_id too long":  {`{"hold_id":"` + strings.Repeat("a", 65) + `","created_at":"` + ok + `"}`, false},
		"reason too long":   {`{"hold_id":"x","created_at":"` + ok + `","reason":"` + strings.Repeat("r", 201) + `"}`, false},
		"reason control":    {`{"hold_id":"x","created_at":"` + ok + `","reason":"a\u0007b"}`, false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			h := Read(writeMarker586(t, tc.body), start586)
			if !h.Held {
				t.Fatalf("%s: not held; a malformed marker must hold", name)
			}
			if h.Err == nil {
				t.Fatalf("%s: no reason given", name)
			}
			if h.BaseFromStart != tc.fromStart {
				t.Fatalf("%s: deadline base from start = %v, want %v", name, h.BaseFromStart, tc.fromStart)
			}
			if h.Deadline != DefaultDeadline {
				t.Fatalf("%s: deadline = %v, want the default", name, h.Deadline)
			}
		})
	}
}

func TestStartHold586_CreatedAtMissingOrFuture_CountsFromStart(t *testing.T) {
	for name, body := range map[string]string{
		"missing":   `{"hold_id":"x"}`,
		"unparsed":  `{"hold_id":"x","created_at":"yesterday"}`,
		"in future": `{"hold_id":"x","created_at":"` + start586.Add(time.Hour).Format(time.RFC3339) + `"}`,
	} {
		h := Read(writeMarker586(t, body), start586)
		if !h.Held || !h.BaseFromStart || !h.Since.Equal(start586) {
			t.Fatalf("%s: %+v, want held, counted from the process start", name, h)
		}
		if h.HoldID != "x" {
			t.Fatalf("%s: hold_id lost: %+v", name, h)
		}
	}
}

func TestStartHold586_Unreadable_Held(t *testing.T) {
	// A directory where the file should be: read fails with something
	// other than ENOENT.
	p := filepath.Join(t.TempDir(), FileName)
	if err := os.Mkdir(p, 0o700); err != nil {
		t.Fatal(err)
	}
	h := Read(p, start586)
	if !h.Held || h.Err == nil || !h.BaseFromStart {
		t.Fatalf("unreadable marker: %+v", h)
	}
}

func TestStartHold586_RemoveAndRestore(t *testing.T) {
	body := `{"hold_id":"x","created_at":"` + start586.Format(time.RFC3339) + `"}`
	p := writeMarker586(t, body)
	if err := Remove(p); err != nil {
		t.Fatal(err)
	}
	if Read(p, start586).Held {
		t.Fatal("held after Remove")
	}
	// Removing an absent marker is not an error: the release is idempotent.
	if err := Remove(p); err != nil {
		t.Fatalf("second remove: %v", err)
	}
	if err := Restore(p, []byte(body)); err != nil {
		t.Fatal(err)
	}
	h := Read(p, start586)
	if !h.Held || h.HoldID != "x" {
		t.Fatalf("after Restore: %+v", h)
	}
	fi, err := os.Stat(p)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("restored mode = %v (%v), want 0600", fi.Mode().Perm(), err)
	}
}

func TestStartHold586_RemoveFailure_IsAnError(t *testing.T) {
	// A non-empty directory cannot be removed by os.Remove.
	p := filepath.Join(t.TempDir(), FileName)
	if err := os.MkdirAll(filepath.Join(p, "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Remove(p); err == nil {
		t.Fatal("a removal that failed reported success")
	}
}
