package hermes

// The H3f arm's check that a forgotten fact was not stored again (lane
// design, "Amendment 2026-10-14 — H3f-forget fails on a re-store", GitHub
// #380). Untagged so the unit suite reaches it; forget_lane_test.go uses it.

import (
	"bytes"
	"fmt"
	"maps"
	"strings"
)

// H3f's fact, here so the matcher's terms are tested against the prompt
// that stores it (review 454e F3).
const (
	h3fFact      = "Svoboda"
	promptH3fAdd = "Use your own memory tool, not Vornik, to save this to the user profile: my physiotherapist is Dr Svoboda at the Karlin rehabilitation clinic."
)

// memChunk is one row of the memory project's chunks, refuted rows included.
type memChunk struct{ id, status, content string }

// h3fFactTerms identify H3f's fact: the physiotherapist's name and clinic.
// Either one names the fact; "physiotherapist" alone names only the topic.
// No other lane arm's prompt, stub or mail uses either word (checked
// 2026-10-14); a new one that does would make H3f-forget fail on it.
var h3fFactTerms = []string{"svoboda", "karlin"}

// h3fNamesFact reports whether content names H3f's fact, in any case.
func h3fNamesFact(content string) bool {
	low := strings.ToLower(content)
	for _, term := range h3fFactTerms {
		if strings.Contains(low, term) {
			return true
		}
	}
	return false
}

// h3fRestored lists the chunks, other than the captured mirrored one, that
// name the fact and are not refuted: after the forget, each is the fact
// stored again (or never forgotten) and still recallable.
func h3fRestored(rows []memChunk, mirroredID string) []memChunk {
	var out []memChunk
	for _, r := range rows {
		if r.id != mirroredID && r.status != "refuted" && h3fNamesFact(r.content) {
			out = append(out, r)
		}
	}
	return out
}

// h3fStart is the state H3f-forget begins with: the memory project's chunk
// ids, Hermes's memory files, and the captured mirrored chunk with its exact
// stored bytes (lane design, "Amendment 2026-10-14 — H3f-forget retries
// start from the step's state").
type h3fStart struct {
	ids      map[string]bool
	files    map[string][]byte // file name -> bytes; a name absent here did not exist
	mirrored memChunk
}

// h3fResetOps are the lane's effects, injected so the attempt boundary is
// unit-tested without a stack.
type h3fResetOps struct {
	chunks       func() []memChunk             // every chunk of the memory project, refuted included
	refute       func(id string) error         // memory_correct by chunk id, forget_command: never reinstated (design 22)
	files        func() map[string][]byte      // Hermes's memory files now
	restoreFiles func(map[string][]byte) error // write the start's files back
	reinstate    func(content string) error    // remember the mirrored note's exact bytes again
	logf         func(format string, args ...any)
}

// h3fClip shortens a note for the log to at most n runes, keeping its head
// and its tail: a garbled note's diagnostic part (the leaked tool-call
// arguments) is often at the end (review 5838).
func h3fClip(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	head := n * 2 / 3
	return string(r[:head]) + " … " + string(r[len(r)-(n-head):])
}

// h3fResetAttempt runs between two H3f-forget attempts, after attempt
// `attempt` failed. It removes every recallable note that names the fact
// and was admitted during the attempt (not the mirrored chunk, not a note
// that existed before the step), logging each one so the evidence keeps it;
// then it puts Hermes's memory files back and, if the attempt's forget
// refuted the mirrored chunk, reinstates it through the daemon's
// identical-re-add path. An error means the start state could not be put
// back, and the step must fail rather than retry from another state.
func h3fResetAttempt(start h3fStart, attempt int, ops h3fResetOps) error {
	for _, r := range h3fRestored(ops.chunks(), start.mirrored.id) {
		if start.ids[r.id] {
			continue
		}
		ops.logf("H3f-forget: removing a note naming the fact admitted during attempt %d: %s (%q) %q",
			attempt, r.id, r.status, h3fClip(r.content, 300))
		if err := ops.refute(r.id); err != nil {
			return fmt.Errorf("remove the note %s left by attempt %d: %w", r.id, attempt, err)
		}
	}
	if now := ops.files(); !maps.EqualFunc(now, start.files, bytes.Equal) {
		ops.logf("H3f-forget: attempt %d changed Hermes's memory files; restoring the step's start state", attempt)
		if err := ops.restoreFiles(start.files); err != nil {
			return fmt.Errorf("restore Hermes's memory files after attempt %d: %w", attempt, err)
		}
	}
	if h3fStatus(ops.chunks(), start.mirrored.id) == "refuted" {
		ops.logf("H3f-forget: attempt %d refuted the mirrored chunk %s; reinstating it by an identical re-add", attempt, start.mirrored.id)
		if err := ops.reinstate(start.mirrored.content); err != nil {
			return fmt.Errorf("reinstate the mirrored chunk %s after attempt %d: %w", start.mirrored.id, attempt, err)
		}
	}
	after := ops.chunks()
	if st := h3fStatus(after, start.mirrored.id); st == "refuted" || st == "missing" {
		return fmt.Errorf("the mirrored chunk %s is %s after the reset of attempt %d: the step's start state could not be restored", start.mirrored.id, st, attempt)
	}
	for _, r := range h3fRestored(after, start.mirrored.id) {
		if !start.ids[r.id] {
			return fmt.Errorf("the note %s left by attempt %d is still recallable after the reset", r.id, attempt)
		}
	}
	return nil
}

// h3fStatus is a chunk's validation status, or "missing".
func h3fStatus(rows []memChunk, id string) string {
	for _, r := range rows {
		if r.id == id {
			return r.status
		}
	}
	return "missing"
}

// h3DentistNote is the note H3's script stores and H3-dup's repeats byte for
// byte, so the daemon's dedup_hash gate refuses the repeat (lane design,
// "Amendment 2026-10-16 — H3-dup", GitHub #380).
const h3DentistNote = "The user's dentist is Dr Novak at the Vinohrady dental clinic, and appointments are booked by phone."

// h3DupCoaching is the retry coaching the plugin must not give for a
// duplicate: the words its own unit test forbids (assertNoRetryCoaching).
var h3DupCoaching = []string{"restate", "try again", "full sentence", "with its context"}

// h3DupProblem judges H3-dup's reply, which echoes the tool result Hermes
// gave the scripted model for a byte-for-byte duplicate: it must say the
// note is already on record and not to store it again, and coach no retry.
// nil means the refusal is the one design 24 specifies (0.21.6 on).
func h3DupProblem(reply string) error {
	low := strings.ToLower(reply)
	if !strings.Contains(low, "already holds this exact note") || !strings.Contains(low, "not store it again") {
		return fmt.Errorf("the duplicate's tool result does not say the note is already on record and not to store it again")
	}
	for _, c := range h3DupCoaching {
		if strings.Contains(low, c) {
			return fmt.Errorf("the duplicate's tool result coaches a retry (%q): the #380 re-store", c)
		}
	}
	return nil
}
