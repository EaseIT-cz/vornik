package service

import (
	"context"
	"errors"
	"strings"

	"vornik.io/vornik/internal/agenttokens"
	"vornik.io/vornik/internal/executor"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/secretstore"
)

// The one-time seal pass (GitHub #571 U3a; mcp-server-authentication-design.md
// §15.2 A3): every MCP OAuth token row written before U3a is sealed at
// startup, before the MCP subsystem wires or refreshes anything. The read path
// refuses an unsealed token whatever this pass did, so a failed pass costs a
// "reconnect", never a plaintext token served.

// sealPassLockServer is the reserved server name whose refresh lock
// serialises the pass across daemons sharing a database
// (persistence.SealPassLockServer says why it cannot collide).
const sealPassLockServer = persistence.SealPassLockServer

// tokenSealCounts is what one pass did.
type tokenSealCounts struct {
	Sealed      int // plaintext rows now sealed
	Quarantined int // sealed values that did not open: tokens emptied, needs_reconnect set
	Lost        int // rows a concurrent writer changed first
	Refused     bool
}

func isSealed(v string) bool { return strings.HasPrefix(v, secretstore.SealedPrefix) }

// sealExistingTokens runs the pass under the cross-daemon lock.
func (c *Container) sealExistingTokens(ctx context.Context) tokenSealCounts {
	var out tokenSealCounts
	if c.repos == nil || c.repos.MCPOAuthTokens == nil {
		return out
	}
	inner := c.repos.MCPOAuthTokens
	err := inner.WithRefreshLock(ctx, "", sealPassLockServer, "", func(ctx context.Context) error {
		out = c.sealPass(ctx, inner)
		c.sealTaskCredentials(ctx)
		return nil
	})
	if err != nil {
		c.Logger.Error().Err(err).Msg("token seal pass: the lock failed; unsealed grants will report reconnect")
	}
	return out
}

func (c *Container) sealPass(ctx context.Context, inner persistence.MCPOAuthTokenRepository) tokenSealCounts {
	var out tokenSealCounts
	rows, err := inner.ListAll(ctx)
	if err != nil {
		c.Logger.Error().Err(err).Msg("token seal pass: listing grants failed")
		return out
	}
	var plain, sealed []*persistence.MCPOAuthToken
	for _, r := range rows {
		switch {
		case hasPlaintextToken(r):
			plain = append(plain, r)
		case isSealed(r.AccessToken) || isSealed(r.RefreshToken):
			sealed = append(sealed, r)
		}
	}
	if len(plain) == 0 && len(sealed) == 0 {
		return out
	}
	wrapper := c.agentSealedTokens()
	if !c.sealPassKeyReady(ctx, len(plain) > 0, len(sealed) > 0) {
		out.Refused = true
		return out
	}
	// The key is wrong only when it opens none of the sealed values; one that
	// does not open under a key that opens others is a bad row (quarantined).
	opened := 0
	var bad []*persistence.MCPOAuthToken
	for _, r := range sealed {
		if _, err := wrapper.Get(ctx, r.ProjectID, r.ServerName, r.OwnerUserID); err == nil {
			opened++
		} else {
			bad = append(bad, r)
		}
	}
	if len(sealed) > 0 && opened == 0 {
		c.Logger.Error().Int("sealed_rows", len(sealed)).
			Msg("token seal pass: this host's store.key opens none of the sealed grants; it is not the key they were sealed with, so nothing is sealed (copy the key the other hosts use)")
		out.Refused = true
		return out
	}
	for _, r := range bad {
		if c.quarantineToken(ctx, inner, r) {
			out.Quarantined++
		}
	}
	for _, r := range plain {
		switch won, err := c.sealOneRow(ctx, inner, wrapper, r); {
		case err != nil:
			c.Logger.Error().Err(err).Str("project_id", r.ProjectID).Str("server", r.ServerName).Msg("token seal pass: sealing a grant failed")
		case won:
			out.Sealed++
		default:
			out.Lost++
		}
	}
	c.Logger.Info().Int("sealed", out.Sealed).Int("quarantined", out.Quarantined).Int("changed_meanwhile", out.Lost).
		Msg("token seal pass: done")
	return out
}

func hasPlaintextToken(r *persistence.MCPOAuthToken) bool {
	return (r.AccessToken != "" && !isSealed(r.AccessToken)) || (r.RefreshToken != "" && !isSealed(r.RefreshToken))
}

// sealPassKeyReady decides whether this host may seal (§15.2 A3: the first key
// is minted once, in the database's order). Under the pass's lock: a host with
// no key mints one only when nothing in the database is sealed yet.
func (c *Container) sealPassKeyReady(ctx context.Context, needSeal, anySealed bool) bool {
	if src := c.currentSecretSource(); src.Store != nil {
		// The key must also open the agent secrets sealed under it: a host
		// holding another host's key would otherwise seal the token rows
		// under a key the database's other sealed values do not use.
		if c.agentSecretsExist(ctx) {
			// Only a proven key seals: nothing to sample is no evidence
			// either way, every other failure (a wrong key, a listing
			// error) refuses.
			if err := src.Store.SampleOpens(ctx); err != nil && !errors.Is(err, secretstore.ErrNothingSampled) {
				c.Logger.Error().Err(err).Msg("token seal pass: this host's store.key could not be shown to open the stored agent secrets; nothing is sealed (if the key is not the one they were sealed with, copy the key the other hosts use)")
				return false
			}
		}
		return true
	}
	if anySealed || c.agentSecretsExist(ctx) {
		c.Logger.Error().Msg("token seal pass: sealed values exist but this host has no store.key; nothing is sealed (copy the key the other hosts use)")
		return false
	}
	if !needSeal {
		return false
	}
	if _, err := c.secretStoreForWrite(); err != nil {
		c.Logger.Error().Err(err).Msg("token seal pass: creating the secret store key failed")
		return false
	}
	return true
}

