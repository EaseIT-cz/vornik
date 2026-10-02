package service

import (
	"context"
	"testing"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/repotest"
)

// The test double must honour the real repository's miss contract
// (repository-miss-contract design): an absent row is ErrNotFound.
func TestAgentSecretDouble_MissContract(t *testing.T) {
	var repo persistence.AgentSecretRepository = &countingSecrets{}
	repotest.AssertMiss(t, "AgentSecretRepository.Get", func() (*persistence.AgentSecretRow, error) {
		return repo.Get(context.Background(), "hermes", "ABSENT")
	})
}
