package subjectstore

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"vornik.io/vornik/internal/datasubject"
	"vornik.io/vornik/internal/persistence"
)

// Repository persists the GDPR data-subject axis and the rights ledger, and
// collects the content an Art 15 export discloses — on either driver.
//
// see LLD § https://docs.vornik.io §4.19
type Repository struct {
	db *sql.DB
	d  Dialect
	// now is the clock every timestamp the repository writes comes from. The
	// statements bind it rather than calling NOW(): SQLite has no NOW(), and a
	// column default would store a different text form there.
	now func() time.Time
}

// New constructs the repository over db with the driver's dialect.
func New(db *sql.DB, d Dialect) *Repository {
	return &Repository{db: db, d: d, now: func() time.Time { return time.Now().UTC() }}
}

func (r *Repository) clock() any {
	if r.now == nil {
		return r.d.bindTime(time.Now().UTC())
	}
	return r.d.bindTime(r.now())
}

// subjectExists reports whether a subject row exists.
func (r *Repository) subjectExists(ctx context.Context, q persistence.DBTX, id string) (bool, error) {
	var one int
	err := q.QueryRowContext(ctx, `SELECT 1 FROM data_subjects WHERE id = $1`, id).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, r.d.mapErr(err)
	}
	return true, nil
}

// unknownSubject is the error an insert naming a missing subject returns —
// persistence.ErrNotFound, the answer the Postgres foreign key gave as 23503.
func unknownSubject(id string) error {
	return fmt.Errorf("subjectstore: data subject %q: %w", id, persistence.ErrNotFound)
}

// CreateSubject inserts a subject.
func (r *Repository) CreateSubject(ctx context.Context, s datasubject.Subject) error {
	now := r.clock()
	_, err := r.db.ExecContext(ctx,
		`INSERT INTO data_subjects (id, display_name, created_at, updated_at) VALUES ($1, $2, $3, $4)`,
		s.ID, s.DisplayName, now, now)
	return r.d.mapErr(err)
}

// GetSubject fetches one subject.
func (r *Repository) GetSubject(ctx context.Context, id string) (datasubject.Subject, error) {
	var s datasubject.Subject
	var state sql.NullString
	err := r.db.QueryRowContext(ctx,
		`SELECT id, display_name, request_state FROM data_subjects WHERE id = $1`, id).
		Scan(&s.ID, &s.DisplayName, &state)
	if err != nil {
		return datasubject.Subject{}, r.d.mapErr(err)
	}
	s.RequestState = state.String
	return s, nil
}

// DeleteSubject removes a subject together with its identifiers and links,
// because the index is itself personal data and must never outlive the subject
// it describes (design §8). It is REFUSED (ErrSubjectRetained) while a rights
// request or an Art 18 restriction names the subject: the ledger must keep
// resolving to a person (design §4.8, Art 17(3)(b)).
//
// The cascade and the refusal are stated here, in one transaction, rather than
// left to the schema: SQLite runs with foreign_keys(OFF), so on that driver the
// schema enforces neither (design §4.19.3). On Postgres the ON DELETE CASCADE /
// RESTRICT foreign keys remain as a second line; before 2026-10-13 a RESTRICT
// violation surfaced there as ErrNotFound, which misdescribed it.
//
// Used by KG adoption to remove the placeholder subject the chat memory-write
// path minted, once its links have moved to the identified person. Deleting a
// subject that still holds links would destroy the index rather than move it,
// so callers move first — see datasubject.KGResolver.adopt for the ordering.
func (r *Repository) DeleteSubject(ctx context.Context, id string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return r.d.mapErr(err)
	}
	defer func() { _ = tx.Rollback() }()
	var held int
	if err := tx.QueryRowContext(ctx, `
		SELECT (SELECT COUNT(*) FROM data_subject_requests WHERE subject_id = $1)
		     + (SELECT COUNT(*) FROM data_subject_restrictions WHERE subject_id = $1)`, id).Scan(&held); err != nil {
		return r.d.mapErr(err)
	}
	if held > 0 {
		return fmt.Errorf("%w: %s has %d ledger record(s)", ErrSubjectRetained, id, held)
	}
	for _, stmt := range []string{
		`DELETE FROM data_subject_identifiers WHERE subject_id = $1`,
		`DELETE FROM data_subject_links WHERE subject_id = $1`,
		`DELETE FROM data_subjects WHERE id = $1`,
	} {
		if _, err := tx.ExecContext(ctx, stmt, id); err != nil {
			return r.d.mapErr(err)
		}
	}
	return r.d.mapErr(tx.Commit())
}

