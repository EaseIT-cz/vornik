package artifacts

// GitHub #85 (artifact heuristic-redaction design §7, 2026-11-20): the
// artifact scan counts every finding by rule and by what happened to it —
// rewritten, stored unchanged (detect), or dropped (heuristic). Before this
// change a dropped finding was visible only in one log line, so nobody could
// measure how often entropy fires on artifacts.
//
// Found at: review (#85, 2026-09-02 round-2 review ed91 F4/F6), observed at
// unit (the store counts nothing).

import (
	"context"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/secrets"
)

type fakeFindingsCounter struct {
	mu sync.Mutex
	n  map[[2]string]int
}

func (f *fakeFindingsCounter) Inc(ruleType, outcome string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.n == nil {
		f.n = map[[2]string]int{}
	}
	f.n[[2]string{ruleType, outcome}]++
}

// byOutcome sums the counter over rule types.
func (f *fakeFindingsCounter) byOutcome() map[string]int {
	out := map[string]int{}
	for k, v := range f.n {
		out[k[1]] += v
	}
	return out
}

// One strong finding (an OpenAI key) and heuristic ones (generic_kv on the
// password line). The counts come from the detector, so the fixture cannot
// silently drift: it must produce exactly one strong finding and at least
// one heuristic one.
const counterFixture = "key=sk-proj1234567890abcdefghijklmnopqrstuv\npassword = \"hunter2hunter2hunter2\"\n"

func fixtureCounts(t *testing.T) (strong, heuristic int) {
	t.Helper()
	det, err := secrets.NewMultiDetector(secrets.Config{})
	require.NoError(t, err)
	all := det.Scan([]byte(counterFixture))
	strong = len(secrets.DropHeuristic(all))
	heuristic = len(all) - strong
	require.Equal(t, 1, strong, "the fixture must carry exactly one strong finding")
	require.Positive(t, heuristic, "the fixture must carry a heuristic finding")
	return strong, heuristic
}

func storeWithCounter(t *testing.T, action secrets.Action) (*Store, *fakeFindingsCounter) {
	t.Helper()
	var actions map[string]secrets.Action
	if action != "" {
		actions = map[string]secrets.Action{secrets.CheckpointArtifacts: action}
	}
	store := newTestStoreWithSecrets(t, actions)
	c := &fakeFindingsCounter{}
	store.SetFindingsCounter(c)
	return store, c
}

func TestStoreFindingsCounter85_ByAction(t *testing.T) {
	strong, heuristic := fixtureCounts(t)
	for _, tc := range []struct {
		action secrets.Action
		want   map[string]int
	}{
		{secrets.ActionRedact, map[string]int{"rewritten": strong, "dropped": heuristic}},
		{secrets.ActionBlock, map[string]int{"rewritten": strong, "dropped": heuristic}}, // block degrades to redact
		{secrets.ActionDetect, map[string]int{"stored": strong, "dropped": heuristic}},
	} {
		t.Run(string(tc.action), func(t *testing.T) {
			store, c := storeWithCounter(t, tc.action)
			src := writeTempSource(t, "result.md", counterFixture)
			_, err := store.Store(context.Background(), "p1", "e1", "t1", "result.md", src)
			require.NoError(t, err)
			assert.Equal(t, tc.want, c.byOutcome())
			assert.Equal(t, strong, c.n[[2]string{"openai_key", outcomeFor(tc.action)}], "counted by rule type")
		})
	}
}

func outcomeFor(a secrets.Action) string {
	if a == secrets.ActionDetect {
		return "stored"
	}
	return "rewritten"
}

func TestStoreFindingsCounter85_CleanArtifactCountsNothing(t *testing.T) {
	store, c := storeWithCounter(t, "")
	src := writeTempSource(t, "result.md", "nothing secret here\n")
	_, err := store.Store(context.Background(), "p1", "e1", "t1", "result.md", src)
	require.NoError(t, err)
	assert.Empty(t, c.n)
}

// A nil counter counts nothing and changes nothing about what is stored.
func TestStoreFindingsCounter85_NilCounterStoresTheSameBytes(t *testing.T) {
	read := func(store *Store) []byte {
		src := writeTempSource(t, "result.md", counterFixture)
		art, err := store.Store(context.Background(), "p1", "e1", "t1", "result.md", src)
		require.NoError(t, err)
		rc, err := store.Open(context.Background(), art.ID)
		require.NoError(t, err)
		defer func() { _ = rc.Close() }()
		b, err := io.ReadAll(rc)
		require.NoError(t, err)
		return b
	}
	with, _ := storeWithCounter(t, "")
	without := newTestStoreWithSecrets(t, nil)
	without.SetFindingsCounter(nil)
	assert.Equal(t, read(with), read(without))
}

// The daemon's counter: registered on the given registry under its design
// name, labelled by rule type and outcome.
func TestStoreFindingsCounter85_PromAdapter(t *testing.T) {
	reg := prometheus.NewRegistry()
	c := NewPromFindingsCounter(reg)
	c.Inc("entropy", FindingDropped)
	c.Inc("entropy", FindingDropped)
	c.Inc("openai_key", FindingRewritten)
	n, err := testutil.GatherAndCount(reg, "vornik_artifact_secret_findings_total")
	require.NoError(t, err)
	assert.Equal(t, 2, n, "two label sets")
	assert.NoError(t, testutil.GatherAndCompare(reg, strings.NewReader(`
# HELP vornik_artifact_secret_findings_total Secret findings in stored artifacts, by rule type and outcome: rewritten (redacted), stored (detect mode, unchanged) or dropped (a heuristic rule, never rewritten on this surface).
# TYPE vornik_artifact_secret_findings_total counter
vornik_artifact_secret_findings_total{outcome="dropped",type="entropy"} 2
vornik_artifact_secret_findings_total{outcome="rewritten",type="openai_key"} 1
`), "vornik_artifact_secret_findings_total"))
}

// "rewritten" under block is honest only while block really rewrites the body
// (it degrades to redact): the stored artifact no longer holds the key.
func TestStoreFindingsCounter85_BlockRewritesTheBytes(t *testing.T) {
	store, _ := storeWithCounter(t, secrets.ActionBlock)
	src := writeTempSource(t, "result.md", counterFixture)
	art, err := store.Store(context.Background(), "p1", "e1", "t1", "result.md", src)
	require.NoError(t, err)
	rc, err := store.Open(context.Background(), art.ID)
	require.NoError(t, err)
	defer func() { _ = rc.Close() }()
	b, err := io.ReadAll(rc)
	require.NoError(t, err)
	assert.NotContains(t, string(b), "sk-proj1234567890abcdefghijklmnopqrstuv")
}
