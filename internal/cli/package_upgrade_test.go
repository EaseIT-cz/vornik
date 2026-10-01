package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"vornik.io/vornik/internal/agentpackage"
	"vornik.io/vornik/internal/persistence"
)

// `package upgrade` apply layer (package design §8): files first, then one
// provenance swap; write-time checks; restore on a file failure; and a swap
// failure that a re-run completes.

type fakeUpgradeRepo struct {
	rows       map[string][]persistence.PackageContribution // by package
	failSwap   int                                          // fail this many ReplaceContributions calls
	swapCalled int
}

func (f *fakeUpgradeRepo) ContributionsByPackage(_ context.Context, pkg string) ([]persistence.PackageContribution, error) {
	return append([]persistence.PackageContribution(nil), f.rows[pkg]...), nil
}

func (f *fakeUpgradeRepo) ContributionOwner(_ context.Context, kind, rowID string) (string, bool, error) {
	for pkg, rows := range f.rows {
		for _, r := range rows {
			if r.Kind == kind && r.RowID == rowID {
				return pkg, true, nil
			}
		}
	}
	return "", false, nil
}

func (f *fakeUpgradeRepo) ReplaceContributions(_ context.Context, pkg string, rows []persistence.PackageContribution) error {
	f.swapCalled++
	if f.failSwap > 0 {
		f.failSwap--
		return errors.New("database went away")
	}
	f.rows[pkg] = append([]persistence.PackageContribution(nil), rows...)
	return nil
}

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for rel, body := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func readTree(t *testing.T, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && !info.IsDir() {
			rel, _ := filepath.Rel(root, p)
			b, _ := os.ReadFile(p)
			out[filepath.ToSlash(rel)] = string(b)
		}
		return nil
	})
	return out
}

// upgradeFixture: v1.2.0 installed (triage + lead), a v1.3.0 package that
// changes triage, drops lead and adds postmortem.
func upgradeFixture(t *testing.T) (configs, pkgDir string, repo *fakeUpgradeRepo) {
	t.Helper()
	configs = t.TempDir()
	writeTree(t, configs, map[string]string{
		"workflows/incident-triage.md":  "# triage v1\n",
		"role-library/incident-lead.md": "# lead v1\n",
	})
	row := func(kind, id, path, body string) persistence.PackageContribution {
		return persistence.PackageContribution{Kind: kind, RowID: id, Package: "acme-incident-response",
			PackageVersion: "1.2.0", Path: path, ContentHashAtInstall: agentpackage.ContentHash([]byte(body))}
	}
	repo = &fakeUpgradeRepo{rows: map[string][]persistence.PackageContribution{
		"acme-incident-response": {
			row("workflow", "incident-triage", "workflows/incident-triage.md", "# triage v1\n"),
			row("role", "incident-lead", "role-library/incident-lead.md", "# lead v1\n"),
		},
	}}
	pkgDir = t.TempDir()
	writeTree(t, pkgDir, map[string]string{
		"vornik-package.yaml": "package: acme-incident-response\nversion: 1.3.0\ncontributes:\n  workflows: [incident-triage.md, postmortem.md]\n",
		"incident-triage.md":  "# triage v2\n",
		"postmortem.md":       "# pm\n",
	})
	return configs, pkgDir, repo
}

func runUpgradeForTest(t *testing.T, configs, pkgDir string, repo *fakeUpgradeRepo, dryRun bool) (string, error) {
	t.Helper()
	src, err := agentpackage.Open(pkgDir)
	if err != nil {
		t.Fatal(err)
	}
	defer src.Close()
	var out bytes.Buffer
	err = upgradePackage(context.Background(), &out, configs, repo, src, dryRun)
	return out.String(), err
}

func TestUpgradePackage_AppliesTheWholePlan(t *testing.T) {
	configs, pkgDir, repo := upgradeFixture(t)
	out, err := runUpgradeForTest(t, configs, pkgDir, repo, false)
	if err != nil {
		t.Fatalf("upgrade: %v\n%s", err, out)
	}
	tree := readTree(t, configs)
	want := map[string]string{"workflows/incident-triage.md": "# triage v2\n", "workflows/postmortem.md": "# pm\n"}
	if len(tree) != len(want) || tree["workflows/incident-triage.md"] != want["workflows/incident-triage.md"] || tree["workflows/postmortem.md"] != want["workflows/postmortem.md"] {
		t.Fatalf("tree = %v, want %v", tree, want)
	}
	rows := repo.rows["acme-incident-response"]
	sort.Slice(rows, func(i, j int) bool { return rows[i].Path < rows[j].Path })
	if len(rows) != 2 || rows[0].PackageVersion != "1.3.0" {
		t.Fatalf("provenance = %+v", rows)
	}
	if !strings.Contains(out, "1.2.0 → 1.3.0") {
		t.Fatalf("output does not show the version arrow:\n%s", out)
	}
}

