package datasubject

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// The engine of slice 5d-1 (GitHub #344): one shared transcript row.
//
// The subject's own turn (its author is exactly one of their identifiers,
// O-5d-7) has the text they wrote replaced by a dated tombstone and its author
// by a per-thread pseudonym; its time and position stay (O-5d-2). Every other
// field of it, and every field of anyone else's turn, has the subject's
// identifiers masked literally, never rewritten by a model (O-5d-1). The
// verification floor runs over every value written; a dirty result writes
// nothing.
//
// see LLD § https://docs.vornik.io §15.14

// TranscriptMaskModel names the deterministic masking in the request record
// and the review proposal, where a 5c redaction names its LLM.
const TranscriptMaskModel = "deterministic-mask-v1"

// Kinds of transcript redaction (RedactedAction.Kind).
const (
	TranscriptTombstoned    = "tombstoned"
	TranscriptMasked        = "masked"
	TranscriptPayloadMasked = "payload_masked"
)

// Tables 5d-1 reports on that are not linkable: reached only through a turn.
const (
	TableTasks             LinkableTable = "tasks"
	TableChatSystemPrompts LinkableTable = "chat_system_prompts"
)

const (
	maskToken      = "[person removed]"
	maskSuffix     = " [identifiers of a participant were masked]"
	pseudonymStem  = "removed-participant-"
	tombstoneStart = "[message removed at the request of a participant on "
)

// authoredColumn is the free-text column the row's author wrote.
var authoredColumn = map[LinkableTable]string{
	TableTaskMessages: "content",
	TableChatAuditLog: "user_message",
}

// jsonColumn marks the columns masked by walking their decoded JSON.
var jsonColumn = map[string]bool{
	"metadata": true, "tool_calls_json": true, "hallucination_signals_json": true,
}

func tombstone(at time.Time) string {
	return tombstoneStart + at.UTC().Format("2006-01-02") + "]"
}

// isOwnTurn is O-5d-7: an exact match of the author with an identifier value.
func isOwnTurn(author string, ids []string) bool {
	if author == "" {
		return false
	}
	for _, id := range ids {
		if author == id {
			return true
		}
	}
	return false
}

// applyTranscriptRedact performs one transcript REDACT and records it.
func (e *Executor) applyTranscriptRedact(ctx context.Context, a Action, ids []string, plan *ErasurePlan, res *ErasureResult) {
	d := e.Redact
	store := d.Transcripts
	ids = liveIdentifiers(ids)
	if len(ids) == 0 {
		e.deferRedaction(a, res, "the subject has no usable identifiers recorded, so no masking "+
			"could be verified; this row has NOT been changed")
		return
	}
	turn, err := store.LoadTurn(ctx, a.Table, a.RowID)
	if err != nil {
		res.Failed = append(res.Failed, FailedAction{Table: a.Table, RowID: a.RowID,
			Err: fmt.Sprintf("load transcript row: %v", err)})
		return
	}

	own := isOwnTurn(turn.Author, ids) || strings.HasPrefix(turn.Author, pseudonymStem) && e.isTombstoned(turn)
	var pseudonym string
	if own || isOwnTurn(turn.ChatID, ids) {
		pseudonym, err = e.pseudonymFor(ctx, turn, ids, plan)
		if err != nil {
			res.Failed = append(res.Failed, FailedAction{Table: a.Table, RowID: a.RowID,
				Err: fmt.Sprintf("compute the thread pseudonym: %v", err)})
			return
		}
	}
	w, badCol := e.buildTurnWrite(turn, own, pseudonym, ids)
	if badCol != "" {
		e.deferRedaction(a, res, fmt.Sprintf("the %s column could not be masked safely (it is not "+
			"valid JSON, or two of its keys would mask to the same key); this row has NOT been changed", badCol))
		return
	}

	// THE FLOOR: every value written, and every string inside a JSON value.
	if err := verifyTurnWrite(w, ids); err != nil {
		e.deferRedaction(a, res, "the masked row did not verify clean (an identifier survives in a form "+
			"literal masking does not reach), so nothing was written; this row has NOT been changed")
		return
	}

	before, after := joinFields(turn.Fields), joinFields(w.Fields)
	kind := TranscriptMasked
	if own {
		kind = TranscriptTombstoned
	}
	changed := before != after || w.Author != turn.Author || w.ChatID != nil || w.Prompt != nil
	if changed && !e.reviewApproved(a, before, after, ids, TranscriptMaskModel, res) {
		return
	}

	beforeHash := hashFields(turn.Fields)
	if changed && !e.commitTurn(ctx, a, turn, w, res) {
		return
	}
	res.Redacted = append(res.Redacted, RedactedAction{
		Table: a.Table, RowID: a.RowID, BeforeHash: beforeHash, AfterHash: hashFields(w.Fields),
		Model: TranscriptMaskModel, Kind: kind, Verified: true, ReviewBypassed: d.ApplyWithoutReview && changed,
	})
	if a.Table == TableChatAuditLog {
		e.maskOriginPayloads(ctx, a, ids, res)
	}
}

