package api

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The EaseIT-cz migration (2026-10-02-easeit-org-migration-design.md §5.2):
// deployed configs keep naming the agent image's legacy registry, because
// config deploys never overwrite. The alias keeps them working; this check
// names the files and the one-line fix, and says how many it examined.
func TestCheckLegacyImageNames(t *testing.T) {
	t.Run("legacy files are named with the denominator", func(t *testing.T) {
		dir := t.TempDir()
		write := func(rel, body string) {
			p := filepath.Join(dir, rel)
			if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		write("swarms/a.md", "image: ghcr.io/grinco/vornik-agent:latest\n")
		write("project-templates/x/swarm.md.tmpl", "image: \"ghcr.io/grinco/vornik-agent:latest\"\n")
		write("swarms/b.md", "image: ghcr.io/easeit-cz/vornik-agent:latest\n")
		write("workflows/w.md", "nothing here\n")
		// The shipped-template baseline is not the operator's to fix.
		write(".templates/swarms/a.md", "image: ghcr.io/grinco/vornik-agent:latest\n")

		c := (&DoctorHandlers{configDir: dir}).checkLegacyImageNames()
		if c.Status != "WARNING" {
			t.Fatalf("status = %s (%s)", c.Status, c.Message)
		}
		if !strings.Contains(c.Message, "examined 4") || !strings.Contains(c.Message, "2 name") {
			t.Errorf("message lacks the denominator: %q", c.Message)
		}
		joined := strings.Join(c.Items, "\n")
		if !strings.Contains(joined, "swarms/a.md") || !strings.Contains(joined, "project-templates/x/swarm.md.tmpl") || strings.Contains(joined, ".templates") {
			t.Errorf("items = %q", c.Items)
		}
		if !strings.Contains(c.Message+joined, "sed -i") {
			t.Errorf("no fix given: %+v", c)
		}
	})
	t.Run("clean tree is OK with the denominator", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "a.md"), []byte("image: ghcr.io/easeit-cz/vornik-agent:latest\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		c := (&DoctorHandlers{configDir: dir}).checkLegacyImageNames()
		if c.Status != "OK" || !strings.Contains(c.Message, "examined 1") {
			t.Fatalf("%+v", c)
		}
	})
	t.Run("no tree is SKIPPED, not OK", func(t *testing.T) {
		c := (&DoctorHandlers{}).checkLegacyImageNames()
		if c.Status != "SKIPPED" {
			t.Fatalf("%+v", c)
		}
	})
}
