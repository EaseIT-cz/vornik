package service

// GitHub #117 step 2 commit 1: the daemon signs Art 50(2) marks with a key
// derived from the secret store's master key, and records the public key so
// marks stay verifiable (Art 50 design §14.7, §14.8 B).

import (
	"bytes"
	"context"
	"sync"
	"testing"

	"vornik.io/vornik/internal/aimark"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/secretstore"
)

type memMarks struct {
	mu   sync.Mutex
	keys []persistence.AIMarkKey
	rows []persistence.AIOutputMark
}

func (m *memMarks) Record(_ context.Context, r persistence.AIOutputMark) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows = append(m.rows, r)
	return nil
}
func (m *memMarks) RecordKey(_ context.Context, k persistence.AIMarkKey) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.keys = append(m.keys, k)
	return nil
}

func TestBuildAIMarker_NoSecretStoreMeansNoMarker(t *testing.T) {
	if m := buildAIMarker(context.Background(), nil, &memMarks{}, func() string { return "x" }, nil); m != nil {
		t.Fatal("without a secret store there is no key, so no marker")
	}
}

func TestBuildAIMarker_SignsWithTheDerivedKeyAndRegistersIt(t *testing.T) {
	st, err := secretstore.New(nil, bytes.Repeat([]byte{7}, 32))
	if err != nil {
		t.Fatal(err)
	}
	ledger := &memMarks{}
	m := buildAIMarker(context.Background(), st, ledger, func() string { return "2026.11.1" }, nil)
	if m == nil {
		t.Fatal("no marker")
	}
	if len(ledger.keys) != 1 {
		t.Fatalf("registered keys %d, want 1", len(ledger.keys))
	}
	compact, _, err := m.New(context.Background(), []byte("hello"), "chat_api", "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := aimark.Verify(compact, aimark.KeysFrom(ledger.keys)); err != nil {
		t.Fatalf("a mark does not verify with the registered key: %v", err)
	}
	seed, _ := st.AIMarkSigningSeed()
	if aimark.NewSigner(seed).KID() != ledger.keys[0].KID {
		t.Error("the registered key is not the store's derived key")
	}
}