// buildTurnWrite builds the replacement for one turn: the tombstone and the
// pseudonym for an own turn, masking everywhere else. badCol names a JSON
// column that could not be masked safely; the row is then not written.
func (e *Executor) buildTurnWrite(turn TranscriptTurn, own bool, pseudonym string, ids []string) (w TurnWrite, badCol string) {
	w = TurnWrite{Fields: map[string]*string{}, Author: turn.Author}
	for col, v := range turn.Fields {
		switch {
		case v == nil:
			w.Fields[col] = nil
		case own && col == authoredColumn[turn.Table]:
			w.Fields[col] = v
			if !e.isTombstoned(turn) {
				t := tombstone(e.now())
				w.Fields[col] = &t
			}
		default:
			masked, err := maskField(col, *v, ids)
			if err != nil {
				return TurnWrite{}, col
			}
			w.Fields[col] = &masked
		}
	}
	if own && isOwnTurn(turn.Author, ids) {
		w.Author = pseudonym
	}
	// A DM's chat id is the subject's identifier wherever it appears, own turn
	// or not (review 0e4e F2); a group chat id is never changed.
	if isOwnTurn(turn.ChatID, ids) {
		w.ChatID = &pseudonym
	}
	if turn.PromptBody != nil {
		if masked := maskText(*turn.PromptBody, ids); masked != *turn.PromptBody {
			w.Prompt = &PromptRekey{OldHash: turn.PromptHash, NewBody: masked}
		}
	}
	return w, ""
}

// commitTurn writes the turn and records a non-applied outcome. It reports
// whether the write was applied.
func (e *Executor) commitTurn(ctx context.Context, a Action, turn TranscriptTurn, w TurnWrite, res *ErasureResult) bool {
	out, err := e.Redact.Transcripts.ApplyTurn(ctx, turn, w)
	if err != nil {
		res.Failed = append(res.Failed, FailedAction{Table: a.Table, RowID: a.RowID,
			Err: fmt.Sprintf("write transcript row: %v", err)})
		return false
	}
	switch out.Outcome {
	case TranscriptApplied:
		e.recordReferrers(a, out, res)
		return true
	case TranscriptLive:
		e.deferLive(a, res, fmt.Sprintf("this conversation is still in use by running task %s, so it "+
			"was left untouched; it is erased on a re-run once that task has finished", out.LiveTaskID), out.LiveTaskID)
	case TranscriptVersionChanged:
		e.deferRedaction(a, res, "this record changed while the erasure was being prepared, so it was "+
			"left untouched rather than overwritten; it will be re-planned on the next run")
	default:
		res.Failed = append(res.Failed, FailedAction{Table: a.Table, RowID: a.RowID,
			Err: fmt.Sprintf("unknown transcript outcome %q", out.Outcome)})
	}
	return false
}

func (e *Executor) now() time.Time {
	if e.Redact != nil && e.Redact.Now != nil {
		return e.Redact.Now()
	}
	return time.Now()
}

// isTombstoned reports whether the turn's authored text is already a 5d
// tombstone: a re-run after a partial erasure.
func (e *Executor) isTombstoned(t TranscriptTurn) bool {
	v := t.Fields[authoredColumn[t.Table]]
	return v != nil && strings.HasPrefix(*v, tombstoneStart)
}

