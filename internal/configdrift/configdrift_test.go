package configdrift

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// config_template_drift (config-tree drift design, third amendment): the
// deployed tree is preserve-existing, so a template FIX never arrives and
// nothing says so. The installer keeps two baselines — .templates/ (current)
// and .origin/ (as created) — and this package compares the deployed tree
// against them. The CE reporter of issue #61(a) had a dev-pipeline.md frozen a
// week before the `recovery: true` marker the firing check honours.

// fixture writes an installer-shaped tree under a temp configs dir.
type fixture struct {
	t    *testing.T
	root string
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, root: t.TempDir()}
	f.write(".templates/.classes", "workflows\ttunable\tdir\nrole-library\tcanonical\tdir\npricing.yaml\tcanonical\tfile\n")
	f.write(".templates/.stamp", "rev-current\n")
	f.write(".origin/.index", "")
	return f
}

func (f *fixture) write(rel, content string) {
	f.t.Helper()
	p := filepath.Join(f.root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) remove(rel string) {
	f.t.Helper()
	if err := os.Remove(filepath.Join(f.root, rel)); err != nil {
		f.t.Fatal(err)
	}
}

// both writes the same content as template and deployed file.
func (f *fixture) both(rel, content string) {
	f.write(".templates/"+rel, content)
	f.write(rel, content)
}

func fixtureHash(s string) string {
	h := sha256.Sum256([]byte(strings.ReplaceAll(s, "\r", "")))
	return hex.EncodeToString(h[:])
}

// origin records an .origin entry the way the installer does.
func (f *fixture) origin(content, rev, how string) {
	f.t.Helper()
	const rel = "workflows/w.md" // every origin fixture describes w.md
	f.write(".origin/"+rel, content)
	idx := filepath.Join(f.root, ".origin/.index")
	b, _ := os.ReadFile(idx)
	line := rel + "\t" + fixtureHash(content) + "\t" + rev + "\t2026-09-01T00:00:00Z\t" + how + "\n"
	if err := os.WriteFile(idx, append(b, []byte(line)...), 0o644); err != nil {
		f.t.Fatal(err)
	}
}

func (f *fixture) compare() *Report {
	f.t.Helper()
	r, err := Compare(f.root)
	if err != nil {
		f.t.Fatalf("Compare: %v", err)
	}
	return r
}

func findFile(r *Report, rel string) *FileFinding {
	for i := range r.Files {
		if r.Files[i].Rel == rel {
			return &r.Files[i]
		}
	}
	return nil
}

func TestCompare_NoBaselineIsNotEvaluated(t *testing.T) {
	r, err := Compare(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if r.Baseline != BaselineAbsent {
		t.Fatalf("baseline = %v, want absent", r.Baseline)
	}
}

// The #61(a) shape: the template gained a line the deployment never got.
func TestCompare_MissedTemplateLineIsAHardFinding(t *testing.T) {
	f := newFixture(t)
	f.write(".templates/workflows/dev-pipeline.md", "---\nid: dev\n---\nsteps:\n  checkpoint:\n    recovery: true\n    terminal: completed\n")
	f.write("workflows/dev-pipeline.md", "---\nid: dev\n---\nsteps:\n  checkpoint:\n    terminal: completed\n")
	r := f.compare()
	ff := findFile(r, "workflows/dev-pipeline.md")
	if ff == nil || len(ff.Missing) != 1 || strings.TrimSpace(ff.Missing[0]) != "recovery: true" {
		t.Fatalf("finding %+v, want the one missing line", ff)
	}
	if ff.Class != Tunable {
		t.Errorf("class %v, want tunable", ff.Class)
	}
}

// Tuning the deployment added is not a missed change, and a changed value is
// tuning too (the asymmetry rule; a template value change is not surfaced).
func TestCompare_TuningAndValueChangesAreNotHardFindings(t *testing.T) {
	f := newFixture(t)
	f.write(".templates/workflows/w.md", "a: 1\nb: 2\n")
	f.write("workflows/w.md", "a: 1\nb: 5\nextra: tuned\n")
	ff := findFile(f.compare(), "workflows/w.md")
	if ff != nil && len(ff.Missing) != 0 {
		t.Fatalf("tuning read as a missed change: %+v", ff)
	}
}

// Blank lines, and # lines inside YAML front matter, are not behaviour; a #
// line in the BODY is prompt content and is compared (parse rule, not visual).
func TestCompare_FrontMatterCommentsFilteredBodyCommentsNot(t *testing.T) {
	f := newFixture(t)
	f.write(".templates/workflows/w.md", "---\n# why retries are omitted\nid: w\n---\n\n# a heading the model reads\nbody\n")
	f.write("workflows/w.md", "---\nid: w\n---\nbody\n")
	ff := findFile(f.compare(), "workflows/w.md")
	if ff == nil || len(ff.Missing) != 1 || ff.Missing[0] != "# a heading the model reads" {
		t.Fatalf("finding %+v, want only the body comment", ff)
	}
}

func TestCompare_CRLFIsNormalised(t *testing.T) {
	f := newFixture(t)
	f.write(".templates/workflows/w.md", "a\nb\n")
	f.write("workflows/w.md", "a\r\nb\r\n")
	if ff := findFile(f.compare(), "workflows/w.md"); ff != nil && (len(ff.Missing) > 0 || ff.SoftCount > 0) {
		t.Fatalf("CRLF read as drift: %+v", ff)
	}
}

// Denominators over the TEMPLATE side: a whole file the deployment never got is
// the maximal missed change and is named.
func TestCompare_DenominatorsAndMissingFiles(t *testing.T) {
	f := newFixture(t)
	f.both("workflows/a.md", "x\n")
	f.write(".templates/workflows/never-deployed.md", "y\n")
	r := f.compare()
	if r.TemplateFiles != 2 || r.DeployedCompared != 1 {
		t.Errorf("denominators %d/%d, want 2 templates, 1 compared", r.TemplateFiles, r.DeployedCompared)
	}
	if len(r.MissingFiles) != 1 || r.MissingFiles[0] != "workflows/never-deployed.md" {
		t.Errorf("missing files %v", r.MissingFiles)
	}
}

// A canonical asset's divergence is its own class, whatever the direction.
func TestCompare_CanonicalDivergence(t *testing.T) {
	f := newFixture(t)
	f.write(".templates/pricing.yaml", "m: 1\n")
	f.write("pricing.yaml", "m: 2\n")
	ff := findFile(f.compare(), "pricing.yaml")
	if ff == nil || !ff.CanonicalDiverged || ff.Class != Canonical {
		t.Fatalf("finding %+v, want a canonical divergence", ff)
	}
}

// Soft class, EXACT mode: a deployed-only line present in .origin is a line the
// template REMOVED (the max_attempts shape); one absent from .origin is tuning.
func TestCompare_SoftClassExactMode(t *testing.T) {
	f := newFixture(t)
	orig := "steps:\n  s:\n    max_attempts: 3\n    role: coder\n"
	f.origin(orig, "rev-old", "created")
	f.write(".templates/workflows/w.md", "steps:\n  s:\n    role: coder\n")
	f.write("workflows/w.md", "steps:\n  s:\n    max_attempts: 3\n    role: coder\n    note: tuned\n")
	ff := findFile(f.compare(), "workflows/w.md")
	if ff == nil || !ff.Exact || len(ff.RemovedByTemplate) != 1 || strings.TrimSpace(ff.RemovedByTemplate[0]) != "max_attempts: 3" {
		t.Fatalf("finding %+v, want exact mode with the one template removal", ff)
	}
	if ff.OriginRevision != "rev-old" {
		t.Errorf("origin revision %q", ff.OriginRevision)
	}
}

// Vague mode (no .origin — the CE default): a bounded count and the largest
// hunk, never the listing.
func TestCompare_SoftClassVagueModeIsBounded(t *testing.T) {
	f := newFixture(t)
	f.write(".templates/workflows/w.md", "a\n")
	var tuned strings.Builder
	tuned.WriteString("a\n")
	for i := 0; i < 1500; i++ {
		tuned.WriteString("tuned line\n")
	}
	f.write("workflows/w.md", tuned.String())
	ff := findFile(f.compare(), "workflows/w.md")
	if ff == nil || ff.Exact || ff.SoftCount != 1500 {
		t.Fatalf("finding %+v, want vague count 1500", ff)
	}
	if len(ff.SoftLargest) > MaxHunkLines {
		t.Errorf("largest hunk carries %d lines, bound %d", len(ff.SoftLargest), MaxHunkLines)
	}
}

// Entry integrity: an .origin whose content no longer hashes as recorded is not
// evidence — the file degrades to the vague form, with the reason.
func TestCompare_OriginIntegrityFailureDegrades(t *testing.T) {
	f := newFixture(t)
	f.origin("a\nb\n", "rev-old", "created")
	f.write(".origin/workflows/w.md", "tampered\n")
	f.write(".templates/workflows/w.md", "a\n")
	f.write("workflows/w.md", "a\nb\n")
	ff := findFile(f.compare(), "workflows/w.md")
	if ff == nil || ff.Exact || !strings.Contains(ff.VagueReason, "integrity") {
		t.Fatalf("finding %+v, want vague with an integrity reason", ff)
	}
}

// A deployed file sharing no line with its origin is a replacement of different
// ancestry: vague, with the reason.
func TestCompare_GrossReplacementDegrades(t *testing.T) {
	f := newFixture(t)
	// The origin shares nothing with the deployed file, which has deployed-only
	// lines to classify (an insert, not a replace).
	f.origin("p\nq\n", "rev-old", "created")
	f.write(".templates/workflows/w.md", "a\n")
	f.write("workflows/w.md", "a\nx\ny\n")
	ff := findFile(f.compare(), "workflows/w.md")
	if ff == nil || ff.Exact || !strings.Contains(ff.VagueReason, "no line") {
		t.Fatalf("finding %+v, want vague: no shared line", ff)
	}
}

// A template the product stopped shipping, whose deployed copy is still there.
func TestCompare_RemovedTemplateStillDeployed(t *testing.T) {
	f := newFixture(t)
	f.write(".templates/.removed", "workflows/old.md\trev-42\t2026-09-01T00:00:00Z\nworkflows/gone.md\trev-42\t2026-09-01T00:00:00Z\n")
	f.write("workflows/old.md", "still here\n")
	r := f.compare()
	if len(r.Removed) != 1 || r.Removed[0].Rel != "workflows/old.md" || r.Removed[0].Revision != "rev-42" {
		t.Fatalf("removed %+v, want old.md from rev-42 only (gone.md has no deployed copy)", r.Removed)
	}
	// Round 8 N2: the per-file diff iterates the TEMPLATE set, so a removed
	// path is not also inferred as a whole-file soft finding.
	if ff := findFile(r, "workflows/old.md"); ff != nil {
		t.Errorf("a removed template was double-surfaced as a per-file finding: %+v", ff)
	}
}

// Round 8 N4/N5: with no class axis — or one that does not name every
// top-level template entry — no file is routed, so none is rendered as a
// canonical divergence.
func TestCompare_NoTrustworthyAxisRoutesNothing(t *testing.T) {
	for name, classes := range map[string]string{
		"absent":     "",
		"incomplete": "role-library\tcanonical\tdir\n",
	} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			if classes == "" {
				f.remove(".templates/.classes")
			} else {
				f.write(".templates/.classes", classes)
			}
			f.write(".templates/workflows/w.md", "a\nb\n")
			f.write("workflows/w.md", "a\n")
			r := f.compare()
			if r.Baseline != BaselineUnstamped {
				t.Errorf("baseline %v, want unstamped", r.Baseline)
			}
			if len(r.Files) != 0 {
				t.Errorf("a file was routed without a trustworthy axis: %+v", r.Files)
			}
		})
	}
}

func TestCompare_BaselineStates(t *testing.T) {
	f := newFixture(t)
	f.both("workflows/a.md", "x\n")
	if r := f.compare(); r.Baseline != BaselinePresent || r.Stamp != "rev-current" {
		t.Fatalf("stamped baseline: %+v", r)
	}
	f.remove(".templates/.stamp")
	if r := f.compare(); r.Baseline != BaselineUnstamped {
		t.Fatalf("no stamp: %v, want unstamped", r.Baseline)
	}
	f.write(".templates/.stamp", "rev-current\n")
	f.remove(".templates/.classes")
	if r := f.compare(); r.Baseline != BaselineUnstamped {
		t.Fatalf("no .classes: %v, want unstamped (the axis cannot be trusted)", r.Baseline)
	}
}

// The check's own artifacts are never compared as config.
func TestCompare_MetadataIsNotATemplate(t *testing.T) {
	f := newFixture(t)
	f.both("workflows/a.md", "x\n")
	f.write(".templates/.removed", "")
	f.write(".templates/.removed.tmp.4242", "left by a crashed install\n")
	if r := f.compare(); r.TemplateFiles != 1 {
		t.Fatalf("template count %d, want 1 (metadata excluded)", r.TemplateFiles)
	}
}