// ReassignLinks moves every link from one subject to another, returning how
// many rows the source held.
//
// INSERT-then-DELETE rather than UPDATE, in one transaction. The primary key is
// (subject_id, table_name, row_id), so an UPDATE collides on any row the target
// already carries — and that collision is the COMMON case here, since the
// operator resolving a person often binds an entity whose chunks are already
// linked. ON CONFLICT DO NOTHING keeps the target's existing row (identical in
// everything but created_at) and the delete then clears the source either way.
//
// The count is the source's row total, not the insert's: a row that was already
// on the target is still one the target now covers, and reporting only the
// newly-inserted ones would understate what moved.
//
// An unknown target is refused (ErrNotFound) before anything moves: moving the
// links to no one would delete them (design §4.19.3).
func (r *Repository) ReassignLinks(ctx context.Context, fromSubjectID, toSubjectID string) (int, error) {
	if fromSubjectID == "" || toSubjectID == "" || fromSubjectID == toSubjectID {
		return 0, fmt.Errorf("datasubject: reassign needs two different subject ids")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, r.d.mapErr(err)
	}
	defer func() { _ = tx.Rollback() }()

	if ok, err := r.subjectExists(ctx, tx, toSubjectID); err != nil {
		return 0, err
	} else if !ok {
		return 0, unknownSubject(toSubjectID)
	}
	var n int
	if err := tx.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM data_subject_links WHERE subject_id = $1`, fromSubjectID).Scan(&n); err != nil {
		return 0, r.d.mapErr(err)
	}
	// The SELECT's WHERE is load-bearing on SQLite too: an INSERT ... SELECT
	// carrying an upsert clause must have one, or SQLite parses ON as a join.
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO data_subject_links
		    (subject_id, table_name, row_id, project_id, source, confidence, exclusivity, created_at)
		SELECT $2, table_name, row_id, project_id, source, confidence, exclusivity, created_at
		  FROM data_subject_links WHERE subject_id = $1
		ON CONFLICT (subject_id, table_name, row_id) DO NOTHING`,
		fromSubjectID, toSubjectID); err != nil {
		return 0, r.d.mapErr(err)
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM data_subject_links WHERE subject_id = $1`, fromSubjectID); err != nil {
		return 0, r.d.mapErr(err)
	}
	if err := tx.Commit(); err != nil {
		return 0, r.d.mapErr(err)
	}
	return n, nil
}

// ListSubjects returns subjects, newest first (id descending breaks a tie).
func (r *Repository) ListSubjects(ctx context.Context, limit int) ([]datasubject.Subject, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, display_name, COALESCE(request_state, '')
		   FROM data_subjects ORDER BY created_at DESC, id DESC LIMIT $1`, limit)
	if err != nil {
		return nil, r.d.mapErr(err)
	}
	defer func() { _ = rows.Close() }()
	var out []datasubject.Subject
	for rows.Next() {
		var s datasubject.Subject
		if err := rows.Scan(&s.ID, &s.DisplayName, &s.RequestState); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// withSubject runs fn in a transaction after confirming the subject exists,
// returning ErrNotFound (the answer the Postgres foreign key gave as 23503) when
// it does not. SQLite enforces no foreign key (foreign_keys OFF), so without
// this an identifier, link or request could name nobody — an index entry no
// export or erasure would ever reach. On SQLite the transaction is BEGIN
// IMMEDIATE (the DSN's _txlock), so the check and the write are atomic; on
// Postgres the foreign key still refuses a subject deleted in between.
func (r *Repository) withSubject(ctx context.Context, subjectID string, fn func(tx *sql.Tx) error) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return r.d.mapErr(err)
	}
	defer func() { _ = tx.Rollback() }()
	ok, err := r.subjectExists(ctx, tx, subjectID)
	if err != nil {
		return err
	}
	if !ok {
		return unknownSubject(subjectID)
	}
	if err := fn(tx); err != nil {
		return err
	}
	return r.d.mapErr(tx.Commit())
}

// AddIdentifier records an identifier. Idempotent: re-binding the same
// identifier is normal (the same person mails in twice), so a repeat is not an
// error. An unknown subject is ErrNotFound.
func (r *Repository) AddIdentifier(ctx context.Context, subjectID string, id datasubject.Identifier) error {
	return r.withSubject(ctx, subjectID, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO data_subject_identifiers (subject_id, kind, value, source, confidence, created_at)
			 VALUES ($1, $2, $3, $4, $5, $6)
			 ON CONFLICT (subject_id, kind, value) DO NOTHING`,
			subjectID, id.Kind, id.Value, string(id.Source), string(id.Confidence), r.clock())
		return r.d.mapErr(err)
	})
}

// ListIdentifiers returns a subject's identifiers.
func (r *Repository) ListIdentifiers(ctx context.Context, subjectID string) ([]datasubject.Identifier, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT kind, value, source, confidence FROM data_subject_identifiers
		  WHERE subject_id = $1 ORDER BY kind, value`, subjectID)
	if err != nil {
		return nil, r.d.mapErr(err)
	}
	defer func() { _ = rows.Close() }()
	var out []datasubject.Identifier
	for rows.Next() {
		var id datasubject.Identifier
		var src, conf string
		if err := rows.Scan(&id.Kind, &id.Value, &src, &conf); err != nil {
			return nil, err
		}
		id.Source, id.Confidence = datasubject.Source(src), datasubject.Confidence(conf)
		out = append(out, id)
	}
	return out, rows.Err()
}

