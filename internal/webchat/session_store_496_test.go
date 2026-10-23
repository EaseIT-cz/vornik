package webchat

// GitHub #496 item 2: Load set AllowedProjects only after its early returns
// (no registry; the registry not holding the project), and an empty list is
// "no restriction" to every dispatcher tool, so such a turn could reach any
// project. Load now pins the project first, and a store with no project id
// admits none (process-spawn-law design, "Amendment 2026-10-21 (#496)", V3).
// The user-level test is ui TestWebChat496_ScopedKeyCannotReachAnotherProjectsTask.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/conversation"
	"vornik.io/vornik/internal/registry"
)

func TestSessionStore496_LoadAlwaysPinsTheProject(t *testing.T) {
	msg := conversation.ChannelMessage{SessionID: "s496"}
	for name, store := range map[string]*SessionStore{
		"no registry":                  NewSessionStore(nil, "p1"),
		"registry without the project": NewSessionStore(registry.New(), "p1"),
	} {
		sess, err := store.Load(context.Background(), msg)
		require.NoError(t, err, name)
		assert.Equal(t, []string{"p1"}, sess.AllowedProjects, "%s: the turn must stay pinned to the URL's project", name)
	}

	sess, err := NewSessionStore(nil, "").Load(context.Background(), msg)
	require.NoError(t, err)
	require.NotEmpty(t, sess.AllowedProjects, "an empty list means no restriction")
	assert.Equal(t, []string{NoProject}, sess.AllowedProjects)
	assert.NotContains(t, sess.AllowedProjects, "*")
}
