package cli

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"vornik.io/vornik/internal/auditredact"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/postgres"
	"vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/secrets"
)

// The historical half of the two transcript stores, chat_audit_log and
// task_messages (chat-audit retention and redaction design §10.2, GitHub
// #414), and of the third store of hallucination signals,
// execution_step_outcomes (§11.2.2, GitHub #425).
//
// A rescan applies the rules the live seam would apply to the row TODAY, by
// calling the seam's own walk (auditredact.Rescan*), not a copy of it here:
//
//   - chat_audit_log: user_message, response and tool_calls_json as raw text
//     (unchanged since §5); hallucination_signals_json with the signal rules
//     (the fragment rule, using this row's selected findings as known
//     matches, and the edge-run rule); chat_id and user_id with the identity
//     rule;
//   - task_messages: content with the full detector, metadata walked without
//     entropy plus the key-name rule, author_id with the identity rule;
//   - execution_step_outcomes: hallucination_signals with the signal rules and
//     no sources, as its live seam has none.
//
// Each reports the denominator per column — rows whose column held something
// to scan, and rows where it matched — so a store of 117,253 rows with 9
// signal values reads as 9 signal values examined, not 117,253 clean ones.
//
// The SQL here is driver-neutral and is tested on SQLite and Postgres; the
// command itself is still Postgres-only (design §10.2.2, open decision).

// scanEnv is a database for a scanner whose SQL runs on either driver.
type scanEnv struct {
	db *sql.DB
	// bindTime renders a timestamp the way the driver stores it.
	bindTime func(time.Time) any
	// bindJSON renders task_messages.metadata the way the driver stores it
	// (jsonb text on Postgres, a BLOB on SQLite).
	bindJSON func([]byte) any
	audit    persistence.SecretRedactionAuditRepository
}

func postgresScanEnv(db *sql.DB) scanEnv {
	return scanEnv{db: db,
		bindTime: func(t time.Time) any { return t },
		bindJSON: func(b []byte) any { return string(b) },
		audit:    postgres.NewSecretRedactionAuditRepository(db)}
}

func sqliteScanEnv(db *sql.DB) scanEnv {
	return scanEnv{db: db,
		bindTime: func(t time.Time) any { return persistence.SQLiteTime(t) },
		bindJSON: func(b []byte) any { return b },
		audit:    sqlite.NewSecretRedactionAuditRepository(db)}
}

// Column orders, for a stable report.
var (
	chatAuditColumns    = []string{"user_message", "response", "tool_calls_json", "hallucination_signals_json", "chat_id", "user_id"}
	taskMessagesColumns = []string{"content", "metadata", "author_id"}
)

// holdsValue reports whether a stored column has anything to scan. Empty JSON
// containers and null are nothing: counting them as examined would let a
// column that never held a value read as one that was scanned clean.
func holdsValue(s string) bool {
	switch strings.TrimSpace(s) {
	case "", "[]", "{}", "null":
		return false
	}
	return true
}

func newTranscriptResult(apply bool, sel ruleSelection, cols []string) scanHistoryResult {
	res := scanHistoryResult{
		Applied: apply, Rules: sel.spec,
		CountsByType: map[string]int{}, SelectedByType: map[string]int{}, ExcludedByType: map[string]int{},
		ExaminedByColumn: map[string]int{}, MatchedByColumn: map[string]int{}, columns: cols,
	}
	for _, c := range cols {
		res.ExaminedByColumn[c] = 0
		res.MatchedByColumn[c] = 0
	}
	return res
}

// countRescan folds one column's rescan into the run's histograms: everything
// found, split into what was written back and what the selection left.
func countRescan(res *scanHistoryResult, r auditredact.Rescan) {
	for ft, n := range r.Found {
		res.CountsByType[ft] += n
		red := r.Redacted[ft]
		if red > 0 {
			res.SelectedByType[ft] += red
		}
		if n > red {
			res.ExcludedByType[ft] += n - red
		}
	}
}

