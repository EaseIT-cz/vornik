package sqlite_test

import (
	"bytes"
	"testing"

	"vornik.io/vornik/internal/agenttokens"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/secretstore"
)

// sealedTokens wraps a token repository in the agent sealing wrapper (plan
// P4.4) with a fixed test key.
func sealedTokens(t *testing.T, inner persistence.MCPOAuthTokenRepository) persistence.MCPOAuthTokenRepository {
	t.Helper()
	st, err := secretstore.New(nil, bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	get := func() (agenttokens.Sealer, error) { return st, nil }
	return &agenttokens.Repo{MCPOAuthTokenRepository: inner, ForWrite: get, ForRead: get}
}
