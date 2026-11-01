package subjectstore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"vornik.io/vornik/internal/datasubject"
	"vornik.io/vornik/internal/persistence"
)

// The store side of slice 5d-1: erasing one participant from a shared
// transcript (GitHub #344). Every write is one row in one transaction, guarded
// on every content and routing column holding the value loaded and on no task
// in the row's subtree being live, both inside the same UPDATE. On Postgres
// the subtree's task rows are locked first, because under READ COMMITTED the
// UPDATE's subquery reads tasks at its own snapshot; SQLite's BEGIN IMMEDIATE
// already serialises every writer.
//
// see LLD § https://docs.vornik.io §15.14

var _ datasubject.TranscriptRedactor = (*Repository)(nil)

// TranscriptHooks are test seams that make the interleavings the design
// closes deterministic (§15.14 tests). Nil in production.
type TranscriptHooks struct {
	// Locked runs inside every guarded write's transaction (ApplyTurn, each
	// referrer re-key, ApplyPayload) after its lock, before its UPDATE.
	Locked func()
	// AfterOldPromptDelete runs after the old body's delete, before the
	// referrers are re-counted.
	AfterOldPromptDelete func()
}

// SetTranscriptHooks installs the test seams.
func (r *Repository) SetTranscriptHooks(h TranscriptHooks) { r.hooks = h }

func runHook(f func()) {
	if f != nil {
		f()
	}
}

// transcriptCol is one guarded column of a transcript table.
type transcriptCol struct {
	name string
	// json marks a jsonb column on Postgres (BLOB on SQLite), compared and
	// written through its text form.
	json bool
}

type transcriptSpec struct {
	table   string
	content []transcriptCol
	author  string
	// authorNullable: task_messages.author_id is nullable, and NULL and ''
	// both load as "", so the guard compares COALESCE(author, '') and an
	// empty author is written as NULL. chat_audit_log.user_id is NOT NULL.
	authorNullable bool
	chatID         string // "" when the table has none
	prompt         string // "" when the table has none
	taskID         string // the task column of a task message, "" otherwise
	sentAt         string
	thread         string // the column a thread is keyed on
	// anchor is the tasks column the subtree starts from: tasks.id = the
	// row's task_id, or tasks.chat_turn_id = the row's id.
	anchor string
}

var transcriptSpecs = map[datasubject.LinkableTable]transcriptSpec{
	datasubject.TableTaskMessages: {
		table:   "task_messages",
		content: []transcriptCol{{name: "content"}, {name: "metadata", json: true}},
		author:  "author_id", authorNullable: true, taskID: "task_id", sentAt: "created_at",
		thread: "task_id", anchor: "id",
	},
	datasubject.TableChatAuditLog: {
		table: "chat_audit_log",
		content: []transcriptCol{{name: "user_message"}, {name: "response"},
			{name: "tool_calls_json"}, {name: "hallucination_signals_json"}},
		author: "user_id", chatID: "chat_id", prompt: "system_prompt_hash", sentAt: "ts",
		thread: "chat_id", anchor: "chat_turn_id",
	},
}

func transcriptSpecFor(table datasubject.LinkableTable) (transcriptSpec, error) {
	s, ok := transcriptSpecs[table]
	if !ok {
		return transcriptSpec{}, fmt.Errorf("subjectstore: %q is not a transcript table", table)
	}
	return s, nil
}

// colText is a column read as text: a JSON column through its text form, so
// the loaded value and the guard compare the same rendering.
func (d Dialect) colText(c transcriptCol) string {
	if !c.json {
		return c.name
	}
	if d.JSONB {
		return c.name + "::text"
	}
	return "CAST(" + c.name + " AS TEXT)"
}

// colParam is a placeholder for a value written to c.
func (d Dialect) colParam(c transcriptCol, n int) string {
	if c.json && d.JSONB {
		return fmt.Sprintf("$%d::jsonb", n)
	}
	return fmt.Sprintf("$%d", n)
}

// nullSafeEq is the NULL-safe equality: Postgres IS NOT DISTINCT FROM,
// SQLite IS. A column loaded as NULL is bound as NULL, never as "", or a NULL
// row would fail its own guard and defer forever (review 0e4e F1).
func (d Dialect) nullSafeEq() string {
	if d.NullSafeEq != "" {
		return d.NullSafeEq
	}
	return "IS NOT DISTINCT FROM"
}

