package datasubject

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Art 16 rectification — GDPR increment 6 (GitHub #343).
//
// The controller supplies the accurate data; every case-insensitive literal
// occurrence of the inaccurate text in a linked memory chunk or knowledge-graph
// entity is replaced with it, verified mechanically, reviewed by an operator, and
// committed through the SAME store transaction slice 5c uses for redaction
// (ChunkRedactor: version guard, re-hash, embedding nulled, re-embed queued,
// pre-change cache row evicted). Deliberately NOT an LLM rewrite: a correction has
// a literal form, and a generative paraphrase on a correction path could alter
// other facts in ways no mechanical check detects.
//
// Transcripts are never edited — they record what was said — and get an Art 16
// "supplementary statement" instead. Rectification never deletes.
//
// see LLD § https://docs.vornik.io §4.16.2

// Correction is the inaccurate text and its accurate replacement.
type Correction struct {
	Incorrect string `json:"incorrect"`
	Correct   string `json:"correct"`
}

// Validate refuses an empty side and a case-only change. Matching is
// case-insensitive and verification case-folds, so a correction that differs only
// in letter case could not be distinguished from no change at all.
func (c Correction) Validate() error {
	if strings.TrimSpace(c.Incorrect) == "" || strings.TrimSpace(c.Correct) == "" {
		return errors.New("datasubject: a correction needs both the inaccurate text and its replacement")
	}
	if normaliseForRedaction(c.Incorrect) == normaliseForRedaction(c.Correct) {
		return errors.New("datasubject: the correction differs from the inaccurate text only in case or " +
			"spacing, which cannot be verified mechanically; correct it by hand")
	}
	return nil
}

func (c Correction) pattern() *regexp.Regexp {
	return regexp.MustCompile(`(?i)` + regexp.QuoteMeta(c.Incorrect))
}

// Matches reports whether text contains the inaccurate text (case-insensitive).
func (c Correction) Matches(text string) bool {
	if strings.TrimSpace(c.Incorrect) == "" {
		return false
	}
	return c.pattern().MatchString(text)
}

// Apply replaces every case-insensitive literal occurrence of Incorrect with
// Correct, returning the result and how many were replaced.
func (c Correction) Apply(text string) (string, int) {
	if strings.TrimSpace(c.Incorrect) == "" {
		return text, 0
	}
	re := c.pattern()
	n := len(re.FindAllStringIndex(text, -1))
	if n == 0 {
		return text, 0
	}
	return re.ReplaceAllLiteralString(text, c.Correct), n
}

// VerifyRectification is the deterministic post-condition: once every occurrence
// of the correction is removed from the output, the inaccurate text must not
// appear; and the correction must. Removing the correction first is what lets a
// correction that CONTAINS the inaccurate text ("Jane Do" → "Jane Doe") verify.
// Both sides are normalised as for redaction (NFKC, zero-width stripped, case-
// folded, whitespace collapsed), so formatting cannot hide a survivor.
func VerifyRectification(c Correction, output string) error {
	if err := c.Validate(); err != nil {
		return err
	}
	out := normaliseForRedaction(output)
	inc, cor := normaliseForRedaction(c.Incorrect), normaliseForRedaction(c.Correct)
	if !strings.Contains(out, cor) {
		return errors.New("rectification verification failed: the corrected text does not contain the correction")
	}
	if strings.Contains(strings.ReplaceAll(out, cor, "\x00"), inc) {
		return errors.New("rectification verification failed: the inaccurate text still appears")
	}
	return nil
}

// RectifyTreatment is what happens to one linked row.
type RectifyTreatment string

const (
	// TreatCorrect — the row's text is corrected in place.
	TreatCorrect RectifyTreatment = "correct"
	// TreatSupplementaryStatement — the row is a transcript; it is not edited and
	// an Art 16 supplementary statement is recorded instead.
	TreatSupplementaryStatement RectifyTreatment = "supplementary_statement"
	// TreatManual — the software has no correction path for this table.
	TreatManual RectifyTreatment = "manual"
)

// RectificationAction is one linked row that contains the inaccuracy.
type RectificationAction struct {
	Table     LinkableTable    `json:"table"`
	RowID     string           `json:"row_id"`
	Treatment RectifyTreatment `json:"treatment"`
	Reason    string           `json:"reason,omitempty"`
}

// RectificationPlan is the decided treatment of every linked row.
type RectificationPlan struct {
	SubjectID  string                `json:"subject_id"`
	RequestID  string                `json:"request_id"`
	Correction Correction            `json:"correction"`
	Actions    []RectificationAction `json:"actions"`
	// Examined is every linked row looked at — the denominator; Unmatched those
	// that did not contain the inaccurate text.
	Examined  int `json:"examined"`
	Unmatched int `json:"unmatched"`
}

// PlanRectification decides, per linked row, what a verified rectification does.
func PlanRectification(req Request, items []Item, c Correction) (*RectificationPlan, error) {
	if req.Kind != RequestRectification {
		return nil, fmt.Errorf("datasubject: PlanRectification handles rectification requests, not %q", req.Kind)
	}
	if !req.MayProduceData() {
		return nil, fmt.Errorf("datasubject: request %s is %q — identity must be verified before "+
			"anything is changed (Art 12(6))", req.ID, req.State)
	}
	if err := c.Validate(); err != nil {
		return nil, err
	}
	p := &RectificationPlan{SubjectID: req.SubjectID, RequestID: req.ID, Correction: c, Examined: len(items)}
	for _, it := range items {
		if !c.Matches(it.Content) {
			p.Unmatched++
			continue
		}
		a := RectificationAction{Table: it.Table, RowID: it.RowID}
		switch it.Table {
		case TableProjectMemoryChunks, TableKnowledgeEntities:
			a.Treatment = TreatCorrect
		case TableChatAuditLog, TableTaskMessages:
			a.Treatment = TreatSupplementaryStatement
			a.Reason = "a transcript records what was said and is not rewritten; the correction is " +
				"recorded as a supplementary statement (Art 16)"
		default:
			a.Treatment = TreatManual
			a.Reason = fmt.Sprintf("the software has no correction path for %s; correct it by hand", it.Table)
		}
		p.Actions = append(p.Actions, a)
	}
	return p, nil
}

// EntityText is the personal-data-bearing text of a knowledge-graph entity.
type EntityText struct {
	CanonicalName string   `json:"canonical_name"`
	Aliases       []string `json:"aliases,omitempty"`
	Description   string   `json:"description,omitempty"`
}

func (t EntityText) joined() string {
	return strings.Join(append(append([]string{t.CanonicalName}, t.Aliases...), t.Description), "\n")
}

// EntityRectifyOutcome is what the entity store did.
type EntityRectifyOutcome string

const (
	// EntityRectifyApplied — name, aliases and description updated and the
	// embedding (derived from the old name) cleared, in one statement.
	EntityRectifyApplied EntityRectifyOutcome = "applied"
	// EntityRectifyVersionChanged — the entity changed since it was read; nothing written.
	EntityRectifyVersionChanged EntityRectifyOutcome = "version_changed"
	// EntityRectifyConflict — the corrected name collides with another entity's
	// (project, type, canonical name). Merging entities is not a correction.
	EntityRectifyConflict EntityRectifyOutcome = "conflict"
)

// EntityRectifier is the store side of entity rectification.
type EntityRectifier interface {
	LoadEntity(ctx context.Context, entityID string) (EntityText, error)
	// RectifyEntity writes updated, guarded on the entity still holding expected.
	RectifyEntity(ctx context.Context, entityID string, expected, updated EntityText) (EntityRectifyOutcome, error)
}

// RectificationProposal is what the operator reviews before a change is written.
type RectificationProposal struct {
	Table  LinkableTable
	RowID  string
	Before string
	After  string
}

// CorrectedRecord is one committed correction. Before is the inaccurate original,
// retained in the request record: the subject is told "corrected, with the
// original retained in the audit record" (§4.5, review Q6), never a bare
// "corrected".
type CorrectedRecord struct {
	Table      LinkableTable `json:"table"`
	RowID      string        `json:"row_id"`
	Before     string        `json:"before"`
	After      string        `json:"after"`
	BeforeHash string        `json:"before_hash,omitempty"`
	AfterHash  string        `json:"after_hash,omitempty"`
}

// SupplementaryStatement is the Art 16 statement recorded against a transcript.
type SupplementaryStatement struct {
	Table     LinkableTable `json:"table"`
	RowID     string        `json:"row_id"`
	Statement string        `json:"statement"`
}

// RectificationResult is what an execution did.
type RectificationResult struct {
	SubjectID  string                   `json:"subject_id"`
	RequestID  string                   `json:"request_id"`
	Correction Correction               `json:"correction"`
	Examined   int                      `json:"examined"`
	Unmatched  int                      `json:"unmatched"`
	Corrected  []CorrectedRecord        `json:"corrected,omitempty"`
	Statements []SupplementaryStatement `json:"supplementary_statements,omitempty"`
	Manual     []RectificationAction    `json:"manual,omitempty"`
	Deferred   []DeferredAction         `json:"deferred,omitempty"`
	Failed     []FailedAction           `json:"failed,omitempty"`
}

// Complete reports whether every matched row was dealt with by the software. A
// row left for manual handling, deferred or failed keeps the request un-actioned:
// marking it complete while inaccurate data remains would put a false completion
// in the ledger.
func (r *RectificationResult) Complete() bool {
	return len(r.Deferred) == 0 && len(r.Failed) == 0 && len(r.Manual) == 0
}

// Narrative is what the subject reads.
func (r *RectificationResult) Narrative() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d record(s) linked to you were examined for the inaccurate data. ", r.Examined)
	if n := len(r.Corrected); n > 0 {
		fmt.Fprintf(&b, "%d record(s) were corrected, with the original retained in the audit record. ", n)
	}
	if n := len(r.Statements); n > 0 {
		fmt.Fprintf(&b, "%d conversation record(s) were not rewritten, because they record what was "+
			"said; a supplementary statement recording the correction was added instead. ", n)
	}
	if n := len(r.Manual) + len(r.Deferred) + len(r.Failed); n > 0 {
		fmt.Fprintf(&b, "%d record(s) could not be corrected automatically and are being handled by "+
			"an operator. ", n)
	}
	b.WriteString("Only records the deployment has linked to you were examined; the inaccuracy may " +
		"also appear in records that were never identified as being about you.")
	return b.String()
}

