package persistence

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SQLMemoryVerificationRepository is the one implementation of
// MemoryVerificationRepository, for both drivers (design 22, amendment
// #482): the statements are portable ($N placeholders, which both drivers
// bind positionally, and a recursive CTE both support), and only the
// timestamp encoding differs, so a second copy of the SQL per driver would
// be two chances for the rule to drift. The driver packages construct it
// through postgres.NewMemoryVerificationRepository and
// sqlite.NewMemoryVerificationRepository.
type SQLMemoryVerificationRepository struct {
	db DBTX
	// timeArg encodes a timestamp for a bind: time.Time for Postgres, the
	// fixed-width SQLiteTimeLayout text for SQLite.
	timeArg func(time.Time) any
	now     func() time.Time
}

// NewSQLMemoryVerificationRepository builds the repository. sqliteTimes
// selects the SQLite timestamp encoding.
func NewSQLMemoryVerificationRepository(db DBTX, sqliteTimes bool) *SQLMemoryVerificationRepository {
	r := &SQLMemoryVerificationRepository{db: db, now: func() time.Time { return time.Now().UTC() }}
	if sqliteTimes {
		r.timeArg = func(t time.Time) any { return SQLiteTime(t) }
	} else {
		r.timeArg = func(t time.Time) any { return t.UTC() }
	}
	return r
}

var _ MemoryVerificationRepository = (*SQLMemoryVerificationRepository)(nil)

func (r *SQLMemoryVerificationRepository) timePtrArg(t *time.Time) any {
	if t == nil || t.IsZero() {
		return nil
	}
	return r.timeArg(*t)
}

// scanTimeValue reads a timestamp column whatever the driver returned.
func scanTimeValue(v any) (time.Time, bool, error) {
	switch x := v.(type) {
	case nil:
		return time.Time{}, false, nil
	case time.Time:
		return x.UTC(), true, nil
	case string:
		return parseTimeText(x)
	case []byte:
		return parseTimeText(string(x))
	}
	return time.Time{}, false, fmt.Errorf("unexpected timestamp type %T", v)
}

func parseTimeText(s string) (time.Time, bool, error) {
	if s == "" {
		return time.Time{}, false, nil
	}
	for _, layout := range []string{SQLiteTimeLayout, time.RFC3339Nano, "2006-01-02 15:04:05.999999999-07:00", "2006-01-02 15:04:05"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), true, nil
		}
	}
	return time.Time{}, false, fmt.Errorf("unparseable timestamp %q", s)
}

const provenanceColumns = `project_id, artifact_id, repo_scope, document_path, commit_sha,
	content_blob_sha, origin_blob_sha, state, reason, checked_at, verified_at, created_at`