// args collects positional arguments and hands out their placeholders.
type args struct{ v []any }

func (a *args) add(x any) int { a.v = append(a.v, x); return len(a.v) }

func ptrArg(p *string) any {
	if p == nil {
		return nil
	}
	return *p
}

// subtreeCTE is the recursive set of task ids under an anchor, through
// parent_task_id. The anchor is a bound parameter, not a correlated
// reference: SQLite refuses a recursive CTE that names the outer row.
func subtreeCTE(anchorCol string, anchorParam int) string {
	return fmt.Sprintf(`WITH RECURSIVE sub(id) AS (
		SELECT id FROM tasks WHERE %s = $%d
		UNION
		SELECT c.id FROM tasks c JOIN sub ON c.parent_task_id = sub.id)`, anchorCol, anchorParam)
}

func statusList(a *args) string {
	live := datasubject.TranscriptLiveStatuses()
	ph := make([]string, len(live))
	for i, s := range live {
		ph[i] = fmt.Sprintf("$%d", a.add(s))
	}
	return strings.Join(ph, ", ")
}

// liveConjunct is "no task in the subtree is live", for an UPDATE's WHERE.
func liveConjunct(a *args, anchorCol string, anchor string) string {
	p := a.add(anchor)
	return fmt.Sprintf(`NOT EXISTS (%s SELECT 1 FROM tasks WHERE id IN (SELECT id FROM sub) AND status IN (%s))`,
		subtreeCTE(anchorCol, p), statusList(a))
}

// lockSubtree takes FOR UPDATE on the subtree's task rows (Postgres). It blocks
// a new child (its parent FK takes FOR KEY SHARE) and a status change until
// commit (review 0e4e F3).
func (r *Repository) lockSubtree(ctx context.Context, tx *sql.Tx, anchorCol, anchor string) error {
	if !r.d.RowLocks {
		return nil
	}
	rows, err := tx.QueryContext(ctx, subtreeCTE(anchorCol, 1)+
		` SELECT id FROM tasks WHERE id IN (SELECT id FROM sub) ORDER BY id FOR UPDATE`, anchor)
	if err != nil {
		return fmt.Errorf("subjectstore: lock task subtree: %w", r.d.mapErr(err))
	}
	return rows.Close()
}

// liveTask names a live task in the subtree, or "".
func (r *Repository) liveTask(ctx context.Context, q persistence.DBTX, anchorCol, anchor string) (string, error) {
	a := &args{}
	p := a.add(anchor)
	query := subtreeCTE(anchorCol, p) + fmt.Sprintf(
		` SELECT id FROM tasks WHERE id IN (SELECT id FROM sub) AND status IN (%s) ORDER BY id LIMIT 1`, statusList(a))
	var id string
	err := q.QueryRowContext(ctx, query, a.v...).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("subjectstore: read live task: %w", r.d.mapErr(err))
	}
	return id, nil
}

