package brokergrants_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"vornik.io/vornik/internal/brokergrants"
	"vornik.io/vornik/internal/persistence"
)

type legacyNumericGrants struct {
	persistence.BrokerGrantRepository
	hash string
}

func (r legacyNumericGrants) ListForAction(ctx context.Context, project, workflow, action string) ([]*persistence.BrokerStandingGrant, error) {
	rows, err := r.BrokerGrantRepository.ListForAction(ctx, project, workflow, action)
	for i, row := range rows {
		copyRow := *row
		copyRow.KeyHash = r.hash
		rows[i] = &copyRow
	}
	return rows, err
}

func TestAuditLegacyNumericGrantRequiresFreshApproval(t *testing.T) {
	e := newEnv(t)
	p := e.props["ns1--mail/send_reply"]
	p.Standing.Key = append(p.Standing.Key, "id")
	p.ArgsSchema["properties"].(map[string]any)["id"] = map[string]any{"type": "integer"}
	e.props["ns1--mail/send_reply"] = p
	const args = `{"to":"a@x.com","id":9007199254740992,"body":"hello"}`
	g := e.seed(args, 1, 5)
	k, err := brokergrants.KeyOf(p.Standing.Key, p.Destinations(), []byte(args))
	if err != nil {
		t.Fatal(err)
	}
	if g.KeyHash != k.Hash {
		t.Fatal("numeric grant creation did not store the matching key hash")
	}
	oldHash := sha256.Sum256(k.Canonical)
	legacy := e.rebuild(legacyNumericGrants{BrokerGrantRepository: e.grants, hash: hex.EncodeToString(oldHash[:])})
	a := e.action("ns1--mail", args)
	if covered, err := legacy.Cover(e.ctx, a); err != nil || covered {
		t.Fatalf("legacy numeric grant must miss: covered=%v err=%v", covered, err)
	}
	if e.state(a.ActionID).Status != persistence.BrokerActionPending {
		t.Fatal("legacy miss approved an action")
	}
	stored, err := e.grants.Get(e.ctx, g.ID)
	if err != nil || stored.UsesLeft != 5 {
		t.Fatalf("legacy miss consumed a grant use: %v", err)
	}
	if covered, err := e.svc.Cover(e.ctx, a); err != nil || !covered {
		t.Fatalf("fresh exact numeric grant must cover: covered=%v err=%v", covered, err)
	}
}
