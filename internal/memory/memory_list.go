package memory

// Memory the user can see (LLD 22, "Memory the user can see: list, forget
// and export from any harness", GitHub #119 item 6): the paged listing of a
// project's live rows that the companion memory_list tool and the approver
// device's memory page both read, and the presentation helpers they share.

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"vornik.io/vornik/internal/memoryfirewall"
)

// Origins of a memory item: a closed set, derived from the row.
const (
	OriginLearnedFromTask = "learned_from_task"
	OriginMirrored        = "mirrored"
	OriginRemembered      = "remembered"
	OriginIngested        = "ingested"
)

// mirrorTokenPattern is the Hermes plugin's whole mirror token
// (contrib/hermes-companion/memory_provider.py TOKEN_RE, literal form).
const mirrorTokenPattern = ` ?⟦vm:[0-9a-f]{16}⟧`

var mirrorTokenRE = regexp.MustCompile(mirrorTokenPattern)

// liveValidationPredicate is the "not forgotten" predicate every read a
// person or a harness sees carries: refuted and superseded rows are never
// listed (the search statements carry the same text, GitHub #94; the
// listings since GitHub #287 and #119 item 6).
const liveValidationPredicate = "validation_status NOT IN ('refuted','superseded')"

// ListPageMax bounds one ListLiveChunksPage call.
const ListPageMax = 500

// ErrBadCursor is returned for a cursor that does not decode.
var ErrBadCursor = errors.New("cursor does not decode")

// ChunkCursor is a keyset position in the (created_at DESC, id DESC) order.
// It is a position, not a credential: every statement that takes it binds
// the project separately.
type ChunkCursor struct {
	CreatedAt time.Time
	ID        string
}

// EncodeChunkCursor returns the opaque form of c.
func EncodeChunkCursor(c ChunkCursor) string {
	raw := c.CreatedAt.UTC().Format(time.RFC3339Nano) + "|" + c.ID
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// DecodeChunkCursor parses an opaque cursor; "" is no cursor.
func DecodeChunkCursor(s string) (*ChunkCursor, error) {
	if s == "" {
		return nil, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, ErrBadCursor
	}
	at, id, ok := strings.Cut(string(raw), "|")
	if !ok || id == "" {
		return nil, ErrBadCursor
	}
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return nil, ErrBadCursor
	}
	return &ChunkCursor{CreatedAt: t, ID: id}, nil
}

// StripMirrorTokens removes every whole Hermes mirror token.
func StripMirrorTokens(s string) string { return mirrorTokenRE.ReplaceAllString(s, "") }

// OriginOf classifies a row: a task's output, a Hermes mirror note, a note a
// companion key remembered, or anything else ingested.
func OriginOf(taskID, sourceName, content string) string {
	switch {
	case taskID != "":
		return OriginLearnedFromTask
	case mirrorTokenRE.MatchString(content):
		return OriginMirrored
	case strings.HasPrefix(sourceName, "companion:"):
		return OriginRemembered
	}
	return OriginIngested
}

// Snippet strips mirror tokens, then cuts on a UTF-8 boundary at maxBytes
// with an ellipsis when cut. Stripping first means no token fragment can
// survive the cut.
func Snippet(content string, maxBytes int) string {
	s := StripMirrorTokens(content)
	if len(s) <= maxBytes {
		return s
	}
	cut := maxBytes
	for cut > 0 && !utf8.RuneStart(s[cut]) {
		cut--
	}
	return s[:cut] + "…"
}

// HumanAge renders an age in whole units: "just now", "5 minutes",
// "1 hour", "3 days". Shared by memory_list and the device page.
func HumanAge(d time.Duration) string {
	plural := func(n int, unit string) string {
		if n == 1 {
			return "1 " + unit
		}
		return fmt.Sprintf("%d %ss", n, unit)
	}
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return plural(int(d/time.Minute), "minute")
	case d < 24*time.Hour:
		return plural(int(d/time.Hour), "hour")
	}
	return plural(int(d/(24*time.Hour)), "day")
}

