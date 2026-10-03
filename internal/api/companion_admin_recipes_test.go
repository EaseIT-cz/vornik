package api

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/apikey"
	"vornik.io/vornik/internal/persistence"
)

// Agent-administered Vornik design §19.2: list_recipes and install_recipe
// are admin verbs, offered to an agent admin key only, and routed to the
// service; install_recipe's schema says what absent and null schedules mean.
func TestCompanionAdmin_RecipeVerbs(t *testing.T) {
	srv, keys, _ := newCompanionMCPServer(t)
	fake := &fakeAgentAdmin{}
	srv.agentAdmin, srv.agentAdminEnabled = fake, func() bool { return true }
	plainRaw, _ := seedCompanionKey(t, keys, "assistant", nil)
	agentRaw, err := apikey.Generate("hermes--home")
	require.NoError(t, err)
	require.NoError(t, keys.Create(context.Background(), &persistence.APIKey{
		ID: "akey-agent", ProjectID: "hermes--home", Name: "hermes", KeyHash: apikey.Hash(agentRaw),
		KeyPrefix: apikey.DisplayPrefix(agentRaw), ClientKind: "hermes", CreatedAt: time.Now().UTC(),
		AgentAdmin: true, AgentNamespace: "hermes",
	}))

	names := listTools(t, srv, agentRaw)
	require.True(t, names[agentadmin.VerbListRecipes], "list_recipes not offered")
	require.True(t, names[agentadmin.VerbInstallRecipe], "install_recipe not offered")
	plain := listTools(t, srv, plainRaw)
	require.False(t, plain[agentadmin.VerbListRecipes] || plain[agentadmin.VerbInstallRecipe], "a plain key was offered the recipe verbs")

	text, isErr := decodeToolText(t, callAdminTool(t, srv, agentRaw, agentadmin.VerbListRecipes))
	require.False(t, isErr, text)
	require.Contains(t, text, "inbox-digest")
	text, isErr = decodeToolText(t, callAdminTool(t, srv, agentRaw, agentadmin.VerbInstallRecipe))
	require.False(t, isErr, text)
	require.Equal(t, []string{agentadmin.VerbListRecipes, agentadmin.VerbInstallRecipe}, fake.calls)

	for _, d := range companionAdminToolDefs() {
		if d.Name != agentadmin.VerbInstallRecipe {
			continue
		}
		props := d.InputSchema["properties"].(map[string]any)
		for _, f := range []string{"recipe", "project", "variables", "schedule"} {
			require.Contains(t, props, f)
		}
		desc := props["schedule"].(map[string]any)["description"].(string)
		require.True(t, strings.Contains(desc, "null") && strings.Contains(desc, "Omit"), desc)
	}
}