func TestUpgradePackage_DryRunWritesNothing(t *testing.T) {
	configs, pkgDir, repo := upgradeFixture(t)
	before := readTree(t, configs)
	if _, err := runUpgradeForTest(t, configs, pkgDir, repo, true); err != nil {
		t.Fatal(err)
	}
	if after := readTree(t, configs); len(after) != len(before) || after["workflows/incident-triage.md"] != before["workflows/incident-triage.md"] {
		t.Fatalf("dry-run changed the tree: %v", after)
	}
	if repo.swapCalled != 0 {
		t.Fatal("dry-run touched provenance")
	}
}

// A file failure mid-apply restores everything changed so far.
func TestUpgradePackage_FileFailureRestoresTheTree(t *testing.T) {
	configs, pkgDir, repo := upgradeFixture(t)
	before := readTree(t, configs)
	orig := upgradeWriteFile
	t.Cleanup(func() { upgradeWriteFile = orig })
	calls := 0
	upgradeWriteFile = func(path string, b []byte, exclusive bool) error {
		calls++
		if calls == 2 { // the second write: adding postmortem.md, after triage was replaced
			return errors.New("disk full")
		}
		return orig(path, b, exclusive)
	}
	_, err := runUpgradeForTest(t, configs, pkgDir, repo, false)
	if err == nil || !strings.Contains(err.Error(), "disk full") {
		t.Fatalf("err = %v, want the write failure", err)
	}
	after := readTree(t, configs)
	if len(after) != len(before) || after["workflows/incident-triage.md"] != "# triage v1\n" || after["role-library/incident-lead.md"] != "# lead v1\n" {
		t.Fatalf("tree not restored: %v", after)
	}
	if repo.swapCalled != 0 {
		t.Fatal("provenance was swapped after a file failure")
	}
}

// An added path that appears between plan and write stops the upgrade (O_EXCL).
func TestUpgradePackage_AddedPathAppearingStopsTheUpgrade(t *testing.T) {
	configs, pkgDir, repo := upgradeFixture(t)
	orig := upgradeBeforeWrite
	t.Cleanup(func() { upgradeBeforeWrite = orig })
	upgradeBeforeWrite = func(rel string) {
		if rel == "workflows/postmortem.md" {
			writeTree(t, configs, map[string]string{rel: "# someone else\n"})
		}
	}
	_, err := runUpgradeForTest(t, configs, pkgDir, repo, false)
	if err == nil {
		t.Fatal("the upgrade overwrote a file that appeared after planning")
	}
	tree := readTree(t, configs)
	if tree["workflows/postmortem.md"] != "# someone else\n" {
		t.Fatalf("the late file was overwritten: %q", tree["workflows/postmortem.md"])
	}
	if tree["workflows/incident-triage.md"] != "# triage v1\n" {
		t.Fatalf("the replaced file was not restored: %q", tree["workflows/incident-triage.md"])
	}
}

// A provenance swap failure leaves the new tree with the old provenance; the
// message says how to finish, and the re-run completes (adopt + reclaim).
func TestUpgradePackage_SwapFailureThenRerunCompletes(t *testing.T) {
	configs, pkgDir, repo := upgradeFixture(t)
	repo.failSwap = 1
	_, err := runUpgradeForTest(t, configs, pkgDir, repo, false)
	if err == nil {
		t.Fatal("a failed swap must be an error")
	}
	for _, want := range []string{"re-run", "[edited]", "uninstall will refuse"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("swap-failure message %q lacks %q", err.Error(), want)
		}
	}
	out, err := runUpgradeForTest(t, configs, pkgDir, repo, false)
	if err != nil {
		t.Fatalf("re-run: %v\n%s", err, out)
	}
	rows := repo.rows["acme-incident-response"]
	if len(rows) != 2 || rows[0].PackageVersion != "1.3.0" {
		t.Fatalf("provenance after the re-run = %+v", rows)
	}
}

// A pure version bump (identical files) is not a no-op: provenance must
// record the new version, and no file is written (review-20261001-df89).
func TestUpgradePackage_PureVersionBumpRecordsTheVersion(t *testing.T) {
	configs, _, repo := upgradeFixture(t)
	pkgDir := t.TempDir()
	writeTree(t, pkgDir, map[string]string{
		"vornik-package.yaml": "package: acme-incident-response\nversion: 1.2.1\ncontributes:\n  workflows: [incident-triage.md]\n  roles: [incident-lead.md]\n",
		"incident-triage.md":  "# triage v1\n",
		"incident-lead.md":    "# lead v1\n",
	})
	before := readTree(t, configs)
	if _, err := runUpgradeForTest(t, configs, pkgDir, repo, false); err != nil {
		t.Fatal(err)
	}
	if after := readTree(t, configs); len(after) != len(before) || after["workflows/incident-triage.md"] != before["workflows/incident-triage.md"] {
		t.Fatalf("a pure version bump changed files: %v", after)
	}
	for _, r := range repo.rows["acme-incident-response"] {
		if r.PackageVersion != "1.2.1" {
			t.Fatalf("row %s kept version %q", r.RowID, r.PackageVersion)
		}
	}
}