// FindSubjectByIdentifier resolves an identifier to a subject id. Returns
// ("", nil) when unknown — an unrecognised handle is an ordinary outcome, not an
// error.
func (r *Repository) FindSubjectByIdentifier(ctx context.Context, kind, value string) (string, error) {
	var id string
	err := r.db.QueryRowContext(ctx,
		`SELECT subject_id FROM data_subject_identifiers WHERE kind = $1 AND value = $2 LIMIT 1`,
		kind, value).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", r.d.mapErr(err)
	}
	return id, nil
}

// AddLink records that a subject appears in a row.
//
// Validates through datasubject.Link so a link can never name a table outside
// the closed set — the same discipline the retention sweeper applies, and the
// reason an erasure cannot silently skip a table it does not know how to act on.
func (r *Repository) AddLink(ctx context.Context, subjectID string, l datasubject.Link) error {
	if err := l.Validate(); err != nil {
		return err
	}
	var project any
	if l.ProjectID != "" {
		project = l.ProjectID
	}
	return r.withSubject(ctx, subjectID, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO data_subject_links
			   (subject_id, table_name, row_id, project_id, source, confidence, exclusivity, created_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			 ON CONFLICT (subject_id, table_name, row_id) DO UPDATE
			   SET confidence  = EXCLUDED.confidence,
			       exclusivity = EXCLUDED.exclusivity`,
			subjectID, string(l.Table), l.RowID, project,
			string(l.Source), string(l.Confidence), string(l.Exclusivity), r.clock())
		return r.d.mapErr(err)
	})
}

// AddLinkIfAbsent inserts a link only when the subject has none to that row,
// reporting whether it did.
//
// The console's free-text confirm (GDPR design §4.17, GitHub #345) writes
// through this rather than AddLink: AddLink's upsert updates confidence but not
// source, so confirming a row a binder had linked meanwhile would leave a
// kg_extraction link claiming `probable` — above that source's ceiling. Never
// overwriting an existing link closes that race in the statement itself.
func (r *Repository) AddLinkIfAbsent(ctx context.Context, subjectID string, l datasubject.Link) (bool, error) {
	if err := l.Validate(); err != nil {
		return false, err
	}
	var project any
	if l.ProjectID != "" {
		project = l.ProjectID
	}
	var inserted bool
	err := r.withSubject(ctx, subjectID, func(tx *sql.Tx) error {
		res, err := tx.ExecContext(ctx,
			`INSERT INTO data_subject_links
			   (subject_id, table_name, row_id, project_id, source, confidence, exclusivity, created_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
			 ON CONFLICT (subject_id, table_name, row_id) DO NOTHING`,
			subjectID, string(l.Table), l.RowID, project,
			string(l.Source), string(l.Confidence), string(l.Exclusivity), r.clock())
		if err != nil {
			return r.d.mapErr(err)
		}
		n, err := res.RowsAffected()
		inserted = n == 1
		return r.d.mapErr(err)
	})
	return inserted, err
}

// DeleteOperatorLink removes this subject's link to one row when, and only
// when, an operator asserted it — the console's undo for a mistaken free-text
// confirm before an erasure acts on it (GDPR design §4.17, GitHub #345).
// Binder-derived links are not removable here: unbinding a knowledge-graph
// entity is a different operation. Reports whether a link was removed.
func (r *Repository) DeleteOperatorLink(ctx context.Context, subjectID, table, rowID string) (bool, error) {
	res, err := r.db.ExecContext(ctx,
		`DELETE FROM data_subject_links
		  WHERE subject_id = $1 AND table_name = $2 AND row_id = $3 AND source = $4`,
		subjectID, table, rowID, string(datasubject.SourceOperatorAsserted))
	if err != nil {
		return false, r.d.mapErr(err)
	}
	n, err := res.RowsAffected()
	return n == 1, r.d.mapErr(err)
}

// ListLinks returns a subject's links.
func (r *Repository) ListLinks(ctx context.Context, subjectID string) ([]datasubject.Link, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT table_name, row_id, COALESCE(project_id, ''), source, confidence, exclusivity
		   FROM data_subject_links WHERE subject_id = $1 ORDER BY table_name, row_id`, subjectID)
	if err != nil {
		return nil, r.d.mapErr(err)
	}
	defer func() { _ = rows.Close() }()
	var out []datasubject.Link
	for rows.Next() {
		var l datasubject.Link
		var table, src, conf, excl string
		if err := rows.Scan(&table, &l.RowID, &l.ProjectID, &src, &conf, &excl); err != nil {
			return nil, err
		}
		l.Table = datasubject.LinkableTable(table)
		l.Source, l.Confidence, l.Exclusivity =
			datasubject.Source(src), datasubject.Confidence(conf), datasubject.Exclusivity(excl)
		out = append(out, l)
	}
	return out, rows.Err()
}

// CountOtherSubjectsOnRow reports how many OTHER subjects are linked to the
// same row.
//
// This is what turns exclusivity from a claim into an observation: a row with
// another subject on it is shared regardless of what a binder guessed, and
// Art 15(4) turns on the answer.
func (r *Repository) CountOtherSubjectsOnRow(ctx context.Context, subjectID, table, rowID string) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM data_subject_links
		  WHERE table_name = $1 AND row_id = $2 AND subject_id <> $3`,
		table, rowID, subjectID).Scan(&n)
	return n, r.d.mapErr(err)
}

// --- rights ledger ---

// CreateRequest opens a request.
func (r *Repository) CreateRequest(ctx context.Context, req datasubject.Request) error {
	return r.withSubject(ctx, req.SubjectID, func(tx *sql.Tx) error {
		_, err := tx.ExecContext(ctx,
			`INSERT INTO data_subject_requests
			   (id, subject_id, kind, state, opened_at, erasure_ground, extended, updated_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			req.ID, req.SubjectID, string(req.Kind), string(req.State), r.d.bindTime(req.OpenedAt),
			nullIfEmpty(string(req.ErasureGround)), false, r.clock())
		return r.d.mapErr(err)
	})
}

