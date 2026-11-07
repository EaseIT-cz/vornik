package registry

// GitHub #633 (reviewable-output design §6.1): a role may switch off advisory
// guidance blocks too, with the same rules as the swarm list. The effective
// list for a role is the union of the two.

import (
	"testing"

	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"

	"vornik.io/vornik/internal/promptblock"
)

func swarmWithRoleSuppression(swarm, role []string) *Swarm {
	sw := swarmWithSuppression(swarm...)
	sw.Roles[0].SuppressedGuidanceBlocks = role
	sw.Roles = append(sw.Roles, SwarmRole{
		Name:    "other",
		Runtime: SwarmRoleRuntime{Image: "localhost/vornik-agent:latest"},
	})
	return sw
}

func TestSwarmRole_SuppressedGuidanceBlocksDecodesFromYAML(t *testing.T) {
	var r SwarmRole
	require.NoError(t, yaml.Unmarshal([]byte("name: writer\nsuppressedGuidanceBlocks: [writing-style]\n"), &r))
	require.Equal(t, []string{promptblock.WritingStyle}, r.SuppressedGuidanceBlocks)
}

func TestSwarmRole_AdvisoryBlockValidates(t *testing.T) {
	sw := swarmWithRoleSuppression(nil, []string{promptblock.WritingStyle})
	require.NoError(t, sw.Validate("test-swarm.md"))
}

func TestSwarmRole_InvariantBlockFailsTheFile(t *testing.T) {
	sw := swarmWithRoleSuppression(nil, []string{promptblock.ReportingIntegrity})
	err := sw.Validate("test-swarm.md")
	var vErr SwarmValidationError
	require.True(t, asSwarmValidationError(err, &vErr), "want SwarmValidationError, got %v", err)
	require.Equal(t, "roles[0].suppressedGuidanceBlocks", vErr.Field)
}

func TestSwarmRole_UnknownBlockWarnsWithItsRole(t *testing.T) {
	sw := swarmWithRoleSuppression([]string{"swarm-typo"}, []string{"role-typo"})
	require.NoError(t, sw.Validate("test-swarm.md"))
	require.Equal(t, []string{"swarm-typo", "role-typo"}, sw.UnknownSuppressedGuidanceBlocks())
}

func TestSwarm_SuppressedGuidanceBlocksForIsTheUnion(t *testing.T) {
	sw := swarmWithRoleSuppression(
		[]string{promptblock.ToolBudget, promptblock.WritingStyle},
		[]string{promptblock.WritingStyle, promptblock.CanonicalContext},
	)
	require.Equal(t,
		[]string{promptblock.ToolBudget, promptblock.WritingStyle, promptblock.CanonicalContext},
		sw.SuppressedGuidanceBlocksFor("worker"), "swarm then role, duplicates merged")
	require.Equal(t,
		[]string{promptblock.ToolBudget, promptblock.WritingStyle},
		sw.SuppressedGuidanceBlocksFor("other"), "a role without a list gets the swarm list")
	require.Equal(t,
		[]string{promptblock.ToolBudget, promptblock.WritingStyle},
		sw.SuppressedGuidanceBlocksFor("no-such-role"))
	var nilSwarm *Swarm
	require.Nil(t, nilSwarm.SuppressedGuidanceBlocksFor("worker"))
}
