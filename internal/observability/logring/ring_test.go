package logring

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func fixedRing(t *testing.T, size int) (*Ring, *time.Time) {
	t.Helper()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	r := New(size, time.Hour)
	r.now = func() time.Time { return now }
	return r, &now
}

func line(fields string) []byte {
	return []byte(`{"level":"warn","time":"2026-10-08T12:00:00Z",` + fields + `}` + "\n")
}

// GitHub #92 (2026-10-08), live design Change C6: a line attributed to no
// project and no task is never stored; a project's line is returned to a
// reader scoped to that project only; a task-only line is stored for the
// reader to resolve.
func TestRing_DropsUnattributedLinesForScopedReaders(t *testing.T) {
	r, _ := fixedRing(t, 10)
	r.Append(line(`"component":"scheduler","message":"tick"`))
	r.Append(line(`"component":"mcp","project":"hermes--fin","server":"mail","message":"mcp: failed to connect"`))
	r.Append(line(`"component":"scheduler","project_id":"other","message":"x"`))
	r.Append(line(`"task_id":"t1","message":"task-only"`))

	got := r.Since(map[string]bool{"hermes--fin": true}, false, time.Time{}, 100)
	require.Len(t, got, 1)
	assert.Equal(t, "hermes--fin", got[0].ProjectID)
	assert.Equal(t, "mail", got[0].Server)

	all := r.Since(nil, true, time.Time{}, 100)
	assert.Len(t, all, 3, "the unattributed line is not stored at all")
	assert.Equal(t, "t1", all[2].TaskID)
	assert.Equal(t, "", all[2].ProjectID)
}

// GitHub #92 (2026-10-08): bounded by size and age.
func TestRing_BoundedBySizeAndAge(t *testing.T) {
	r, now := fixedRing(t, 2000)
	for i := 0; i < 2001; i++ {
		r.Append(line(fmt.Sprintf(`"project":"p","message":"m%d"`, i)))
	}
	all := r.Since(nil, true, time.Time{}, 5000)
	require.Len(t, all, 2000)
	assert.Equal(t, "m1", all[0].Message, "the oldest line was dropped")

	*now = now.Add(61 * time.Minute)
	r.Append(line(`"project":"p","message":"fresh"`))
	all = r.Since(nil, true, time.Time{}, 5000)
	require.Len(t, all, 1, "lines older than an hour are gone")
	assert.Equal(t, "fresh", all[0].Message)
}

// GitHub #92 (2026-10-08): garbage never panics and is not stored.
func TestRing_NonJSONLineIgnoredNotPanicking(t *testing.T) {
	r, _ := fixedRing(t, 10)
	for _, b := range [][]byte{nil, []byte("\n"), []byte("not json project task"), []byte(`{"project":`), []byte(`{"project":5}`)} {
		assert.NotPanics(t, func() { r.Append(b) })
	}
	assert.Empty(t, r.Since(nil, true, time.Time{}, 10))
}

// GitHub #92 (2026-10-08), Change C6: only allowlisted fields are kept; the
// error field and every other field never enter the ring; the message is
// capped at 512 bytes.
func TestRing_KeepsOnlyAllowlistedFields(t *testing.T) {
	r, _ := fixedRing(t, 10)
	long := strings.Repeat("a", 2000)
	r.Append(line(`"component":"mcp","project":"p","server":"s","http_status":401,"error":"401 bad token sk-secret","url":"https://h/x","message":"` + long + `"`))
	all := r.Since(nil, true, time.Time{}, 10)
	require.Len(t, all, 1)
	l := all[0]
	assert.Len(t, l.Message, 512)
	assert.Equal(t, 401, l.HTTPStatus)
	assert.NotContains(t, fmt.Sprintf("%+v", l), "sk-secret")
	assert.NotContains(t, fmt.Sprintf("%+v", l), "https://h/x")
}

// GitHub #92 (2026-10-08): the spellings in use (counted in A0.10) are all
// attribution: project_id and project; task_id and taskId; execution_id and
// executionId.
func TestRing_AcceptsBothAttributionSpellings(t *testing.T) {
	r, _ := fixedRing(t, 10)
	r.Append(line(`"projectId":"nope","message":"a"`))
	r.Append(line(`"taskId":"t2","executionId":"e2","message":"b"`))
	all := r.Since(nil, true, time.Time{}, 10)
	require.Len(t, all, 1)
	assert.Equal(t, "t2", all[0].TaskID)
	assert.Equal(t, "e2", all[0].ExecutionID)
}

// GitHub #92 (2026-10-08): the tap never fails a log write and reports every
// byte written.
func TestTap_NeverErrors(t *testing.T) {
	r, _ := fixedRing(t, 10)
	tap := r.Tap()
	for _, b := range [][]byte{nil, []byte("x"), line(`"project":"p","message":"m"`)} {
		n, err := tap.Write(b)
		assert.NoError(t, err)
		assert.Equal(t, len(b), n)
	}
	assert.Len(t, r.Since(nil, true, time.Time{}, 10), 1)
}

// GitHub #92 (2026-10-08): since and max bound a read; the newest are kept.
func TestRing_SinceAndMax(t *testing.T) {
	r, now := fixedRing(t, 10)
	r.Append(line(`"project":"p","message":"old"`))
	*now = now.Add(time.Minute)
	cut := *now
	r.Append(line(`"project":"p","message":"new1"`))
	r.Append(line(`"project":"p","message":"new2"`))
	got := r.Since(map[string]bool{"p": true}, false, cut, 1)
	require.Len(t, got, 1)
	assert.Equal(t, "new2", got[0].Message)
	assert.Equal(t, cut.Add(-time.Minute), r.Oldest())
}

// review-20261008-f7c9 findings 2 and 5, GitHub #92 (2026-10-08): every
// stored string is capped, and concurrent writers and readers are safe
// (run under -race).
func TestRing_CapsFieldsAndIsConcurrencySafe(t *testing.T) {
	r := New(100, time.Hour)
	long := strings.Repeat("p", 1000)
	r.Append(line(`"project":"` + long + `","component":"` + long + `","message":"m"`))
	got := r.Since(nil, true, time.Time{}, 10)
	require.Len(t, got, 1)
	assert.Len(t, got[0].ProjectID, 128)
	assert.Len(t, got[0].Component, 128)

	done := make(chan struct{})
	for w := 0; w < 4; w++ {
		go func() {
			for i := 0; i < 500; i++ {
				r.Append(line(`"project":"p","message":"x"`))
			}
			done <- struct{}{}
		}()
	}
	for i := 0; i < 200; i++ {
		_ = r.Since(map[string]bool{"p": true}, false, time.Time{}, 50)
		_ = r.Oldest()
	}
	for w := 0; w < 4; w++ {
		<-done
	}
	assert.Len(t, r.Since(nil, true, time.Time{}, 1000), 100)
}