// Rectifier performs a decided rectification plan.
type Rectifier struct {
	Chunks   ChunkRedactor
	Entities EntityRectifier
	// Approve is the operator review gate — MANDATORY, with no bypass: a literal
	// replacement can match an unintended place, which only a reader catches.
	// Nil defers every change.
	Approve func(RectificationProposal) (bool, error)
}

// ErrPartialRectification signals that one or more rows errored.
var ErrPartialRectification = errors.New("datasubject: rectification failed for one or more rows")

// Execute carries out the plan. A failing row does not abandon the rest; the
// result is returned alongside any error.
func (e *Rectifier) Execute(ctx context.Context, plan *RectificationPlan) (*RectificationResult, error) {
	if plan == nil {
		return nil, errors.New("datasubject: no rectification plan to execute")
	}
	if e == nil || e.Chunks == nil || e.Entities == nil {
		return nil, errors.New("datasubject: rectifier is missing its chunk or entity store")
	}
	res := &RectificationResult{
		SubjectID: plan.SubjectID, RequestID: plan.RequestID, Correction: plan.Correction,
		Examined: plan.Examined, Unmatched: plan.Unmatched,
	}
	c := plan.Correction
	for _, a := range plan.Actions {
		switch a.Treatment {
		case TreatCorrect:
			switch a.Table {
			case TableProjectMemoryChunks:
				e.correctChunk(ctx, a, c, res)
			case TableKnowledgeEntities:
				e.correctEntity(ctx, a, c, res)
			default:
				res.Failed = append(res.Failed, FailedAction{Table: a.Table, RowID: a.RowID,
					Err: fmt.Sprintf("no correction path for %s", a.Table)})
			}
		case TreatSupplementaryStatement:
			res.Statements = append(res.Statements, SupplementaryStatement{
				Table: a.Table, RowID: a.RowID,
				Statement: fmt.Sprintf("Rectified under Art 16 (request %s): where this record states %q, "+
					"the accurate data is %q.", plan.RequestID, c.Incorrect, c.Correct),
			})
		case TreatManual:
			res.Manual = append(res.Manual, a)
		default:
			res.Failed = append(res.Failed, FailedAction{Table: a.Table, RowID: a.RowID,
				Err: fmt.Sprintf("unknown treatment %q", a.Treatment)})
		}
	}
	if len(res.Failed) > 0 {
		return res, fmt.Errorf("%w: %d row(s)", ErrPartialRectification, len(res.Failed))
	}
	return res, nil
}

