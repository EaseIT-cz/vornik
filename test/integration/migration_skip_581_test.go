//go:build integration
// +build integration

package integration_test

import (
	"database/sql"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #581 D7 (enterprise-packaging-design.md, amendment 2026-10-29): the
// release skip the daemon states is tested from sampled schema prefixes. A
// database is built at an earlier schema (the first k migrations, in version
// order, what a release with those migrations left), then the full runner
// runs, as an upgrade to this binary would; it must succeed and leave the
// same schema as a fresh database. A prefix not sampled is not tested.
func TestIntegration_MigrationSkip581_FromSampledPrefixes(t *testing.T) {
	if testing.Short() {
		t.Skip("builds one database per sampled prefix")
	}
	all := append([]persistence.Migration(nil), persistence.DefaultMigrations...)
	sort.Slice(all, func(i, j int) bool { return all[i].Version < all[j].Version })

	admin := connectDB(t)
	t.Cleanup(func() { _ = admin.Close() })

	start := time.Now()
	want := schemaFingerprint(t, freshDatabase(t, admin, "full", all, nil))
	t.Logf("fresh database: %d migrations, %d schema rows, %s", len(all), len(want), time.Since(start).Round(time.Millisecond))

	for _, k := range skipPrefixes(len(all)) {
		start := time.Now()
		db := freshDatabase(t, admin, fmt.Sprintf("p%d", k), all[:k], all)
		if got := schemaFingerprint(t, db); strings.Join(got, "\n") != strings.Join(want, "\n") {
			t.Errorf("prefix %d (through migration %d): the upgraded schema differs from a fresh one:\n%s",
				k, all[k-1].Version, fingerprintDiff(want, got))
		}
		t.Logf("prefix %d (through migration %d): upgraded in %s", k, all[k-1].Version, time.Since(start).Round(time.Millisecond))
	}
}

// skipPrefixes are the sampled prefix lengths: every 20th, and each of the
// newest 20, where the releases a fleet may still run sit. k never covers the
// whole list (that is the fresh database) and is never 0.
func skipPrefixes(n int) []int {
	seen := map[int]bool{}
	var out []int
	add := func(k int) {
		if k >= 1 && k < n && !seen[k] {
			seen[k] = true
			out = append(out, k)
		}
	}
	for k := 20; k < n; k += 20 {
		add(k)
	}
	for k := n - 20; k < n; k++ {
		add(k)
	}
	sort.Ints(out)
	return out
}

// freshDatabase creates a throwaway database on the lane's server, applies
// first, then (when non-nil) then, each with its own runner, and drops the
// database at the end of the test.
func freshDatabase(t *testing.T, admin *sql.DB, suffix string, first, then []persistence.Migration) *sql.DB {
	t.Helper()
	name := "vornik_it_skip_581_" + suffix
	if _, err := admin.Exec(`DROP DATABASE IF EXISTS ` + name + ` WITH (FORCE)`); err != nil {
		t.Fatalf("drop %s: %v", name, err)
	}
	if _, err := admin.Exec(`CREATE DATABASE ` + name); err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	u, err := url.Parse(getTestDBURL())
	if err != nil {
		t.Fatal(err)
	}
	u.Path = "/" + name
	db, err := sql.Open("postgres", u.String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = db.Close()
		_, _ = admin.Exec(`DROP DATABASE IF EXISTS ` + name + ` WITH (FORCE)`)
	})
	if err := runMigrations(persistence.NewMigrationRunnerFor(db, first), 10*time.Minute); err != nil {
		t.Fatalf("%s: build the earlier schema: %v", name, err)
	}
	if then != nil {
		if err := runMigrations(persistence.NewMigrationRunnerFor(db, then), 10*time.Minute); err != nil {
			t.Fatalf("%s: upgrade to this binary: %v", name, err)
		}
	}
	return db
}

// schemaFingerprint lists the public schema's columns, indexes, constraints
// and applied migration versions, sorted, so two databases compare by
// content and not by the order columns were added.
func schemaFingerprint(t *testing.T, db *sql.DB) []string {
	t.Helper()
	var out []string
	for _, q := range []string{
		`SELECT 'col ' || table_name || '.' || column_name || ' ' || data_type || ' null=' || is_nullable || ' default=' || COALESCE(column_default, '')
		   FROM information_schema.columns WHERE table_schema = 'public'`,
		`SELECT 'idx ' || indexdef FROM pg_indexes WHERE schemaname = 'public'`,
		`SELECT 'con ' || c.conrelid::regclass || ' ' || c.conname || ' ' || pg_get_constraintdef(c.oid)
		   FROM pg_constraint c JOIN pg_namespace n ON n.oid = c.connamespace WHERE n.nspname = 'public'`,
		`SELECT 'mig ' || version FROM migrations`,
	} {
		rows, err := db.Query(q)
		if err != nil {
			t.Fatalf("fingerprint: %v", err)
		}
		for rows.Next() {
			var s string
			if err := rows.Scan(&s); err != nil {
				t.Fatal(err)
			}
			out = append(out, s)
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		_ = rows.Close()
	}
	sort.Strings(out)
	return out
}

func fingerprintDiff(want, got []string) string {
	w, g := map[string]bool{}, map[string]bool{}
	for _, s := range want {
		w[s] = true
	}
	for _, s := range got {
		g[s] = true
	}
	var b strings.Builder
	for _, s := range want {
		if !g[s] {
			fmt.Fprintf(&b, "  - %s\n", s)
		}
	}
	for _, s := range got {
		if !w[s] {
			fmt.Fprintf(&b, "  + %s\n", s)
		}
	}
	return b.String()
}