// GetRequest fetches one request.
func (r *Repository) GetRequest(ctx context.Context, id string) (datasubject.Request, error) {
	return r.scanRequest(r.db.QueryRowContext(ctx,
		`SELECT id, subject_id, kind, state, opened_at, verified_by, verified_how, verified_at,
		        extended, extended_reason, report_hash, refused_reason, erasure_ground
		   FROM data_subject_requests WHERE id = $1`, id).Scan)
}

// SaveRequest persists a state transition.
func (r *Repository) SaveRequest(ctx context.Context, req datasubject.Request) error {
	_, err := r.db.ExecContext(ctx,
		`UPDATE data_subject_requests
		    SET state = $2, verified_by = $3, verified_how = $4, verified_at = $5,
		        extended = $6, extended_reason = $7, report_hash = $8, refused_reason = $9,
		        updated_at = $10
		  WHERE id = $1`,
		req.ID, string(req.State), nullIfEmpty(req.VerifiedBy), nullIfEmpty(req.VerifiedHow),
		r.d.bindTimeOrNull(req.VerifiedAt), req.Extended, nullIfEmpty(req.ExtendedReason),
		nullIfEmpty(req.ReportHash), nullIfEmpty(req.RefusedReason), r.clock())
	return r.d.mapErr(err)
}

// ListLiveRequests returns requests still awaiting a response, oldest first —
// the query the Art 12(3) deadline check runs.
func (r *Repository) ListLiveRequests(ctx context.Context) ([]datasubject.Request, error) {
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, subject_id, kind, state, opened_at, extended
		   FROM data_subject_requests
		  WHERE state IN ('open', 'verified')
		  ORDER BY opened_at`)
	if err != nil {
		return nil, r.d.mapErr(err)
	}
	defer func() { _ = rows.Close() }()
	var out []datasubject.Request
	for rows.Next() {
		var req datasubject.Request
		var kind, state string
		if err := rows.Scan(&req.ID, &req.SubjectID, &kind, &state, r.d.scanTime(&req.OpenedAt), &req.Extended); err != nil {
			return nil, err
		}
		req.Kind, req.State = datasubject.RequestKind(kind), datasubject.RequestState(state)
		out = append(out, req)
	}
	return out, rows.Err()
}

// maxRegulatoryPage caps one page of the regulatory record (design §6).
const maxRegulatoryPage = 200

func clampRegulatoryPage(limit, offset int) (int, int) {
	if limit <= 0 || limit > maxRegulatoryPage {
		limit = maxRegulatoryPage
	}
	if offset < 0 {
		offset = 0
	}
	return limit, offset
}

// ListRequests returns requests in EVERY state, newest first, id descending as
// the tiebreaker so offset paging is deterministic (regulatory record design
// §6). ListLiveRequests is the deadline query; this is the record.
func (r *Repository) ListRequests(ctx context.Context, limit, offset int) ([]datasubject.Request, error) {
	limit, offset = clampRegulatoryPage(limit, offset)
	rows, err := r.db.QueryContext(ctx,
		`SELECT id, subject_id, kind, state, opened_at, verified_by, verified_how, verified_at,
		        extended, extended_reason, report_hash, refused_reason, erasure_ground,
		        report_json IS NOT NULL
		   FROM data_subject_requests
		  ORDER BY opened_at DESC, id DESC
		  LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, r.d.mapErr(err)
	}
	defer func() { _ = rows.Close() }()
	var out []datasubject.Request
	for rows.Next() {
		var retained bool
		req, err := r.scanRequest(func(dest ...any) error { return rows.Scan(append(dest, &retained)...) })
		if err != nil {
			return nil, err
		}
		req.ReportRetained = retained
		out = append(out, req)
	}
	return out, rows.Err()
}

