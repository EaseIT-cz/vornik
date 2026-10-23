package hermes

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// GitHub #388 (found at review, W9-F 2026-10-15): Hermes swallows a memory
// provider's register_skill error at debug level, and no lane step looked at
// the skill registry, so a plugin skill that failed to register left the
// lane green. H0-skills derives the expected set from the bundle and fails
// on a missing one (lane design, "Amendment 2026-10-16 — H0-skills").

// writeBundle lays out a plugin bundle fixture: plugin.yaml and one
// skills/<dir>/SKILL.md per entry, whose body is the given text.
func writeBundle(t *testing.T, manifest string, skills map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "plugin.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	if skills == nil {
		return dir
	}
	if err := os.MkdirAll(filepath.Join(dir, "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range skills {
		d := filepath.Join(dir, "skills", name)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if body == "" {
			continue // a directory with no SKILL.md
		}
		if err := os.WriteFile(filepath.Join(d, "SKILL.md"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func skillMD(name string) string {
	return "---\nname: " + name + "\ndescription: \"Use when testing.\"\n---\n\n# Body\n"
}

func TestExpectedPluginSkills_DerivesQualifiedNamesFromTheSkillsDirectory(t *testing.T) {
	dir := writeBundle(t, "name: vornik-companion\nversion: 1.0.0\n", map[string]string{
		"vornik-broker":       skillMD("vornik-broker"),
		"develop-with-memory": skillMD("develop-with-memory"),
	})
	got, err := expectedPluginSkills(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"vornik-companion:develop-with-memory", "vornik-companion:vornik-broker"}
	if !slices.Equal(got, want) {
		t.Fatalf("expected = %v, want %v", got, want)
	}
}

func TestExpectedPluginSkills_SkillNamespaceWinsOverName(t *testing.T) {
	dir := writeBundle(t, "name: vornik-companion\nskill_namespace: vk\n", map[string]string{
		"vornik-admin": skillMD("vornik-admin"),
	})
	got, err := expectedPluginSkills(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got, []string{"vk:vornik-admin"}) {
		t.Fatalf("expected = %v", got)
	}
}

func TestExpectedPluginSkills_RefusesWhatItCannotCheck(t *testing.T) {
	cases := map[string]struct {
		manifest string
		skills   map[string]string
		want     string
	}{
		"no skills directory": {"name: p\n", nil, "skills"},
		"empty skills":        {"name: p\n", map[string]string{}, "no skill"},
		"no SKILL.md":         {"name: p\n", map[string]string{"a": skillMD("a"), "b": ""}, "SKILL.md"},
		"name differs":        {"name: p\n", map[string]string{"a": skillMD("b")}, "name"},
		"no frontmatter":      {"name: p\n", map[string]string{"a": "# just a body\n"}, "frontmatter"},
		"no plugin name":      {"version: 1\n", map[string]string{"a": skillMD("a")}, "name"},
	}
	for label, c := range cases {
		t.Run(label, func(t *testing.T) {
			dir := writeBundle(t, c.manifest, c.skills)
			got, err := expectedPluginSkills(dir)
			if err == nil {
				t.Fatalf("no error; expected = %v", got)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Fatalf("error %q does not mention %q", err, c.want)
			}
		})
	}
}

// The real bundle: derivation succeeds, finds at least the skill #388 names,
// and its namespace is the memory provider name the lane configures
// (prepareHermesHome writes memory.provider: vornik-companion), so both of
// Hermes's registration paths key the skills the same way.
func TestExpectedPluginSkills_TheRealBundle(t *testing.T) {
	got, err := expectedPluginSkills(filepath.Join("..", "..", "..", "contrib", "hermes-companion"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got, "vornik-companion:develop-with-memory") {
		t.Fatalf("expected = %v, missing develop-with-memory", got)
	}
	for _, q := range got {
		if !strings.HasPrefix(q, "vornik-companion:") {
			t.Fatalf("%q is not under the lane's memory provider name", q)
		}
	}
}

// The probe goes through the memory provider path whose errors Hermes
// swallows, reads the provider name with Hermes's own reader, and prints
// the line checkPluginSkills reads.
func TestSkillsProbe_FollowsTheRunPathAndPrintsTheMarker(t *testing.T) {
	for _, want := range []string{"discover_plugins()", "_get_active_memory_provider()", "load_memory_provider(name)", "skills_list()", `print("` + skillsMarker + `"`, `"memory_provider": name`} {
		if !strings.Contains(skillsProbe, want) {
			t.Errorf("skillsProbe lacks %q", want)
		}
	}
}

var twoExpected = []string{"vornik-companion:vornik-admin", "vornik-companion:vornik-broker"}

func TestCheckPluginSkills_AllListedPassesAndStatesTheCount(t *testing.T) {
	out := "some Hermes noise\n" + skillsMarker + `{"success": true, "memory_provider": "vornik-companion", "skills": ["vornik-companion:vornik-admin", "vornik-companion:vornik-broker", "other:thing"]}` + "\n"
	summary, err := checkPluginSkills(twoExpected, out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(summary, "checked 2 expected skills") {
		t.Fatalf("summary %q does not state the denominator", summary)
	}
}

func TestCheckPluginSkills_NamesTheMissingSkill(t *testing.T) {
	out := skillsMarker + `{"success": true, "memory_provider": "vornik-companion", "skills": ["vornik-companion:vornik-broker"]}`
	summary, err := checkPluginSkills(twoExpected, out)
	if err == nil {
		t.Fatalf("a missing skill passed: %s", summary)
	}
	if !strings.Contains(err.Error(), "vornik-companion:vornik-admin") || !strings.Contains(err.Error(), "checked 2") {
		t.Fatalf("error %q does not name the missing skill and the count", err)
	}
}

func TestCheckPluginSkills_NeverExaminedIsNotClean(t *testing.T) {
	for label, out := range map[string]string{
		"no marker":       "Traceback (most recent call last):\nImportError: no module\n",
		"success false":   skillsMarker + `{"success": false, "memory_provider": "vornik-companion", "skills": []}`,
		"unparsable":      skillsMarker + `{"success": tru`,
		"no skills field": skillsMarker + `{"success": true, "memory_provider": "vornik-companion"}`,
		// The memory provider would key its skills under another namespace,
		// so the swallowed-error path would go unchecked.
		"provider differs": skillsMarker + `{"success": true, "memory_provider": "other", "skills": ["vornik-companion:vornik-admin", "vornik-companion:vornik-broker"]}`,
		"no provider":      skillsMarker + `{"success": true, "memory_provider": "", "skills": ["vornik-companion:vornik-admin", "vornik-companion:vornik-broker"]}`,
	} {
		t.Run(label, func(t *testing.T) {
			if summary, err := checkPluginSkills(twoExpected, out); err == nil {
				t.Fatalf("passed: %s", summary)
			}
		})
	}
	if _, err := checkPluginSkills(nil, skillsMarker+`{"success": true, "memory_provider": "vornik-companion", "skills": []}`); err == nil {
		t.Fatal("an empty expected set passed")
	}
}
