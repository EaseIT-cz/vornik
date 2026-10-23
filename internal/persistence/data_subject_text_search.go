package persistence

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// The GDPR §5.1 free-text backstop (GitHub #345): find stored memory chunks
// whose text contains a data subject's name, for the console resolution page to
// present as CANDIDATES for human confirmation — never as links.
//
// It searches what is STORED, not what recall would serve: recall is
// project-scoped, ranked, truncated and filters refuted, superseded, unpublished
// and expired rows, every one of which an erasure must still reach. It never
// searches a chunk under an active Art 18 restriction: the shared predicate
// (ChunkNotRestrictedSQL) is in the WHERE, so restricted text is not read.
//
// One statement for both drivers: $n placeholders, LOWER … LIKE … ESCAPE, no
// driver-specific function (SQLite folds ASCII case only; Postgres folds
// Unicode).
//
// see LLD § https://docs.vornik.io §4.17

// SubjectTextQuery is one search term for one subject.
type SubjectTextQuery struct {
	// SubjectID is the subject the page resolves; it decides LinkedHereSource
	// and which links count as OTHER subjects.
	SubjectID string
	// Term is matched case-insensitively as a literal substring; LIKE
	// wildcards in it are escaped. A blank term is refused.
	Term string
	// ProjectID narrows to one project; empty searches every project.
	ProjectID string
	// ChunkID narrows to one chunk — the write-time re-check of a confirm.
	ChunkID string
	// Limit caps the rows the statement returns, literally; <= 0 means 50. The
	// console handler passes its render cap + 1 so the extra row signals
	// "more matches exist" (design §4.17.2).
	Limit int
}

// SubjectTextMention is one matching chunk.
type SubjectTextMention struct {
	ChunkID   string
	ProjectID string
	// Content is the whole chunk, for the daemon to cut a bounded snippet
	// from. Callers must never render or export it whole.
	Content string
	// CreatedAt is the stored timestamp as text (the drivers store it
	// differently; it is display-only).
	CreatedAt string
	// LinkedHereSource is the source of THIS subject's existing link to the
	// chunk, or "" when there is none.
	LinkedHereSource string
	// OtherSubjects is how many other subjects are linked to the chunk.
	OtherSubjects int
}

// SubjectTextSearch is the free-text candidate search.
type SubjectTextSearch interface {
	FindMentions(ctx context.Context, q SubjectTextQuery) ([]SubjectTextMention, error)
}

// ErrBlankSearchTerm is returned for an empty term: under LIKE it would match
// every row, which is a dump, not a search.
var ErrBlankSearchTerm = errors.New("subject text search: blank search term")

const subjectTextDefaultLimit = 50

// SQLSubjectTextSearch is the production SubjectTextSearch over the daemon's
// handle, on either driver.
type SQLSubjectTextSearch struct{ db DBTX }

// NewSQLSubjectTextSearch builds the search.
func NewSQLSubjectTextSearch(db DBTX) *SQLSubjectTextSearch { return &SQLSubjectTextSearch{db: db} }

var _ SubjectTextSearch = (*SQLSubjectTextSearch)(nil)

// subjectTextSearchSQL is the statement. Each optional filter compares its
// parameter with the empty string OR the column, so the statement text never
// varies with the input; the casts give Postgres a parameter type it cannot
// infer from an empty-string literal.
func subjectTextSearchSQL() string {
	return `
SELECT c.id, c.project_id, c.content, CAST(c.created_at AS TEXT),
       COALESCE((SELECT h.source FROM data_subject_links h
                  WHERE h.subject_id = $1 AND h.table_name = 'project_memory_chunks'
                    AND h.row_id = c.id), ''),
       (SELECT COUNT(DISTINCT o.subject_id) FROM data_subject_links o
         WHERE o.table_name = 'project_memory_chunks' AND o.row_id = c.id
           AND o.subject_id <> $1)
  FROM project_memory_chunks c
 WHERE LOWER(c.content) LIKE '%' || LOWER(CAST($2 AS TEXT)) || '%' ESCAPE '\'
   AND (CAST($3 AS TEXT) = '' OR c.project_id = CAST($3 AS TEXT))
   AND (CAST($4 AS TEXT) = '' OR c.id = CAST($4 AS TEXT))
   AND ` + ChunkNotRestrictedSQL("c.id") + `
 ORDER BY c.created_at DESC, c.id
 LIMIT $5`
}

// escapeLikeTerm makes a user-supplied term literal inside a LIKE pattern
// (paired with ESCAPE '\'): without it "%" or "_" would match nearly every row.
// It runs in Go BEFORE the SQL applies LOWER, which is safe because the three
// escaped characters are ASCII and LOWER leaves them unchanged — do not move
// the escaping into SQL.
func escapeLikeTerm(s string) string { return EscapeLike(s) }

// FindMentions implements SubjectTextSearch.
func (s *SQLSubjectTextSearch) FindMentions(ctx context.Context, q SubjectTextQuery) ([]SubjectTextMention, error) {
	if s == nil || s.db == nil {
		return nil, fmt.Errorf("subject text search: no database handle")
	}
	term := strings.TrimSpace(q.Term)
	if term == "" {
		return nil, ErrBlankSearchTerm
	}
	limit := q.Limit
	if limit <= 0 {
		limit = subjectTextDefaultLimit
	}
	rows, err := s.db.QueryContext(ctx, subjectTextSearchSQL(),
		q.SubjectID, escapeLikeTerm(term), q.ProjectID, q.ChunkID, limit)
	if err != nil {
		return nil, fmt.Errorf("subject text search: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []SubjectTextMention
	for rows.Next() {
		var m SubjectTextMention
		if err := rows.Scan(&m.ChunkID, &m.ProjectID, &m.Content, &m.CreatedAt,
			&m.LinkedHereSource, &m.OtherSubjects); err != nil {
			return nil, fmt.Errorf("subject text search: scan: %w", err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("subject text search: %w", err)
	}
	return out, nil
}
