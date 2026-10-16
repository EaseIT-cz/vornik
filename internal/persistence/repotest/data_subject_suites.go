package repotest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/datasubject"
	"vornik.io/vornik/internal/persistence"
)

// Shared suites for the GDPR data-subject store, the rights ledger and the
// Art 18 restriction record (GitHub #355; design
// 2026-07-29-gdpr-data-subject-rights-design.md §4.19).
//
// audit 2026-10-12 (GitHub #355): the SQLite driver had none of these tables,
// so on Community's default backend a data-subject request could not even be
// recorded. Every contract below runs on SQLite (unit) and Postgres
// (integration lane), against the one subjectstore implementation.

// DataSubjectStore is the repository surface the suites pin. Implemented by
// subjectstore.Repository (postgres.DataSubjectRepository, and the SQLite
// constructor) — repotest names the methods rather than the type so the suite
// stays decoupled from the package.
type DataSubjectStore interface {
	CreateSubject(ctx context.Context, s datasubject.Subject) error
	GetSubject(ctx context.Context, id string) (datasubject.Subject, error)
	DeleteSubject(ctx context.Context, id string) error
	ReassignLinks(ctx context.Context, fromSubjectID, toSubjectID string) (int, error)
	ListSubjects(ctx context.Context, limit int) ([]datasubject.Subject, error)
	AddIdentifier(ctx context.Context, subjectID string, id datasubject.Identifier) error
	ListIdentifiers(ctx context.Context, subjectID string) ([]datasubject.Identifier, error)
	FindSubjectByIdentifier(ctx context.Context, kind, value string) (string, error)
	AddLink(ctx context.Context, subjectID string, l datasubject.Link) error
	AddLinkIfAbsent(ctx context.Context, subjectID string, l datasubject.Link) (bool, error)
	DeleteOperatorLink(ctx context.Context, subjectID, table, rowID string) (bool, error)
	ListLinks(ctx context.Context, subjectID string) ([]datasubject.Link, error)
	CountOtherSubjectsOnRow(ctx context.Context, subjectID, table, rowID string) (int, error)

	CreateRequest(ctx context.Context, req datasubject.Request) error
	GetRequest(ctx context.Context, id string) (datasubject.Request, error)
	SaveRequest(ctx context.Context, req datasubject.Request) error
	ListLiveRequests(ctx context.Context) ([]datasubject.Request, error)
	ListRequests(ctx context.Context, limit, offset int) ([]datasubject.Request, error)
	GetRequestReport(ctx context.Context, id string) (reportJSON, hash string, err error)
	SaveRequestReport(ctx context.Context, requestID, reportJSON, hash string) error

	CreateRestriction(ctx context.Context, res datasubject.Restriction) error
	ActiveRestriction(ctx context.Context, subjectID string) (datasubject.Restriction, error)
	SaveRestrictionLift(ctx context.Context, res datasubject.Restriction) error

	CollectItems(ctx context.Context, subjectID string, links []datasubject.Link) ([]datasubject.Item, error)
	DeleteRow(ctx context.Context, table datasubject.LinkableTable, rowID string) error
}

// SeedChatTurn inserts a chat_audit_log row whose user_message is message.
type SeedChatTurn func(ctx context.Context, id, projectID, message string) error

// sameInstant compares timestamps at microsecond precision — Postgres keeps
// microseconds, SQLite nanoseconds.
func sameInstant(a, b time.Time) bool {
	return a.UTC().Truncate(time.Microsecond).Equal(b.UTC().Truncate(time.Microsecond))
}

func mustSubject(ctx context.Context, t *testing.T, s DataSubjectStore, name string) string {
	t.Helper()
	id := uniqueID("ds")
	if err := s.CreateSubject(ctx, datasubject.Subject{ID: id, DisplayName: name}); err != nil {
		t.Fatalf("CreateSubject: %v", err)
	}
	return id
}

func chatLink(row, project string, src datasubject.Source, conf datasubject.Confidence) datasubject.Link {
	return datasubject.Link{Table: datasubject.TableChatAuditLog, RowID: row, ProjectID: project,
		Source: src, Confidence: conf, Exclusivity: datasubject.ExclusiveRow}
}

// RunDataSubjectStoreSuite pins subjects, identifiers and links: round trips,
// idempotence, the reverse identifier lookup, the operator-only unlink, link
// reassignment, and the referential rules SQLite cannot leave to its schema
// (foreign_keys OFF): no child row may name an unknown subject, deleting a
// subject takes its identifiers and links, and a subject the ledger names is
// retained.
func RunDataSubjectStoreSuite(t *testing.T, s DataSubjectStore) {
	t.Helper()
	ctx := context.Background()

	t.Run("subject round trip and not found", func(t *testing.T) { storeSubjectRoundTrip(ctx, t, s) })

	t.Run("list newest first", func(t *testing.T) { storeListNewestFirst(ctx, t, s) })

	t.Run("identifiers idempotent, sorted, reverse lookup", func(t *testing.T) { storeIdentifiers(ctx, t, s) })

	t.Run("children of an unknown subject are refused", func(t *testing.T) { storeUnknownParent(ctx, t, s) })

	t.Run("links upsert, if-absent, other subjects, operator unlink", func(t *testing.T) { storeLinks(ctx, t, s) })

	t.Run("reassign moves links, keeps the target's row, refuses an unknown target", func(t *testing.T) { storeReassign(ctx, t, s) })

	t.Run("delete cascades the index, retains a subject the ledger names", func(t *testing.T) { storeDeleteAndRetain(ctx, t, s) })
}