// GetRequestReport returns the retained report and its hash. Two misses, two
// errors (design §6, review F2): no row is persistence.ErrNotFound; a row with
// no retained report is persistence.ErrReportNotRetained (which also satisfies
// errors.Is(err, ErrNotFound)).
func (r *Repository) GetRequestReport(ctx context.Context, id string) (reportJSON, hash string, err error) {
	var body, h sql.NullString
	err = r.db.QueryRowContext(ctx,
		`SELECT report_json, report_hash FROM data_subject_requests WHERE id = $1`, id).Scan(&body, &h)
	if err != nil {
		return "", "", r.d.mapErr(err)
	}
	if !body.Valid || strings.TrimSpace(body.String) == "" {
		return "", "", persistence.ErrReportNotRetained
	}
	return body.String, h.String, nil
}

// scanRequest reads the full request column set GetRequest and ListRequests share.
func (r *Repository) scanRequest(scan func(...any) error) (datasubject.Request, error) {
	var req datasubject.Request
	var kind, state string
	var by, how, extReason, hash, refused, ground sql.NullString
	if err := scan(&req.ID, &req.SubjectID, &kind, &state, r.d.scanTime(&req.OpenedAt), &by, &how,
		r.d.scanTime(&req.VerifiedAt),
		&req.Extended, &extReason, &hash, &refused, &ground); err != nil {
		return datasubject.Request{}, r.d.mapErr(err)
	}
	req.Kind, req.State = datasubject.RequestKind(kind), datasubject.RequestState(state)
	req.VerifiedBy, req.VerifiedHow = by.String, how.String
	req.ExtendedReason, req.ReportHash, req.RefusedReason = extReason.String, hash.String, refused.String
	req.ErasureGround = datasubject.ErasureGround(ground.String)
	return req, nil
}

// --- content collection ---

// collectorSpec says how to read a linkable table's personal data.
//
// Table-driven rather than per-table code so a table cannot be silently
// forgotten: the specs are asserted against datasubject's closed set by
// TestEveryLinkableTableHasACollector. Column and table names come only from
// this map — never from a link row — so nothing user-influenced reaches SQL.
type collectorSpec struct {
	idCol       string
	contentCols []string
	// jsonCols are the content columns holding JSON whose empty default
	// ([], {}, null) is not content and is dropped like an empty string
	// (design 2026-07-29-art17-redact-and-reembed §15.11.4). channel_sessions
	// history is deliberately not one: its shipped export is unchanged.
	jsonCols   []string
	projectCol string // empty when the table is global
	// originNote describes where such a row comes from, answering Art 14(2)(f).
	originNote string
}

var collectorSpecs = map[datasubject.LinkableTable]collectorSpec{
	// The transcript tables export every column that holds conversation text or
	// who wrote it: the speaker, the tool-call arguments, the hallucination
	// signals quoting the reply, and the message metadata (§15.11.4, #344).
	datasubject.TableChatAuditLog: {
		idCol: "id",
		contentCols: []string{"user_message", "response", "user_id",
			"tool_calls_json", "hallucination_signals_json"},
		jsonCols:   []string{"tool_calls_json", "hallucination_signals_json"},
		projectCol: "project_id",
		originNote: "a chat turn you sent to this deployment",
	},
	datasubject.TableTaskMessages: {
		idCol: "id", contentCols: []string{"content", "author_id", "metadata"},
		jsonCols:   []string{"metadata"},
		originNote: "a message in a task conversation",
	},
	datasubject.TableProjectMemoryChunks: {
		idCol: "id", contentCols: []string{"content_title", "content"}, projectCol: "project_id",
		originNote: "long-term memory derived from correspondence, documents, or chat",
	},
	datasubject.TableArtifacts: {
		idCol: "id", contentCols: []string{"name"},
		originNote: "a file uploaded to this deployment",
	},
	datasubject.TableExtractedDocuments: {
		idCol: "id", contentCols: []string{"mime_type"}, projectCol: "project_id",
		originNote: "text extracted from an uploaded file",
	},
	// Keyed on (kind, session_id) by channelSessionKey, not on idCol alone: the
	// primary key is composite and a session id repeats across kinds. history is
	// the conversation itself (design 2026-07-29-art17-redact-and-reembed §15.10).
	datasubject.TableChannelSessions: {
		idCol: "session_id", contentCols: []string{"active_project", "history"},
		originNote: "the running history of a conversation with this deployment in a chat channel",
	},
	datasubject.TableKnowledgeEntities: {
		idCol: "id", contentCols: []string{"canonical_name", "description"}, projectCol: "project_id",
		originNote: "an entity extracted from ingested content",
	},
	datasubject.TableOperatorProfile: {
		idCol: "operator_id", contentCols: []string{"notes"},
		originNote: "the profile this assistant keeps about you",
	},
	datasubject.TableUserIdentities: {
		idCol: "id", contentCols: []string{"channel", "external_id", "display"},
		originNote: "your identity mapping for a channel",
	},
}