func addCounts(dst map[string]int, src map[string]int) {
	for k, n := range src {
		dst[k] += n
	}
}

// chatAuditHit is one chat_audit_log row queued for in-place redaction. A nil
// field is left as it is.
type chatAuditHit struct {
	id, projectID string
	cols          map[string]*string
	counts        map[string]int
}

// scanChatAuditHistory scans (and with apply=true redacts) chat_audit_log.
func scanChatAuditHistory(ctx context.Context, env scanEnv, detector secrets.Detector, project string, since time.Time, apply bool, sel ruleSelection, sampleN int) (scanHistoryResult, error) {
	res := newTranscriptResult(apply, sel, chatAuditColumns)
	salt := newRunSalt()
	sampled := map[string]int{}

	q := `SELECT id, project_id, chat_id, user_id, user_message, response, tool_calls_json,
	             COALESCE(hallucination_signals_json, '') FROM chat_audit_log WHERE 1=1`
	args := []any{}
	if project != "" {
		args = append(args, project)
		q += fmt.Sprintf(" AND project_id = $%d", len(args))
	}
	if !since.IsZero() {
		args = append(args, env.bindTime(since))
		q += fmt.Sprintf(" AND ts >= $%d", len(args))
	}
	q += " ORDER BY ts ASC, id ASC"

	rows, err := env.db.QueryContext(ctx, q, args...)
	if err != nil {
		return res, fmt.Errorf("query chat_audit_log: %w", err)
	}
	var hits []chatAuditHit
	func() {
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id, projectID, chatID, userID, userMsg, response, toolCalls, sigs string
			if err = rows.Scan(&id, &projectID, &chatID, &userID, &userMsg, &response, &toolCalls, &sigs); err != nil {
				err = fmt.Errorf("scan chat_audit_log row: %w", err)
				return
			}
			res.RowsScanned++
			if h, ok := scanChatRow(&res, detector, sel, salt, sampled, sampleN,
				id, projectID, chatID, userID, userMsg, response, toolCalls, sigs); ok {
				hits = append(hits, h)
			}
		}
		err = rows.Err()
	}()
	if err != nil {
		return res, err
	}
	if !apply {
		return res, nil
	}
	return res, applyChatAuditRedactions(ctx, env, hits)
}

// scanChatRow scans one chat_audit_log row and returns the rewrite it needs.
func scanChatRow(res *scanHistoryResult, detector secrets.Detector, sel ruleSelection, salt []byte, sampled map[string]int, sampleN int,
	id, projectID, chatID, userID, userMsg, response, toolCalls, sigs string) (chatAuditHit, bool) {
	raw := map[string]string{"user_message": userMsg, "response": response, "tool_calls_json": toolCalls,
		"hallucination_signals_json": sigs, "chat_id": chatID, "user_id": userID}
	for _, c := range chatAuditColumns {
		if holdsValue(raw[c]) {
			res.ExaminedByColumn[c]++
		}
	}
	h := chatAuditHit{id: id, projectID: projectID, cols: map[string]*string{}, counts: map[string]int{}}
	matched := false

	// The three free-text columns: raw text, as since §5.
	for _, c := range []string{"user_message", "response", "tool_calls_json"} {
		findings := detector.Scan([]byte(raw[c]))
		if len(findings) == 0 {
			continue
		}
		matched = true
		res.MatchedByColumn[c]++
		countBySelection(res, sel, findings, nil)
		if sampleN > 0 {
			collectSamples(res, salt, sampled, sampleN, sel, id, "chat", raw[c], findings)
		}
		if selected := selectFindings(findings, sel); len(selected) > 0 {
			out := string(secrets.Redact([]byte(raw[c]), selected))
			h.cols[c] = &out
			addCounts(h.counts, secrets.CountByType(selected))
		}
	}

	// The signals, with the seam's rules; the row's free text supplies the
	// known matches.
	sigOut, sigR := auditredact.RescanChatSignals(detector, sel.selects, []string{userMsg, response, toolCalls}, sigs)
	// The identity columns.
	chatOut, chatR := auditredact.RescanIdentity(detector, sel.selects, chatID)
	userOut, userR := auditredact.RescanIdentity(detector, sel.selects, userID)
	for _, part := range []struct {
		col     string
		in, out string
		r       auditredact.Rescan
	}{
		{"hallucination_signals_json", sigs, sigOut, sigR},
		{"chat_id", chatID, chatOut, chatR},
		{"user_id", userID, userOut, userR},
	} {
		if !part.r.Matched() {
			continue
		}
		matched = true
		res.MatchedByColumn[part.col]++
		countRescan(res, part.r)
		if sampleN > 0 {
			collectSamples(res, salt, sampled, sampleN, sel, id, "chat", part.in, detector.Scan([]byte(part.in)))
		}
		if part.out != part.in {
			out := part.out
			h.cols[part.col] = &out
			addCounts(h.counts, part.r.Redacted)
		}
	}
	if matched {
		res.RowsMatched++
	}
	return h, len(h.cols) > 0
}