func storeSubjectRoundTrip(ctx context.Context, t *testing.T, s DataSubjectStore) {
	id := mustSubject(ctx, t, s, "Jane Roe")
	got, err := s.GetSubject(ctx, id)
	if err != nil || got.ID != id || got.DisplayName != "Jane Roe" || got.RequestState != "" {
		t.Fatalf("GetSubject = %+v, %v", got, err)
	}
	if _, err := s.GetSubject(ctx, uniqueID("ds-missing")); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("missing subject: want ErrNotFound, got %v", err)
	}
	if err := s.CreateSubject(ctx, datasubject.Subject{ID: id, DisplayName: "dup"}); !errors.Is(err, persistence.ErrDuplicateKey) {
		t.Fatalf("duplicate subject id: want ErrDuplicateKey, got %v", err)
	}
}

func storeListNewestFirst(ctx context.Context, t *testing.T, s DataSubjectStore) {
	// b is created after a; on an equal clock reading the id (later by
	// construction) breaks the tie the same way.
	a := mustSubject(ctx, t, s, "first")
	b := mustSubject(ctx, t, s, "second")
	got, err := s.ListSubjects(ctx, 500)
	if err != nil {
		t.Fatal(err)
	}
	pos := map[string]int{}
	for i, x := range got {
		pos[x.ID] = i
	}
	ia, okA := pos[a]
	ib, okB := pos[b]
	if !okA || !okB || ib > ia {
		t.Fatalf("want %s before %s, positions %d/%v %d/%v", b, a, ib, okB, ia, okA)
	}
}

func storeIdentifiers(ctx context.Context, t *testing.T, s DataSubjectStore) {
	id := mustSubject(ctx, t, s, "ids")
	email := uniqueID("e") + "@example.com"
	for i := 0; i < 2; i++ {
		if err := s.AddIdentifier(ctx, id, datasubject.Identifier{Kind: "email", Value: email,
			Source: datasubject.SourceOperatorAsserted, Confidence: datasubject.ConfidenceCertain}); err != nil {
			t.Fatalf("AddIdentifier #%d: %v", i, err)
		}
	}
	if err := s.AddIdentifier(ctx, id, datasubject.Identifier{Kind: "channel", Value: "telegram:1",
		Source: datasubject.SourceAuthenticated, Confidence: datasubject.ConfidenceCertain}); err != nil {
		t.Fatal(err)
	}
	ids, err := s.ListIdentifiers(ctx, id)
	if err != nil || len(ids) != 2 || ids[0].Kind != "channel" || ids[1].Value != email ||
		ids[1].Source != datasubject.SourceOperatorAsserted || ids[1].Confidence != datasubject.ConfidenceCertain {
		t.Fatalf("ListIdentifiers = %+v, %v", ids, err)
	}
	if got, err := s.FindSubjectByIdentifier(ctx, "email", email); err != nil || got != id {
		t.Fatalf("FindSubjectByIdentifier = %q, %v", got, err)
	}
	if got, err := s.FindSubjectByIdentifier(ctx, "email", "nobody@example.com"); err != nil || got != "" {
		t.Fatalf("unknown identifier: want (\"\", nil), got %q, %v", got, err)
	}
}

func storeUnknownParent(ctx context.Context, t *testing.T, s DataSubjectStore) {
	ghost := uniqueID("ds-ghost")
	if err := s.AddIdentifier(ctx, ghost, datasubject.Identifier{Kind: "email", Value: "g@example.com",
		Source: datasubject.SourceOperatorAsserted, Confidence: datasubject.ConfidenceCertain}); !errors.Is(err, persistence.ErrNotFound) {
		t.Errorf("AddIdentifier(unknown): want ErrNotFound, got %v", err)
	}
	l := chatLink(uniqueID("row"), "p", datasubject.SourceOperatorAsserted, datasubject.ConfidenceCertain)
	if err := s.AddLink(ctx, ghost, l); !errors.Is(err, persistence.ErrNotFound) {
		t.Errorf("AddLink(unknown): want ErrNotFound, got %v", err)
	}
	if ok, err := s.AddLinkIfAbsent(ctx, ghost, l); ok || !errors.Is(err, persistence.ErrNotFound) {
		t.Errorf("AddLinkIfAbsent(unknown): want (false, ErrNotFound), got %v, %v", ok, err)
	}
	if err := s.CreateRequest(ctx, datasubject.Request{ID: uniqueID("dsr"), SubjectID: ghost,
		Kind: datasubject.RequestAccess, State: datasubject.StateOpen, OpenedAt: time.Now()}); !errors.Is(err, persistence.ErrNotFound) {
		t.Errorf("CreateRequest(unknown): want ErrNotFound, got %v", err)
	}
	if links, _ := s.ListLinks(ctx, ghost); len(links) != 0 {
		t.Errorf("no link may name an unknown subject, found %v", links)
	}
	if ids, _ := s.ListIdentifiers(ctx, ghost); len(ids) != 0 {
		t.Errorf("no identifier may name an unknown subject, found %v", ids)
	}
}

