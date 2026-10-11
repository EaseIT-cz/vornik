package brokeractions

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"
	"vornik.io/vornik/internal/persistence"
)

func TestActionContextCopiesPersistedVector(t *testing.T) {
	a := &persistence.BrokerAction{ProjectID: "hermes--mail", AccountGuard: persistence.AccountDecisionGuard{{Scope: "daemon", Epoch: 1}, {Scope: "ns:hermes", Epoch: 2}}}
	ctx := withActionContext(context.Background(), a)
	a.AccountGuard[0].Epoch = 99
	p, v, ok := ActionContext(ctx)
	if !ok || p != a.ProjectID || v[0].Epoch != 1 {
		t.Fatal("worker did not copy persisted vector")
	}
	v[0].Epoch = 88
	_, fresh, _ := ActionContext(ctx)
	if fresh[0].Epoch != 1 {
		t.Fatal("caller can mutate private vector")
	}
	raw, err := json.Marshal(a)
	if err != nil || bytes.Contains(raw, []byte("AccountGuard")) || bytes.Contains(raw, []byte("ns:hermes")) {
		t.Fatalf("vector exposed: %s %v", raw, err)
	}
	if err = json.Unmarshal([]byte(`{"AccountGuard":[{"Scope":"daemon","Epoch":100}]}`), a); err != nil || a.AccountGuard[0].Epoch != 99 {
		t.Fatal("JSON accepted action authority")
	}
}