// ListLiveChunksPage returns up to limit live rows of one project, newest
// first, strictly after the (createdAt, id) position when after is set.
// Live: not refuted, not superseded, not expired.
func (r *Repository) ListLiveChunksPage(ctx context.Context, projectID string, after *ChunkCursor, limit int) ([]RecentChunkRow, error) {
	if r == nil || r.db == nil || projectID == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > ListPageMax {
		limit = ListPageMax
	}
	var at any
	afterID := ""
	if after != nil {
		at, afterID = after.CreatedAt.UTC(), after.ID
	}
	q := `
SELECT id,
       COALESCE(task_id, ''),
       COALESCE(source_name, ''),
       COALESCE(content_class, ''),
       content,
       created_at,
       COALESCE(repo_scope, ''),
       (embedding IS NOT NULL) AS has_embedding
FROM project_memory_chunks
WHERE project_id = $1
  AND ` + liveValidationPredicate + `
  AND ` + r.expiryClause(projectID, "") + `
  AND ($3::timestamptz IS NULL OR (created_at, id) < ($3::timestamptz, $4::text))
ORDER BY created_at DESC, id DESC
LIMIT $2`
	rows, err := r.db.QueryContext(ctx, q, projectID, limit, at, afterID)
	if err != nil {
		return nil, fmt.Errorf("ListLiveChunksPage: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := make([]RecentChunkRow, 0, min(limit, maxRecentChunkPrealloc))
	for rows.Next() {
		var row RecentChunkRow
		if err := rows.Scan(&row.ChunkID, &row.TaskID, &row.SourceName, &row.ContentClass, &row.Content, &row.CreatedAt, &row.RepoScope, &row.HasEmbedding); err != nil {
			return nil, fmt.Errorf("scan live chunk: %w", err)
		}
		out = append(out, row)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate live chunks: %w", err)
	}
	return out, nil
}

// ListLiveChunkIDsBySource returns up to limit ids of the live rows of one
// project with exactly this source_name, in id order after afterID. The
// device's "forget everything from this source" reads its ids here.
func (r *Repository) ListLiveChunkIDsBySource(ctx context.Context, projectID, sourceName, afterID string, limit int) ([]string, error) {
	if r == nil || r.db == nil || projectID == "" {
		return nil, nil
	}
	if limit <= 0 || limit > ListPageMax {
		limit = ListPageMax
	}
	q := `
SELECT id
FROM project_memory_chunks
WHERE project_id = $1
  AND source_name = $2
  AND ` + liveValidationPredicate + `
  AND ` + r.expiryClause(projectID, "") + `
  AND id > $3
ORDER BY id
LIMIT $4`
	rows, err := r.db.QueryContext(ctx, q, projectID, sourceName, afterID, limit)
	if err != nil {
		return nil, fmt.Errorf("ListLiveChunkIDsBySource: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan chunk id: %w", err)
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ListPage is one page of a project's live rows after the firewall.
// Examined counts the rows the statement returned, so a row the firewall
// withheld is counted, not silent; Next is set from the last examined row
// whenever the page was full, so a withheld row never ends the listing.
type ListPage struct {
	Rows     []RecentChunkRow
	Examined int
	Next     *ChunkCursor
}

// ListPageWithContext reads one page and runs it through the firewall step
// recent_memory uses (firewallListedRows).
func (s *Searcher) ListPageWithContext(ctx context.Context, projectID string, after *ChunkCursor, limit int, reqCtx memoryfirewall.RequestContext) (ListPage, error) {
	if s == nil || s.repo == nil {
		return ListPage{}, nil
	}
	if limit <= 0 {
		limit = 20
	}
	if limit > ListPageMax {
		limit = ListPageMax
	}
	rows, err := s.repo.ListLiveChunksPage(ctx, projectID, after, limit)
	if err != nil {
		return ListPage{}, err
	}
	page := ListPage{Examined: len(rows)}
	if len(rows) == limit {
		last := rows[len(rows)-1]
		page.Next = &ChunkCursor{CreatedAt: last.CreatedAt, ID: last.ChunkID}
	}
	page.Rows = s.firewallListedRows(ctx, projectID, rows, reqCtx)
	return page, nil
}