func (r *SQLMemoryVerificationRepository) upsertProvenance(ctx context.Context, db DBTX, p *ChunkProvenance) error {
	if p == nil || p.ProjectID == "" || p.ArtifactID == "" {
		return errors.New("memory verification: provenance needs project_id and artifact_id")
	}
	now := r.now()
	if p.CheckedAt.IsZero() {
		p.CheckedAt = now
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO memory_chunk_provenance (`+provenanceColumns+`)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
		ON CONFLICT (project_id, artifact_id) DO UPDATE SET
			repo_scope       = excluded.repo_scope,
			document_path    = excluded.document_path,
			commit_sha       = excluded.commit_sha,
			content_blob_sha = excluded.content_blob_sha,
			origin_blob_sha  = excluded.origin_blob_sha,
			state            = excluded.state,
			reason           = excluded.reason,
			checked_at       = excluded.checked_at,
			verified_at      = excluded.verified_at`,
		p.ProjectID, p.ArtifactID, p.RepoScope, p.DocumentPath, p.CommitSHA,
		p.ContentBlobSHA, p.OriginBlobSHA, p.State, p.Reason,
		r.timeArg(p.CheckedAt), r.timePtrArg(p.VerifiedAt), r.timeArg(p.CreatedAt))
	if err != nil {
		return fmt.Errorf("memory verification: upsert provenance: %w", err)
	}
	return nil
}

// UpsertProvenance implements MemoryVerificationRepository.
func (r *SQLMemoryVerificationRepository) UpsertProvenance(ctx context.Context, p *ChunkProvenance) error {
	return r.upsertProvenance(ctx, r.db, p)
}

func scanProvenance(rows interface{ Scan(...any) error }) (*ChunkProvenance, error) {
	var p ChunkProvenance
	var checked, verified, created any
	if err := rows.Scan(&p.ProjectID, &p.ArtifactID, &p.RepoScope, &p.DocumentPath, &p.CommitSHA,
		&p.ContentBlobSHA, &p.OriginBlobSHA, &p.State, &p.Reason, &checked, &verified, &created); err != nil {
		return nil, err
	}
	var err error
	if p.CheckedAt, _, err = scanTimeValue(checked); err != nil {
		return nil, err
	}
	if t, ok, err := scanTimeValue(verified); err != nil {
		return nil, err
	} else if ok {
		p.VerifiedAt = &t
	}
	if p.CreatedAt, _, err = scanTimeValue(created); err != nil {
		return nil, err
	}
	return &p, nil
}

// GetProvenance implements MemoryVerificationRepository.
func (r *SQLMemoryVerificationRepository) GetProvenance(ctx context.Context, projectID, artifactID string) (*ChunkProvenance, error) {
	row := r.db.QueryRowContext(ctx, `SELECT `+provenanceColumns+` FROM memory_chunk_provenance
		WHERE project_id = $1 AND artifact_id = $2`, projectID, artifactID)
	p, err := scanProvenance(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("memory verification: get provenance: %w", err)
	}
	return p, nil
}

// ListProvenance implements MemoryVerificationRepository.
func (r *SQLMemoryVerificationRepository) ListProvenance(ctx context.Context, projectID, after string, limit int) ([]*ChunkProvenance, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := r.db.QueryContext(ctx, `SELECT `+provenanceColumns+` FROM memory_chunk_provenance
		WHERE project_id = $1 AND artifact_id > $2 ORDER BY artifact_id LIMIT $3`, projectID, after, limit)
	if err != nil {
		return nil, fmt.Errorf("memory verification: list provenance: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []*ChunkProvenance
	for rows.Next() {
		p, err := scanProvenance(rows)
		if err != nil {
			return nil, fmt.Errorf("memory verification: list provenance: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListProvenanceProjects implements MemoryVerificationRepository.
func (r *SQLMemoryVerificationRepository) ListProvenanceProjects(ctx context.Context) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT DISTINCT project_id FROM memory_chunk_provenance ORDER BY project_id`)
	if err != nil {
		return nil, fmt.Errorf("memory verification: list provenance projects: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// companionChainSQL walks each start task's parent chain. A row per visited
// task: the start id, the task's own parent, whether that parent row exists,
// its creation source, its depth, and whether the key that created it is
// untrusted (a companion key, an agent-admin or agent-namespace key, or a key
// id whose row cannot be read).
const companionChainSQL = `
WITH RECURSIVE chain(start_id, id, parent, source, key_id, actor, depth) AS (
    SELECT t.id, t.id, t.parent_task_id, CAST(t.creation_source AS TEXT), t.created_by_api_key_id,
           t.created_by_actor, 0
      FROM tasks t WHERE t.id IN (%s)
    UNION ALL
    SELECT c.start_id, p.id, p.parent_task_id, CAST(p.creation_source AS TEXT), p.created_by_api_key_id,
           p.created_by_actor, c.depth + 1
      FROM chain c JOIN tasks p ON p.id = c.parent
     WHERE c.depth < %d
)
SELECT c.start_id, c.parent, c.source, c.depth, COALESCE(c.actor, ''),
       CASE WHEN c.key_id IS NOT NULL AND c.key_id <> '' AND k.id IS NOT NULL
             AND COALESCE(k.client_kind, '') = '' AND NOT k.agent_admin
             AND COALESCE(k.agent_namespace, '') = ''
             AND k.name IS NOT NULL AND SUBSTR(k.name, 1, 6) <> 'agent:' THEN 1 ELSE 0 END AS key_plain,
       CASE WHEN c.parent IS NULL OR c.parent = '' THEN 1
            WHEN EXISTS (SELECT 1 FROM tasks x WHERE x.id = c.parent) THEN 1
            ELSE 0 END AS parent_ok,
       CASE WHEN c.key_id IS NULL OR c.key_id = '' THEN 0
            WHEN k.id IS NULL THEN 1
            WHEN COALESCE(k.client_kind, '') <> '' THEN 1
            WHEN k.agent_admin THEN 1
            WHEN COALESCE(k.agent_namespace, '') <> '' THEN 1
            WHEN k.name IS NULL OR SUBSTR(k.name, 1, 6) = 'agent:' THEN 1
            ELSE 0 END AS key_untrusted
  FROM chain c LEFT JOIN api_keys k ON k.id = c.key_id`

// trustedRootSources are the only creation sources a chain may start at
// and still count as not companion-delegated (security finding 2026-10-22,
// GitHub #482): an operator's own task and the autonomy loop. Every other
// source (COMPANION, A2A, SCHEDULED, which a broker schedule fire uses with
// the companion's key, and any source added later) is delegated. A derived
// source (DELEGATION, ROUTE, CHECKPOINT, FORK) is never a root: with no
// parent row it is an orphan whose origin is unknown.
var trustedRootSources = map[string]bool{
	string(TaskCreationSourceUser):       true,
	string(TaskCreationSourceAutonomous): true,
}

// CompanionDelegatedTasks implements MemoryVerificationRepository.
func (r *SQLMemoryVerificationRepository) CompanionDelegatedTasks(ctx context.Context, taskIDs []string) (map[string]bool, error) {
	out := make(map[string]bool, len(taskIDs))
	var ids []string
	seen := map[string]bool{}
	for _, id := range taskIDs {
		if id == "" {
			out[id] = true // no task: origin unknown, fail closed
			continue
		}
		if !seen[id] {
			seen[id] = true
			ids = append(ids, id)
		}
	}
	const batch = 200
	for start := 0; start < len(ids); start += batch {
		end := start + batch
		if end > len(ids) {
			end = len(ids)
		}
		got, err := r.companionChainBatch(ctx, ids[start:end])
		if err != nil {
			return nil, fmt.Errorf("memory verification: companion chain: %w", err)
		}
		for id, v := range got {
			out[id] = v
		}
	}
	return out, nil
}

// chainState accumulates one start task's walk.
type chainState struct{ found, companion, unknown bool }

// observe folds one visited task of the chain into the state.
func (s *chainState) observe(parent sql.NullString, source string, depth int, actor string, keyPlain, parentOK, keyUntrusted int) {
	s.found = true
	hasParent := parent.Valid && parent.String != ""
	switch {
	case source == string(TaskCreationSourceCompanion):
		s.companion = true
	case hasParent && parentOK == 0:
		s.unknown = true // the parent row is gone
	case hasParent && depth >= CompanionChainMaxDepth:
		s.unknown = true // over the cap, or a cycle
	case keyUntrusted != 0:
		s.unknown = true // created by a companion or agent-admin key, or an unreadable one
	case !hasParent && !trustedRootSources[source]:
		s.unknown = true // an unlisted root source, or an orphan child
	case !hasParent && !attributableOperator(source, actor, keyPlain != 0):
		s.unknown = true // a trusted source with no attributable operator principal
	}
}

// attributableOperator is the root's principal test (security finding
// 2026-10-22, GitHub #482: a root with no key id counted as the operator's,
// so a static key's task, the broker's or the A2A adapter's, or any path that
// stamps no principal, came out trusted). Trusted only: a USER task created
// in a session (`user:<id>`) or by a plain DB-backed project key, and an
// AUTONOMOUS task carrying the autonomy loop's own principal. A static key
// stamps `system:static_key:<hash>`, a webhook `system:webhook:<name>`;
// neither is trusted, and neither is a task with no principal at all.
//
// Amended 2026-10-22 (#499, operator decision A19): the operator's own
// admin-class static key, listed in admin.operator_key_ids, is stamped
// `system:operator_key:<16 hex>` and is trusted on a USER root; an
// admin-linked chat identity is stamped `user:<id>`. Agent per-task and
// warm-pool keys are never plain (key_plain excludes `agent:` names).
func attributableOperator(source, actor string, keyPlain bool) bool {
	switch source {
	case string(TaskCreationSourceUser):
		return keyPlain || (strings.HasPrefix(actor, "user:") && len(actor) > len("user:")) ||
			isOperatorKeyPrincipal(actor)
	case string(TaskCreationSourceAutonomous):
		return actor == "system:autonomy"
	}
	return false
}

// companionChainBatch answers one batch of distinct, non-empty task ids.
func (r *SQLMemoryVerificationRepository) companionChainBatch(ctx context.Context, part []string) (map[string]bool, error) {
	ph := make([]string, len(part))
	args := make([]any, len(part))
	for i, id := range part {
		ph[i] = fmt.Sprintf("$%d", i+1)
		args[i] = id
	}
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(companionChainSQL, strings.Join(ph, ","), CompanionChainMaxDepth), args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	st := map[string]*chainState{}
	for rows.Next() {
		var startID, source string
		var parent sql.NullString
		var depth, keyPlain, parentOK, keyUntrusted int
		var actor string
		if err := rows.Scan(&startID, &parent, &source, &depth, &actor, &keyPlain, &parentOK, &keyUntrusted); err != nil {
			return nil, err
		}
		s := st[startID]
		if s == nil {
			s = &chainState{}
			st[startID] = s
		}
		s.observe(parent, source, depth, actor, keyPlain, parentOK, keyUntrusted)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	out := make(map[string]bool, len(part))
	for _, id := range part {
		s := st[id]
		out[id] = s == nil || !s.found || s.companion || s.unknown
	}
	return out, nil
}

// roleOfRecordFilter selects the role-of-record stamp on an agent producer.
const roleOfRecordFilter = `c.validation_status = 'verified'
   AND c.validator_role IS NOT NULL AND c.validator_role <> ''
   AND c.validator_role = c.producer_role
   AND SUBSTR(c.producer_role, 1, 10) <> 'companion:'
   AND SUBSTR(c.producer_role, 1, 5) <> 'chat:'
   AND c.producer_role <> 'operator_correction'`

// ListRoleOfRecordCandidates implements MemoryVerificationRepository.
func (r *SQLMemoryVerificationRepository) ListRoleOfRecordCandidates(ctx context.Context, projectID, after string, limit int) ([]VerificationCandidate, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT c.artifact_id,
		       CASE WHEN COUNT(DISTINCT COALESCE(c.task_id, '')) = 1 THEN MAX(COALESCE(c.task_id, '')) ELSE '' END,
		       MAX(c.producer_role),
		       MAX(COALESCE(c.source_name, '')),
		       MAX(COALESCE(c.repo_scope, '')),
		       COUNT(*)
		  FROM project_memory_chunks c
		 WHERE c.project_id = $1 AND c.artifact_id IS NOT NULL AND c.artifact_id > $2
		   AND `+roleOfRecordFilter+`
		 GROUP BY c.artifact_id
		 ORDER BY c.artifact_id
		 LIMIT $3`, projectID, after, limit)
	if err != nil {
		return nil, fmt.Errorf("memory verification: list candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []VerificationCandidate
	for rows.Next() {
		var c VerificationCandidate
		if err := rows.Scan(&c.ArtifactID, &c.TaskID, &c.ProducerRole, &c.SourceName, &c.RepoScope, &c.Chunks); err != nil {
			return nil, fmt.Errorf("memory verification: list candidates: %w", err)
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ArtifactChunks implements MemoryVerificationRepository.
func (r *SQLMemoryVerificationRepository) ArtifactChunks(ctx context.Context, projectID, artifactID string, limit int) ([]VerificationChunk, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := r.db.QueryContext(ctx, `
		SELECT id, content, COALESCE(source_name, '') FROM project_memory_chunks
		 WHERE project_id = $1 AND artifact_id = $2
		   AND validation_status NOT IN ('superseded','refuted')
		   AND `+ChunkNotRestrictedSQL("project_memory_chunks.id")+`
		 ORDER BY id LIMIT $3`, projectID, artifactID, limit)
	if err != nil {
		return nil, fmt.Errorf("memory verification: artifact chunks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []VerificationChunk
	for rows.Next() {
		var c VerificationChunk
		if err := rows.Scan(&c.ID, &c.Content, &c.SourceName); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func (r *SQLMemoryVerificationRepository) insertEvent(ctx context.Context, db DBTX, ev *MemoryVerificationEvent) error {
	var restores any
	if ev.RestoresEventID != "" {
		restores = ev.RestoresEventID
	}
	_, err := db.ExecContext(ctx, `
		INSERT INTO memory_verification_events
			(id, project_id, kind, actor, reason, restores_event_id,
			 examined, unchanged, verified, deverified, skipped, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)`,
		ev.ID, ev.ProjectID, ev.Kind, ev.Actor, ev.Reason, restores,
		ev.Examined, ev.Unchanged, ev.Verified, ev.Deverified, ev.Skipped, r.timeArg(ev.CreatedAt))
	return err
}

func (r *SQLMemoryVerificationRepository) updateEventCounts(ctx context.Context, db DBTX, ev *MemoryVerificationEvent) error {
	_, err := db.ExecContext(ctx, `
		UPDATE memory_verification_events
		   SET verified = $2, deverified = $3, skipped = $4
		 WHERE id = $1`, ev.ID, ev.Verified, ev.Deverified, ev.Skipped)
	return err
}

func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// changeChunks backs up and changes the chunks one change selects, returning
// how many changed. The selection excludes terminal rows and rows already in
// the target state; OnlyValidatorRole narrows it further.
//
// The prior state is copied into the backup first, in the caller's
// transaction, and the update then touches exactly the rows the backup
// holds for this artifact: nothing changes without its prior state kept.
func (r *SQLMemoryVerificationRepository) changeChunks(ctx context.Context, db DBTX, eventID, projectID string, ch ArtifactVerificationChange) (int, error) {
	// $1 event, $2 target status, $3 target validator (NULL for none),
	// $4 project, $5 artifact, $6 the OnlyValidatorRole filter.
	args := []any{eventID, ch.NewStatus, nullIfEmpty(ch.NewValidatorRole), projectID, ch.ArtifactID}
	pred := `project_id = $4 AND artifact_id = $5 AND `
	switch ch.NewStatus {
	case "verified":
		pred += `validation_status NOT IN ('superseded','refuted')
		  AND NOT (validation_status = 'verified' AND COALESCE(validator_role, '') = COALESCE(CAST($3 AS TEXT), ''))`
		if ch.OnlyNeverStamped {
			pred += `
		  AND validation_status = 'unverified' AND validator_role IS NULL
		  AND NOT EXISTS (SELECT 1 FROM memory_verification_backup b WHERE b.chunk_id = project_memory_chunks.id)`
		}
	case "unverified":
		pred += `validation_status = 'verified'`
	default:
		return 0, fmt.Errorf("memory verification: unknown target status %q", ch.NewStatus)
	}
	backupArgs := args
	only := ch.OnlyValidatorRoles
	if ch.OnlyValidatorRole != "" {
		only = append([]string{ch.OnlyValidatorRole}, only...)
	}
	if len(only) > 0 {
		backupArgs = append([]any(nil), args...)
		ph := make([]string, len(only))
		for i, role := range only {
			ph[i] = fmt.Sprintf("$%d", 6+i)
			backupArgs = append(backupArgs, role)
		}
		pred += ` AND COALESCE(validator_role, '') IN (` + strings.Join(ph, ", ") + `)`
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO memory_verification_backup
			(event_id, chunk_id, project_id, prior_status, prior_validator_role, new_status, new_validator_role)
		SELECT CAST($1 AS TEXT), id, project_id, validation_status, validator_role,
		       CAST($2 AS TEXT), CAST($3 AS TEXT)
		  FROM project_memory_chunks
		 WHERE `+pred, backupArgs...); err != nil {
		return 0, fmt.Errorf("memory verification: back up chunks of %s: %w", ch.ArtifactID, err)
	}
	res, err := db.ExecContext(ctx, `
		UPDATE project_memory_chunks
		   SET validation_status = CAST($2 AS TEXT), validator_role = CAST($3 AS TEXT)
		 WHERE project_id = $4 AND artifact_id = $5
		   AND id IN (SELECT chunk_id FROM memory_verification_backup
		               WHERE event_id = $1 AND project_id = $4)`, args...)
	if err != nil {
		return 0, fmt.Errorf("memory verification: change chunks of %s: %w", ch.ArtifactID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("memory verification: count changed chunks of %s: %w", ch.ArtifactID, err)
	}
	return int(n), nil
}

// ApplyVerificationChange implements MemoryVerificationRepository.
func (r *SQLMemoryVerificationRepository) ApplyVerificationChange(ctx context.Context, ev *MemoryVerificationEvent, changes []ArtifactVerificationChange) error {
	if ev == nil || ev.ProjectID == "" || ev.Actor == "" || ev.Kind == "" {
		return errors.New("memory verification: an event needs project, kind and actor")
	}
	if ev.ID == "" {
		ev.ID = GenerateID("mverify")
	}
	if ev.CreatedAt.IsZero() {
		ev.CreatedAt = r.now()
	}
	ev.Verified, ev.Deverified = 0, 0
	tx, owned, err := BeginTx(ctx, r.db, nil)
	if err != nil {
		return fmt.Errorf("memory verification: begin: %w", err)
	}
	db := r.db
	if owned {
		db = tx
		defer func() { _ = tx.Rollback() }()
	}
	if err := r.insertEvent(ctx, db, ev); err != nil {
		return fmt.Errorf("memory verification: record event: %w", err)
	}
	seen := make(map[string]bool, len(changes))
	for _, ch := range changes {
		if ch.ArtifactID == "" {
			return errors.New("memory verification: a change needs an artifact id")
		}
		if seen[ch.ArtifactID] {
			return fmt.Errorf("memory verification: artifact %s changed twice in one event", ch.ArtifactID)
		}
		seen[ch.ArtifactID] = true
		if ch.Provenance != nil {
			ch.Provenance.ProjectID, ch.Provenance.ArtifactID = ev.ProjectID, ch.ArtifactID
			if err := r.upsertProvenance(ctx, db, ch.Provenance); err != nil {
				return err
			}
		}
		n, err := r.changeChunks(ctx, db, ev.ID, ev.ProjectID, ch)
		if err != nil {
			return err
		}
		if ch.NewStatus == "verified" {
			ev.Verified += n
		} else {
			ev.Deverified += n
		}
	}
	if err := r.updateEventCounts(ctx, db, ev); err != nil {
		return fmt.Errorf("memory verification: record counts: %w", err)
	}
	if owned {
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("memory verification: commit: %w", err)
		}
	}
	return nil
}

// GetVerificationEvent implements MemoryVerificationRepository.
func (r *SQLMemoryVerificationRepository) GetVerificationEvent(ctx context.Context, id string) (*MemoryVerificationEvent, error) {
	var ev MemoryVerificationEvent
	var restores sql.NullString
	var created any
	err := r.db.QueryRowContext(ctx, `
		SELECT id, project_id, kind, actor, reason, restores_event_id,
		       examined, unchanged, verified, deverified, skipped, created_at
		  FROM memory_verification_events WHERE id = $1`, id).Scan(
		&ev.ID, &ev.ProjectID, &ev.Kind, &ev.Actor, &ev.Reason, &restores,
		&ev.Examined, &ev.Unchanged, &ev.Verified, &ev.Deverified, &ev.Skipped, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("memory verification: get event: %w", err)
	}
	ev.RestoresEventID = restores.String
	if ev.CreatedAt, _, err = scanTimeValue(created); err != nil {
		return nil, err
	}
	return &ev, nil
}

// restorableSQL selects an event's backup rows whose chunk is still in the
// state the event left it in.
const restorableSQL = `
	FROM memory_verification_backup b
	JOIN project_memory_chunks c ON c.id = b.chunk_id AND c.project_id = b.project_id
	WHERE b.event_id = $1
	  AND c.validation_status = b.new_status
	  AND COALESCE(c.validator_role, '') = COALESCE(b.new_validator_role, '')`

// CountRestorableSelfCertified implements MemoryVerificationRepository.
func (r *SQLMemoryVerificationRepository) CountRestorableSelfCertified(ctx context.Context, eventID string) (int, error) {
	var n int
	err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) `+restorableSQL+`
	  AND b.prior_status = 'verified'
	  AND b.prior_validator_role IS NOT NULL
	  AND b.prior_validator_role = c.producer_role`, eventID).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("memory verification: count restorable: %w", err)
	}
	return n, nil
}

// RestoreVerificationEvent implements MemoryVerificationRepository.
func (r *SQLMemoryVerificationRepository) RestoreVerificationEvent(ctx context.Context, eventID, actor, reason string) (*MemoryVerificationEvent, error) {
	if actor == "" || strings.TrimSpace(reason) == "" {
		return nil, errors.New("memory verification: a restore needs an actor and a reason")
	}
	orig, err := r.GetVerificationEvent(ctx, eventID)
	if err != nil {
		return nil, err
	}
	if orig == nil {
		return nil, fmt.Errorf("memory verification: no event %q", eventID)
	}
	ev := &MemoryVerificationEvent{
		ID: GenerateID("mverify"), ProjectID: orig.ProjectID, Kind: MemoryVerificationKindRestore,
		Actor: actor, Reason: reason, RestoresEventID: eventID, CreatedAt: r.now(),
	}
	tx, owned, err := BeginTx(ctx, r.db, nil)
	if err != nil {
		return nil, fmt.Errorf("memory verification: begin: %w", err)
	}
	db := r.db
	if owned {
		db = tx
		defer func() { _ = tx.Rollback() }()
	}
	// The event is re-read inside the transaction (code review 690f F6): a
	// restore of an event deleted since the read above records nothing.
	var exists int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_verification_events WHERE id = $1`, eventID).Scan(&exists); err != nil {
		return nil, fmt.Errorf("memory verification: re-read event: %w", err)
	}
	if exists == 0 {
		return nil, fmt.Errorf("memory verification: no event %q", eventID)
	}
	var total int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_verification_backup WHERE event_id = $1`, eventID).Scan(&total); err != nil {
		return nil, fmt.Errorf("memory verification: count backup: %w", err)
	}
	ev.Examined = total
	if err := r.insertEvent(ctx, db, ev); err != nil {
		return nil, fmt.Errorf("memory verification: record restore event: %w", err)
	}
	// Back up what the restore changes, so the restore is itself recoverable.
	if _, err := db.ExecContext(ctx, `
		INSERT INTO memory_verification_backup
			(event_id, chunk_id, project_id, prior_status, prior_validator_role, new_status, new_validator_role)
		SELECT $2, c.id, c.project_id, c.validation_status, c.validator_role, b.prior_status, b.prior_validator_role
		`+restorableSQL, eventID, ev.ID); err != nil {
		return nil, fmt.Errorf("memory verification: back up restore: %w", err)
	}
	res, err := db.ExecContext(ctx, `
		UPDATE project_memory_chunks
		   SET validation_status = (SELECT b.prior_status FROM memory_verification_backup b
		                             WHERE b.event_id = $1 AND b.chunk_id = project_memory_chunks.id),
		       validator_role    = (SELECT b.prior_validator_role FROM memory_verification_backup b
		                             WHERE b.event_id = $1 AND b.chunk_id = project_memory_chunks.id)
		 WHERE id IN (SELECT chunk_id FROM memory_verification_backup WHERE event_id = $2)
		   AND project_id = $3`, eventID, ev.ID, orig.ProjectID)
	if err != nil {
		return nil, fmt.Errorf("memory verification: restore chunks: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, fmt.Errorf("memory verification: count restored chunks: %w", err)
	}
	var toVerified int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM memory_verification_backup
		WHERE event_id = $1 AND new_status = 'verified'`, ev.ID).Scan(&toVerified); err != nil {
		return nil, fmt.Errorf("memory verification: count restore: %w", err)
	}
	ev.Verified = toVerified
	ev.Deverified = int(n) - toVerified
	ev.Skipped = total - int(n)
	if err := r.updateEventCounts(ctx, db, ev); err != nil {
		return nil, fmt.Errorf("memory verification: record restore counts: %w", err)
	}
	if owned {
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("memory verification: commit restore: %w", err)
		}
	}
	return ev, nil
}
