package repotest

import (
	"context"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// sameInstall compares two rows, the time by instant.
func sameInstall(a, b persistence.AgentConnectorInstall) bool {
	at, bt := a.InstalledAt, b.InstalledAt
	a.InstalledAt, b.InstalledAt = time.Time{}, time.Time{}
	return a == b && at.Equal(bt)
}

// RunAgentConnectorInstallSuite pins agent_connector_installs on both
// drivers (GitHub #314, 2026-10-13; agent-administered design §19.14.8 item
// 6): one row per workflow, written when a paired device installed a
// connector; a re-install replaces it; a namespace reads only its own rows;
// a delete removes it and a second delete is not an error. Namespaces are
// run-unique because the Postgres lane's database is shared.
func RunAgentConnectorInstallSuite(t *testing.T, repo persistence.AgentGrantRepository) {
	t.Helper()
	ctx := context.Background()
	suffix := strings.ToLower(strings.NewReplacer("_", "", "-", "", ".", "").Replace(uniqueID("c")))
	ns, other := "c"+uniqueTail(suffix), "d"+uniqueTail(suffix)
	now := time.Now().UTC().Truncate(time.Second)
	wf := ns + "--personal--agenda"
	row := persistence.AgentConnectorInstall{WorkflowID: wf, Namespace: ns, ProjectID: ns + "--personal",
		Connector: "agenda", AccountLabel: "work", DeviceID: "dev_a", InstalledAt: now}

	t.Run("Upsert_list_replace", func(t *testing.T) {
		if err := repo.UpsertConnectorInstall(ctx, row); err != nil {
			t.Fatal(err)
		}
		o := row
		o.WorkflowID, o.Namespace, o.ProjectID = other+"--personal--agenda", other, other+"--personal"
		if err := repo.UpsertConnectorInstall(ctx, o); err != nil {
			t.Fatal(err)
		}
		got, err := repo.ListConnectorInstalls(ctx, ns)
		if err != nil || len(got) != 1 || !sameInstall(got[0], row) {
			t.Fatalf("list %s: %+v, %v", ns, got, err)
		}
		again := row
		again.DeviceID, again.AccountLabel, again.InstalledAt = "dev_b", "default", now.Add(time.Hour)
		if err := repo.UpsertConnectorInstall(ctx, again); err != nil {
			t.Fatal(err)
		}
		got, err = repo.ListConnectorInstalls(ctx, ns)
		if err != nil || len(got) != 1 || !sameInstall(got[0], again) {
			t.Fatalf("after a re-install: %+v, %v", got, err)
		}
	})
	t.Run("Delete_is_idempotent", func(t *testing.T) {
		if err := repo.DeleteConnectorInstall(ctx, wf); err != nil {
			t.Fatal(err)
		}
		if err := repo.DeleteConnectorInstall(ctx, wf); err != nil {
			t.Fatalf("a second delete: %v", err)
		}
		got, err := repo.ListConnectorInstalls(ctx, ns)
		if err != nil || len(got) != 0 {
			t.Fatalf("after delete: %+v, %v", got, err)
		}
		if got, err := repo.ListConnectorInstalls(ctx, other); err != nil || len(got) != 1 {
			t.Fatalf("the other namespace lost its row: %+v, %v", got, err)
		}
	})
}
