// Package agenttokens stores MCP OAuth tokens sealed. It wraps the ordinary
// token repository: an agent-namespace project's access and refresh tokens
// are sealed with the secret store's namespace key (agent-administered Vornik
// plan P4.4, design §8.4), and since GitHub #571 (U3a,
// mcp-server-authentication-design.md §15.2) every other row, operator
// projects and the daemon scope, is sealed in the operator domain. Every
// other column stays plaintext, because SQL reads it (the injection path, the
// stale-redirect sweep, the status UI). A read refuses an unsealed token.
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
	"net/url"
	"strings"

	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/secretstore"
)

// Sealer seals and opens values for a namespace; *secretstore.Store is one.
type Sealer interface {
	Seal(ns, label string, plain []byte) (string, error)
	Open(ns, label, sealed string) ([]byte, error)
	// The operator domain, for rows of no agent namespace (#571 A1).
	SealOperator(label string, plain []byte) (string, error)
	OpenOperator(label, sealed string) ([]byte, error)
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

// label binds a namespace-domain value to its server, owner and field. A
// shared row's label keeps its P4.4 shape, unescaped, so no existing value is
// re-sealed. An owned row's label has the distinct prefix OAUTHU_ and every
// segment escaped, because an added segment alone would collide with a
// shared server whose name holds a "/" (GitHub #571 U3b, §15.3a C2): the
// sixth character is "_" in every shared label and "U" in every owned one.
func label(server, owner, field string) string {
	if owner == "" {
		return "OAUTH_" + server + "/" + field
	}
	return "OAUTHU_" + url.PathEscape(server) + "/" + url.PathEscape(owner) + "/" + field
}

// operatorLabel binds an operator-domain value to its project, server, owner
// and field. Every variable segment is percent-escaped, so a "/" inside a
// name cannot forge a segment boundary (#571 A1); an owned row takes the
// prefix OAUTHU/ (§15.3a C2).
func operatorLabel(project, server, owner, field string) string {
	if owner == "" {
		return "OAUTH/" + url.PathEscape(project) + "/" + url.PathEscape(server) + "/" + field
	}
	return "OAUTHU/" + url.PathEscape(project) + "/" + url.PathEscape(server) + "/" + url.PathEscape(owner) + "/" + field
}

// sealField and openField route one token field to the row's domain: the
// namespace domain for an agent project, the operator domain otherwise.
func sealField(s Sealer, t *persistence.MCPOAuthToken, field, plain string) (string, error) {
	if ns, agent := namespaceOf(t.ProjectID); agent {
		return s.Seal(ns, label(t.ServerName, t.OwnerUserID, field), []byte(plain))
	}
	return s.SealOperator(operatorLabel(t.ProjectID, t.ServerName, t.OwnerUserID, field), []byte(plain))
}

func openField(s Sealer, t *persistence.MCPOAuthToken, field, sealed string) ([]byte, error) {
	if ns, agent := namespaceOf(t.ProjectID); agent {
		return s.Open(ns, label(t.ServerName, t.OwnerUserID, field), sealed)
	}
	return s.OpenOperator(operatorLabel(t.ProjectID, t.ServerName, t.OwnerUserID, field), sealed)
}

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

// open returns a copy of t with its tokens opened. An unsealed value is
// refused (secretstore.ErrNotSealed): no read hands out a plaintext token.
func (r *Repo) open(t *persistence.MCPOAuthToken) (*persistence.MCPOAuthToken, error) {
	s, err := r.sealer(false)
	if err != nil {
		return nil, err
	}
	out := *t
	for _, f := range []struct {
		dst  *string
		name string
	}{{&out.AccessToken, "access"}, {&out.RefreshToken, "refresh"}} {
		plain, err := openField(s, t, f.name, *f.dst)
		if err != nil {
			return nil, fmt.Errorf("agenttokens: %s/%s %s token: %w", t.ProjectID, t.ServerName, f.name, err)
		}
		*f.dst = string(plain)
	}
	return &out, nil
}

// seal returns a copy of t with its tokens sealed.
func (r *Repo) seal(t *persistence.MCPOAuthToken) (*persistence.MCPOAuthToken, error) {
	s, err := r.sealer(true)
	if err != nil {
		return nil, err
	}
	out := *t
	if out.AccessToken, err = sealField(s, t, "access", t.AccessToken); err != nil {
		return nil, err
	}
	if out.RefreshToken, err = sealField(s, t, "refresh", t.RefreshToken); err != nil {
		return nil, err
	}
	return &out, nil
}

// Get opens the grant's tokens.
func (r *Repo) Get(ctx context.Context, projectID, serverName, owner string) (*persistence.MCPOAuthToken, error) {
	t, err := r.MCPOAuthTokenRepository.Get(ctx, projectID, serverName, owner)
	if err != nil {
		return t, err
	}
	return r.open(t)
}

// ListForProject opens every grant's tokens.
func (r *Repo) ListForProject(ctx context.Context, projectID string) ([]*persistence.MCPOAuthToken, error) {
	rows, err := r.MCPOAuthTokenRepository.ListForProject(ctx, projectID)
	if err != nil {
		return rows, err
	}
	out := make([]*persistence.MCPOAuthToken, 0, len(rows))
	for _, t := range rows {
		o, err := r.open(t)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, nil
}

// Upsert seals an agent project's tokens.
func (r *Repo) Upsert(ctx context.Context, tok *persistence.MCPOAuthToken) error {
	row, err := r.PrepareSealedToken(tok)
	if err != nil {
		return err
	}
	return r.MCPOAuthTokenRepository.Upsert(ctx, row)
}

// PrepareSealedToken seals a row's tokens in its domain (namespace for an
// agent project, operator otherwise). No row or cache is published by this
// method.
func (r *Repo) PrepareSealedToken(tok *persistence.MCPOAuthToken) (*persistence.MCPOAuthToken, error) {
	if tok == nil {
		return nil, errors.New("agenttokens: nil candidate")
	}
	return r.seal(tok)
}

// SwapRefreshToken is the rotation guard for a sealed row (package doc).
func (r *Repo) SwapRefreshToken(ctx context.Context, used string, next *persistence.MCPOAuthToken) (bool, error) {
	row, err := r.MCPOAuthTokenRepository.Get(ctx, next.ProjectID, next.ServerName, next.OwnerUserID)
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
	stored, err := openField(s, next, "refresh", row.RefreshToken)
	if err != nil {
		return false, err
	}
	if subtle.ConstantTimeCompare(stored, []byte(used)) != 1 {
		return false, nil
	}
	sealed, err := r.seal(next)
	if err != nil {
		return false, err
	}
	return r.MCPOAuthTokenRepository.SwapRefreshToken(ctx, row.RefreshToken, sealed)
}

// SealRow returns t with every token sealed in its domain: a sealed field is
// opened and sealed again, a plaintext one sealed. It never returns a
// plaintext token, so the package's read refusal holds for every caller; it is
// the seal pass's way to seal a row that is partly sealed (GitHub #571 A3).
func (r *Repo) SealRow(t *persistence.MCPOAuthToken) (*persistence.MCPOAuthToken, error) {
	s, err := r.sealer(true)
	if err != nil {
		return nil, err
	}
	plain := *t
	for _, f := range []struct {
		dst  *string
		name string
	}{{&plain.AccessToken, "access"}, {&plain.RefreshToken, "refresh"}} {
		if !strings.HasPrefix(*f.dst, secretstore.SealedPrefix) {
			continue
		}
		v, err := openField(s, t, f.name, *f.dst)
		if err != nil {
			return nil, err
		}
		*f.dst = string(v)
	}
	return r.seal(&plain)
}
