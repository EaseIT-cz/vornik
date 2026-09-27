package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// config_template_drift (drift design, third amendment; issue #61(a)): a CE
// operator's dev-pipeline.md predated the `recovery: true` marker the firing
// workflow_onfail_masking check honours, and nothing said the deployed file
// was older than the template.

func driftTree(t *testing.T, files map[string]string) string {
	t.Helper()
	root := t.TempDir()
	for rel, content := range files {
		p := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

const driftClasses = "workflows\ttunable\tdir\nrole-library\tcanonical\tdir\npricing.yaml\tcanonical\tfile\n"

func driftHandler(dir, rev string) *DoctorHandlers {
	return &DoctorHandlers{configDir: dir, buildRevision: func() (string, bool, bool) { return rev, false, rev != "" }}
}

func TestConfigTemplateDrift_SkippedWithoutADirOrABaseline(t *testing.T) {
	if got := (&DoctorHandlers{}).checkConfigTemplateDrift(); got.Status != "SKIPPED" {
		t.Errorf("no dir: %s", got.Status)
	}
	dir := driftTree(t, map[string]string{"workflows/a.md": "x\n"})
	got := driftHandler(dir, "1111111aaaaa").checkConfigTemplateDrift()
	if got.Status != "SKIPPED" || !strings.Contains(got.Message, dir) {
		t.Errorf("no baseline: %s %q — must name the directory it looked in", got.Status, got.Message)
	}
}

// The #61(a) finding, against a current baseline: WARNING naming the file and
// the missing line, with both denominators.
func TestConfigTemplateDrift_MissedTemplateLine(t *testing.T) {
	dir := driftTree(t, map[string]string{
		".templates/.stamp":                    "1111111aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n",
		".templates/.classes":                  driftClasses,
		".templates/workflows/dev-pipeline.md": "steps:\n  checkpoint:\n    recovery: true\n",
		"workflows/dev-pipeline.md":            "steps:\n  checkpoint:\n",
		".templates/workflows/other.md":        "same\n",
		"workflows/other.md":                   "same\n",
	})
	got := driftHandler(dir, "1111111aaaaa").checkConfigTemplateDrift()
	if got.Status != "WARNING" {
		t.Fatalf("status %s: %s", got.Status, got.Message)
	}
	joined := strings.Join(got.Items, "\n")
	if !strings.Contains(joined, "workflows/dev-pipeline.md") || !strings.Contains(joined, "recovery: true") {
		t.Errorf("items do not name the file and line:\n%s", joined)
	}
	if !strings.Contains(got.Message, "2 template") || !strings.Contains(got.Message, "2 deployed") {
		t.Errorf("message lacks the denominators: %q", got.Message)
	}
}

func TestConfigTemplateDrift_CleanIsOKWithDenominators(t *testing.T) {
	dir := driftTree(t, map[string]string{
		".templates/.stamp": "1111111aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n", ".templates/.classes": driftClasses,
		".templates/workflows/a.md": "x\n", "workflows/a.md": "x\n",
	})
	got := driftHandler(dir, "1111111aaaaa").checkConfigTemplateDrift()
	if got.Status != "OK" || !strings.Contains(got.Message, "1 template") {
		t.Fatalf("clean tree: %s %q", got.Status, got.Message)
	}
}

// Precedence (the design's table): a stale or unstamped baseline SUPPRESSES
// tunable findings — reporting "your file predates the template" against an old
// reference is the CE false positive this check exists to explain — but a
// canonical divergence is NEVER suppressed, and is named.
func TestConfigTemplateDrift_StaleAndUnstampedSuppressTunableNotCanonical(t *testing.T) {
	for _, tc := range []struct {
		name  string
		stamp string
		rev   string
		want  string
	}{
		{"stale", "2222222bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n", "3333333ccccc", "stale"},
		{"unstamped", "", "3333333ccccc", "unstamped"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			files := map[string]string{
				".templates/.classes":       driftClasses,
				".templates/workflows/w.md": "a\nb\n",
				"workflows/w.md":            "a\n",
				".templates/pricing.yaml":   "m: 1\n",
				"pricing.yaml":              "m: 2\n",
			}
			if tc.stamp != "" {
				files[".templates/.stamp"] = tc.stamp
			}
			got := driftHandler(driftTree(t, files), tc.rev).checkConfigTemplateDrift()
			joined := strings.Join(got.Items, "\n")
			if got.Status != "WARNING" || !strings.Contains(got.Message, tc.want) {
				t.Fatalf("%s: %s %q", tc.name, got.Status, got.Message)
			}
			if strings.Contains(joined, "workflows/w.md") {
				t.Errorf("a tunable finding rendered against a %s baseline:\n%s", tc.name, joined)
			}
			if !strings.Contains(joined, "pricing.yaml") {
				t.Errorf("the canonical divergence was suppressed:\n%s", joined)
			}
		})
	}
}

// A template the product stopped shipping, still deployed.
func TestConfigTemplateDrift_StoppedShipping(t *testing.T) {
	dir := driftTree(t, map[string]string{
		".templates/.stamp": "1111111aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n", ".templates/.classes": driftClasses,
		".templates/.removed": "workflows/old.md\tr0\t2026-09-01T00:00:00Z\n",
		"workflows/old.md":    "still loaded\n",
	})
	got := driftHandler(dir, "1111111aaaaa").checkConfigTemplateDrift()
	if got.Status != "WARNING" || !strings.Contains(strings.Join(got.Items, "\n"), "workflows/old.md") {
		t.Fatalf("%s %v", got.Status, got.Items)
	}
}

// A template with no deployed file at all is the maximal missed change.
func TestConfigTemplateDrift_MissingDeployedFileIsNamed(t *testing.T) {
	dir := driftTree(t, map[string]string{
		".templates/.stamp": "1111111aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n", ".templates/.classes": driftClasses,
		".templates/workflows/new.md": "x\n",
	})
	got := driftHandler(dir, "1111111aaaaa").checkConfigTemplateDrift()
	if got.Status != "WARNING" || !strings.Contains(strings.Join(got.Items, "\n"), "workflows/new.md") {
		t.Fatalf("%s %v", got.Status, got.Items)
	}
}

// Case 19: config_crlf neither reports nor fixes the baselines, and still
// reports a CRLF file an operator did write.
func TestConfigCRLF_SkipsTheBaselines(t *testing.T) {
	dir := driftTree(t, map[string]string{
		".templates/workflows/a.md": "x\r\n",
		".origin/workflows/a.md":    "x\r\n",
		"workflows/b.md":            "y\r\n",
	})
	h := &DoctorHandlers{configDir: dir}
	got := h.checkConfigCRLF(true)
	joined := strings.Join(got.Items, "\n") + got.Message
	if strings.Contains(joined, ".templates") || strings.Contains(joined, ".origin") {
		t.Errorf("config_crlf touched the baselines: %s", joined)
	}
	if !strings.Contains(joined, "b.md") {
		t.Errorf("config_crlf missed a real CRLF file: %s", joined)
	}
	if b, _ := os.ReadFile(filepath.Join(dir, ".templates/workflows/a.md")); string(b) != "x\r\n" {
		t.Errorf("--fix rewrote the baseline: %q", b)
	}
}

// The soft class alone (vague mode — no .origin, the CE default) is rendered,
// bounded to ONE line per file with its count, per the precedence table; and in
// EXACT mode pure tuning is silent.
func TestConfigTemplateDrift_SoftClassRenderedBoundedAndExactTuningSilent(t *testing.T) {
	var tuned strings.Builder
	tuned.WriteString("x\n")
	for i := 0; i < 500; i++ {
		tuned.WriteString("tuned line\n")
	}
	dir := driftTree(t, map[string]string{
		".templates/.stamp": "1111111aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n", ".templates/.classes": driftClasses,
		".templates/workflows/a.md": "x\n", "workflows/a.md": tuned.String(),
	})
	got := driftHandler(dir, "1111111aaaaa").checkConfigTemplateDrift()
	if got.Status != "WARNING" || len(got.Items) != 1 || !strings.Contains(got.Items[0], "500 line(s)") {
		t.Fatalf("vague soft class: %s %v", got.Status, got.Items)
	}
	if strings.Count(got.Items[0], "tuned line") > 3 {
		t.Errorf("the soft class enumerated its lines: %s", got.Items[0])
	}
}

// The first live run (2026-09-24) read a CURRENT baseline as stale: the
// installer stamps the full sha, version.BuildRevision shortens it to 12
// characters, and the check compared them with ==. Real shapes here, not
// matching strings through the seam.
func TestStampMatches_RealShapes(t *testing.T) {
	full := "de573c9bedb3d4d714ed01157fb9e7a70defb0d3"
	for _, tc := range []struct {
		name  string
		stamp string
		rev   string
		dirty bool
		want  bool
	}{
		{"full stamp, short binary revision", full, "de573c9bedb3", false, true},
		{"dirty stamp, dirty binary", full + "-dirty", "de573c9bedb3", true, true},
		{"dirty stamp, clean binary", full + "-dirty", "de573c9bedb3", false, false},
		{"clean stamp, dirty binary", full, "de573c9bedb3", true, false},
		{"another commit", full, "b268330f6aaa", false, false},
		{"a revision too short to trust", full, "de5", false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := stampMatches(tc.stamp, tc.rev, tc.dirty); got != tc.want {
				t.Errorf("stampMatches(%q, %q, %v) = %v, want %v", tc.stamp, tc.rev, tc.dirty, got, tc.want)
			}
		})
	}
}

// And through the row: a full-sha stamp with the binary's short revision is
// CURRENT, so tunable findings render.
func TestConfigTemplateDrift_FullStampShortRevisionIsCurrent(t *testing.T) {
	dir := driftTree(t, map[string]string{
		".templates/.stamp": "de573c9bedb3d4d714ed01157fb9e7a70defb0d3\n", ".templates/.classes": driftClasses,
		".templates/workflows/w.md": "a\nfix\n", "workflows/w.md": "a\n",
	})
	got := driftHandler(dir, "de573c9bedb3").checkConfigTemplateDrift()
	if strings.Contains(got.Message, "stale") || !strings.Contains(strings.Join(got.Items, "\n"), "workflows/w.md") {
		t.Fatalf("%s %q %v", got.Status, got.Message, got.Items)
	}
}