// pseudonymFor is removed-participant-<n> (review 0e4e F5): n is the
// subject's 1-based position among the thread's authors by first turn, with
// every author string matching an identifier collapsed into one entry dated by
// the earliest. A linked row of the same thread already carrying a pseudonym
// (a re-run after a partial erasure) supplies it instead, because the
// subject's tombstoned turns no longer carry their identifiers and the
// position would move.
func (e *Executor) pseudonymFor(ctx context.Context, turn TranscriptTurn, ids []string, plan *ErasurePlan) (string, error) {
	store := e.Redact.Transcripts
	if turn.ThreadKey == "" {
		return pseudonymStem + "1", nil
	}
	for _, a := range plan.Actions {
		if a.Table != turn.Table || a.RowID == turn.RowID {
			continue
		}
		other, err := store.LoadTurn(ctx, a.Table, a.RowID)
		if err != nil || other.ThreadKey != turn.ThreadKey {
			continue
		}
		if strings.HasPrefix(other.Author, pseudonymStem) && e.isTombstoned(other) {
			return other.Author, nil
		}
	}
	authors, err := store.ThreadAuthors(ctx, turn.Table, turn.ThreadKey)
	if err != nil {
		return "", err
	}
	var entries []ThreadAuthor
	var subject *ThreadAuthor
	for _, au := range authors {
		if !isOwnTurn(au.Author, ids) {
			entries = append(entries, au)
			continue
		}
		if subject == nil || threadAuthorBefore(au, *subject) {
			c := au
			subject = &c
		}
	}
	if subject == nil {
		// The thread holds no turn under the subject's identifiers (a DM row
		// whose chat id is theirs, authored by the assistant): first.
		return pseudonymStem + "1", nil
	}
	entries = append(entries, *subject)
	sort.SliceStable(entries, func(i, j int) bool { return threadAuthorBefore(entries[i], entries[j]) })
	for i, en := range entries {
		if en == *subject {
			return fmt.Sprintf("%s%d", pseudonymStem, i+1), nil
		}
	}
	return pseudonymStem + "1", nil
}

func threadAuthorBefore(x, y ThreadAuthor) bool {
	if !x.FirstAt.Equal(y.FirstAt) {
		return x.FirstAt.Before(y.FirstAt)
	}
	return x.FirstID < y.FirstID
}

// recordReferrers turns other prompt referrers left on the old body into
// deferrals of their own.
func (e *Executor) recordReferrers(a Action, out TurnResult, res *ErasureResult) {
	for _, r := range out.DeferredReferrers {
		ref := Action{Table: TableChatSystemPrompts, RowID: r.RowID, Disposition: a.Disposition}
		if r.Arrived {
			e.deferRedaction(ref, res, "a new conversation turn began using a system prompt that contains "+
				"your data while it was being erased, so that prompt was kept for it: its source (for example "+
				"an operator profile) still holds your data and must be erased there; a re-run then moves it")
			continue
		}
		e.deferLive(ref, res, fmt.Sprintf("a system prompt containing your data is still used by running "+
			"task %s, so it was kept for that task; a re-run once it has finished removes it", r.LiveTaskID), r.LiveTaskID)
	}
}