// CollectItems reads the content behind a subject's links.
//
// Exclusivity is RE-DERIVED here rather than trusted from the link row: if any
// other subject is linked to the same row it is shared, whatever a binder
// guessed. Art 15(4) turns on that answer, so it is observed at export time
// rather than inherited from an earlier assumption.
//
// A row that has since disappeared is skipped rather than reported — telling a
// subject about data that no longer exists would be its own inaccuracy.
func (r *Repository) CollectItems(ctx context.Context, subjectID string, links []datasubject.Link) ([]datasubject.Item, error) {
	out := make([]datasubject.Item, 0, len(links))
	for _, l := range links {
		spec, ok := collectorSpecs[l.Table]
		if !ok {
			// Unreachable while the collector test passes; refuse rather than
			// silently omit, so a new table cannot slip out of exports.
			return nil, fmt.Errorf("subjectstore: no collector for linkable table %q", l.Table)
		}
		cols := append([]string{}, spec.contentCols...)
		q, args, sessionKind, found, err := r.collectQuery(ctx, l, spec, cols)
		if err != nil {
			return nil, err
		}
		if !found {
			continue // row is gone; reporting it would be inaccurate
		}
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := r.db.QueryRowContext(ctx, q, args...).Scan(ptrs...); err != nil {
			if err == sql.ErrNoRows {
				continue // row is gone; reporting it would be inaccurate
			}
			return nil, fmt.Errorf("subjectstore: collect %s/%s: %w", l.Table, l.RowID, err)
		}
		parts := make([]string, 0, len(vals))
		for i, v := range vals {
			s, _ := v.(string)
			if strings.TrimSpace(s) == "" {
				continue
			}
			if slices.Contains(spec.jsonCols, cols[i]) {
				if emptyJSONValue(s) {
					continue
				}
				// These columns are not (all) scanned at write; the export is
				// the last point to keep a credential out (export_redact.go).
				s = redactExportJSON(cols[i], s)
			}
			parts = append(parts, s)
		}

		others, err := r.CountOtherSubjectsOnRow(ctx, subjectID, string(l.Table), l.RowID)
		if err != nil {
			return nil, err
		}
		excl := l.Exclusivity
		if others > 0 {
			excl = datasubject.SharedRow
		}
		// A session's history has no per-entry speaker, and every kind but web
		// chat is multi-party by construction, so it is shared under Art 15(4)
		// whatever the link says — the store cannot show it is one person's.
		if l.Table == datasubject.TableChannelSessions && !datasubject.ChannelSessionSingleSpeaker(sessionKind) {
			excl = datasubject.SharedRow
		}

		out = append(out, datasubject.Item{
			Table: l.Table, RowID: l.RowID, ProjectID: l.ProjectID,
			Source: l.Source, Confidence: l.Confidence, Exclusivity: excl,
			Content: strings.Join(parts, "\n"),
			Context: fmt.Sprintf("%s record %s", l.Table, l.RowID),
			Origin:  spec.originNote,
			// Only data the subject themselves supplied is portable under
			// Art 20; a derived summary is not, however much it is about them.
			ProvidedBySubject: l.Table == datasubject.TableChatAuditLog ||
				l.Table == datasubject.TableTaskMessages ||
				l.Table == datasubject.TableChannelSessions,
		})
	}
	return out, nil
}

// collectQuery builds the read for one link. found=false means the row the link
// names cannot exist (any more) and the link is skipped. Table and column names
// come only from collectorSpecs, never from the link.
func (r *Repository) collectQuery(ctx context.Context, l datasubject.Link, spec collectorSpec, cols []string) (q string, args []any, sessionKind string, found bool, err error) {
	if l.Table != datasubject.TableChannelSessions {
		return fmt.Sprintf(`SELECT %s FROM %s WHERE %s = $1`,
			strings.Join(coalesceAll(cols), ", "), l.Table, spec.idCol), []any{l.RowID}, "", true, nil
	}
	if !r.d.ChannelSessions {
		// The driver stores no channel sessions (SQLite: process memory only),
		// so the row this link names does not exist here — the same answer as a
		// vanished row (design §4.19.3, residual R1).
		return "", nil, "", false, nil
	}
	kind, sid, ok, err := r.channelSessionKey(ctx, l.RowID)
	if err != nil || !ok {
		return "", nil, "", false, err
	}
	return fmt.Sprintf(`SELECT %s FROM channel_sessions WHERE kind = $1 AND session_id = $2`,
		strings.Join(coalesceAll(cols), ", ")), []any{kind, sid}, kind, true, nil
}