func (e *Rectifier) deferRow(a RectificationAction, res *RectificationResult, reason string) {
	res.Deferred = append(res.Deferred, DeferredAction{
		Table: a.Table, RowID: a.RowID, Disposition: Disposition("rectify"), Reason: reason,
	})
}

// review applies the mandatory gate; false means the change must not be written
// and the deferral is already recorded.
func (e *Rectifier) review(a RectificationAction, before, after string, res *RectificationResult) bool {
	if e.Approve == nil {
		e.deferRow(a, res, "the correction awaits operator review; this record has NOT been changed")
		return false
	}
	ok, err := e.Approve(RectificationProposal{Table: a.Table, RowID: a.RowID, Before: before, After: after})
	if err != nil {
		e.deferRow(a, res, fmt.Sprintf("operator review could not be completed (%v)", err))
		return false
	}
	if !ok {
		e.deferRow(a, res, "an operator declined the proposed correction; this record has NOT been changed")
		return false
	}
	return true
}

func (e *Rectifier) correctChunk(ctx context.Context, a RectificationAction, c Correction, res *RectificationResult) {
	before, beforeHash, err := e.Chunks.LoadChunk(ctx, a.RowID)
	if err != nil {
		res.Failed = append(res.Failed, FailedAction{Table: a.Table, RowID: a.RowID,
			Err: fmt.Sprintf("load chunk for rectification: %v", err)})
		return
	}
	after, n := c.Apply(before)
	if n == 0 {
		// Matched at planning (which also reads the generated title) but not in
		// the text now. Never an unchanged write reported as a correction.
		e.deferRow(a, res, "the inaccurate text is no longer in this record's text (it may appear only "+
			"in its generated title, or the record changed); this record has NOT been changed")
		return
	}
	if err := VerifyRectification(c, after); err != nil {
		e.deferRow(a, res, fmt.Sprintf("the corrected text did not verify (%v); this record has NOT been changed", err))
		return
	}
	if !e.review(a, before, after, res) {
		return
	}
	out, err := e.Chunks.RedactChunk(ctx, a.RowID, beforeHash, after)
	if err != nil {
		res.Failed = append(res.Failed, FailedAction{Table: a.Table, RowID: a.RowID,
			Err: fmt.Sprintf("commit correction: %v", err)})
		return
	}
	switch out.Outcome {
	case RedactionApplied:
		res.Corrected = append(res.Corrected, CorrectedRecord{
			Table: a.Table, RowID: a.RowID, Before: before, After: after,
			BeforeHash: beforeHash, AfterHash: out.NewHash,
		})
	case RedactionVersionChanged:
		e.deferRow(a, res, "this record changed while the correction was being prepared, so it was left "+
			"untouched; re-run the rectification")
	case RedactionCollision:
		// Rectification NEVER deletes (5c would): a correction that destroyed a row
		// would be an erasure nobody asked for.
		e.deferRow(a, res, fmt.Sprintf("after correction this record would duplicate record %s; it was "+
			"left untouched and needs manual handling", out.SurvivorID))
	default:
		res.Failed = append(res.Failed, FailedAction{Table: a.Table, RowID: a.RowID,
			Err: fmt.Sprintf("unknown store outcome %q", out.Outcome)})
	}
}

