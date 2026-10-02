package repotest

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// RunAgentSecretSuite pins the agent secret table on both drivers
// (agent-administered Vornik design §8.1). The subtests run in order, each
// building on the rows the previous ones left. Namespaces are unique per run
// because the Postgres lane's database is shared and never truncated; global
// counts are asserted as deltas from a baseline for the same reason.
func RunAgentSecretSuite(t *testing.T, repo persistence.AgentSecretRepository) {
	t.Helper()
	now := time.Now().UTC().Truncate(time.Second)
	suffix := strings.ToLower(strings.NewReplacer("_", "", "-", "", ".", "").Replace(uniqueID("n")))
	nsA, nsB := "a"+uniqueTail(suffix), "b"+uniqueTail(suffix)
	base, err := repo.CountAll(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	h := &agentSecretHarness{repo: repo, ctx: context.Background(), now: now, nsA: nsA, nsB: nsB, base: base,
		row: persistence.AgentSecretRow{Namespace: nsA, Name: "FIO_TOKEN", Kind: "secret",
			Ciphertext: []byte{0, 1, 2, 255}, Nonce: bytes.Repeat([]byte{7}, 12), CreatedByDevice: "dev_1",
			CreatedAt: now, UpdatedAt: now}}
	t.Run("MissContract", func(t *testing.T) { agentSecretMissContract(t, h) })
	t.Run("Upsert_then_Get_round_trips_bytes", func(t *testing.T) { agentSecretRoundTrip(t, h) })
	t.Run("Large_and_NUL_bytes_round_trip", func(t *testing.T) { agentSecretLargeBytes(t, h) })
	t.Run("Upsert_replaces_and_keeps_CreatedAt", func(t *testing.T) { agentSecretReplace(t, h) })
	t.Run("Namespaces_are_separate", func(t *testing.T) { agentSecretNamespacesSeparate(t, h) })
	t.Run("List_is_metadata_only_and_ordered", func(t *testing.T) { agentSecretList(t, h) })
	t.Run("ListNamespaces_is_distinct_and_ordered", func(t *testing.T) { agentSecretListNamespaces(t, h) })
	t.Run("CountAll_and_Delete", func(t *testing.T) { agentSecretCountAndDelete(t, h) })
	t.Run("Kind_is_constrained", func(t *testing.T) { agentSecretKindConstrained(t, h) })
}

type agentSecretHarness struct {
	repo     persistence.AgentSecretRepository
	ctx      context.Context
	now      time.Time
	row      persistence.AgentSecretRow
	nsA, nsB string
	base     int
}

// uniqueTail keeps the run-unique last 12 characters of an ID, within the
// namespace length limit (a namespace is at most 16 characters).
func uniqueTail(s string) string {
	const n = 12
	if len(s) <= n {
		return s
	}
	return s[len(s)-n:]
}

func (h *agentSecretHarness) mustGet(t *testing.T, ns string) *persistence.AgentSecretRow {
	t.Helper()
	got, err := h.repo.Get(h.ctx, ns, h.row.Name)
	if err != nil {
		t.Fatalf("Get(%s, %s): %v", ns, h.row.Name, err)
	}
	return got
}

func agentSecretMissContract(t *testing.T, h *agentSecretHarness) {
	AssertMiss(t, "AgentSecretRepository.Get", func() (*persistence.AgentSecretRow, error) {
		return h.repo.Get(h.ctx, h.nsA, "ABSENT_ROW")
	})
}

func agentSecretRoundTrip(t *testing.T, h *agentSecretHarness) {
	if err := h.repo.Upsert(h.ctx, h.row); err != nil {
		t.Fatal(err)
	}
	got := h.mustGet(t, h.nsA)
	if !bytes.Equal(got.Ciphertext, h.row.Ciphertext) || !bytes.Equal(got.Nonce, h.row.Nonce) || got.Kind != "secret" || got.CreatedByDevice != "dev_1" {
		t.Fatalf("round trip mismatch: %+v", got)
	}
	if !got.CreatedAt.Equal(h.now) {
		t.Fatalf("CreatedAt = %v, want %v", got.CreatedAt, h.now)
	}
}

// Review 20261002-a6af finding 4: the 64 KiB and embedded-NUL property is
// asserted through the real drivers (BYTEA, BLOB), not only the store's
// in-memory double. Restores the base row so later subtests see it.
func agentSecretLargeBytes(t *testing.T, h *agentSecretHarness) {
	big := h.row // a copy (struct value), so h.row is untouched
	big.Ciphertext = bytes.Repeat([]byte{0, 0xff, 'A', 0}, 16*1024+1)
	big.Nonce = []byte{0, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0}
	if err := h.repo.Upsert(h.ctx, big); err != nil {
		t.Fatal(err)
	}
	got := h.mustGet(t, h.nsA)
	if !bytes.Equal(got.Ciphertext, big.Ciphertext) || !bytes.Equal(got.Nonce, big.Nonce) {
		t.Fatalf("large/NUL round trip: got %d ciphertext bytes, want %d", len(got.Ciphertext), len(big.Ciphertext))
	}
	if err := h.repo.Upsert(h.ctx, h.row); err != nil {
		t.Fatal(err)
	}
}

func agentSecretReplace(t *testing.T, h *agentSecretHarness) {
	later := h.row
	later.Ciphertext = []byte{9}
	later.CreatedAt = h.now.Add(time.Hour)
	later.UpdatedAt = h.now.Add(time.Hour)
	if err := h.repo.Upsert(h.ctx, later); err != nil {
		t.Fatal(err)
	}
	got := h.mustGet(t, h.nsA)
	if !bytes.Equal(got.Ciphertext, []byte{9}) || !got.CreatedAt.Equal(h.now) || !got.UpdatedAt.Equal(h.now.Add(time.Hour)) {
		t.Fatalf("replace: %+v", got)
	}
}

func agentSecretNamespacesSeparate(t *testing.T, h *agentSecretHarness) {
	other := h.row
	other.Namespace = h.nsB
	other.Ciphertext = []byte{42}
	if err := h.repo.Upsert(h.ctx, other); err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(h.mustGet(t, h.nsA).Ciphertext, h.mustGet(t, h.nsB).Ciphertext) {
		t.Fatal("namespaces share a row")
	}
}

func agentSecretList(t *testing.T, h *agentSecretHarness) {
	b := h.row
	b.Name = "AAA_FIRST"
	if err := h.repo.Upsert(h.ctx, b); err != nil {
		t.Fatal(err)
	}
	list, err := h.repo.List(h.ctx, h.nsA)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 2 || list[0].Name != "AAA_FIRST" || list[1].Name != "FIO_TOKEN" {
		t.Fatalf("list = %+v", list)
	}
	for _, r := range list {
		if len(r.Ciphertext) != 0 || len(r.Nonce) != 0 {
			t.Fatal("List returned key material")
		}
	}
}

func agentSecretListNamespaces(t *testing.T, h *agentSecretHarness) {
	got, err := h.repo.ListNamespaces(h.ctx)
	if err != nil {
		t.Fatal(err)
	}
	ia, ib := -1, -1
	for i, ns := range got {
		switch ns {
		case h.nsA:
			ia = i
		case h.nsB:
			ib = i
		}
	}
	if ia < 0 || ib < 0 || ia > ib { // nsA < nsB lexically, so it must come first
		t.Fatalf("ListNamespaces = %v, want %s before %s", got, h.nsA, h.nsB)
	}
	for i := 1; i < len(got); i++ {
		if got[i-1] >= got[i] {
			t.Fatalf("ListNamespaces not distinct and ordered: %v", got)
		}
	}
}

func agentSecretCountAndDelete(t *testing.T, h *agentSecretHarness) {
	n, err := h.repo.CountAll(h.ctx)
	if err != nil || n != h.base+3 {
		t.Fatalf("CountAll = %d, %v; want baseline %d + 3", n, err, h.base)
	}
	if err := h.repo.Delete(h.ctx, h.nsA, "AAA_FIRST"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.repo.Get(h.ctx, h.nsA, "AAA_FIRST"); !errors.Is(err, persistence.ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
	if err := h.repo.Delete(h.ctx, h.nsA, "AAA_FIRST"); err != nil {
		t.Fatalf("delete of a missing row must be a no-op: %v", err)
	}
}

func agentSecretKindConstrained(t *testing.T, h *agentSecretHarness) {
	bad := h.row
	bad.Name = "BAD_KIND"
	bad.Kind = "plaintext"
	if err := h.repo.Upsert(h.ctx, bad); err == nil {
		t.Fatal("a kind outside secret|oauth_token was stored")
	}
}
