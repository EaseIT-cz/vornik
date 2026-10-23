package auditredact

// GitHub #414 (1): the rescan entry points `secrets scan-history` uses. Unit
// level; the stored-row regressions run the CLI scanners against real SQLite
// and Postgres in internal/cli.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/secrets"
)

func strongOnly(t string) bool { return !secrets.IsHeuristicType(t) && t != FindingCutToken }

func TestRescanTaskMessage414(t *testing.T) {
	meta := []byte(`{"kind":"note","auth":{"password":"hunter2-long-enough"},"source_set_hash":"` +
		strings.Repeat("0123456789abcdef", 4) + `"}`)
	got := RescanTaskMessage(det395(t), strongOnly, "deploy "+aws395, "ops-"+tok395, meta)
	require.NotContains(t, got.Content, aws395)
	require.NotContains(t, got.AuthorID, tok395)
	require.True(t, strings.HasPrefix(got.AuthorID, "ops-"))
	require.NotContains(t, string(got.Metadata), "hunter2")
	var m map[string]any
	require.NoError(t, json.Unmarshal(got.Metadata, &m), "metadata still decodes")
	require.Equal(t, strings.Repeat("0123456789abcdef", 4), m["source_set_hash"], "the latch hash survives")
	require.True(t, got.ContentScan.Matched() && got.AuthorScan.Matched() && got.MetadataScan.Matched())
	require.Equal(t, 1, got.MetadataScan.Redacted[FindingSecretField])

	clean := RescanTaskMessage(det395(t), strongOnly, "hello", "lead", []byte(`{"kind":"note"}`))
	require.False(t, clean.ContentScan.Matched() || clean.AuthorScan.Matched() || clean.MetadataScan.Matched())
	require.Equal(t, `{"kind":"note"}`, string(clean.Metadata), "clean metadata is returned byte-identical")
}

func TestRescanChatSignals414(t *testing.T) {
	sig := `[{"claim_value":"https://x.test/` + tok395[:20] + `…","sentence":"plain words here","detail":"d"}]`
	// The row's response holds the whole token: its prefix in the signal is a
	// fragment of a known match.
	out, r := RescanChatSignals(det395(t), strongOnly, []string{"", "reply " + tok395, "[]"}, sig)
	require.NotContains(t, out, tok395[:12])
	require.True(t, r.Matched())
	var list []map[string]any
	require.NoError(t, json.Unmarshal([]byte(out), &list))

	// Without a known match the cut-edge run is the only rule: counted under
	// strong, applied only when cut_token is selected.
	kept, r2 := RescanChatSignals(det395(t), strongOnly, nil, sig)
	require.Contains(t, kept, tok395[:20])
	require.Equal(t, 1, r2.Found[FindingCutToken])
	require.Empty(t, r2.Redacted)
	cut, r3 := RescanChatSignals(det395(t), nil, nil, sig)
	require.NotContains(t, cut, tok395[:12])
	require.Equal(t, 1, r3.Redacted[FindingCutToken])
}

// A finding only in the sources is the sources' to count: the signals column's
// Rescan reports nothing for it (code review 6652, suggestion 3).
func TestRescanChatSignals414_SourceFindingsAreNotCountedHere(t *testing.T) {
	sig := `[{"claim_value":"https://x.test/plain","detail":"d","sentence":"plain words"}]`
	out, r := RescanChatSignals(det395(t), nil, []string{"key " + aws395}, sig)
	require.Equal(t, sig, out, "a clean signals column is returned byte-identical")
	require.False(t, r.Matched())
}

func TestRescanIdentity414(t *testing.T) {
	out, r := RescanIdentity(det395(t), nil, "api:client "+aws395)
	require.NotContains(t, out, aws395)
	require.True(t, strings.HasPrefix(out, "api:client [REDACTED:"), out)
	require.True(t, r.Matched())
	same, r2 := RescanIdentity(det395(t), nil, "telegram:-1001234567890")
	require.Equal(t, "telegram:-1001234567890", same, "a channel id matches nothing without entropy")
	require.False(t, r2.Matched())
}
