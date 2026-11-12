package secretstore

import (
	"bytes"
	"context"
	"errors"
	"testing"
)

// GitHub #625 item 4. Found at: unit, by the doctor test written for item 4.
// A user would observe it at production, in the doctor's secret_store_key
// check (TestCheckSecretStoreKey_NamesOperatorCredentials_625, internal/api,
// is that level): with an operator credential stored,
// SampleOpens picked the reserved namespace, which sorts first, and Get
// refused it ("invalid namespace"), so the check warned on every install that
// had one. It now opens the sampled row directly, and still tells a wrong key.
func TestSampleOpens_AnOperatorRowIsSampled_625(t *testing.T) {
	repo := newMemRepo()
	key := bytes.Repeat([]byte{1}, 32)
	st, err := New(repo, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.PutOperatorCredential(context.Background(), "CONTEXT7_API_KEY", []byte("v"), "console"); err != nil {
		t.Fatal(err)
	}
	if err := st.SampleOpens(context.Background()); err != nil {
		t.Fatalf("SampleOpens with an operator row: %v", err)
	}
	wrong, _ := New(repo, bytes.Repeat([]byte{2}, 32))
	if err := wrong.SampleOpens(context.Background()); !errors.Is(err, ErrKeyUnavailable) {
		t.Fatalf("a wrong key must still be told: %v", err)
	}
}
