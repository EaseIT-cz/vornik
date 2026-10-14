package hermes

import (
	"fmt"
	"slices"
	"strings"
	"testing"
)

// GitHub #380, Hermes release lane H3f-forget (run 4400844a, 2026-10-14):
// asked to forget, the model re-stored the fact with vornik_remember, padded
// so dedup_hash let it in. Any note naming the fact that is still recallable
// after the forget fails the step, whatever its wording or case.
func TestH3fRestored_FindsAReAdmittedNoteNamingTheFact(t *testing.T) {
	rows := []memChunk{
		{"mirrored", "refuted", "[user] The user's physiotherapist is Dr Svoboda at the Karlin rehabilitation clinic. ⟦vm:0123456789abcdef⟧"},
		{"dentist", "", "The user's dentist is Dr Novak in Vinohrady."},
		{"padded", "", "The user's physiotherapist is Dr Svoboda at the Karlin rehabilitation clinic. The user wants this removed from memory. This is a short factual statement that can be safely removed."},
		{"lower", "unverified", "physio: dr svoboda"},
		{"clinic-only", "", "The user goes to the KARLIN rehabilitation clinic for physiotherapy."},
		{"old-refuted", "refuted", "Dr Svoboda, physiotherapist."},
		{"topic-only", "", "The user asked to forget their physiotherapist."},
	}
	var ids []string
	for _, r := range h3fRestored(rows, "mirrored") {
		ids = append(ids, r.id)
	}
	if want := []string{"padded", "lower", "clinic-only"}; !slices.Equal(ids, want) {
		t.Fatalf("h3fRestored = %v, want %v", ids, want)
	}
	// The captured mirrored chunk is judged by its own check (refuted), even
	// while it is still live.
	if got := h3fRestored([]memChunk{{"mirrored", "", "Dr Svoboda"}}, "mirrored"); len(got) != 0 {
		t.Fatalf("the mirrored chunk itself was reported: %v", got)
	}
}

// The matcher's terms are the fact H3f stores: change the prompt and this
// fails, so the matcher cannot silently stop matching (review 454e F3).
func TestH3fFactTerms_AreInTheStoredFact(t *testing.T) {
	for _, term := range h3fFactTerms {
		if !strings.Contains(strings.ToLower(promptH3fAdd), term) {
			t.Errorf("term %q is not in the H3f prompt %q", term, promptH3fAdd)
		}
	}
	if !strings.Contains(promptH3fAdd, h3fFact) || !h3fNamesFact(h3fFact) {
		t.Errorf("h3fFact %q is not in the prompt or not matched", h3fFact)
	}
}

// fakeH3fState is the lane's state between two H3f-forget attempts: the
// memory project's rows and Hermes's two memory files.
type fakeH3fState struct {
	rows       []memChunk
	files      map[string][]byte
	reinstated []string
	logs       []string
	// reinstateWorks is the daemon's identical-re-add reinstate (design 22,
	// "Reinstating a refuted mirrored note"); false simulates it failing.
	reinstateWorks bool
}

func (f *fakeH3fState) ops() h3fResetOps {
	return h3fResetOps{
		chunks: func() []memChunk { return slices.Clone(f.rows) },
		refute: func(id string) error {
			for i := range f.rows {
				if f.rows[i].id == id {
					f.rows[i].status = "refuted"
				}
			}
			return nil
		},
		files: func() map[string][]byte { return f.files },
		restoreFiles: func(files map[string][]byte) error {
			f.files = files
			return nil
		},
		reinstate: func(content string) error {
			f.reinstated = append(f.reinstated, content)
			for i := range f.rows {
				if f.reinstateWorks && f.rows[i].content == content && f.rows[i].status == "refuted" {
					f.rows[i].status = "unverified"
				}
			}
			return nil
		},
		logf: func(format string, args ...any) { f.logs = append(f.logs, fmt.Sprintf(format, args...)) },
	}
}