// applyChatAuditRedactions rewrites each matched row in place and records its
// per-type counts to secret_redaction_audit (source="scan").
func applyChatAuditRedactions(ctx context.Context, env scanEnv, hits []chatAuditHit) error {
	for _, h := range hits {
		// COALESCE keeps a column that had nothing selected exactly as it is,
		// so one statement covers every combination.
		if _, err := env.db.ExecContext(ctx, `
			UPDATE chat_audit_log
			   SET user_message               = COALESCE($1, user_message),
			       response                   = COALESCE($2, response),
			       tool_calls_json            = COALESCE($3, tool_calls_json),
			       hallucination_signals_json = COALESCE($4, hallucination_signals_json),
			       chat_id                    = COALESCE($5, chat_id),
			       user_id                    = COALESCE($6, user_id)
			 WHERE id = $7`,
			nullableCol(h.cols["user_message"]), nullableCol(h.cols["response"]),
			nullableCol(h.cols["tool_calls_json"]), nullableCol(h.cols["hallucination_signals_json"]),
			nullableCol(h.cols["chat_id"]), nullableCol(h.cols["user_id"]),
			h.id); err != nil {
			return fmt.Errorf("redact chat_audit_log row %s: %w", h.id, err)
		}
		if err := recordScanEvents(ctx, env, h.projectID, "", h.counts); err != nil {
			return fmt.Errorf("record audit for chat row %s: %w", h.id, err)
		}
	}
	return nil
}

// taskMessageHit is one task_messages row queued for in-place redaction.
type taskMessageHit struct {
	id, taskID, projectID string
	content, author       *string
	metadata              []byte
	counts                map[string]int
}

// scanTaskMessagesHistory scans (and with apply=true redacts) task_messages.
//
// task_messages has no project_id. --project joins tasks; without it the join
// is a LEFT JOIN, so a message whose task is gone (SQLite ran without foreign
// keys until #361) is still examined.
func scanTaskMessagesHistory(ctx context.Context, env scanEnv, detector secrets.Detector, project string, since time.Time, apply bool, sel ruleSelection, sampleN int) (scanHistoryResult, error) {
	res := newTranscriptResult(apply, sel, taskMessagesColumns)
	salt := newRunSalt()
	sampled := map[string]int{}

	q := `SELECT tm.id, tm.task_id, COALESCE(t.project_id, ''), tm.content, tm.author_id, tm.metadata
	        FROM task_messages tm LEFT JOIN tasks t ON t.id = tm.task_id WHERE 1=1`
	args := []any{}
	if project != "" {
		args = append(args, project)
		q += fmt.Sprintf(" AND t.project_id = $%d", len(args))
	}
	if !since.IsZero() {
		args = append(args, env.bindTime(since))
		q += fmt.Sprintf(" AND tm.created_at >= $%d", len(args))
	}
	q += " ORDER BY tm.created_at ASC, tm.id ASC"

	rows, err := env.db.QueryContext(ctx, q, args...)
	if err != nil {
		return res, fmt.Errorf("query task_messages: %w", err)
	}
	var hits []taskMessageHit
	func() {
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id, taskID, projectID, content string
			var author sql.NullString
			var meta []byte
			if err = rows.Scan(&id, &taskID, &projectID, &content, &author, &meta); err != nil {
				err = fmt.Errorf("scan task_messages row: %w", err)
				return
			}
			res.RowsScanned++
			if h, ok := scanTaskMessageRow(&res, detector, sel, salt, sampled, sampleN,
				id, taskID, projectID, content, author, meta); ok {
				hits = append(hits, h)
			}
		}
		err = rows.Err()
	}()
	if err != nil {
		return res, err
	}
	if !apply {
		return res, nil
	}
	return res, applyTaskMessageRedactions(ctx, env, hits)
}

