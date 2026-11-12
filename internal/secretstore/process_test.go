package secretstore

import (
	"testing"

	"github.com/stretchr/testify/assert"

	"vornik.io/vornik/internal/mcpauth"
)

type mapSource map[string]string

func (m mapSource) Get(name string) (string, bool) { v, ok := m[name]; return v, ok }

// GitHub #626 E: a value only in the store is what a point-of-use read gets.
func TestValue_ReadsTheProcessSource_626(t *testing.T) {
	t.Cleanup(func() { SetProcessSource(nil) })
	t.Setenv("VALUE_626_ENV", "from-env")

	assert.Equal(t, "from-env", Value("VALUE_626_ENV"), "before boot installs a source: the environment")
	assert.Equal(t, "", Value("VALUE_626_STORED"))
	assert.Equal(t, "", Value(""))

	SetProcessSource(mapSource{"VALUE_626_STORED": "from-store"})
	assert.Equal(t, "from-store", Value("VALUE_626_STORED"))
	assert.Equal(t, "", Value("VALUE_626_ENV"), "the installed source decides; the daemon's includes the environment")
}

// The daemon's source is Source{Env, Store}: the environment wins, then the
// store (the precedence of the 2026-11-02 operator decision).
func TestValue_EnvironmentThenStore_626(t *testing.T) {
	t.Cleanup(func() { SetProcessSource(nil) })
	t.Setenv("VALUE_626_BOTH", "env-wins")
	stored := mapSource{"VALUE_626_BOTH": "stored", "VALUE_626_ONLY": "stored-only"}
	SetProcessSource(chain{envSource{}, stored})
	assert.Equal(t, "env-wins", Value("VALUE_626_BOTH"))
	assert.Equal(t, "stored-only", Value("VALUE_626_ONLY"))
}

type envSource struct{}

func (envSource) Get(name string) (string, bool) {
	return mcpauth.EnvSecretSource{}.Get(name)
}

type chain []interface{ Get(string) (string, bool) }

func (c chain) Get(name string) (string, bool) {
	for _, s := range c {
		if v, ok := s.Get(name); ok {
			return v, true
		}
	}
	return "", false
}
