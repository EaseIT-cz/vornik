// Package agenttokens stores an agent project's MCP OAuth tokens sealed
// (agent-administered Vornik plan P4.4, design §8.4). It wraps the ordinary
// token repository: an agent-namespace project's access and refresh tokens
// are sealed with the secret store's namespace key into the same row's
// columns; every other column stays plaintext, because SQL reads it (the
// injection path, the stale-redirect sweep, the status UI). Operator
// projects pass through untouched.
//
// The rotation guard stays one statement on one row: SwapRefreshToken opens
// the stored refresh token, compares it with the one the caller used, and
// calls the inner conditional UPDATE with the stored SEALED value as the
// expected one. A seal's nonce is fresh per write, so that value is unique
// and the compare-and-swap is exact on both drivers.
package agenttokens

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"

	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/persistence"
)

// Sealer seals and opens values for a namespace; *secretstore.Store is one.
type Sealer interface {
	Seal(ns, label string, plain []byte) (string, error)
	Open(ns, label, sealed string) ([]byte, error)
}

// Repo is the routing repository the MCP connector uses.
type Repo struct {
	persistence.MCPOAuthTokenRepository
	// ForWrite returns the sealer, creating the store's key on first use.
	ForWrite func() (Sealer, error)
	// ForRead returns the sealer, never creating a key.
	ForRead func() (Sealer, error)
}

var _ persistence.MCPOAuthTokenRepository = (*Repo)(nil)

func label(server, field string) string { return "OAUTH_" + server + "/" + field }

func namespaceOf(projectID string) (string, bool) { return agentns.FromID(projectID) }

func (r *Repo) sealer(write bool) (Sealer, error) {
	f := r.ForRead
	if write {
		f = r.ForWrite
	}
	if f == nil {
		return nil, errors.New("agenttokens: the agent credential store is not available")
	}
	return f()
}

// open returns a copy of t with its tokens opened.
func (r *Repo) open(ns string, t *persistence.MCPOAuthToken) (*persistence.MCPOAuthToken, error) {
	s, err := r.sealer(false)
	if err != nil {
		return nil, err
	}
	out := *t
	for _, f := range []struct {
		dst  *string
		name string
	}{{&out.AccessToken, "access"}, {&out.RefreshToken, "refresh"}} {
		plain, err := s.Open(ns, label(t.ServerName, f.name), *f.dst)
		if err != nil {
			return nil, fmt.Errorf("agenttokens: %s/%s %s token: %w", t.ProjectID, t.ServerName, f.name, err)
		}
		*f.dst = string(plain)
	}
	return &out, nil
}

// seal returns a copy of t with its tokens sealed.
func (r *Repo) seal(ns string, t *persistence.MCPOAuthToken) (*persistence.MCPOAuthToken, error) {
	s, err := r.sealer(true)
	if err != nil {
		return nil, err
	}
	out := *t
	if out.AccessToken, err = s.Seal(ns, label(t.ServerName, "access"), []byte(t.AccessToken)); err != nil {
		return nil, err
	}
	if out.RefreshToken, err = s.Seal(ns, label(t.ServerName, "refresh"), []byte(t.RefreshToken)); err != nil {
		return nil, err
	}
	return &out, nil
}

// Get opens an agent project's tokens.
func (r *Repo) Get(ctx context.Context, projectID, serverName string) (*persistence.MCPOAuthToken, error) {
	t, err := r.MCPOAuthTokenRepository.Get(ctx, projectID, serverName)
	ns, agent := namespaceOf(projectID)
	if err != nil || !agent {
		return t, err
	}
	return r.open(ns, t)
}

// ListForProject opens an agent project's tokens.
func (r *Repo) ListForProject(ctx context.Context, projectID string) ([]*persistence.MCPOAuthToken, error) {
	rows, err := r.MCPOAuthTokenRepository.ListForProject(ctx, projectID)
	ns, agent := namespaceOf(projectID)
	if err != nil || !agent {
		return rows, err
	}
	out := make([]*persistence.MCPOAuthToken, 0, len(rows))
	for _, t := range rows {
		o, err := r.open(ns, t)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, nil
}

// Upsert seals an agent project's tokens.
func (r *Repo) Upsert(ctx context.Context, tok *persistence.MCPOAuthToken) error {
	ns, agent := namespaceOf(tok.ProjectID)
	if !agent {
		return r.MCPOAuthTokenRepository.Upsert(ctx, tok)
	}
	sealed, err := r.seal(ns, tok)
	if err != nil {
		return err
	}
	return r.MCPOAuthTokenRepository.Upsert(ctx, sealed)
}

// SwapRefreshToken is the rotation guard for a sealed row (package doc).
func (r *Repo) SwapRefreshToken(ctx context.Context, used string, next *persistence.MCPOAuthToken) (bool, error) {
	ns, agent := namespaceOf(next.ProjectID)
	if !agent {
		return r.MCPOAuthTokenRepository.SwapRefreshToken(ctx, used, next)
	}
	row, err := r.MCPOAuthTokenRepository.Get(ctx, next.ProjectID, next.ServerName)
	if errors.Is(err, persistence.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	s, err := r.sealer(false)
	if err != nil {
		return false, err
	}
	stored, err := s.Open(ns, label(next.ServerName, "refresh"), row.RefreshToken)
	if err != nil {
		return false, err
	}
	if subtle.ConstantTimeCompare(stored, []byte(used)) != 1 {
		return false, nil
	}
	sealed, err := r.seal(ns, next)
	if err != nil {
		return false, err
	}
	return r.MCPOAuthTokenRepository.SwapRefreshToken(ctx, row.RefreshToken, sealed)
}