func storeLinks(ctx context.Context, t *testing.T, s DataSubjectStore) {
	a, b := mustSubject(ctx, t, s, "a"), mustSubject(ctx, t, s, "b")
	row := uniqueID("chat")
	if err := s.AddLink(ctx, a, chatLink(row, "proj", datasubject.SourceKGExtraction, datasubject.ConfidencePossible)); err != nil {
		t.Fatal(err)
	}
	// Upsert: confidence and exclusivity move, source does not.
	up := chatLink(row, "proj", datasubject.SourceKGExtraction, datasubject.ConfidencePossible)
	up.Exclusivity = datasubject.SharedRow
	if err := s.AddLink(ctx, a, up); err != nil {
		t.Fatal(err)
	}
	links, err := s.ListLinks(ctx, a)
	if err != nil || len(links) != 1 || links[0].Exclusivity != datasubject.SharedRow || links[0].ProjectID != "proj" {
		t.Fatalf("ListLinks after upsert = %+v, %v", links, err)
	}
	// AddLinkIfAbsent never overwrites.
	ins, err := s.AddLinkIfAbsent(ctx, a, chatLink(row, "proj", datasubject.SourceOperatorAsserted, datasubject.ConfidenceCertain))
	if err != nil || ins {
		t.Fatalf("AddLinkIfAbsent over an existing link = %v, %v; want false, nil", ins, err)
	}
	if links, _ := s.ListLinks(ctx, a); links[0].Source != datasubject.SourceKGExtraction || links[0].Confidence != datasubject.ConfidencePossible {
		t.Fatalf("AddLinkIfAbsent must not overwrite: %+v", links[0])
	}
	// A global-table link keeps an empty project.
	ins, err = s.AddLinkIfAbsent(ctx, b, datasubject.Link{Table: datasubject.TableTaskMessages, RowID: uniqueID("msg"),
		Source: datasubject.SourceOperatorAsserted, Confidence: datasubject.ConfidenceCertain, Exclusivity: datasubject.ExclusiveRow})
	if err != nil || !ins {
		t.Fatalf("AddLinkIfAbsent new = %v, %v", ins, err)
	}
	if err := s.AddLink(ctx, b, chatLink(row, "proj", datasubject.SourceOperatorAsserted, datasubject.ConfidenceCertain)); err != nil {
		t.Fatal(err)
	}
	if n, err := s.CountOtherSubjectsOnRow(ctx, a, string(datasubject.TableChatAuditLog), row); err != nil || n != 1 {
		t.Fatalf("CountOtherSubjectsOnRow = %d, %v; want 1", n, err)
	}
	// Only an operator-asserted link is removable.
	if ok, err := s.DeleteOperatorLink(ctx, a, string(datasubject.TableChatAuditLog), row); err != nil || ok {
		t.Fatalf("unlinking a kg_extraction link = %v, %v; want false", ok, err)
	}
	if ok, err := s.DeleteOperatorLink(ctx, b, string(datasubject.TableChatAuditLog), row); err != nil || !ok {
		t.Fatalf("unlinking an operator link = %v, %v; want true", ok, err)
	}
	if n, _ := s.CountOtherSubjectsOnRow(ctx, a, string(datasubject.TableChatAuditLog), row); n != 0 {
		t.Fatalf("after unlink, others = %d", n)
	}
	if err := s.AddLink(ctx, a, datasubject.Link{Table: "tool_audit_log", RowID: "x",
		Source: datasubject.SourceOperatorAsserted, Confidence: datasubject.ConfidenceCertain, Exclusivity: datasubject.ExclusiveRow}); err == nil {
		t.Fatal("a link outside the closed table set must be refused")
	}
}