func (c *Container) agentSecretsExist(ctx context.Context) bool {
	if c.repos == nil || c.repos.AgentSecrets == nil {
		return false
	}
	n, err := c.repos.AgentSecrets.CountAll(ctx)
	return err != nil || n > 0 // an unknown count is treated as "exists": keep, never mint
}

// sealOneRow seals a row's plaintext fields (a field already sealed is opened
// first, so it is not sealed twice) and replaces them only while the row still
// holds what was read.
func (c *Container) sealOneRow(ctx context.Context, inner persistence.MCPOAuthTokenRepository, wrapper *agenttokens.Repo, r *persistence.MCPOAuthToken) (bool, error) {
	next, err := wrapper.SealRow(r)
	if err != nil {
		return false, err
	}
	return inner.ReplaceTokens(ctx, r.ProjectID, r.ServerName, r.OwnerUserID, r.AccessToken, r.RefreshToken, next.AccessToken, next.RefreshToken, false)
}

// quarantineToken empties a grant whose sealed value does not open and marks
// it for reconnect, so one bad row costs that grant alone.
func (c *Container) quarantineToken(ctx context.Context, inner persistence.MCPOAuthTokenRepository, r *persistence.MCPOAuthToken) bool {
	// One statement empties the tokens and sets needs_reconnect, so a grant
	// can never end up empty and unflagged.
	won, err := inner.ReplaceTokens(ctx, r.ProjectID, r.ServerName, r.OwnerUserID, r.AccessToken, r.RefreshToken, "", "", true)
	if err != nil || !won {
		return false
	}
	c.Logger.Warn().Str("project_id", r.ProjectID).Str("server", r.ServerName).
		Msg("token seal pass: a sealed grant does not open with this key; it was emptied and needs reconnecting")
	return true
}

// sealedTaskCredentials is the task_credentials store every consumer uses
// (the executor, the Telegram bot, the UI): values sealed in the operator
// domain (GitHub #578 B1). The key is minted on first write.
func (c *Container) sealedTaskCredentials() persistence.TaskCredentialRepository {
	if c.repos == nil || c.repos.TaskCredentials == nil {
		return nil
	}
	return executor.NewSealedTaskCredentials(c.repos.TaskCredentials,
		func() (executor.OperatorSealer, error) { return c.secretStoreForWrite() }, &c.Logger)
}

// sealTaskCredentials is the pass's second part (GitHub #578 B3), under the
// same lock and the same key decision: every plaintext task credential is
// sealed in place, and its copy in the tool audit replaced by the marker.
func (c *Container) sealTaskCredentials(ctx context.Context) {
	if c.repos == nil {
		return
	}
	pass, ok := c.repos.TaskCredentials.(persistence.TaskCredentialSealPass)
	if !ok {
		return
	}
	rows, err := pass.ListAll(ctx)
	if err != nil {
		c.Logger.Error().Err(err).Msg("credential seal pass: listing task credentials failed")
		return
	}
	var plain, sealed []*persistence.TaskCredential
	for _, r := range rows {
		switch {
		case r.Value == "":
		case isSealed(r.Value):
			sealed = append(sealed, r)
		default:
			plain = append(plain, r)
		}
	}
	if len(plain) == 0 {
		return
	}
	if !c.sealPassKeyReady(ctx, true, len(sealed) > 0) {
		return
	}
	store := c.currentSecretSource().Store
	if store == nil {
		return
	}
	if len(sealed) > 0 && !anyCredentialOpens(store, sealed) {
		c.Logger.Error().Int("sealed_rows", len(sealed)).
			Msg("credential seal pass: this host's store.key opens none of the sealed task credentials; nothing is sealed (copy the key the other hosts use)")
		return
	}
	scrub, _ := c.repos.ToolAudit.(persistence.ToolAuditValueScrubber)
	var done, scrubbed int
	for _, r := range plain {
		next, err := store.SealOperator(executor.TaskCredentialLabel(r), []byte(r.Value))
		if err != nil {
			c.Logger.Error().Err(err).Str("credential_id", r.ID).Msg("credential seal pass: sealing failed")
			continue
		}
		won, err := pass.ReplaceValue(ctx, r.ID, r.Value, next)
		if err != nil {
			c.Logger.Error().Err(err).Str("credential_id", r.ID).Msg("credential seal pass: storing the sealed value failed")
			continue
		}
		if !won {
			continue // a writer changed the row first; its value is sealed already
		}
		done++
		if scrub != nil {
			for _, form := range executor.CarriedCredentialForms(r.Value) {
				n, err := scrub.ReplaceOutputValue(ctx, r.ExecutionID, r.Tool, form, executor.CarriedCredentialMarker)
				if err != nil {
					c.Logger.Error().Err(err).Str("credential_id", r.ID).Msg("credential seal pass: scrubbing the audit copy failed")
				}
				scrubbed += int(n)
			}
		}
	}
	c.Logger.Info().Int("sealed", done).Int("audit_rows_scrubbed", scrubbed).Msg("credential seal pass: done")
}

func anyCredentialOpens(store *secretstore.Store, sealed []*persistence.TaskCredential) bool {
	for _, r := range sealed {
		if _, err := store.OpenOperator(executor.TaskCredentialLabel(r), r.Value); err == nil {
			return true
		}
	}
	return false
}
