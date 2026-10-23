package telemetryclient

import (
	"encoding/json"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

// The URL dimensions exist so the edge can aggregate them without reading the
// body (design §"Transport"). template and autonomy_enabled were body-only, so
// "which templates get created, and is autonomy switched on" was unanswerable
// from anywhere: the Worker never parses the body. They are low-cardinality
// (catalog slug or "custom"; a bool), so they belong in the query too.
func TestProjectRequestExposesTemplateAndAutonomyAsURLDimensions(t *testing.T) {
	req, err := BuildRequest(DefaultEndpoint,
		ProjectEvent("2026.7.4", SourceCLITemplate, "personal-assistant", true, true))
	require.NoError(t, err)

	q := req.URL.Query()
	require.Equal(t, "project_created", q.Get("e"))
	require.Equal(t, "personal-assistant", q.Get("tpl"))
	require.Equal(t, "1", q.Get("auto"))
	require.Len(t, q, 8, "project_created carries the six base dimensions plus tpl+auto")

	// The body must keep saying the same thing, so a future collector can
	// validate URL/body agreement (design §"Transport").
	body, err := io.ReadAll(req.Body)
	require.NoError(t, err)
	var got struct {
		Properties struct {
			Template        string `json:"template"`
			AutonomyEnabled bool   `json:"autonomy_enabled"`
		} `json:"properties"`
	}
	require.NoError(t, json.Unmarshal(body, &got))
	require.Equal(t, "personal-assistant", got.Properties.Template)
	require.True(t, got.Properties.AutonomyEnabled)
}

func TestProjectRequestAutonomyDisabledIsExplicitZero(t *testing.T) {
	req, err := BuildRequest(DefaultEndpoint,
		ProjectEvent("2026.7.4", SourceCLIBasic, "", false, false))
	require.NoError(t, err)
	q := req.URL.Query()
	require.Equal(t, "0", q.Get("auto"), "absent would be indistinguishable from an old client")
	require.Equal(t, "custom", q.Get("tpl"))
}

// An install event has no properties, so it must not grow the two dimensions.
func TestInstallRequestHasNoProjectDimensions(t *testing.T) {
	req, err := BuildRequest(DefaultEndpoint, InstallEvent("2026.7.4", SourceQuickstart))
	require.NoError(t, err)
	q := req.URL.Query()
	require.Empty(t, q.Get("tpl"))
	require.Empty(t, q.Get("auto"))
	require.Len(t, q, 6)
}

// Design §"Transport": "never put a unique value in them because URLs are
// especially likely to be logged." A `git describe` version is effectively a
// unique build fingerprint — 2026.7.4-112-g29df3bdb identifies one commit, and
// paired with the edge-visible source IP that weakens the anonymity claim the
// privacy page makes. Only bounded release identifiers may go on the wire.
func TestVersionIsBoundedToReleaseIdentifiers(t *testing.T) {
	for _, tc := range []struct {
		in, want, why string
	}{
		{"2026.7.4", "2026.7.4", "plain calendar release passes through"},
		{"2026.7.5-rc1", "2026.7.5-rc1", "simple pre-release tag is still bounded"},
		{"2026.7.4-112-g29df3bdb", "dev", "git describe commit fingerprint must collapse"},
		{"2026.7.4-112-g29df3bdb-dirty", "dev", "dirty build fingerprint must collapse"},
		{"2026.7.4+dev", "dev", "build metadata is unbounded, so collapse it"},
		// #371 moved the grammar to version.IsTaggedRelease; these rows pin
		// that the accepted set did not move with it (they pass on both sides).
		{"2026.09.01", "2026.09.01", "leading zeros were accepted and still are"},
		{"2026.7.5-rc.1", "2026.7.5-rc.1", "dotted pre-release is still bounded"},
		{"2026.7.4-dirty", "dev", "a dirty tagged build is not the release"},
		{"2026.7.5-rc", "dev", "a pre-release with no number is not a release"},
		{"2026.7.4-dev", "dev", "an unlisted suffix is not a release"},
		{"dev+g44e675c12", "dev", "the VCS fallback version collapses"},
		{"", "unknown", "missing version stays unknown"},
		{"bad version /home/alice", "unknown", "unsafe charset stays unknown"},
	} {
		require.Equal(t, tc.want, sanitizeVersion(tc.in), tc.why)
	}
}

func TestBuildRequestVersionCollapsedInURL(t *testing.T) {
	req, err := BuildRequest(DefaultEndpoint,
		InstallEvent("2026.7.4-112-g29df3bdb-dirty", SourceQuickstart))
	require.NoError(t, err)
	full := req.URL.String()
	require.NotContains(t, full, "g29df3bdb", "commit sha must never reach the URL")
	require.Equal(t, "dev", req.URL.Query().Get("v"))
}
