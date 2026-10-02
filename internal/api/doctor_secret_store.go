package api

import (
	"context"
	"errors"
	"fmt"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/secretstore"
)

// SetAgentSecretStore wires the secret_store_key check: the agent secret
// table and the master key's path (agent-administered Vornik design §8.1).
func (h *DoctorHandlers) SetAgentSecretStore(repo persistence.AgentSecretRepository, keyPath string) {
	h.agentSecrets, h.agentSecretKeyPath = repo, keyPath
}

// checkSecretStoreKey reports whether the stored agent credentials can be
// opened. A missing or wrong key while rows exist is an ERROR naming the key
// path, never a "missing secret" further down the line (plan Review Focus 2).
// One sampled row is enough to tell a wrong key from a right one: every row
// is sealed under keys derived from the same master.
func (h *DoctorHandlers) checkSecretStoreKey(ctx context.Context, _ bool) DoctorCheck {
	const name = "secret_store_key"
	if h.agentSecrets == nil {
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "agent secret store not wired"}
	}
	n, err := h.agentSecrets.CountAll(ctx)
	if err != nil {
		return DoctorCheck{Name: name, Status: "WARNING", Message: "could not count agent credentials: " + err.Error()}
	}
	key, kerr := secretstore.LoadKey(h.agentSecretKeyPath)
	if n == 0 {
		// doctor-vacuous: the check ran and counted zero stored credentials, so
		// there is nothing a missing key could make unreadable.
		if kerr == nil {
			return DoctorCheck{Name: name, Status: "OK", Message: "key present; no agent credentials stored"}
		}
		return DoctorCheck{Name: name, Status: "OK", Message: "no agent credentials stored"}
	}
	if kerr != nil {
		return DoctorCheck{Name: name, Status: "ERROR", Message: fmt.Sprintf(
			"%d agent credential(s) stored but the key at %s is missing or unreadable; restore it from the backup that holds the rows", n, h.agentSecretKeyPath)}
	}
	if err := sampleOpens(ctx, h.agentSecrets, key); err != nil {
		if errors.Is(err, secretstore.ErrKeyUnavailable) {
			return DoctorCheck{Name: name, Status: "ERROR", Message: fmt.Sprintf(
				"the key at %s does not open the stored agent credentials (a different key?)", h.agentSecretKeyPath)}
		}
		return DoctorCheck{Name: name, Status: "WARNING", Message: "could not sample an agent credential: " + err.Error()}
	}
	return DoctorCheck{Name: name, Status: "OK", Message: fmt.Sprintf("%d agent credential(s); key opens them", n)}
}

// errNothingSampled means rows were counted but none could be listed, so no
// credential was opened and the key is unproven either way.
var errNothingSampled = errors.New("rows were counted but none could be listed to sample")

func sampleOpens(ctx context.Context, repo persistence.AgentSecretRepository, key []byte) error {
	st, err := secretstore.New(repo, key)
	if err != nil {
		return err
	}
	nss, err := repo.ListNamespaces(ctx)
	if err != nil {
		return err
	}
	if len(nss) == 0 {
		return errNothingSampled
	}
	metas, err := st.List(ctx, nss[0])
	if err != nil {
		return err
	}
	if len(metas) == 0 {
		return errNothingSampled
	}
	_, err = st.Get(ctx, nss[0], metas[0].Name)
	return err
}
