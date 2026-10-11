package registry

import (
	"github.com/stretchr/testify/require"
	"strings"
	"testing"
)

// GitHub #318: raw output declarations must never include transcripts,
// credential proposal files or paths; an absent declaration stays inert.
func TestBrokerArtifactDeclaration(t *testing.T) {
	for _, names := range [][]string{{"report.pdf", "summary.md"}, nil} {
		b := parseBroker(t, validBrokerYAML)
		b.Egress.Artifacts = names
		require.NoError(t, b.Validate())
	}
	for _, name := range []string{"", "..", "dir/file.pdf", `dir\file.pdf`, "digest.json", "agent-response.md", "lead-response-20261011-abcd.md", "bad\x00name", "line\nname", "summary.md "} {
		t.Run(name, func(t *testing.T) {
			b := parseBroker(t, validBrokerYAML)
			b.Egress.Artifacts = []string{name}
			require.Error(t, b.Validate(), name)
		})
	}
	b := parseBroker(t, validBrokerYAML)
	b.Egress.Artifacts = []string{"report.pdf", "report.pdf"}
	require.Error(t, b.Validate())
	b.Egress.Artifacts = strings.Fields(strings.Repeat("file.pdf ", 17))
	require.Error(t, b.Validate())
	b.Egress.Artifacts = []string{"proposal.json"}
	b.Proposes = []BrokerProposal{{Output: "proposal.json"}}
	require.Error(t, b.validateEgress())
}

func TestBrokerArtifactDeclarationCannotBypassTrimmedEgress(t *testing.T) {
	b := parseBroker(t, validBrokerYAML)
	b.Egress.Output = " digest.json "
	b.Egress.Artifacts = []string{"digest.json"}
	require.Error(t, b.validateEgress())
	b.Egress.Output = "digest.json"
	b.Proposes = []BrokerProposal{{Output: " proposal.json "}}
	b.Egress.Artifacts = []string{"proposal.json"}
	require.Error(t, b.validateEgress())
}
