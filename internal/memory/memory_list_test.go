package memory

import (
	"context"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/memoryfirewall"
)

// GitHub #119 item 6 (2026-10-08): the user sees what is kept, paged, with
// source and age (LLD 22, "Memory the user can see").

var listCols = []string{"id", "task_id", "source_name", "content_class", "content", "created_at", "repo_scope", "has_embedding"}

func TestListLiveChunksPage_StatementExcludesRefutedAndSupersededAndPagesByKeyset(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	repo := NewRepository(db)
	at := time.Date(2026, 10, 8, 12, 0, 0, 123, time.UTC)
	mock.ExpectQuery(regexp.QuoteMeta("validation_status NOT IN ('refuted','superseded')")+`(?s).*`+
		regexp.QuoteMeta("(created_at, id) < ($3::timestamptz, $4::text)")+`(?s).*`+
		regexp.QuoteMeta("ORDER BY created_at DESC, id DESC")).
		WithArgs("proj", 100, at, "c-7").
		WillReturnRows(sqlmock.NewRows(listCols).AddRow("c-6", "", "companion:hermes:note", "unclassified", "x", at, "", true))
	rows, err := repo.ListLiveChunksPage(context.Background(), "proj", &ChunkCursor{CreatedAt: at, ID: "c-7"}, 100)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "c-6", rows[0].ChunkID)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestListLiveChunksPage_CapsTheLimit(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	mock.ExpectQuery("FROM project_memory_chunks").
		WithArgs("proj", ListPageMax, nil, "").
		WillReturnRows(sqlmock.NewRows(listCols))
	_, err = NewRepository(db).ListLiveChunksPage(context.Background(), "proj", nil, 10_000)
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

// GitHub #287 (2026-10-08): recent_memory and the SessionStart digest listed
// refuted and superseded notes; the recency statement now carries the live
// predicate the search statements carry.
func TestListRecentChunks_ExcludesRefutedAndSuperseded(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	mock.ExpectQuery(regexp.QuoteMeta("validation_status NOT IN ('refuted','superseded')")).
		WillReturnRows(sqlmock.NewRows(listCols))
	_, err = NewRepository(db).ListRecentChunksWithOptions(context.Background(), "proj", 5, "", false, false)
	require.NoError(t, err, "the recency statement must carry the live predicate")
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestChunkCursor_RoundTripsAndRejectsGarbage(t *testing.T) {
	c := ChunkCursor{CreatedAt: time.Date(2026, 10, 8, 1, 2, 3, 456789, time.UTC), ID: "abc|def"}
	enc := EncodeChunkCursor(c)
	require.NotEmpty(t, enc)
	got, err := DecodeChunkCursor(enc)
	require.NoError(t, err)
	require.NotNil(t, got)
	assert.True(t, got.CreatedAt.Equal(c.CreatedAt))
	assert.Equal(t, c.ID, got.ID)

	none, err := DecodeChunkCursor("")
	require.NoError(t, err)
	assert.Nil(t, none)
	for _, bad := range []string{"%%%", "bm90LWEtY3Vyc29y", EncodeChunkCursor(ChunkCursor{ID: ""})} {
		_, err := DecodeChunkCursor(bad)
		assert.ErrorIs(t, err, ErrBadCursor, bad)
	}
}

func TestSnippet_StripsTokensBeforeCuttingOnACharacterBoundary(t *testing.T) {
	tok := " ⟦vm:0123456789abcdef⟧"
	assert.Equal(t, "[USER] likes tea", Snippet("[USER] likes tea"+tok, 240))
	long := strings.Repeat("é", 200) + tok // 400 bytes of text, token past the cut
	s := Snippet(long, 240)
	assert.True(t, utf8.ValidString(s), "cut mid-character")
	assert.NotContains(t, s, "⟦")
	assert.NotContains(t, s, "vm:")
	assert.True(t, strings.HasSuffix(s, "…"))
	assert.LessOrEqual(t, len(strings.TrimSuffix(s, "…")), 240)
	assert.Equal(t, "a b", StripMirrorTokens("a"+tok+" b"))
}

func TestHumanAge_WholeUnits(t *testing.T) {
	assert.Equal(t, "just now", HumanAge(30*time.Second))
	assert.Equal(t, "1 minute", HumanAge(90*time.Second))
	assert.Equal(t, "5 hours", HumanAge(5*time.Hour+10*time.Minute))
	assert.Equal(t, "1 day", HumanAge(25*time.Hour))
	assert.Equal(t, "3 days", HumanAge(72*time.Hour))
}

func TestOriginOf_ClosedSet(t *testing.T) {
	tok := " ⟦vm:0123456789abcdef⟧"
	assert.Equal(t, OriginLearnedFromTask, OriginOf("task_1", "companion:hermes:note", "x"+tok))
	assert.Equal(t, OriginMirrored, OriginOf("", "companion:hermes:note", "x"+tok))
	assert.Equal(t, OriginRemembered, OriginOf("", "companion:claude-code:note", "x"))
	assert.Equal(t, OriginIngested, OriginOf("", "docs/design.md", "x"))
}

// The Go pattern and the Hermes plugin's whole-token pattern must name the
// same token (LLD 22: the daemon gains a Go copy and a test that they agree).
func TestMirrorTokenPattern_AgreesWithThePlugin(t *testing.T) {
	src, err := os.ReadFile("../../contrib/hermes-companion/memory_provider.py")
	require.NoError(t, err)
	var line string
	for _, l := range strings.Split(string(src), "\n") {
		if strings.HasPrefix(l, "TOKEN_RE = ") {
			line = l
		}
	}
	require.NotEmpty(t, line, "the plugin's TOKEN_RE moved")
	assert.Contains(t, line, "vm:[0-9a-f]{16}")
	assert.Contains(t, line, "⟦")
	assert.Contains(t, line, "⟧")
	assert.Contains(t, mirrorTokenPattern, "vm:[0-9a-f]{16}")
}

func TestListPageWithContext_FirewallDropsButPagingContinues(t *testing.T) {
	db, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	s := NewSearcher(Config{}, NewRepository(db), nil)
	s.SetFirewall(recentAuditFirewall(memoryfirewall.EnforcementEnforce))
	t1 := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	t0 := t1.Add(-time.Minute)
	mock.ExpectQuery("FROM project_memory_chunks").
		WillReturnRows(sqlmock.NewRows(listCols).
			AddRow("keep", "", "companion:hermes:note", "decision", "fine", t1, "", true).
			AddRow("blocked", "", "companion:hermes:note", "decision", "blocked by policy", t0, "", true))
	recentAuditExpectPolicyQuery(mock, "keep", "blocked")
	page, err := s.ListPageWithContext(context.Background(), "proj", nil, 2, memoryfirewall.RequestContext{
		Role: "companion:hermes", OperatorID: "key_1", Purpose: memoryfirewall.PurposeOperational,
	})
	require.NoError(t, err)
	require.Len(t, page.Rows, 1)
	assert.Equal(t, "keep", page.Rows[0].ChunkID)
	assert.Equal(t, 2, page.Examined, "a withheld row is counted")
	require.NotNil(t, page.Next, "a full page has a next position even when its last row was withheld")
	assert.Equal(t, "blocked", page.Next.ID)
	require.NoError(t, mock.ExpectationsWereMet())
}