const h3fMirroredRaw = "[user] The user's physiotherapist is Dr Svoboda at the Karlin rehabilitation clinic. ⟦vm:0123456789abcdef⟧"

func h3fTestStart() h3fStart {
	return h3fStart{
		ids:      map[string]bool{"older": true, "mirrored": true, "dentist": true},
		files:    map[string][]byte{"USER.md": []byte("The user's physiotherapist is Dr Svoboda at the Karlin rehabilitation clinic.")},
		mirrored: memChunk{"mirrored", "", h3fMirroredRaw},
	}
}

// GitHub #380, Hermes release lane H3f-forget (run at 02c6b5272,
// 2026-10-14): attempt 1 removed Hermes's entry (the mirror refuted its
// copy) and also left a garbled note naming the fact, admitted; attempts 2
// and 3 then failed at once on attempt 1's note. Between attempts the lane
// now removes that note, logging it, and puts the step's start state back:
// Hermes's file and the live mirrored copy.
func TestH3fResetAttempt_RemovesTheStrayAndRestoresTheStartState(t *testing.T) {
	stray := `my physiotherapist is Dr Svoboda at the Karlin rehabilitation clinic.  old_text":"my physiotherapist is Dr Svoboda at the Karlin rehabilitation clinic.  action":"remove","target":"memory"}`
	f := &fakeH3fState{
		reinstateWorks: true,
		rows: []memChunk{
			{"older", "refuted", "Dr Svoboda, physiotherapist."},
			{"mirrored", "refuted", h3fMirroredRaw},
			{"dentist", "", "The user's dentist is Dr Novak in Vinohrady."},
			{"aa8e3fd0ad843d1c985b8240cd3b795c", "unverified", stray},
			{"unrelated-new", "", "The user's dentist appointment moved to Friday."},
		},
		files: map[string][]byte{"USER.md": []byte("")},
	}
	if err := h3fResetAttempt(h3fTestStart(), 1, f.ops()); err != nil {
		t.Fatalf("reset: %v", err)
	}
	status := map[string]string{}
	for _, r := range f.rows {
		status[r.id] = r.status
	}
	if status["aa8e3fd0ad843d1c985b8240cd3b795c"] != "refuted" {
		t.Errorf("the stray note was not removed: %v", status)
	}
	if status["mirrored"] == "refuted" {
		t.Errorf("the mirrored copy was not reinstated: %v", status)
	}
	if status["unrelated-new"] != "" || status["dentist"] != "" || status["older"] != "refuted" {
		t.Errorf("a note that does not name the fact, or one from before the step, was touched: %v", status)
	}
	if !slices.Equal(f.reinstated, []string{h3fMirroredRaw}) {
		t.Errorf("reinstate was called with %q, want the mirrored note's exact bytes", f.reinstated)
	}
	if string(f.files["USER.md"]) != string(h3fTestStart().files["USER.md"]) {
		t.Errorf("Hermes's memory file was not restored: %q", f.files["USER.md"])
	}
	logged := strings.Join(f.logs, "\n")
	if !strings.Contains(logged, "aa8e3fd0ad843d1c985b8240cd3b795c") || !strings.Contains(logged, "old_text") {
		t.Errorf("the removed note's id and content are not in the log: %s", logged)
	}
}

// An attempt that changed nothing but left a stray: only the stray goes.
func TestH3fResetAttempt_LeavesAnUntouchedStartStateAlone(t *testing.T) {
	start := h3fTestStart()
	f := &fakeH3fState{
		reinstateWorks: true,
		rows:           []memChunk{{"mirrored", "", h3fMirroredRaw}, {"padded", "", "The user's physiotherapist is Dr Svoboda. Removed."}},
		files:          map[string][]byte{"USER.md": start.files["USER.md"]},
	}
	restored := false
	ops := f.ops()
	ops.restoreFiles = func(map[string][]byte) error { restored = true; return nil }
	if err := h3fResetAttempt(start, 2, ops); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if len(f.reinstated) != 0 || restored {
		t.Errorf("an unchanged start state was rewritten: reinstated %q, files restored %v", f.reinstated, restored)
	}
	if f.rows[1].status != "refuted" || f.rows[0].status != "" {
		t.Errorf("rows after reset: %v", f.rows)
	}
}