// channelSessionKey resolves a channel_sessions link row id to its primary key.
//
// "<kind>/<session_id>" is the form Link.Validate enforces for new links. A
// legacy link holding a bare session_id is resolved only when exactly one row has
// that id; when several kinds share it the collection is REFUSED rather than one
// row picked silently, which is how the session_id-only lookup used to behave.
// found=false means no such row exists any more.
func (r *Repository) channelSessionKey(ctx context.Context, rowID string) (kind, sessionID string, found bool, err error) {
	if k, sid, perr := datasubject.ParseChannelSessionRowID(rowID); perr == nil {
		return k, sid, true, nil
	}
	rows, err := r.db.QueryContext(ctx,
		`SELECT kind FROM channel_sessions WHERE session_id = $1 ORDER BY kind LIMIT 2`, rowID)
	if err != nil {
		return "", "", false, fmt.Errorf("subjectstore: resolve channel session %q: %w", rowID, err)
	}
	defer func() { _ = rows.Close() }()
	var kinds []string
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return "", "", false, fmt.Errorf("subjectstore: resolve channel session %q: %w", rowID, err)
		}
		kinds = append(kinds, k)
	}
	if err := rows.Err(); err != nil {
		return "", "", false, fmt.Errorf("subjectstore: resolve channel session %q: %w", rowID, err)
	}
	switch len(kinds) {
	case 0:
		return "", "", false, nil
	case 1:
		return kinds[0], rowID, true, nil
	default:
		return "", "", false, fmt.Errorf("subjectstore: channel_sessions link %q is ambiguous: session id %q "+
			"exists under more than one kind (%s, ...) — re-link it as <kind>/<session_id>",
			rowID, rowID, strings.Join(kinds, ", "))
	}
}

// emptyJSONValue reports whether s is blank or a JSON empty array, empty object
// or null, judged by parsing rather than by literal match so "[ ]" counts too.
// Text that does not parse as JSON is content.
func emptyJSONValue(s string) bool {
	if strings.TrimSpace(s) == "" {
		return true
	}
	var v any
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return false
	}
	switch x := v.(type) {
	case nil:
		return true
	case []any:
		return len(x) == 0
	case map[string]any:
		return len(x) == 0
	}
	return false
}

// coalesceAll wraps each column so a NULL scans as an empty string rather than
// failing the whole collection.
func coalesceAll(cols []string) []string {
	out := make([]string, len(cols))
	for i, c := range cols {
		out[i] = fmt.Sprintf("COALESCE(CAST(%s AS TEXT), '')", c)
	}
	return out
}