// maskOriginPayloads masks the stored instructions of every settled task the
// turn started, through parent_task_id (O-5d-8 reversed, memo Q3).
func (e *Executor) maskOriginPayloads(ctx context.Context, a Action, ids []string, res *ErasureResult) {
	store := e.Redact.Transcripts
	payloads, err := store.OriginPayloads(ctx, a.RowID)
	if err != nil {
		res.Failed = append(res.Failed, FailedAction{Table: TableTasks, RowID: "chat_turn_id=" + a.RowID,
			Err: fmt.Sprintf("list the tasks this turn started: %v", err)})
		return
	}
	for _, p := range payloads {
		if p.Payload == nil {
			continue // holds nothing (review 9a80 F3)
		}
		pa := Action{Table: TableTasks, RowID: p.TaskID, Disposition: a.Disposition}
		masked, err := maskJSON(*p.Payload, ids)
		if err != nil {
			e.deferRedaction(pa, res, "the stored instructions of a task started from this conversation are "+
				"not valid JSON, so they could not be masked safely; they have NOT been changed")
			continue
		}
		// Verified before the unchanged check: a payload masking cannot
		// change may still hold an identifier as a number.
		if err := verifyJSONStrings(masked, ids); err != nil {
			e.deferRedaction(pa, res, "the masked task instructions did not verify clean, so nothing was "+
				"written; they have NOT been changed")
			continue
		}
		if masked == *p.Payload {
			continue
		}
		out, err := store.ApplyPayload(ctx, p, masked)
		if err != nil {
			res.Failed = append(res.Failed, FailedAction{Table: TableTasks, RowID: p.TaskID,
				Err: fmt.Sprintf("write task payload: %v", err)})
			continue
		}
		switch out.Outcome {
		case TranscriptApplied:
			before, after := *p.Payload, masked
			res.Redacted = append(res.Redacted, RedactedAction{
				Table: TableTasks, RowID: p.TaskID,
				BeforeHash: hashFields(map[string]*string{"payload": &before}),
				AfterHash:  hashFields(map[string]*string{"payload": &after}),
				Model:      TranscriptMaskModel, Kind: TranscriptPayloadMasked, Verified: true,
				ReviewBypassed: e.Redact.ApplyWithoutReview,
			})
		case TranscriptLive:
			e.deferLive(pa, res, fmt.Sprintf("task %s, started from this conversation, is still running, so "+
				"its stored instructions were left untouched; a re-run once it has finished masks them", p.TaskID), p.TaskID)
		default:
			e.deferRedaction(pa, res, "a task's stored instructions changed while the erasure was being "+
				"prepared, so they were left untouched; they will be re-planned on the next run")
		}
	}
}

// maskField masks one column: JSON columns by walking their value, free text
// literally with the visible suffix when it changed.
func maskField(col, v string, ids []string) (string, error) {
	if jsonColumn[col] {
		if strings.TrimSpace(v) == "" {
			return v, nil // hallucination_signals_json is "" when nothing fired
		}
		return maskJSON(v, ids)
	}
	m := maskText(v, ids)
	if m != v {
		m += maskSuffix
	}
	return m, nil
}

// maskJSON masks every string value and every object key of a JSON document.
// The original bytes are kept when nothing matched; a value that is not one
// valid JSON document is an error, never written.
func maskJSON(v string, ids []string) (string, error) {
	dec := json.NewDecoder(strings.NewReader(v))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return "", err
	}
	if strings.TrimSpace(v[dec.InputOffset():]) != "" {
		return "", errors.New("trailing data after the JSON value")
	}
	changed := false
	doc, err := walkJSONKeysChecked(doc, func(s string) string {
		m := maskText(s, ids)
		if m != s {
			changed = true
		}
		return m
	})
	if err != nil {
		return "", err
	}
	if !changed {
		return v, nil
	}
	out, err := json.Marshal(doc)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

// walkJSONKeysChecked is walkJSON for a write: two keys of one object that
// mask to the same key would keep only one value, silently dropping another
// person's data (review 6648 F3), so a collision is an error and the row is
// deferred, never written.
func walkJSONKeysChecked(v any, f func(string) string) (any, error) {
	switch t := v.(type) {
	case string:
		return f(t), nil
	case []any:
		for i := range t {
			x, err := walkJSONKeysChecked(t[i], f)
			if err != nil {
				return nil, err
			}
			t[i] = x
		}
		return t, nil
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			x, err := walkJSONKeysChecked(val, f)
			if err != nil {
				return nil, err
			}
			nk := f(k)
			if _, dup := out[nk]; dup {
				return nil, errors.New("two keys mask to the same key")
			}
			out[nk] = x
		}
		return out, nil
	default:
		return v, nil
	}
}

func walkJSON(v any, f func(string) string) any {
	switch t := v.(type) {
	case string:
		return f(t)
	case []any:
		for i := range t {
			t[i] = walkJSON(t[i], f)
		}
		return t
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			out[f(k)] = walkJSON(val, f)
		}
		return out
	default:
		return v
	}
}