func (e *Rectifier) correctEntity(ctx context.Context, a RectificationAction, c Correction, res *RectificationResult) {
	before, err := e.Entities.LoadEntity(ctx, a.RowID)
	if err != nil {
		res.Failed = append(res.Failed, FailedAction{Table: a.Table, RowID: a.RowID,
			Err: fmt.Sprintf("load entity for rectification: %v", err)})
		return
	}
	after := EntityText{Aliases: make([]string, len(before.Aliases))}
	total := 0
	var n int
	after.CanonicalName, n = c.Apply(before.CanonicalName)
	total += n
	for i, al := range before.Aliases {
		after.Aliases[i], n = c.Apply(al)
		total += n
	}
	after.Description, n = c.Apply(before.Description)
	total += n
	if total == 0 {
		e.deferRow(a, res, "the inaccurate text is no longer in this entity; it has NOT been changed")
		return
	}
	if err := VerifyRectification(c, after.joined()); err != nil {
		e.deferRow(a, res, fmt.Sprintf("the corrected entity did not verify (%v); it has NOT been changed", err))
		return
	}
	if !e.review(a, before.joined(), after.joined(), res) {
		return
	}
	out, err := e.Entities.RectifyEntity(ctx, a.RowID, before, after)
	if err != nil {
		res.Failed = append(res.Failed, FailedAction{Table: a.Table, RowID: a.RowID,
			Err: fmt.Sprintf("commit entity correction: %v", err)})
		return
	}
	switch out {
	case EntityRectifyApplied:
		res.Corrected = append(res.Corrected, CorrectedRecord{
			Table: a.Table, RowID: a.RowID, Before: before.joined(), After: after.joined(),
		})
	case EntityRectifyVersionChanged:
		e.deferRow(a, res, "this entity changed while the correction was being prepared; re-run the rectification")
	case EntityRectifyConflict:
		e.deferRow(a, res, "the corrected name is already another entity's; merging entities is not a "+
			"correction, so this needs manual handling")
	default:
		res.Failed = append(res.Failed, FailedAction{Table: a.Table, RowID: a.RowID,
			Err: fmt.Sprintf("unknown entity outcome %q", out)})
	}
}
