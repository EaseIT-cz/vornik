package persistence

import (
	"context"
	"time"
)

// AgentSecretRow is one sealed agent credential (agent-administered Vornik
// design §8.1). Value material is ciphertext only: this package never sees a
// plaintext value, and List never returns key material at all.
type AgentSecretRow struct {
	Namespace       string
	Name            string
	Kind            string // "secret" | "oauth_token"
	Ciphertext      []byte
	Nonce           []byte
	CreatedByDevice string
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// AgentSecretRepository persists sealed agent credentials.
type AgentSecretRepository interface {
	// Upsert stores or replaces (namespace, name); CreatedAt is kept on replace.
	Upsert(ctx context.Context, row AgentSecretRow) error
	// Get returns ErrNotFound when the row does not exist.
	Get(ctx context.Context, namespace, name string) (*AgentSecretRow, error)
	// List returns the namespace's rows without Ciphertext/Nonce, ordered by name.
	List(ctx context.Context, namespace string) ([]AgentSecretRow, error)
	// ListNamespaces returns every namespace holding at least one row, ordered.
	ListNamespaces(ctx context.Context) ([]string, error)
	// Delete removes (namespace, name); a missing row is not an error.
	Delete(ctx context.Context, namespace, name string) error
	// CountAll counts rows across namespaces.
	CountAll(ctx context.Context) (int, error)
}
