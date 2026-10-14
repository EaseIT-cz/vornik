package datasubject

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Art 18 restriction of processing — GDPR increment 6 (GitHub #343).
//
// A restriction is a subject-level record: while it is active, rows LINKED to the
// subject are stored but not used (Art 18(2)). This file is the decision layer —
// who may impose one, what a lift requires, and what the report must say. The
// enforcement is a live predicate over data_subject_links evaluated where memory
// is used (persistence.ChunkNotRestrictedSQL, memory.Searcher), never a flag
// copied onto rows, so a later link is covered at once and a lift has nothing to
// un-stamp.
//
// see LLD § https://docs.vornik.io §4.16.1

// RestrictionGround is the Art 18(1) limb a restriction is imposed under.
type RestrictionGround string

const (
	// RestrictAccuracyContested — Art 18(1)(a), for the period needed to verify accuracy.
	RestrictAccuracyContested RestrictionGround = "art18_1_a"
	// RestrictUnlawfulOpposesErasure — Art 18(1)(b), unlawful processing and the
	// subject opposes erasure.
	RestrictUnlawfulOpposesErasure RestrictionGround = "art18_1_b"
	// RestrictLegalClaims — Art 18(1)(c), the controller no longer needs the data
	// but the subject needs it for legal claims.
	RestrictLegalClaims RestrictionGround = "art18_1_c"
	// RestrictObjectionPending — Art 18(1)(d), an Art 21(1) objection pending
	// verification of the controller's grounds.
	RestrictObjectionPending RestrictionGround = "art18_1_d"
)

var restrictionGrounds = map[RestrictionGround]struct{ article, label string }{
	RestrictAccuracyContested:      {"Art 18(1)(a)", "accuracy contested, for the period needed to verify it"},
	RestrictUnlawfulOpposesErasure: {"Art 18(1)(b)", "processing unlawful and the subject opposes erasure"},
	RestrictLegalClaims:            {"Art 18(1)(c)", "no longer needed by the controller but required by the subject for legal claims"},
	RestrictObjectionPending:       {"Art 18(1)(d)", "objection under Art 21(1) pending verification"},
}

// RestrictionGrounds lists every valid ground in a stable order, for help text.
func RestrictionGrounds() []RestrictionGround {
	return []RestrictionGround{
		RestrictAccuracyContested, RestrictUnlawfulOpposesErasure,
		RestrictLegalClaims, RestrictObjectionPending,
	}
}

// Validate refuses an empty or unknown ground. The limb is recorded and reported
// to the subject, so a guessed one would misstate the legal basis.
func (g RestrictionGround) Validate() error {
	if _, ok := restrictionGrounds[g]; !ok {
		return fmt.Errorf("datasubject: unknown Art 18(1) ground %q", g)
	}
	return nil
}

// Article is the citation, e.g. "Art 18(1)(a)".
func (g RestrictionGround) Article() string { return restrictionGrounds[g].article }

// Label is the plain-language ground.
func (g RestrictionGround) Label() string { return restrictionGrounds[g].label }

// Restriction is one Art 18 restriction and, once lifted, the record of the lift.
// It is updated on lift, never deleted: a lifted restriction is evidence that the
// right was honoured and for how long (Art 5(2)).
type Restriction struct {
	ID           string
	SubjectID    string
	RequestID    string
	Ground       RestrictionGround
	RestrictedBy string
	RestrictedAt time.Time

	LiftedAt      time.Time
	LiftedBy      string
	LiftReason    string
	LiftRequestID string
	// SubjectInformedAt records the operator's Art 18(3) affirmation that the
	// subject was told before the lift.
	SubjectInformedAt time.Time
}

// Active reports whether the restriction is in force.
func (r Restriction) Active() bool { return r.LiftedAt.IsZero() }

// ErrRestrictionActive is returned when a subject already has a restriction in
// force. One active restriction per subject: a second would make "which lift
// ends it" ambiguous.
var ErrRestrictionActive = errors.New("datasubject: this subject already has an active restriction")

// ErrNoActiveRestriction is returned when a subject has no restriction in force.
var ErrNoActiveRestriction = errors.New("datasubject: this subject has no active restriction")

// NewRestriction builds the restriction a verified Art 18 request imposes.
//
// The request must be a RESTRICTION request that has cleared the Art 12(6)
// identity gate: restricting a stranger's data on an unverified claim is an
// interference with the controller's processing that nobody was entitled to ask
// for, and it would also suppress data the real subject may be relying on.
func NewRestriction(req Request, ground RestrictionGround, by string, now time.Time) (Restriction, error) {
	if req.Kind != RequestRestriction {
		return Restriction{}, fmt.Errorf("datasubject: only a restriction request imposes a restriction, not %q", req.Kind)
	}
	if !req.MayProduceData() {
		return Restriction{}, fmt.Errorf("datasubject: request %s is %q — identity must be verified "+
			"before a restriction is imposed (Art 12(6))", req.ID, req.State)
	}
	if err := ground.Validate(); err != nil {
		return Restriction{}, err
	}
	if strings.TrimSpace(by) == "" {
		return Restriction{}, errors.New("datasubject: a restriction must record who imposed it")
	}
	return Restriction{
		ID: newRestrictionID(now), SubjectID: req.SubjectID, RequestID: req.ID,
		Ground: ground, RestrictedBy: strings.TrimSpace(by), RestrictedAt: now.UTC(),
	}, nil
}

// Lift ends the restriction.
//
// Art 18(3): a subject who obtained a restriction "shall be informed by the
// controller before the restriction of processing is lifted". The software cannot
// inform anyone, so the lift refuses unless the operator affirms they were, and the
// affirmation is recorded with its time. A reason is required because the
// grounds for lifting (accuracy verified, objection decided) are what a subject
// would contest.
func (r *Restriction) Lift(by, reason string, subjectInformed bool, liftRequestID string, now time.Time) error {
	if !r.Active() {
		return errors.New("datasubject: this restriction was already lifted")
	}
	if strings.TrimSpace(by) == "" {
		return errors.New("datasubject: a lift must record who lifted it")
	}
	if strings.TrimSpace(reason) == "" {
		return errors.New("datasubject: a lift needs a reason — it is what the subject would contest")
	}
	if !subjectInformed {
		return errors.New("datasubject: Art 18(3) requires the subject to be informed BEFORE the " +
			"restriction is lifted; inform them, then lift with --subject-informed")
	}
	now = now.UTC()
	r.LiftedAt, r.LiftedBy, r.LiftReason = now, strings.TrimSpace(by), strings.TrimSpace(reason)
	r.LiftRequestID = strings.TrimSpace(liftRequestID)
	r.SubjectInformedAt = now
	return nil
}

// RestrictionWithheld reports whether the software withholds rows of this table
// from use while a restriction is active. Knowledge-graph entities joined the
// set 2026-10-12 (GitHub #350, design §4.16.7): every graph read screens them.
func RestrictionWithheld(t LinkableTable) bool {
	return t == TableProjectMemoryChunks || t == TableKnowledgeEntities
}

// restrictionWithheldNote states what a WITHHELD table's withholding does not
// reach, so a withheld row is not read as "fully restricted".
var restrictionWithheldNote = map[LinkableTable]string{
	TableKnowledgeEntities: "withheld from graph search, entity pages, the subgraph view and agent context; " +
		"the extraction resolver can still compare a new, unidentified mention against it",
}

func restrictionWithheldNoteFor(t LinkableTable) string { return restrictionWithheldNote[t] }

// restrictionNotWithheld states, per table, why the software does NOT withhold
// it. Stated so the report can say it and so widening the closed set forces a
// decision (TestEveryLinkableTableHasARestrictionPosition).
var restrictionNotWithheld = map[LinkableTable]string{
	TableChatAuditLog: "conversation transcripts are read by chat history and audit views that do not " +
		"consult the subject index; restrict them by other means",
	TableTaskMessages: "task conversation messages are read by task views and resumption, which do " +
		"not consult the subject index; restrict them by other means",
	TableArtifacts: "uploaded files are read by artifact views and re-extraction, which do not consult " +
		"the subject index; their derived memory chunks ARE withheld when linked",
	TableExtractedDocuments: "extracted text is read by document views and re-ingestion, which do not " +
		"consult the subject index; derived memory chunks ARE withheld when linked",
	TableChannelSessions: "per-channel session state is used to route the subject's own messages",
	TableOperatorProfile: "the operator profile is used by the assistant's own dispatcher, which does not " +
		"consult the subject index",
	TableUserIdentities: "identity mappings are needed to authenticate and route the subject's own " +
		"messages, which is processing the subject's own use requires",
}

func restrictionNotWithheldReason(t LinkableTable) string { return restrictionNotWithheld[t] }

// TableCount is one table's share of a restriction report.
type TableCount struct {
	Table  string `json:"table"`
	Rows   int    `json:"rows"`
	Reason string `json:"reason,omitempty"`
}

// RestrictionReport is what imposing a restriction did and did not cover.
//
// RowsExamined is the denominator: every linked row, withheld or not. A report
// listing only the withheld rows would read as "everything about you is
// restricted", which is exactly the false assurance this design refuses.
type RestrictionReport struct {
	SubjectID    string       `json:"subject_id"`
	RequestID    string       `json:"request_id"`
	Ground       string       `json:"ground"`
	Article      string       `json:"article"`
	RestrictedAt time.Time    `json:"restricted_at"`
	RowsExamined int          `json:"rows_examined"`
	Withheld     []TableCount `json:"withheld"`
	// SharedWithheld counts withheld rows that also concern other people: they are
	// withheld whole, because a record cannot be half-used.
	SharedWithheld int          `json:"shared_withheld"`
	NotWithheld    []TableCount `json:"not_withheld,omitempty"`
	Limitations    []string     `json:"limitations"`
	Narrative      string       `json:"subject_narrative"`
}

// BuildRestrictionReport summarises a restriction against the subject's links.
func BuildRestrictionReport(r Restriction, links []Link) RestrictionReport {
	withheld := map[string]int{}
	notWithheld := map[string]int{}
	shared := 0
	for _, l := range links {
		if RestrictionWithheld(l.Table) {
			withheld[string(l.Table)]++
			if l.Exclusivity.TreatAsShared() {
				shared++
			}
			continue
		}
		notWithheld[string(l.Table)]++
	}
	rep := RestrictionReport{
		SubjectID: r.SubjectID, RequestID: r.RequestID,
		Ground: string(r.Ground), Article: r.Ground.Article(),
		RestrictedAt: r.RestrictedAt, RowsExamined: len(links), SharedWithheld: shared,
		Withheld:    sortedCounts(withheld, restrictionWithheldNoteFor),
		NotWithheld: sortedCounts(notWithheld, restrictionNotWithheldReason),
		Limitations: []string{
			"Only records the deployment has linked to you are covered. Records that mention you " +
				"but were never identified as being about you are not restricted.",
			"Memory records that also concern other people are withheld whole, including what they " +
				"say about those people.",
			"Restricted records remain stored and visible to the operator for administration, and are " +
				"still included when you exercise your rights of access, portability or erasure.",
			"Model providers that received your data before the restriction cannot be made to stop " +
				"processing it by this software; the operator handles that separately.",
		},
	}
	rep.Narrative = fmt.Sprintf("Processing of your personal data is restricted under %s (%s) from %s. "+
		"While the restriction is in force, %d record(s) linked to you (memory records and knowledge-graph "+
		"entries) are stored but not used, and so is any memory record that mentions a knowledge-graph "+
		"entity linked to you, and any knowledge-graph relation that involves one. "+
		"%d other linked record(s) are listed separately with the reason the software does not "+
		"withhold them; the operator must restrict those by other means. You will be informed before "+
		"the restriction is lifted.",
		r.Ground.Article(), r.Ground.Label(), r.RestrictedAt.Format("2006-01-02"),
		sumCounts(withheld), sumCounts(notWithheld))
	return rep
}

func sortedCounts(m map[string]int, reason func(LinkableTable) string) []TableCount {
	out := make([]TableCount, 0, len(m))
	for t, n := range m {
		tc := TableCount{Table: t, Rows: n}
		if reason != nil {
			tc.Reason = reason(LinkableTable(t))
		}
		out = append(out, tc)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Table < out[j].Table })
	return out
}

func sumCounts(m map[string]int) int {
	n := 0
	for _, v := range m {
		n += v
	}
	return n
}

func newRestrictionID(now time.Time) string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return "dsx_" + now.UTC().Format("20060102150405") + "_" + hex.EncodeToString(b[:])
}