func scanTaskMessageRow(res *scanHistoryResult, detector secrets.Detector, sel ruleSelection, salt []byte, sampled map[string]int, sampleN int,
	id, taskID, projectID, content string, author sql.NullString, meta []byte) (taskMessageHit, bool) {
	if holdsValue(content) {
		res.ExaminedByColumn["content"]++
	}
	if holdsValue(string(meta)) {
		res.ExaminedByColumn["metadata"]++
	}
	if author.Valid && holdsValue(author.String) {
		res.ExaminedByColumn["author_id"]++
	}
	r := auditredact.RescanTaskMessage(detector, sel.selects, content, author.String, meta)
	h := taskMessageHit{id: id, taskID: taskID, projectID: projectID, counts: map[string]int{}}
	matched := false
	for _, part := range []struct {
		col  string
		in   string
		scan auditredact.Rescan
	}{
		{"content", content, r.ContentScan},
		{"metadata", string(meta), r.MetadataScan},
		{"author_id", author.String, r.AuthorScan},
	} {
		if !part.scan.Matched() {
			continue
		}
		matched = true
		res.MatchedByColumn[part.col]++
		countRescan(res, part.scan)
		addCounts(h.counts, part.scan.Redacted)
		if sampleN > 0 {
			collectSamples(res, salt, sampled, sampleN, sel, id, "task_message", part.in, detector.Scan([]byte(part.in)))
		}
	}
	if matched {
		res.RowsMatched++
	}
	if r.Content != content {
		c := r.Content
		h.content = &c
	}
	if author.Valid && r.AuthorID != author.String {
		a := r.AuthorID
		h.author = &a
	}
	if string(r.Metadata) != string(meta) {
		h.metadata = r.Metadata
	}
	return h, h.content != nil || h.author != nil || h.metadata != nil
}

func applyTaskMessageRedactions(ctx context.Context, env scanEnv, hits []taskMessageHit) error {
	for _, h := range hits {
		var meta any
		if h.metadata != nil {
			meta = env.bindJSON(h.metadata)
		}
		if _, err := env.db.ExecContext(ctx, `
			UPDATE task_messages
			   SET content   = COALESCE($1, content),
			       author_id = COALESCE($2, author_id),
			       metadata  = COALESCE($3, metadata)
			 WHERE id = $4`,
			nullableCol(h.content), nullableCol(h.author), meta, h.id); err != nil {
			return fmt.Errorf("redact task_messages row %s: %w", h.id, err)
		}
		if err := recordScanEvents(ctx, env, h.projectID, h.taskID, h.counts); err != nil {
			return fmt.Errorf("record audit for task message %s: %w", h.id, err)
		}
	}
	return nil
}

// stepOutcomeColumns is the one column of execution_step_outcomes that quotes
// model output (design §11.2.2, GitHub #425).
var stepOutcomeColumns = []string{"hallucination_signals"}

// stepOutcomeHit is one execution_step_outcomes row queued for rewrite.
type stepOutcomeHit struct {
	id, taskID, projectID string
	signals               []byte
	counts                map[string]int
}

