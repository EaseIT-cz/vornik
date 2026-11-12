package ui

import (
	"context"
	"sync"

	"vornik.io/vornik/internal/persistence"
)

// memCreds626 is an operator credential service that keeps values in
// memory (GitHub #626: a project integration's secret is stored as an
// operator credential, audited as Source "project-setup").
type memCreds626 struct {
	mu     sync.Mutex
	values map[string]string
	who    []persistence.AdminAuditEntry
}

func newMemCreds626() *memCreds626 { return &memCreds626{values: map[string]string{}} }

func (m *memCreds626) Status(context.Context) (CredentialsView, error) { return CredentialsView{}, nil }
func (m *memCreds626) Put(_ context.Context, name string, value []byte, who persistence.AdminAuditEntry) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.values[name] = string(value)
	m.who = append(m.who, who)
	return nil
}
func (m *memCreds626) Delete(context.Context, string, persistence.AdminAuditEntry) error { return nil }
func (m *memCreds626) Refresh(context.Context) (CredentialsRefresh, error) {
	return CredentialsRefresh{}, nil
}