func storeReassign(ctx context.Context, t *testing.T, s DataSubjectStore) {
	from, to := mustSubject(ctx, t, s, "from"), mustSubject(ctx, t, s, "to")
	shared, only := uniqueID("chat"), uniqueID("chat")
	for _, l := range []struct {
		subj, row string
		conf      datasubject.Confidence
	}{{from, shared, datasubject.ConfidencePossible}, {from, only, datasubject.ConfidencePossible}, {to, shared, datasubject.ConfidenceCertain}} {
		if err := s.AddLink(ctx, l.subj, chatLink(l.row, "p", datasubject.SourceOperatorAsserted, l.conf)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.ReassignLinks(ctx, from, uniqueID("ds-ghost")); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("reassign to an unknown subject: want ErrNotFound, got %v", err)
	}
	if links, _ := s.ListLinks(ctx, from); len(links) != 2 {
		t.Fatalf("a refused reassign must move nothing, from has %d links", len(links))
	}
	n, err := s.ReassignLinks(ctx, from, to)
	if err != nil || n != 2 {
		t.Fatalf("ReassignLinks = %d, %v; want 2", n, err)
	}
	if links, _ := s.ListLinks(ctx, from); len(links) != 0 {
		t.Fatalf("source keeps %d links", len(links))
	}
	links, _ := s.ListLinks(ctx, to)
	if len(links) != 2 {
		t.Fatalf("target has %d links, want 2", len(links))
	}
	for _, l := range links {
		if l.RowID == shared && l.Confidence != datasubject.ConfidenceCertain {
			t.Fatalf("the target's existing row must win: %+v", l)
		}
	}
	if _, err := s.ReassignLinks(ctx, to, to); err == nil {
		t.Fatal("reassign to self must be refused")
	}
}

func storeDeleteAndRetain(ctx context.Context, t *testing.T, s DataSubjectStore) {
	id := mustSubject(ctx, t, s, "gone")
	if err := s.AddIdentifier(ctx, id, datasubject.Identifier{Kind: "email", Value: uniqueID("x") + "@example.com",
		Source: datasubject.SourceOperatorAsserted, Confidence: datasubject.ConfidenceCertain}); err != nil {
		t.Fatal(err)
	}
	row := uniqueID("chat")
	if err := s.AddLink(ctx, id, chatLink(row, "p", datasubject.SourceOperatorAsserted, datasubject.ConfidenceCertain)); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSubject(ctx, id); err != nil {
		t.Fatalf("DeleteSubject: %v", err)
	}
	if _, err := s.GetSubject(ctx, id); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("subject survives delete: %v", err)
	}
	if ids, _ := s.ListIdentifiers(ctx, id); len(ids) != 0 {
		t.Fatalf("identifiers outlive their subject: %v", ids)
	}
	if links, _ := s.ListLinks(ctx, id); len(links) != 0 {
		t.Fatalf("links outlive their subject: %v", links)
	}
	if n, _ := s.CountOtherSubjectsOnRow(ctx, "nobody", string(datasubject.TableChatAuditLog), row); n != 0 {
		t.Fatalf("a deleted subject is still counted on its row: %d", n)
	}

	held := mustSubject(ctx, t, s, "held")
	if err := s.CreateRequest(ctx, datasubject.Request{ID: uniqueID("dsr"), SubjectID: held,
		Kind: datasubject.RequestAccess, State: datasubject.StateOpen, OpenedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSubject(ctx, held); err == nil {
		t.Fatal("a subject named by a rights request must be retained")
	}
	if _, err := s.GetSubject(ctx, held); err != nil {
		t.Fatalf("a refused delete removed the subject: %v", err)
	}
}

// RunDataSubjectLedgerSuite pins the rights ledger: request round trip with
// every column, the Art 12(3) live list (open and verified only, oldest first),
// the full record (every state, newest first, id breaking ties, paged), and the
// retained report — absent row vs. no retained report are different errors.
func RunDataSubjectLedgerSuite(t *testing.T, s DataSubjectStore) {
	t.Helper()
	ctx := context.Background()
	subj := mustSubject(ctx, t, s, "ledger")
	base := time.Date(2026, 10, 1, 9, 0, 0, 123456000, time.UTC)

	reqID := uniqueID("dsr")
	req := datasubject.Request{ID: reqID, SubjectID: subj, Kind: datasubject.RequestErasure,
		State: datasubject.StateOpen, OpenedAt: base, ErasureGround: datasubject.GroundConsentWithdrawn}
	if err := s.CreateRequest(ctx, req); err != nil {
		t.Fatalf("CreateRequest: %v", err)
	}
	if err := s.CreateRequest(ctx, req); !errors.Is(err, persistence.ErrDuplicateKey) {
		t.Fatalf("duplicate request id: want ErrDuplicateKey, got %v", err)
	}
	got, err := s.GetRequest(ctx, reqID)
	if err != nil || got.SubjectID != subj || got.Kind != datasubject.RequestErasure || got.State != datasubject.StateOpen ||
		!sameInstant(got.OpenedAt, base) || got.ErasureGround != datasubject.GroundConsentWithdrawn ||
		!got.VerifiedAt.IsZero() || got.Extended {
		t.Fatalf("GetRequest = %+v, %v", got, err)
	}
	if _, err := s.GetRequest(ctx, uniqueID("dsr-missing")); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("missing request: want ErrNotFound, got %v", err)
	}

	verified := base.Add(time.Hour)
	got.State, got.VerifiedBy, got.VerifiedHow, got.VerifiedAt = datasubject.StateVerified, "op", "video call", verified
	got.Extended, got.ExtendedReason, got.ReportHash = true, "complex", "sha256:aa"
	if err := s.SaveRequest(ctx, got); err != nil {
		t.Fatalf("SaveRequest: %v", err)
	}
	back, err := s.GetRequest(ctx, reqID)
	if err != nil || back.State != datasubject.StateVerified || back.VerifiedBy != "op" || back.VerifiedHow != "video call" ||
		!sameInstant(back.VerifiedAt, verified) || !back.Extended || back.ExtendedReason != "complex" || back.ReportHash != "sha256:aa" {
		t.Fatalf("after SaveRequest = %+v, %v", back, err)
	}

	newer := ledgerLists(ctx, t, s, subj, reqID, base)
	ledgerReport(ctx, t, s, reqID, newer)
}

// ledgerLists pins the live list and the full record; it returns the newest
// request's id.
func ledgerLists(ctx context.Context, t *testing.T, s DataSubjectStore, subj, reqID string, base time.Time) string {
	t.Helper()
	// Two more requests: one older and refused (not live), one newer and open.
	refused := datasubject.Request{ID: uniqueID("dsr"), SubjectID: subj, Kind: datasubject.RequestAccess,
		State: datasubject.StateOpen, OpenedAt: base.Add(-time.Hour)}
	newer := datasubject.Request{ID: uniqueID("dsr"), SubjectID: subj, Kind: datasubject.RequestPortability,
		State: datasubject.StateOpen, OpenedAt: base.Add(2 * time.Hour)}
	for _, r := range []datasubject.Request{refused, newer} {
		if err := s.CreateRequest(ctx, r); err != nil {
			t.Fatal(err)
		}
	}
	refused.State, refused.RefusedReason = datasubject.StateRefused, "manifestly unfounded"
	if err := s.SaveRequest(ctx, refused); err != nil {
		t.Fatal(err)
	}

	live, err := s.ListLiveRequests(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, r := range live {
		if r.SubjectID == subj {
			order = append(order, r.ID)
			if r.ID == reqID && (!r.Extended || !sameInstant(r.OpenedAt, base)) {
				t.Fatalf("live row lost its clock fields: %+v", r)
			}
		}
	}
	if strings.Join(order, ",") != reqID+","+newer.ID {
		t.Fatalf("live requests = %v, want [%s %s] (oldest first, refused excluded)", order, reqID, newer.ID)
	}

	all, err := listAllRequests(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	var mine []datasubject.Request
	for _, r := range all {
		if r.SubjectID == subj {
			mine = append(mine, r)
		}
	}
	if len(mine) != 3 || mine[0].ID != newer.ID || mine[1].ID != reqID || mine[2].ID != refused.ID ||
		mine[2].RefusedReason != "manifestly unfounded" {
		t.Fatalf("ListRequests (newest first, every state) = %+v", mine)
	}
	// Paging is deterministic: page 1 + page 2 of size 1 are the first two rows.
	p1, _ := s.ListRequests(ctx, 1, 0)
	p2, _ := s.ListRequests(ctx, 1, 1)
	if len(p1) != 1 || len(p2) != 1 || p1[0].ID != all[0].ID || p2[0].ID != all[1].ID {
		t.Fatalf("paging: p1=%v p2=%v all[0:2]=%v", p1, p2, all[:2])
	}
	return newer.ID
}

// ledgerReport pins the retained report: absent row, no report, stored.
func ledgerReport(ctx context.Context, t *testing.T, s DataSubjectStore, reqID, newerID string) {
	t.Helper()
	// Report: no row vs. no retained report vs. retained.
	if _, _, err := s.GetRequestReport(ctx, uniqueID("dsr-missing")); !errors.Is(err, persistence.ErrNotFound) ||
		errors.Is(err, persistence.ErrReportNotRetained) {
		t.Fatalf("report of a missing request: want plain ErrNotFound, got %v", err)
	}
	if _, _, err := s.GetRequestReport(ctx, reqID); !errors.Is(err, persistence.ErrReportNotRetained) {
		t.Fatalf("report not yet stored: want ErrReportNotRetained, got %v", err)
	}
	if err := s.SaveRequestReport(ctx, reqID, `{"plan":{}}`, "sha256:bb"); err != nil {
		t.Fatalf("SaveRequestReport: %v", err)
	}
	body, hash, err := s.GetRequestReport(ctx, reqID)
	if err != nil || body != `{"plan":{}}` || hash != "sha256:bb" {
		t.Fatalf("GetRequestReport = %q, %q, %v", body, hash, err)
	}
	all, err := listAllRequests(ctx, s)
	if err != nil {
		t.Fatal(err)
	}
	found := 0
	for _, r := range all {
		if r.ID == reqID || r.ID == newerID {
			found++
		}
		if r.ID == reqID && !r.ReportRetained {
			t.Fatal("ListRequests must flag a retained report")
		}
		if r.ID == newerID && r.ReportRetained {
			t.Fatal("ListRequests flags a report that was never stored")
		}
	}
	if found != 2 {
		t.Fatalf("ListRequests returned %d of the 2 requests this suite filed (of %d rows read)", found, len(all))
	}
}

// RunDataSubjectRestrictionStoreSuite pins the Art 18 record: one active
// restriction per subject, read back, lifted once (the row is kept), a second
// lift refused, a new restriction possible after a lift, and a restriction
// that would resolve to no subject or no request refused.
func RunDataSubjectRestrictionStoreSuite(t *testing.T, s DataSubjectStore) {
	t.Helper()
	ctx := context.Background()
	subj := mustSubject(ctx, t, s, "restricted")
	reqID := uniqueID("dsr")
	if err := s.CreateRequest(ctx, datasubject.Request{ID: reqID, SubjectID: subj,
		Kind: datasubject.RequestRestriction, State: datasubject.StateVerified, OpenedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ActiveRestriction(ctx, subj); !errors.Is(err, datasubject.ErrNoActiveRestriction) {
		t.Fatalf("no restriction yet: want ErrNoActiveRestriction, got %v", err)
	}
	at := time.Date(2026, 10, 12, 8, 30, 0, 0, time.UTC)
	res := datasubject.Restriction{ID: uniqueID("dsx"), SubjectID: subj, RequestID: reqID,
		Ground: datasubject.RestrictAccuracyContested, RestrictedBy: "op", RestrictedAt: at}
	if err := s.CreateRestriction(ctx, res); err != nil {
		t.Fatalf("CreateRestriction: %v", err)
	}
	second := res
	second.ID = uniqueID("dsx")
	if err := s.CreateRestriction(ctx, second); !errors.Is(err, datasubject.ErrRestrictionActive) {
		t.Fatalf("second active restriction: want ErrRestrictionActive, got %v", err)
	}
	got, err := s.ActiveRestriction(ctx, subj)
	if err != nil || got.ID != res.ID || got.RequestID != reqID || got.Ground != datasubject.RestrictAccuracyContested ||
		got.RestrictedBy != "op" || !sameInstant(got.RestrictedAt, at) {
		t.Fatalf("ActiveRestriction = %+v, %v", got, err)
	}

	ghost := res
	ghost.ID, ghost.SubjectID = uniqueID("dsx"), uniqueID("ds-ghost")
	if err := s.CreateRestriction(ctx, ghost); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("restriction of an unknown subject: want ErrNotFound, got %v", err)
	}
	other := mustSubject(ctx, t, s, "other")
	noReq := datasubject.Restriction{ID: uniqueID("dsx"), SubjectID: other, RequestID: uniqueID("dsr-ghost"),
		Ground: datasubject.RestrictLegalClaims, RestrictedBy: "op", RestrictedAt: at}
	if err := s.CreateRestriction(ctx, noReq); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("restriction citing an unknown request: want ErrNotFound, got %v", err)
	}
	noReq.RequestID = reqID // a request, but another subject's
	if err := s.CreateRestriction(ctx, noReq); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("restriction citing another subject's request: want ErrNotFound, got %v", err)
	}

	lifted := got
	lifted.LiftedAt, lifted.LiftedBy, lifted.LiftReason = at.Add(time.Hour), "op2", "accuracy verified"
	lifted.SubjectInformedAt = at.Add(30 * time.Minute)
	if err := s.SaveRestrictionLift(ctx, lifted); err != nil {
		t.Fatalf("SaveRestrictionLift: %v", err)
	}
	if err := s.SaveRestrictionLift(ctx, lifted); err == nil {
		t.Fatal("a second lift of the same restriction must be refused")
	}
	if _, err := s.ActiveRestriction(ctx, subj); !errors.Is(err, datasubject.ErrNoActiveRestriction) {
		t.Fatalf("after lift: want ErrNoActiveRestriction, got %v", err)
	}
	if err := s.CreateRestriction(ctx, second); err != nil {
		t.Fatalf("a new restriction after a lift: %v", err)
	}
	if err := s.DeleteSubject(ctx, subj); err == nil {
		t.Fatal("a subject named by a restriction must be retained")
	}
}

// RunDataSubjectCollectEraseSuite pins the export collector and the plain-row
// eraser: content is read through the closed spec set (NULL columns read as
// empty), exclusivity is RE-DERIVED from other subjects' links, a vanished row
// and a channel session the store does not hold are skipped rather than
// reported, and DeleteRow removes the row and every subject's link to it while
// refusing artifacts and channel sessions.
func RunDataSubjectCollectEraseSuite(t *testing.T, s DataSubjectStore, seedChat SeedChatTurn, seedChunk SeedChunk) {
	t.Helper()
	ctx := context.Background()
	subj, other := mustSubject(ctx, t, s, "collected"), mustSubject(ctx, t, s, "bystander")
	proj := uniqueID("proj")
	mine, shared, gone := uniqueID("chat"), uniqueID("chat"), uniqueID("chat")
	if err := seedChat(ctx, mine, proj, "my own message"); err != nil {
		t.Fatalf("seed chat: %v", err)
	}
	if err := seedChat(ctx, shared, proj, "message naming two people"); err != nil {
		t.Fatalf("seed chat: %v", err)
	}
	chunk := uniqueID("chunk")
	if err := seedChunk(ctx, chunk, proj, "a memory about them", false, time.Now()); err != nil {
		t.Fatalf("seed chunk: %v", err)
	}
	links := []datasubject.Link{
		chatLink(mine, proj, datasubject.SourceAuthenticated, datasubject.ConfidenceCertain),
		chatLink(shared, proj, datasubject.SourceAuthenticated, datasubject.ConfidenceCertain),
		chatLink(gone, proj, datasubject.SourceAuthenticated, datasubject.ConfidenceCertain),
		{Table: datasubject.TableProjectMemoryChunks, RowID: chunk, ProjectID: proj,
			Source: datasubject.SourceKGExtraction, Confidence: datasubject.ConfidencePossible, Exclusivity: datasubject.UnknownExclusivity},
		{Table: datasubject.TableChannelSessions, RowID: "telegram/" + uniqueID("sess"),
			Source: datasubject.SourceAuthenticated, Confidence: datasubject.ConfidenceCertain, Exclusivity: datasubject.ExclusiveRow},
	}
	for _, l := range links {
		if err := s.AddLink(ctx, subj, l); err != nil {
			t.Fatalf("AddLink %s: %v", l.Table, err)
		}
	}
	if err := s.AddLink(ctx, other, chatLink(shared, proj, datasubject.SourceKGExtraction, datasubject.ConfidencePossible)); err != nil {
		t.Fatal(err)
	}

	items, err := s.CollectItems(ctx, subj, links)
	if err != nil {
		t.Fatalf("CollectItems: %v", err)
	}
	byRow := map[string]datasubject.Item{}
	for _, it := range items {
		byRow[it.RowID] = it
	}
	if len(items) != 3 {
		t.Fatalf("collected %d items, want 3 (the vanished chat row and the absent session skipped): %+v", len(items), items)
	}
	if it := byRow[mine]; it.Content != "my own message" || it.Exclusivity != datasubject.ExclusiveRow || !it.ProvidedBySubject {
		t.Fatalf("own row = %+v", it)
	}
	if it := byRow[shared]; it.Exclusivity != datasubject.SharedRow {
		t.Fatalf("a row another subject is linked to must be shared, got %+v", it)
	}
	if it := byRow[chunk]; it.Content != "a memory about them" || it.ProvidedBySubject {
		t.Fatalf("chunk item = %+v", it)
	}

	collectEraseDeletes(ctx, t, s, subj, proj, shared, chunk)
}

// collectEraseDeletes pins DeleteRow after a collection: the row and every
// subject's link go together, a repeat is a no-op, and artifacts and channel
// sessions are refused.
func collectEraseDeletes(ctx context.Context, t *testing.T, s DataSubjectStore, subj, proj, shared, chunk string) {
	t.Helper()
	// Erase: row and every subject's link go together.
	if err := s.DeleteRow(ctx, datasubject.TableChatAuditLog, shared); err != nil {
		t.Fatalf("DeleteRow: %v", err)
	}
	if n, _ := s.CountOtherSubjectsOnRow(ctx, subj, string(datasubject.TableChatAuditLog), shared); n != 0 {
		t.Fatalf("another subject's link to an erased row survives (%d)", n)
	}
	for _, l := range mustLinks(ctx, t, s, subj) {
		if l.RowID == shared {
			t.Fatal("the requester's link to an erased row survives")
		}
	}
	items, err := s.CollectItems(ctx, subj, []datasubject.Link{chatLink(shared, proj, datasubject.SourceAuthenticated, datasubject.ConfidenceCertain)})
	if err != nil || len(items) != 0 {
		t.Fatalf("an erased row is still collected: %+v, %v", items, err)
	}
	if err := s.DeleteRow(ctx, datasubject.TableProjectMemoryChunks, chunk); err != nil {
		t.Fatalf("DeleteRow chunk: %v", err)
	}
	if err := s.DeleteRow(ctx, datasubject.TableProjectMemoryChunks, chunk); err != nil {
		t.Fatalf("a second erasure of the same row must be a no-op (erasure retries): %v", err)
	}
	if err := s.DeleteRow(ctx, datasubject.TableArtifacts, "art"); err == nil {
		t.Fatal("artifacts must go through the erasure cascade, not a row delete")
	}
	if err := s.DeleteRow(ctx, datasubject.TableChannelSessions, "telegram/x"); err == nil {
		t.Fatal("a channel session must not be deleted as a plain row")
	}
}

func mustLinks(ctx context.Context, t *testing.T, s DataSubjectStore, subj string) []datasubject.Link {
	t.Helper()
	links, err := s.ListLinks(ctx, subj)
	if err != nil {
		t.Fatal(err)
	}
	return links
}

// listAllRequests reads every page. The Postgres lane shares one database
// across runs, so the requests a suite files are not guaranteed to be in the
// first page: a fixed 200-row read lost them once the shared table passed
// 200 rows (TestDataSubjectLedger_PostgresContract, 2026-10-14, 235 rows).
func listAllRequests(ctx context.Context, s DataSubjectStore) ([]datasubject.Request, error) {
	const page = 200
	var all []datasubject.Request
	for offset := 0; ; offset += page {
		rows, err := s.ListRequests(ctx, page, offset)
		if err != nil {
			return nil, err
		}
		all = append(all, rows...)
		if len(rows) < page {
			return all, nil
		}
	}
}

// TranscriptSeeder writes the transcript rows the export-completeness suite
// reads, with every column the collector is meant to export.
type TranscriptSeeder interface {
	// SeedChatTurnFull inserts a chat_audit_log row.
	SeedChatTurnFull(ctx context.Context, id, projectID, userID, message, toolCallsJSON, signalsJSON string) error
	// SeedTaskMessage inserts a task_messages row and the task it belongs to.
	// An empty metadataJSON stores NULL.
	SeedTaskMessage(ctx context.Context, id, projectID, authorID, content, metadataJSON string) error
}

// RunDataSubjectTranscriptExportSuite pins Art 15 export completeness for the
// two transcript tables (design 2026-07-29-art17-redact-and-reembed §15.11.4,
// slice 5d-E).
//
// audit 2026-10-16 (GitHub #344, W10-E): the collector read chat_audit_log as
// user_message+response and task_messages as content only, so an exclusive
// row's export left out the subject's tool-call arguments, the signals quoting
// the reply, the metadata they supplied and who wrote the turn. Found at:
// review/audit, observed by a subject in the export, so the test sits at the
// export collector against a real database on both drivers.
func RunDataSubjectTranscriptExportSuite(t *testing.T, s DataSubjectStore, seed TranscriptSeeder) {
	t.Helper()
	ctx := context.Background()
	subj := mustSubject(ctx, t, s, "transcript-export")
	proj := uniqueID("proj")

	full, plain := uniqueID("chat"), uniqueID("chat")
	if err := seed.SeedChatTurnFull(ctx, full, proj, "telegram:4242", "send it to my sister",
		`[{"name":"send_mail","args":"{\"to\":\"anna.example@mail.test\"}","result":"inbox of carol.other@mail.test","error":"dave.other@mail.test bounced"}]`,
		`[{"detector":"url_not_fetched","claim_value":"https://anna.example/cv","sentence":"I sent your CV to Anna."}]`); err != nil {
		t.Fatalf("seed chat: %v", err)
	}
	if err := seed.SeedChatTurnFull(ctx, plain, proj, "", "just a question", "[]", ""); err != nil {
		t.Fatalf("seed chat: %v", err)
	}
	msg, bare := uniqueID("msg"), uniqueID("msg")
	if err := seed.SeedTaskMessage(ctx, msg, proj, "tg:4242", "please use the new address",
		`{"reply_to_message_id":77,"reason":"moved to Main Street 5","chat_id":-100777,"custom":"erin.other@mail.test"}`); err != nil {
		t.Fatalf("seed task message: %v", err)
	}
	if err := seed.SeedTaskMessage(ctx, bare, proj, "", "no metadata here", ""); err != nil {
		t.Fatalf("seed task message: %v", err)
	}

	links := []datasubject.Link{
		chatLink(full, proj, datasubject.SourceOperatorAsserted, datasubject.ConfidenceCertain),
		chatLink(plain, proj, datasubject.SourceOperatorAsserted, datasubject.ConfidenceCertain),
		{Table: datasubject.TableTaskMessages, RowID: msg, Source: datasubject.SourceOperatorAsserted,
			Confidence: datasubject.ConfidenceCertain, Exclusivity: datasubject.ExclusiveRow},
		{Table: datasubject.TableTaskMessages, RowID: bare, Source: datasubject.SourceOperatorAsserted,
			Confidence: datasubject.ConfidenceCertain, Exclusivity: datasubject.ExclusiveRow},
	}
	for _, l := range links {
		if err := s.AddLink(ctx, subj, l); err != nil {
			t.Fatalf("AddLink %s: %v", l.Table, err)
		}
	}
	items, err := s.CollectItems(ctx, subj, links)
	if err != nil {
		t.Fatalf("CollectItems: %v", err)
	}
	byRow := map[string]datasubject.Item{}
	for _, it := range items {
		byRow[it.RowID] = it
	}
	if len(byRow) != 4 {
		t.Fatalf("collected %d items, want 4: %+v", len(byRow), items)
	}

	got := byRow[full].Content
	for _, want := range []string{"send it to my sister", "telegram:4242", "anna.example@mail.test", "I sent your CV to Anna."} {
		if !strings.Contains(got, want) {
			t.Errorf("chat turn export is missing %q; content = %q", want, got)
		}
	}
	// A tool's RESULT is external data (an API response, another person's
	// mail) that the subject did not supply; it is withheld, while its
	// arguments, the subject's own instruction, are kept (security review
	// 2026-10-16).
	for _, gone := range []string{"carol.other@mail.test", "dave.other@mail.test"} {
		if strings.Contains(got, gone) {
			t.Errorf("chat turn export carries a tool result %q: %q", gone, got)
		}
	}
	if !strings.Contains(got, "[WITHHELD:tool_result]") {
		t.Errorf("chat turn export does not mark the withheld tool result: %q", got)
	}
	// A turn with no tool calls, no signals and no speaker exports exactly as
	// it did before 5d-E: an empty JSON default is not content.
	if got := byRow[plain].Content; got != "just a question" {
		t.Errorf("plain chat turn content = %q, want exactly %q", got, "just a question")
	}

	assertTaskMessageExport(t, byRow[msg].Content, byRow[bare].Content)
	transcriptExportRedactsSecrets(ctx, t, s, seed, subj, proj)
	transcriptExportFailsClosed(ctx, t, s, seed, subj, proj)
}

// transcriptExportFailsClosed pins the fail-closed rule (design §15.11.4a,
// security review 2026-10-16 on b720b4ad2): a JSON column that is not exactly
// one well-formed value of the writer's shape is withheld whole, so a second
// value, a trailing comma or a wrapper object cannot carry a secret-named
// value past the key-name rule.
func transcriptExportFailsClosed(ctx context.Context, t *testing.T, s DataSubjectStore, seed TranscriptSeeder, subj, proj string) {
	t.Helper()
	chat, msg := uniqueID("chat"), uniqueID("msg")
	if err := seed.SeedChatTurnFull(ctx, chat, proj, "", "near-json turn",
		`[{"name":"x","args":"{}"}] {"password":"SECRETINT-ONE"}`,
		`{"wrapped":[{"sentence":"SECRETINT-TWO"}]}`); err != nil {
		t.Fatalf("seed chat: %v", err)
	}
	if err := seed.SeedTaskMessage(ctx, msg, proj, "", "near-json message",
		// Postgres stores metadata as jsonb, which refuses near-JSON and folds
		// duplicate keys on insert, so the driver-neutral case is a valid JSON
		// value of the wrong shape; the near-JSON metadata cases are unit tests.
		`["SECRETINT-THREE"]`); err != nil {
		t.Fatalf("seed task message: %v", err)
	}
	links := []datasubject.Link{
		chatLink(chat, proj, datasubject.SourceOperatorAsserted, datasubject.ConfidenceCertain),
		{Table: datasubject.TableTaskMessages, RowID: msg, Source: datasubject.SourceOperatorAsserted,
			Confidence: datasubject.ConfidenceCertain, Exclusivity: datasubject.ExclusiveRow},
	}
	for _, l := range links {
		if err := s.AddLink(ctx, subj, l); err != nil {
			t.Fatalf("AddLink: %v", err)
		}
	}
	items, err := s.CollectItems(ctx, subj, links)
	if err != nil {
		t.Fatalf("CollectItems: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("collected %d items, want 2", len(items))
	}
	for _, it := range items {
		if strings.Contains(it.Content, "SECRETINT") {
			t.Errorf("%s export carries a near-JSON secret: %q", it.Table, it.Content)
		}
		if !strings.Contains(it.Content, "[WITHHELD:") {
			t.Errorf("%s export does not say a value was withheld: %q", it.Table, it.Content)
		}
	}
}

// assertTaskMessageExport pins the exported task_messages rows of
// RunDataSubjectTranscriptExportSuite: author and allowed metadata kept,
// unreviewed metadata withheld, a row with neither unchanged.
func assertTaskMessageExport(t *testing.T, got, bare string) {
	t.Helper()
	for _, want := range []string{"please use the new address", "tg:4242", "Main Street 5", "[WITHHELD:unreviewed_fields]"} {
		if !strings.Contains(got, want) {
			t.Errorf("task message export is missing %q; content = %q", want, got)
		}
	}
	// Metadata keys outside the reviewed allowlist (caller-supplied JSON, a
	// group chat id) may hold other people's data and are withheld by name.
	for _, gone := range []string{"erin.other@mail.test", "-100777"} {
		if strings.Contains(got, gone) {
			t.Errorf("task message export carries an unreviewed metadata value %q: %q", gone, got)
		}
	}
	if bare != "no metadata here" {
		t.Errorf("bare task message content = %q, want exactly %q", bare, "no metadata here")
	}
}

// transcriptExportRedactsSecrets pins the export-time secret scan over the JSON
// columns 5d-E added (design §15.11.4, security review 2026-10-16). The
// write-time chat-audit decorator redacts tool_calls_json only when a detector
// is wired, never touches hallucination_signals_json, and task_messages has no
// write-time scan at all, so a credential stored there would otherwise reach
// the export file verbatim. Shapes: a vendor-prefixed token (detector) and a
// credential under a secret-named JSON key (which the detector's key=value
// rule cannot see in JSON syntax).
func transcriptExportRedactsSecrets(ctx context.Context, t *testing.T, s DataSubjectStore, seed TranscriptSeeder, subj, proj string) {
	t.Helper()
	const (
		pat     = "ghp_0123456789abcdefghijABCDEFGHIJ012345"
		awsKey  = "AKIAABCDEFGHIJKLMNOP"
		keyed   = "plain-looking-value-1234"
		bearer  = "eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiJ4eXoifQ.c2lnbmF0dXJlLXZhbHVl"
		visible = "deploy the site"
	)
	chat, msg := uniqueID("chat"), uniqueID("msg")
	if err := seed.SeedChatTurnFull(ctx, chat, proj, "telegram:4242", visible,
		`[{"name":"set_secret","args":"{\"value\":\"`+pat+`\",\"password\":\"`+keyed+`\"}"}]`,
		`[{"detector":"x","claim_value":"`+awsKey+`","sentence":"your key is `+awsKey+`"}]`); err != nil {
		t.Fatalf("seed chat: %v", err)
	}
	if err := seed.SeedTaskMessage(ctx, msg, proj, "tg:4242", visible,
		`{"reason":"token `+pat+` and bearer `+bearer+`","reply_to_message_id":7}`); err != nil {
		t.Fatalf("seed task message: %v", err)
	}
	links := []datasubject.Link{
		chatLink(chat, proj, datasubject.SourceOperatorAsserted, datasubject.ConfidenceCertain),
		{Table: datasubject.TableTaskMessages, RowID: msg, Source: datasubject.SourceOperatorAsserted,
			Confidence: datasubject.ConfidenceCertain, Exclusivity: datasubject.ExclusiveRow},
	}
	for _, l := range links {
		if err := s.AddLink(ctx, subj, l); err != nil {
			t.Fatalf("AddLink: %v", err)
		}
	}
	items, err := s.CollectItems(ctx, subj, links)
	if err != nil {
		t.Fatalf("CollectItems: %v", err)
	}
	if len(items) != 2 {
		t.Fatalf("collected %d items, want 2", len(items))
	}
	for _, it := range items {
		for _, secret := range []string{pat, awsKey, keyed, bearer} {
			if strings.Contains(it.Content, secret) {
				t.Errorf("%s export carries the secret %q verbatim: %q", it.Table, secret, it.Content)
			}
		}
		if !strings.Contains(it.Content, "[REDACTED:") {
			t.Errorf("%s export has no redaction marker: %q", it.Table, it.Content)
		}
		if !strings.Contains(it.Content, visible) {
			t.Errorf("%s export lost the non-secret text: %q", it.Table, it.Content)
		}
	}
}