// scanStepOutcomesHistory scans (and with apply=true redacts)
// execution_step_outcomes.hallucination_signals with the live seam's signal
// walk. The live seam has no other column to take known matches from, so the
// rescan passes no sources (design §11.2.2).
func scanStepOutcomesHistory(ctx context.Context, env scanEnv, detector secrets.Detector, project string, since time.Time, apply bool, sel ruleSelection, sampleN int) (scanHistoryResult, error) {
	res := newTranscriptResult(apply, sel, stepOutcomeColumns)
	salt := newRunSalt()
	sampled := map[string]int{}

	q := `SELECT id, task_id, project_id, hallucination_signals FROM execution_step_outcomes WHERE 1=1`
	args := []any{}
	if project != "" {
		args = append(args, project)
		q += fmt.Sprintf(" AND project_id = $%d", len(args))
	}
	if !since.IsZero() {
		args = append(args, env.bindTime(since))
		q += fmt.Sprintf(" AND recorded_at >= $%d", len(args))
	}
	q += " ORDER BY recorded_at ASC, id ASC"

	rows, err := env.db.QueryContext(ctx, q, args...)
	if err != nil {
		return res, fmt.Errorf("query execution_step_outcomes: %w", err)
	}
	var hits []stepOutcomeHit
	func() {
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id, taskID, projectID string
			var sigs []byte
			if err = rows.Scan(&id, &taskID, &projectID, &sigs); err != nil {
				err = fmt.Errorf("scan execution_step_outcomes row: %w", err)
				return
			}
			res.RowsScanned++
			raw := string(sigs)
			if !holdsValue(raw) {
				continue
			}
			res.ExaminedByColumn["hallucination_signals"]++
			out, r := auditredact.RescanChatSignals(detector, sel.selects, nil, raw)
			if !r.Matched() {
				continue
			}
			res.RowsMatched++
			res.MatchedByColumn["hallucination_signals"]++
			countRescan(&res, r)
			if sampleN > 0 {
				collectSamples(&res, salt, sampled, sampleN, sel, id, "step_outcome", raw, detector.Scan(sigs))
			}
			if out != raw {
				hits = append(hits, stepOutcomeHit{id: id, taskID: taskID, projectID: projectID,
					signals: []byte(out), counts: r.Redacted})
			}
		}
		err = rows.Err()
	}()
	if err != nil {
		return res, err
	}
	if !apply {
		return res, nil
	}
	for _, h := range hits {
		if _, err := env.db.ExecContext(ctx, `UPDATE execution_step_outcomes SET hallucination_signals = $1 WHERE id = $2`,
			env.bindJSON(h.signals), h.id); err != nil {
			return res, fmt.Errorf("redact execution_step_outcomes row %s: %w", h.id, err)
		}
		if err := recordScanEvents(ctx, env, h.projectID, h.taskID, h.counts); err != nil {
			return res, fmt.Errorf("record audit for step outcome %s: %w", h.id, err)
		}
	}
	return res, nil
}

// recordScanEvents writes one secret_redaction_audit row per finding type with
// source="scan".
func recordScanEvents(ctx context.Context, env scanEnv, projectID, taskID string, counts map[string]int) error {
	events := make([]persistence.SecretRedactionEvent, 0, len(counts))
	for ft, n := range counts {
		if n > 0 {
			events = append(events, persistence.SecretRedactionEvent{
				ProjectID: projectID, TaskID: taskID, Checkpoint: secrets.CheckpointToolAudit,
				FindingType: ft, Count: n, Source: "scan",
			})
		}
	}
	if len(events) == 0 {
		return nil
	}
	return env.audit.Record(ctx, events)
}

// nullableCol returns *v, or NULL when v is nil, so a single UPDATE with
// COALESCE leaves untouched columns untouched.
func nullableCol(v *string) any {
	if v == nil {
		return nil
	}
	return *v
}
