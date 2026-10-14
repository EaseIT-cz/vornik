package repotest

import (
	"context"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// RunAgentAdminRefusalSuite pins agent_admin_refusals on both drivers
// (agent-administered design §23.2). GitHub #172 (2026-10-13): a refused
// agent-admin call left no row of its own, so an assistant probing its
// boundaries was invisible to the operator. Namespaces are run-unique because
// the Postgres lane's database is shared.
func RunAgentAdminRefusalSuite(t *testing.T, repo persistence.AgentAdminRefusalRepository) {
	t.Helper()
	ctx := context.Background()
	suffix := strings.ToLower(strings.NewReplacer("_", "", "-", "", ".", "").Replace(uniqueID("r")))
	ns, other := "r"+uniqueTail(suffix), "s"+uniqueTail(suffix)
	base := time.Date(2026, 10, 13, 12, 0, 0, 0, time.UTC)

	rec := func(namespace, verb, rule string, at time.Time) {
		t.Helper()
		err := repo.Record(ctx, persistence.AgentAdminRefusal{
			ID: uniqueID("aar"), ProjectID: namespace + "--home", Namespace: namespace,
			KeyID: "akey-" + namespace, Verb: verb, Rule: rule, CreatedAt: at,
		})
		if err != nil {
			t.Fatalf("Record: %v", err)
		}
	}
	// Sub-second spacing: the newest-first order must hold below one second
	// (the SQLite text form is fixed-width to nine digits).
	rec(ns, "add_api", "url_not_https", base.Add(-2*time.Hour))
	rec(ns, "create_project", "already_exists", base.Add(-30*time.Minute))
	rec(ns, "define_workflow", "out_of_namespace", base.Add(-10*time.Minute))
	rec(ns, "define_workflow", "out_of_namespace", base.Add(-10*time.Minute+500*time.Millisecond))
	rec(other, "remove", "not_found", base.Add(-5*time.Minute))

	t.Run("ListRecent_newest_first_limited_and_isolated", func(t *testing.T) {
		got, err := repo.ListRecent(ctx, ns, 3)
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 3 {
			t.Fatalf("ListRecent returned %d rows, want 3: %+v", len(got), got)
		}
		if !got[0].CreatedAt.Equal(base.Add(-10*time.Minute+500*time.Millisecond)) || got[2].Verb != "create_project" {
			t.Fatalf("not newest first: %+v", got)
		}
		assertRefusalRows(t, got, ns)
	})
	t.Run("CountByRule_since", func(t *testing.T) {
		got, err := repo.CountByRule(ctx, ns, base.Add(-time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || got["out_of_namespace"] != 2 || got["already_exists"] != 1 {
			t.Fatalf("CountByRule = %v, want out_of_namespace:2 already_exists:1", got)
		}
	})
	t.Run("CountByNamespace_since", func(t *testing.T) {
		got, err := repo.CountByNamespace(ctx, base.Add(-time.Hour))
		if err != nil {
			t.Fatal(err)
		}
		// Other tests share the Postgres database: assert only our namespaces.
		if got[ns] != 3 || got[other] != 1 {
			t.Fatalf("CountByNamespace = %v, want %s:3 %s:1", got, ns, other)
		}
	})
	t.Run("Empty_namespace", func(t *testing.T) {
		got, err := repo.ListRecent(ctx, "z"+uniqueTail(suffix), 20)
		if err != nil || len(got) != 0 {
			t.Fatalf("an unknown namespace: %+v, %v", got, err)
		}
		counts, err := repo.CountByRule(ctx, "z"+uniqueTail(suffix), base.Add(-time.Hour))
		if err != nil || len(counts) != 0 {
			t.Fatalf("an unknown namespace's counts: %v, %v", counts, err)
		}
	})
}

// assertRefusalRows checks every row round-trips and belongs to ns.
func assertRefusalRows(t *testing.T, rows []persistence.AgentAdminRefusal, ns string) {
	t.Helper()
	for _, r := range rows {
		if r.Namespace != ns || r.ProjectID != ns+"--home" || r.KeyID != "akey-"+ns || r.ID == "" {
			t.Fatalf("row does not round-trip or leaks another namespace: %+v", r)
		}
	}
}