// LoadTurn reads one transcript row, with its prompt body when it has one.
func (r *Repository) LoadTurn(ctx context.Context, table datasubject.LinkableTable, rowID string) (datasubject.TranscriptTurn, error) {
	spec, err := transcriptSpecFor(table)
	if err != nil {
		return datasubject.TranscriptTurn{}, err
	}
	cols := make([]string, 0, len(spec.content)+5)
	for _, c := range spec.content {
		cols = append(cols, r.d.colText(c))
	}
	cols = append(cols, spec.author, spec.sentAt, spec.thread)
	if spec.chatID != "" {
		cols = append(cols, spec.chatID)
	}
	if spec.prompt != "" {
		cols = append(cols, spec.prompt)
	}
	if spec.taskID != "" {
		cols = append(cols, spec.taskID)
	}
	vals := make([]sql.NullString, len(spec.content))
	var author, thread, chatID, prompt, taskID sql.NullString
	turn := datasubject.TranscriptTurn{Table: table, RowID: rowID, Fields: map[string]*string{}}
	dst := make([]any, 0, len(cols))
	for i := range vals {
		dst = append(dst, &vals[i])
	}
	dst = append(dst, &author, r.d.scanTime(&turn.SentAt), &thread)
	if spec.chatID != "" {
		dst = append(dst, &chatID)
	}
	if spec.prompt != "" {
		dst = append(dst, &prompt)
	}
	if spec.taskID != "" {
		dst = append(dst, &taskID)
	}
	err = r.db.QueryRowContext(ctx, fmt.Sprintf(`SELECT %s FROM %s WHERE id = $1`,
		strings.Join(cols, ", "), spec.table), rowID).Scan(dst...)
	if err != nil {
		return datasubject.TranscriptTurn{}, fmt.Errorf("subjectstore: load %s/%s: %w", table, rowID, r.d.mapErr(err))
	}
	for i, c := range spec.content {
		if vals[i].Valid {
			v := vals[i].String
			turn.Fields[c.name] = &v
		} else {
			turn.Fields[c.name] = nil
		}
	}
	turn.Author, turn.ThreadKey, turn.ChatID = author.String, thread.String, chatID.String
	turn.PromptHash, turn.TaskID = prompt.String, taskID.String
	if turn.PromptHash != "" {
		var body string
		switch err := r.db.QueryRowContext(ctx, `SELECT body FROM chat_system_prompts WHERE hash = $1`,
			turn.PromptHash).Scan(&body); {
		case errors.Is(err, sql.ErrNoRows):
			// A hash with no body: the dispatcher's save is best-effort.
		case err != nil:
			return datasubject.TranscriptTurn{}, fmt.Errorf("subjectstore: load prompt of %s: %w", rowID, r.d.mapErr(err))
		default:
			turn.PromptBody = &body
		}
	}
	return turn, nil
}

// ThreadAuthors lists the thread's distinct non-empty authors by first turn.
func (r *Repository) ThreadAuthors(ctx context.Context, table datasubject.LinkableTable, threadKey string) ([]datasubject.ThreadAuthor, error) {
	spec, err := transcriptSpecFor(table)
	if err != nil {
		return nil, err
	}
	if threadKey == "" {
		return nil, nil
	}
	// The first turn of each author: its earliest time, then its smallest id
	// at that time.
	rows, err := r.db.QueryContext(ctx, fmt.Sprintf(`
		SELECT t.%[1]s, t.%[2]s, MIN(t.id) FROM %[3]s t
		JOIN (SELECT %[1]s AS a, MIN(%[2]s) AS first FROM %[3]s
		      WHERE %[4]s = $1 AND %[1]s IS NOT NULL AND %[1]s <> '' GROUP BY %[1]s) f
		  ON t.%[1]s = f.a AND t.%[2]s = f.first
		WHERE t.%[4]s = $1
		GROUP BY t.%[1]s, t.%[2]s`, spec.author, spec.sentAt, spec.table, spec.thread), threadKey)
	if err != nil {
		return nil, fmt.Errorf("subjectstore: thread authors: %w", r.d.mapErr(err))
	}
	defer func() { _ = rows.Close() }()
	var out []datasubject.ThreadAuthor
	for rows.Next() {
		var a datasubject.ThreadAuthor
		if err := rows.Scan(&a.Author, r.d.scanTime(&a.FirstAt), &a.FirstID); err != nil {
			return nil, fmt.Errorf("subjectstore: scan thread author: %w", err)
		}
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sortThreadAuthors(out)
	return out, nil
}

func sortThreadAuthors(a []datasubject.ThreadAuthor) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && threadAuthorLess(a[j], a[j-1]); j-- {
			a[j], a[j-1] = a[j-1], a[j]
		}
	}
}

func threadAuthorLess(x, y datasubject.ThreadAuthor) bool {
	if !x.FirstAt.Equal(y.FirstAt) {
		return x.FirstAt.Before(y.FirstAt)
	}
	return x.FirstID < y.FirstID
}

func (s transcriptSpec) anchorValue(t datasubject.TranscriptTurn) string {
	if s.anchor == "id" {
		return t.TaskID
	}
	return t.RowID
}

