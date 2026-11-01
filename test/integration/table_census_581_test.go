//go:build integration
// +build integration

package integration_test

import (
	"sort"
	"testing"

	"github.com/lib/pq"

	"vornik.io/vornik/internal/persistence"
)

// GitHub #581 D5 (enterprise-packaging-design.md, amendment 2026-10-29): the
// owner export empties every SecretMaterial table and exports the rest, so
// every table a fully migrated database holds must be in the census; and no
// foreign key may reference a SecretMaterial table, because pg_dump adds
// foreign keys after it loads the data, and a row referencing an excluded
// one would fail the export's restore (review aaef, checked here).
func TestIntegration_TableCensus581_ClassifiesEveryTable(t *testing.T) {
	db := connectDB(t)
	t.Cleanup(func() { _ = db.Close() })

	rows, err := db.Query(`SELECT table_name FROM information_schema.tables
		WHERE table_schema = 'public' AND table_type = 'BASE TABLE'`)
	if err != nil {
		t.Fatal(err)
	}
	var missing []string
	seen := map[string]bool{}
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			t.Fatal(err)
		}
		seen[name] = true
		if _, ok := persistence.TableCensus[name]; !ok {
			missing = append(missing, name)
		}
	}
	_ = rows.Close()
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("%d table(s) not in persistence.TableCensus: %v. Classify each as OwnerData or SecretMaterial (it reaches the owner export otherwise)", len(missing), missing)
	}
	var stale []string
	for name := range persistence.TableCensus {
		if !seen[name] {
			stale = append(stale, name)
		}
	}
	sort.Strings(stale)
	if len(stale) > 0 {
		t.Errorf("census entries with no table in a fully migrated database: %v", stale)
	}
	t.Logf("census: %d tables examined, %d SecretMaterial", len(seen), len(persistence.SecretMaterialTables()))

	secret := map[string]bool{}
	for _, name := range persistence.SecretMaterialTables() {
		secret[name] = true
	}
	fks, err := db.Query(`SELECT c.conrelid::regclass::text, c.confrelid::regclass::text, c.conname
		FROM pg_constraint c JOIN pg_namespace n ON n.oid = c.connamespace
		WHERE c.contype = 'f' AND n.nspname = 'public'`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = fks.Close() }()
	examined := 0
	for fks.Next() {
		var from, to, name string
		if err := fks.Scan(&from, &to, &name); err != nil {
			t.Fatal(err)
		}
		examined++
		if secret[to] && !secret[from] {
			t.Errorf("foreign key %s: %s references the SecretMaterial table %s; the export's restore would fail on it", name, from, to)
		}
	}
	t.Logf("foreign keys examined: %d", examined)
}

// Commit security review of bab44b9b2: config_apply_journal's pre-images (a
// managed config.yaml holds the instance_admin key literally), proposals'
// diffs and snapshots, audit states and session transcripts went out as
// owner data. Every table with a config-snapshot column must be
// SecretMaterial.
func TestIntegration_TableCensus581_ConfigTextIsSecretMaterial(t *testing.T) {
	db := connectDB(t)
	t.Cleanup(func() { _ = db.Close() })
	// By pattern, not a list (commit security review of 41e07408b): a list
	// names the columns its author thought of.
	rows, err := db.Query(`SELECT table_name, column_name FROM information_schema.columns
		WHERE table_schema = 'public' AND column_name LIKE ANY($1)`,
		pq.Array([]string{"%yaml%", "%diff%", "%snapshot%", "%config%", "%transcript%", "%pre_image%", "%before_state%", "%after_state%", "%apply_content%", "%bundle%", "%envelope%"}))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	n, reviewed := 0, 0
	for rows.Next() {
		var table, column string
		if err := rows.Scan(&table, &column); err != nil {
			t.Fatal(err)
		}
		n++
		if e, ok := persistence.TableCensus[table]; ok && e.Class == persistence.SecretMaterial {
			continue
		}
		if _, ok := persistence.ReviewedOwnerTextColumns[table+"."+column]; ok {
			reviewed++
			continue
		}
		t.Errorf("%s.%s is named like config text, its table is not SecretMaterial, and it is not in ReviewedOwnerTextColumns: classify the table or review the column", table, column)
	}
	if n == 0 {
		t.Fatal("no column named like config text found; the rule examined nothing")
	}
	t.Logf("config-text columns examined: %d (%d in reviewed owner-data columns, the rest in SecretMaterial tables)", n, reviewed)
}