func nullIfEmpty(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

// DeleteRow removes one row of a linkable table, together with every
// data_subject_link pointing at it, as part of an Art 17 erasure.
//
// The table and its id column come ONLY from collectorSpecs — the same closed map
// the export collector uses, never from a link row — so nothing user-influenced
// reaches SQL. Reusing that map rather than adding a parallel eraser map means
// TestEveryLinkableTableHasACollector guards erasure coverage too: a new linkable
// table cannot become erasable-but-uncollectable or the reverse.
//
// ARTIFACTS ARE REFUSED. `extracted_documents` has no foreign key on
// `source_artifact_id`, and `project_memory_chunks.artifact_id` is ON DELETE SET
// NULL, so deleting an artifact row orphans its extraction and leaves the derived
// embedding in the vector store while destroying the provenance that would let
// anyone find it. internal/erasure is the only correct path; the datasubject
// Executor routes artifacts there, and this refusal is the defence in depth for
// when something routes them wrongly.
//
// The links go with the row because a link asserts "this person appears in this
// row". Once the row is gone that assertion is itself stale personal data, and
// keeping it would retain a record of the person's presence after erasing the
// thing it referred to. The durable record of what was erased lives in
// data_subject_requests, which is declared retained under Art 17(3)(b).
func (r *Repository) DeleteRow(ctx context.Context, table datasubject.LinkableTable, rowID string) error {
	if table == datasubject.TableArtifacts {
		return fmt.Errorf("subjectstore: refusing to delete %s/%s as a plain row — "+
			"artifacts must go through the erasure cascade (internal/erasure), which also removes the "+
			"extraction rows, the derived memory chunks, and the on-disk storage directory",
			table, rowID)
	}
	if table == datasubject.TableChannelSessions {
		// Defence in depth (design 2026-07-29-art17-redact-and-reembed §15.10 item
		// 5): the running daemon holds the session in memory and saves it again on
		// the next message, so a stored-row delete is undone while reported done;
		// and session_id alone is not the key. The Executor defers these; nothing
		// may delete one as a plain row.
		return fmt.Errorf("subjectstore: refusing to delete %s/%s as a plain row — a channel session is "+
			"live conversation state the daemon would save again", table, rowID)
	}
	spec, ok := collectorSpecs[table]
	if !ok {
		return fmt.Errorf("subjectstore: no spec for linkable table %q — refusing to build a DELETE for it", table)
	}
	if strings.TrimSpace(rowID) == "" {
		return fmt.Errorf("subjectstore: row id is required to delete from %s", table)
	}
	if r == nil || r.db == nil {
		return fmt.Errorf("subjectstore: DeleteRow requires a database handle")
	}

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("subjectstore: begin erasure tx for %s/%s: %w", table, rowID, err)
	}
	defer func() { _ = tx.Rollback() }()

	// Slice 5c §4.4 retro-fit. A deleted memory chunk leaves its embedding_cache
	// row behind, keyed by the hash of the text we were asked to erase. That vector
	// is data DERIVED from the subject's personal data — the same reasoning that
	// makes the artifact cascade delete derived chunks — and nothing else evicts it:
	// the retention sweeper prunes by last_hit_at age and only when
	// retention.embedding_cache_days is set, which is time-based cold-entry pruning,
	// not compliance with an erasure request.
	//
	// Read the hash BEFORE the delete and evict in the SAME transaction, so there is
	// no window in which the chunk is gone but its vector is not, and no separate
	// call that can fail on its own and leave the pair inconsistent.
	//
	// This was a gap in code already shipped by 5b, not only in 5c.
	// The cache key is the hash of the CONTEXTUALISED embed input (source/section
	// prefix + content), NOT content_hash — see memory.EmbedInputHash. Evicting by
	// content_hash alone deleted nothing: measured on the live deployment, 0 of 500
	// sampled chunks had a cache row under their content_hash and 500 of 500 had one
	// under the embed-input hash. Both are evicted here because they coincide when the
	// prefix is empty, and evicting an absent key is free.
	//
	// A driver with no embedding cache (SQLite: no embedder runs there) has no
	// vector to evict, so the read is skipped rather than failing on a missing
	// table (design §4.19.3).
	var cacheKeys []string
	if table == datasubject.TableProjectMemoryChunks && r.d.EmbedCacheKey != nil {
		var recordedKey sql.NullString
		var sourceName, content, contentHash string
		switch err := tx.QueryRowContext(ctx,
			`SELECT embed_input_hash, source_name, content, content_hash
			   FROM project_memory_chunks WHERE id = $1`, rowID,
		).Scan(&recordedKey, &sourceName, &content, &contentHash); {
		case err == nil:
			// Recorded key first (migration 150) — it survives a change to the
			// contextualisation prefix, which a recomputed key does not. NULL means the
			// row predates the column, and recomputing is correct for those.
			for _, k := range []string{recordedKey.String, r.d.EmbedCacheKey(sourceName, content), contentHash} {
				if k == "" {
					continue
				}
				if !slices.Contains(cacheKeys, k) {
					cacheKeys = append(cacheKeys, k)
				}
			}
		case errors.Is(err, sql.ErrNoRows):
			// Already gone: erasure paths retry, so this is the expected second run.
		default:
			return fmt.Errorf("subjectstore: read cache keys before erasing %s/%s: %w", table, rowID, err)
		}
	}

	if _, err := tx.ExecContext(ctx,
		fmt.Sprintf(`DELETE FROM %s WHERE %s = $1`, table, spec.idCol), rowID); err != nil {
		return fmt.Errorf("subjectstore: delete %s/%s: %w", table, rowID, err)
	}
	for _, key := range cacheKeys {
		// EVERY model's vector for this key, not just the current one: the operator
		// may have re-embedded under several models over the deployment's life, and
		// erasing the text while leaving an older model's vector is not erasure.
		//
		// embedding_cache is keyed (content_hash, model) with no project column, so
		// this can evict an entry an identical chunk in ANOTHER project would have
		// hit. That is deliberate and cheap — the cost is one re-embed on the next
		// miss, against retaining a vector derived from erased data.
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM embedding_cache WHERE content_hash = $1`, key); err != nil {
			return fmt.Errorf("subjectstore: evict embedding cache for %s/%s: %w", table, rowID, err)
		}
	}
	// Every subject's link to this row, not just the requester's: the row is gone,
	// so any remaining link would point at nothing.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM data_subject_links WHERE table_name = $1 AND row_id = $2`,
		string(table), rowID); err != nil {
		return fmt.Errorf("subjectstore: delete links for %s/%s: %w", table, rowID, err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("subjectstore: commit erasure of %s/%s: %w", table, rowID, err)
	}
	return nil
}

// SaveRequestReport stores the rights-request report and its hash together.
//
// Together deliberately: report_hash is the Art 5(2) evidence that a right was
// honoured, and a hash without the document it fingerprints attests to nothing.
// Before this existed the hash was written to the ledger whenever a request was
// actioned while the report itself was saved only if the operator passed --out.
//
// This is also what makes deleting data_subject_links on erasure defensible. A
// link asserts "this person appears in this row" and becomes stale personal data
// once the row is gone, so it is removed — but the record of WHAT was erased has
// to survive somewhere, and it survives here: the report enumerates every table,
// row, disposition and reason, retained under Art 17(3)(b).
func (r *Repository) SaveRequestReport(ctx context.Context, requestID, reportJSON, hash string) error {
	if strings.TrimSpace(requestID) == "" {
		return fmt.Errorf("subjectstore: request id is required to store a report")
	}
	if strings.TrimSpace(reportJSON) == "" {
		return fmt.Errorf("subjectstore: refusing to record a report hash with no report body — " +
			"a fingerprint of an unsaved document is not accountability evidence")
	}
	if strings.TrimSpace(hash) == "" {
		return fmt.Errorf("subjectstore: refusing to store a report with no hash — nothing would pin it")
	}
	if r == nil || r.db == nil {
		return fmt.Errorf("subjectstore: SaveRequestReport requires a database handle")
	}
	_, err := r.db.ExecContext(ctx,
		`UPDATE data_subject_requests
		    SET report_json = $2, report_hash = $3, updated_at = $4
		  WHERE id = $1`, requestID, reportJSON, hash, r.clock())
	return r.d.mapErr(err)
}