// ApplyTurn writes w over prior, guarded, in one transaction; then moves the
// other referrers of the old prompt body, each in its own.
func (r *Repository) ApplyTurn(ctx context.Context, prior datasubject.TranscriptTurn, w datasubject.TurnWrite) (datasubject.TurnResult, error) {
	spec, err := transcriptSpecFor(prior.Table)
	if err != nil {
		return datasubject.TurnResult{}, err
	}
	if w.Prompt != nil && (spec.prompt == "" || w.Prompt.OldHash != prior.PromptHash || prior.PromptHash == "") {
		return datasubject.TurnResult{}, fmt.Errorf("subjectstore: prompt re-key of %s/%s does not match the loaded prompt", prior.Table, prior.RowID)
	}
	if w.ChatID != nil && spec.chatID == "" {
		return datasubject.TurnResult{}, fmt.Errorf("subjectstore: %s has no chat id", prior.Table)
	}
	anchor := spec.anchorValue(prior)

	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return datasubject.TurnResult{}, fmt.Errorf("subjectstore: begin transcript tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := r.lockSubtree(ctx, tx, spec.anchor, anchor); err != nil {
		return datasubject.TurnResult{}, err
	}
	runHook(r.hooks.Locked)

	var res datasubject.TurnResult
	a := &args{}
	if w.Prompt != nil {
		res.NewPromptHash = persistence.HashChatSystemPrompt(w.Prompt.NewBody)
		if _, err := tx.ExecContext(ctx, `INSERT INTO chat_system_prompts (hash, body, created_at)
			VALUES ($1, $2, $3) ON CONFLICT (hash) DO NOTHING`,
			res.NewPromptHash, w.Prompt.NewBody, r.clock()); err != nil {
			return datasubject.TurnResult{}, fmt.Errorf("subjectstore: save masked prompt: %w", r.d.mapErr(err))
		}
	}
	set, err := r.turnSet(spec, prior, w, res.NewPromptHash, a)
	if err != nil {
		return datasubject.TurnResult{}, err
	}
	guard := r.turnGuard(spec, prior, a)
	idP := a.add(prior.RowID)
	guard = append(guard, liveConjunct(a, spec.anchor, anchor))
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE %s SET %s WHERE id = $%d AND %s`,
		spec.table, strings.Join(set, ", "), idP, strings.Join(guard, " AND ")), a.v...)
	if err != nil {
		return datasubject.TurnResult{}, fmt.Errorf("subjectstore: write %s/%s: %w", prior.Table, prior.RowID, r.d.mapErr(err))
	}
	n, err := result.RowsAffected()
	if err != nil {
		return datasubject.TurnResult{}, err
	}
	if n == 0 {
		// Nothing written. Tell a live subtree from a changed row.
		live, err := r.liveTask(ctx, tx, spec.anchor, anchor)
		if err != nil {
			return datasubject.TurnResult{}, err
		}
		if live != "" {
			return datasubject.TurnResult{Outcome: datasubject.TranscriptLive, LiveTaskID: live}, nil
		}
		return datasubject.TurnResult{Outcome: datasubject.TranscriptVersionChanged}, nil
	}
	if err := tx.Commit(); err != nil {
		return datasubject.TurnResult{}, fmt.Errorf("subjectstore: commit %s/%s: %w", prior.Table, prior.RowID, r.d.mapErr(err))
	}
	res.Outcome = datasubject.TranscriptApplied
	if w.Prompt != nil && res.NewPromptHash != w.Prompt.OldHash {
		if err := r.rekeyReferrers(ctx, w.Prompt.OldHash, res.NewPromptHash, &res); err != nil {
			return res, err
		}
	}
	return res, nil
}

// turnSet is the SET list of a turn write: every content column, the author,
// and the chat id and prompt hash when they move.
func (r *Repository) turnSet(spec transcriptSpec, prior datasubject.TranscriptTurn, w datasubject.TurnWrite, newPrompt string, a *args) ([]string, error) {
	var set []string
	for _, c := range spec.content {
		v, ok := w.Fields[c.name]
		if !ok {
			return nil, fmt.Errorf("subjectstore: write of %s/%s lacks column %s", prior.Table, prior.RowID, c.name)
		}
		set = append(set, fmt.Sprintf("%s = %s", c.name, r.d.colParam(c, a.add(ptrArg(v)))))
	}
	set = append(set, fmt.Sprintf("%s = $%d", spec.author, a.add(spec.authorArg(w.Author))))
	if w.ChatID != nil {
		set = append(set, fmt.Sprintf("%s = $%d", spec.chatID, a.add(*w.ChatID)))
	}
	if newPrompt != "" {
		set = append(set, fmt.Sprintf("%s = $%d", spec.prompt, a.add(newPrompt)))
	}
	return set, nil
}

// turnGuard compares every content and routing column with the value loaded.
func (r *Repository) turnGuard(spec transcriptSpec, prior datasubject.TranscriptTurn, a *args) []string {
	eq := r.d.nullSafeEq()
	var guard []string
	for _, c := range spec.content {
		guard = append(guard, fmt.Sprintf("%s %s $%d", r.d.colText(c), eq, a.add(ptrArg(prior.Fields[c.name]))))
	}
	if spec.authorNullable {
		guard = append(guard, fmt.Sprintf("COALESCE(%s, '') = $%d", spec.author, a.add(prior.Author)))
	} else {
		guard = append(guard, fmt.Sprintf("%s = $%d", spec.author, a.add(prior.Author)))
	}
	if spec.chatID != "" {
		guard = append(guard, fmt.Sprintf("%s = $%d", spec.chatID, a.add(prior.ChatID)))
	}
	if spec.prompt != "" {
		guard = append(guard, fmt.Sprintf("%s = $%d", spec.prompt, a.add(prior.PromptHash)))
	}
	if spec.taskID != "" {
		guard = append(guard, fmt.Sprintf("%s = $%d", spec.taskID, a.add(prior.TaskID)))
	}
	return guard
}

func (s transcriptSpec) authorArg(author string) any {
	if author == "" && s.authorNullable {
		return nil
	}
	return author
}

// rekeyReferrers repoints every other row on the old prompt body, one row per
// transaction under its own subtree lock (review e671 M1), deletes the old
// body once nothing refers to it, and re-counts the referrers afterwards: a
// row that arrived during the erasure keeps the old body and is reported
// (review 0e4e F4).
func (r *Repository) rekeyReferrers(ctx context.Context, oldHash, newHash string, res *datasubject.TurnResult) error {
	refs, err := r.promptReferrers(ctx, oldHash)
	if err != nil {
		return err
	}
	known := map[string]bool{}
	for _, ref := range refs {
		known[ref] = true
		moved, live, err := r.repointReferrer(ctx, ref, oldHash, newHash)
		if err != nil {
			return err
		}
		switch {
		case moved:
			res.Repointed++
		case live != "":
			res.DeferredReferrers = append(res.DeferredReferrers, datasubject.ReferrerDeferral{RowID: ref, LiveTaskID: live})
		}
	}
	result, err := r.db.ExecContext(ctx, `DELETE FROM chat_system_prompts WHERE hash = $1
		AND NOT EXISTS (SELECT 1 FROM chat_audit_log WHERE system_prompt_hash = $1)`, oldHash)
	if err != nil {
		return fmt.Errorf("subjectstore: delete old prompt body: %w", r.d.mapErr(err))
	}
	if n, _ := result.RowsAffected(); n > 0 {
		res.OldPromptDeleted = true
	}
	runHook(r.hooks.AfterOldPromptDelete)
	after, err := r.promptReferrers(ctx, oldHash)
	if err != nil {
		return err
	}
	for _, ref := range after {
		if !known[ref] {
			res.DeferredReferrers = append(res.DeferredReferrers, datasubject.ReferrerDeferral{RowID: ref, Arrived: true})
		}
	}
	return nil
}

func (r *Repository) promptReferrers(ctx context.Context, hash string) ([]string, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id FROM chat_audit_log WHERE system_prompt_hash = $1 ORDER BY id`, hash)
	if err != nil {
		return nil, fmt.Errorf("subjectstore: list prompt referrers: %w", r.d.mapErr(err))
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func (r *Repository) repointReferrer(ctx context.Context, rowID, oldHash, newHash string) (moved bool, live string, err error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return false, "", fmt.Errorf("subjectstore: begin re-key tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := r.lockSubtree(ctx, tx, "chat_turn_id", rowID); err != nil {
		return false, "", err
	}
	runHook(r.hooks.Locked)
	a := &args{}
	newP, idP, oldP := a.add(newHash), a.add(rowID), a.add(oldHash)
	result, err := tx.ExecContext(ctx, fmt.Sprintf(
		`UPDATE chat_audit_log SET system_prompt_hash = $%d WHERE id = $%d AND system_prompt_hash = $%d AND %s`,
		newP, idP, oldP, liveConjunct(a, "chat_turn_id", rowID)), a.v...)
	if err != nil {
		return false, "", fmt.Errorf("subjectstore: re-key %s: %w", rowID, r.d.mapErr(err))
	}
	if n, _ := result.RowsAffected(); n == 0 {
		live, err := r.liveTask(ctx, tx, "chat_turn_id", rowID)
		return false, live, err
	}
	if err := tx.Commit(); err != nil {
		return false, "", fmt.Errorf("subjectstore: commit re-key %s: %w", rowID, r.d.mapErr(err))
	}
	return true, "", nil
}

// payloadCol is tasks.payload: jsonb on Postgres, BLOB on SQLite.
var payloadCol = transcriptCol{name: "payload", json: true}

// OriginPayloads lists the subtree anchored on tasks.chat_turn_id.
func (r *Repository) OriginPayloads(ctx context.Context, chatTurnID string) ([]datasubject.TaskPayload, error) {
	rows, err := r.db.QueryContext(ctx, subtreeCTE("chat_turn_id", 1)+fmt.Sprintf(
		` SELECT id, status, %s FROM tasks WHERE id IN (SELECT id FROM sub) ORDER BY id`, r.d.colText(payloadCol)), chatTurnID)
	if err != nil {
		return nil, fmt.Errorf("subjectstore: origin payloads: %w", r.d.mapErr(err))
	}
	defer func() { _ = rows.Close() }()
	var out []datasubject.TaskPayload
	for rows.Next() {
		var p datasubject.TaskPayload
		var status string
		var payload sql.NullString
		if err := rows.Scan(&p.TaskID, &status, &payload); err != nil {
			return nil, err
		}
		p.Status = persistence.TaskStatus(status)
		if payload.Valid {
			v := payload.String
			p.Payload = &v
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ApplyPayload replaces one task's payload, guarded, in its own transaction.
func (r *Repository) ApplyPayload(ctx context.Context, prior datasubject.TaskPayload, payload string) (datasubject.TurnResult, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return datasubject.TurnResult{}, fmt.Errorf("subjectstore: begin payload tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	// The task's own row and status, as §15.14 states: a task whose
	// descendants are still running is WAITING_FOR_CHILDREN, itself live, and
	// each descendant's payload is its own copy, written on its own.
	if r.d.RowLocks {
		var one int
		if err := tx.QueryRowContext(ctx, `SELECT 1 FROM tasks WHERE id = $1 FOR UPDATE`, prior.TaskID).Scan(&one); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return datasubject.TurnResult{}, fmt.Errorf("subjectstore: lock task %s: %w", prior.TaskID, r.d.mapErr(err))
		}
	}
	runHook(r.hooks.Locked)
	a := &args{}
	setP := r.d.colParam(payloadCol, a.add(payload))
	idP, oldP := a.add(prior.TaskID), a.add(ptrArg(prior.Payload))
	result, err := tx.ExecContext(ctx, fmt.Sprintf(`UPDATE tasks SET payload = %s WHERE id = $%d AND %s %s $%d AND status NOT IN (%s)`,
		setP, idP, r.d.colText(payloadCol), r.d.nullSafeEq(), oldP, statusList(a)), a.v...)
	if err != nil {
		return datasubject.TurnResult{}, fmt.Errorf("subjectstore: write payload of %s: %w", prior.TaskID, r.d.mapErr(err))
	}
	if n, _ := result.RowsAffected(); n == 0 {
		var status string
		err := tx.QueryRowContext(ctx, `SELECT status FROM tasks WHERE id = $1`, prior.TaskID).Scan(&status)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return datasubject.TurnResult{}, fmt.Errorf("subjectstore: re-read task %s: %w", prior.TaskID, r.d.mapErr(err))
		}
		for _, l := range datasubject.TranscriptLiveStatuses() {
			if status == l {
				return datasubject.TurnResult{Outcome: datasubject.TranscriptLive, LiveTaskID: prior.TaskID}, nil
			}
		}
		return datasubject.TurnResult{Outcome: datasubject.TranscriptVersionChanged}, nil
	}
	if err := tx.Commit(); err != nil {
		return datasubject.TurnResult{}, fmt.Errorf("subjectstore: commit payload of %s: %w", prior.TaskID, r.d.mapErr(err))
	}
	return datasubject.TurnResult{Outcome: datasubject.TranscriptApplied}, nil
}