// maskText replaces each identifier, case-insensitively and on word
// boundaries, by the mask token. Longest identifier first, so a full name is
// masked before a part of it.
func maskText(s string, ids []string) string {
	sorted := append([]string(nil), ids...)
	sort.SliceStable(sorted, func(i, j int) bool { return len(sorted[i]) > len(sorted[j]) })
	for _, id := range sorted {
		if strings.TrimSpace(id) == "" {
			continue
		}
		re := regexp.MustCompile(`(?i)` + regexp.QuoteMeta(id))
		var b strings.Builder
		last := 0
		for _, m := range re.FindAllStringIndex(s, -1) {
			if !onWordBoundary(s, m[0], m[1]) {
				continue
			}
			b.WriteString(s[last:m[0]])
			b.WriteString(maskToken)
			last = m[1]
		}
		if last > 0 {
			b.WriteString(s[last:])
			s = b.String()
		}
	}
	return s
}

// onWordBoundary reports whether s[start:end] is not part of a longer word: an
// edge that is a letter or digit must not continue into another.
func onWordBoundary(s string, start, end int) bool {
	first, _ := utf8.DecodeRuneInString(s[start:end])
	lastR, _ := utf8.DecodeLastRuneInString(s[start:end])
	if isWordRune(first) && start > 0 {
		if r, _ := utf8.DecodeLastRuneInString(s[:start]); isWordRune(r) {
			return false
		}
	}
	if isWordRune(lastR) && end < len(s) {
		if r, _ := utf8.DecodeRuneInString(s[end:]); isWordRune(r) {
			return false
		}
	}
	return true
}

func isWordRune(r rune) bool { return unicode.IsLetter(r) || unicode.IsDigit(r) }

// verifyTurnWrite runs the floor over every value the write carries.
func verifyTurnWrite(w TurnWrite, ids []string) error {
	for col, v := range w.Fields {
		if v == nil || strings.TrimSpace(*v) == "" {
			continue
		}
		if jsonColumn[col] {
			if err := verifyJSONStrings(*v, ids); err != nil {
				return err
			}
			continue
		}
		if err := VerifyRedaction(ids, *v); err != nil {
			return err
		}
	}
	for _, v := range []string{w.Author, ptrVal(w.ChatID)} {
		if v != "" {
			if err := VerifyRedaction(ids, v); err != nil {
				return err
			}
		}
	}
	if w.Prompt != nil {
		if err := VerifyRedaction(ids, w.Prompt.NewBody); err != nil {
			return err
		}
	}
	return nil
}

func ptrVal(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

// verifyJSONStrings runs the floor over every string and key inside v, and
// over the whole document: an identifier held as a JSON number or any other
// non-string value is reached by no string, so masking cannot touch it and
// the row must defer (security review of cc464bb70, GitHub #344).
func verifyJSONStrings(v string, ids []string) error {
	var doc any
	if err := json.Unmarshal([]byte(v), &doc); err != nil {
		return err
	}
	if err := VerifyRedaction(ids, v); err != nil {
		return err
	}
	var firstErr error
	walkJSON(doc, func(s string) string {
		if firstErr == nil && strings.TrimSpace(s) != "" {
			firstErr = VerifyRedaction(ids, s)
		}
		return s
	})
	return firstErr
}

// joinFields renders the fields NUL-joined in column order, for the review
// proposal.
func joinFields(f map[string]*string) string {
	keys := make([]string, 0, len(f))
	for k := range f {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = k + "=" + ptrVal(f[k])
	}
	return strings.Join(parts, "\x00")
}

// hashFields is the report's before/after hash: sha256 over the columns in
// name order, each prefixed N for NULL or V<len>: for a value, so NULL and ""
// differ.
func hashFields(f map[string]*string) string {
	keys := make([]string, 0, len(f))
	for k := range f {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	h := sha256.New()
	for _, k := range keys {
		// hash.Hash's Write never returns an error.
		_, _ = fmt.Fprintf(h, "%d:%s", len(k), k)
		if v := f[k]; v == nil {
			_, _ = h.Write([]byte("N"))
		} else {
			_, _ = fmt.Fprintf(h, "V%d:%s", len(*v), *v)
		}
	}
	return hex.EncodeToString(h.Sum(nil))
}