// A start state that cannot be put back fails the reset, so a retry never
// runs from a state the step did not begin with.
func TestH3fResetAttempt_FailsWhenTheMirrorIsNotReinstated(t *testing.T) {
	f := &fakeH3fState{
		reinstateWorks: false,
		rows:           []memChunk{{"mirrored", "refuted", h3fMirroredRaw}},
		files:          map[string][]byte{"USER.md": []byte("")},
	}
	if err := h3fResetAttempt(h3fTestStart(), 1, f.ops()); err == nil {
		t.Fatal("reset succeeded with the mirrored copy still refuted")
	}
}

// A mirrored chunk that is gone (not refuted) cannot be reinstated under
// its id: a re-add would make a new chunk the step's checks do not track.
// The reset fails instead, without a re-add (review 5838).
func TestH3fResetAttempt_FailsWhenTheMirrorIsMissing(t *testing.T) {
	f := &fakeH3fState{
		reinstateWorks: true,
		rows:           []memChunk{{"dentist", "", "The user's dentist is Dr Novak in Vinohrady."}},
		files:          map[string][]byte{"USER.md": h3fTestStart().files["USER.md"]},
	}
	if err := h3fResetAttempt(h3fTestStart(), 1, f.ops()); err == nil || !strings.Contains(err.Error(), "missing") {
		t.Fatalf("reset with the mirrored chunk gone: err = %v, want a failure naming it missing", err)
	}
	if len(f.reinstated) != 0 {
		t.Errorf("a missing mirrored chunk was re-added: %q", f.reinstated)
	}
}

// A memory file the attempt created is removed: the start state is "no such
// file", not "empty file" (review 5838).
func TestH3fResetAttempt_RemovesAFileTheAttemptCreated(t *testing.T) {
	start := h3fTestStart()
	f := &fakeH3fState{
		reinstateWorks: true,
		rows:           []memChunk{{"mirrored", "", h3fMirroredRaw}},
		files:          map[string][]byte{"USER.md": start.files["USER.md"], "MEMORY.md": []byte("The user said to forget the physio.")},
	}
	if err := h3fResetAttempt(start, 1, f.ops()); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if _, ok := f.files["MEMORY.md"]; ok {
		t.Errorf("MEMORY.md, absent at the start, is still there: %q", f.files)
	}
}

// The log keeps a long garbled note's tail, where the leaked arguments are.
func TestH3fClip_KeepsHeadAndTail(t *testing.T) {
	long := strings.Repeat("my physiotherapist is Dr Svoboda. ", 20) + `action":"remove","target":"memory"}`
	got := h3fClip(long, 300)
	if len([]rune(got)) > 303 || !strings.HasPrefix(got, "my physiotherapist") || !strings.HasSuffix(got, `"target":"memory"}`) {
		t.Errorf("h3fClip = %q", got)
	}
	if h3fClip("short", 300) != "short" {
		t.Error("a short note was changed")
	}
}

// A note naming the fact that was live before the step is not the
// attempt's and is not removed: it fails every attempt, as it should.
func TestH3fResetAttempt_KeepsNotesFromBeforeTheStep(t *testing.T) {
	start := h3fTestStart()
	start.ids["pre-existing"] = true
	f := &fakeH3fState{
		reinstateWorks: true,
		rows:           []memChunk{{"mirrored", "", h3fMirroredRaw}, {"pre-existing", "", "Dr Svoboda again."}},
		files:          map[string][]byte{"USER.md": start.files["USER.md"]},
	}
	if err := h3fResetAttempt(start, 1, f.ops()); err != nil {
		t.Fatalf("reset: %v", err)
	}
	if f.rows[1].status != "" {
		t.Errorf("a note from before the step was removed: %v", f.rows)
	}
}
