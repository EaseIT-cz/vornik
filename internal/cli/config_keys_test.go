package cli

import (
	"bytes"
	"strings"
	"testing"

	"vornik.io/vornik/internal/version"

	"github.com/spf13/cobra"
)

func runKeys(t *testing.T, args ...string) string {
	t.Helper()
	out, err := runKeysErr(t, args...)
	if err != nil {
		t.Fatalf("config keys: %v", err)
	}
	return out
}

func runKeysErr(t *testing.T, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	// Drive the RunE directly with the package flags set, rather than
	// re-registering the flag set on a throwaway command: the flags are
	// package-level (the cobra shape this file already uses), so a second
	// registration would be a second source of truth for the same flag.
	cmd := &cobra.Command{RunE: runConfigKeys}
	cmd.SetOut(&out)
	configKeysSince, configJSON = "", false
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--since" {
			configKeysSince = args[i+1]
		}
	}
	err := runConfigKeys(cmd, nil)
	return out.String(), err
}

// The full list is the answer to "would my config load", and it must be
// non-empty and derived — a schema-derived list that came back empty would mean
// the walk broke, which is worse than a wrong key.
func TestConfigKeys_ListsTheSchema(t *testing.T) {
	got := runKeys(t)
	if !strings.Contains(got, "swarmId") || !strings.Contains(got, "KEY") {
		t.Fatalf("the key list is missing the schema:\n%s", got)
	}
	if !strings.Contains(got, "predating per-key release provenance") {
		t.Fatalf("the untagged count is not reported:\n%s", got)
	}
}

// An empty --since result must say WHY it is empty. "No keys are newer" and "no
// key carries provenance yet" are different answers, and an operator planning
// an upgrade must not read the second as the first.
func TestConfigKeys_EmptySinceExplainsItself(t *testing.T) {
	// A release NOBODY has shipped yet, so the result is empty by
	// construction. It used to name a past release, which stopped being
	// empty the moment a key was tagged newer than it (`dependencies`,
	// 2026.9.6) — a fixture that silently stops exercising the branch it
	// was written for.
	got := runKeys(t, "--since", "2999.1.0")
	if !strings.Contains(got, "NOT a statement that nothing changed") {
		t.Fatalf("an empty --since did not explain itself:\n%s", got)
	}
}

// The converse, which the case above cannot pin once it is empty by
// construction: a --since that DOES match must list the key and not the
// disclaimer.
func TestConfigKeys_SinceListsNewerKeys(t *testing.T) {
	got := runKeys(t, "--since", "2026.9.5")
	if !strings.Contains(got, "dependencies") {
		t.Fatalf("--since did not list a key tagged newer than it:\n%s", got)
	}
	if strings.Contains(got, "NOT a statement that nothing changed") {
		t.Fatalf("a non-empty --since printed the empty-result disclaimer:\n%s", got)
	}
}

// keyRows returns the KEY -> SINCE rows of a `config keys` table.
func keyRows(out string) map[string]string {
	rows := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		f := strings.Fields(line)
		if len(f) == 2 && f[0] != "KEY" && f[1] != "key(s)." {
			rows[f[0]] = f[1]
		}
	}
	return rows
}

// Issue #371 (found at: review): --since compared releases as strings, so
// "2026.10.x" sorted before "2026.9.x". On 028bd6ef0 `--since 2026.9.8` printed
// "No project config key declares a release after 2026.9.8" although every
// 2026.10.x key is newer, and `--since 2026.10.5` listed only 2026.9.x keys,
// all of them older. This is the operator's answer, so the test drives it.
func TestConfigKeys_SinceCrossesAMonthBoundary(t *testing.T) {
	cases := []struct {
		since      string
		wantPrefix string // at least one listed key carries a release with this prefix
		noPrefix   string // no listed key carries a release with this prefix
	}{
		{"2026.9.8", "2026.10.", "2026.9."},
		{"2026.10.5", "2026.10.6", "2026.9."},
	}
	for _, tc := range cases {
		got := runKeys(t, "--since", tc.since)
		rows := keyRows(got)
		found := false
		for k, since := range rows {
			if strings.HasPrefix(since, tc.noPrefix) {
				t.Errorf("--since %s listed %s (since %s), which is older:\n%s", tc.since, k, since, got)
			}
			if strings.HasPrefix(since, tc.wantPrefix) {
				found = true
			}
		}
		if !found {
			t.Errorf("--since %s listed no key tagged %s*:\n%s", tc.since, tc.wantPrefix, got)
		}
	}
}

// The running daemon's own `git describe` stamp is a valid --since: the build
// already contains the keys of the release it was cut after, and not the next.
func TestConfigKeys_SinceAcceptsADescribeStamp(t *testing.T) {
	rows := keyRows(runKeys(t, "--since", "2026.10.5-115-g44e675c12-dirty"))
	if len(rows) == 0 {
		t.Fatal("a describe stamp after 2026.10.5 listed nothing; the 2026.10.6 keys are newer")
	}
	// Every listed key is tagged after 2026.10.5 (2026.10.6 or later; the
	// release line moves on, so the test names a floor, not one release),
	// and the 2026.10.6 keys are among them.
	floor, _ := version.ParseRelease("2026.10.5")
	saw1006 := false
	for k, since := range rows {
		r, ok := version.ParseRelease(since)
		if !ok || version.CompareReleases(r, floor) <= 0 {
			t.Errorf("listed %s (since %s), not newer than 2026.10.5-115", k, since)
		}
		if since == "2026.10.6" {
			saw1006 = true
		}
	}
	if !saw1006 {
		t.Error("a describe stamp after 2026.10.5 did not list the 2026.10.6 keys")
	}
}

// A --since that is not a release has no order, so it is an error, never a
// string comparison against, say, the dev+g VCS fallback version.
func TestConfigKeys_SinceRejectsANonRelease(t *testing.T) {
	for _, bad := range []string{"dev+g44e675c12", "unstamped", "44e675c12", "2026.10", "v2026.10.6"} {
		out, err := runKeysErr(t, "--since", bad)
		if err == nil {
			t.Errorf("--since %q: no error; output:\n%s", bad, out)
			continue
		}
		if !strings.Contains(err.Error(), "2026.9.4") {
			t.Errorf("--since %q: error does not show an accepted form: %v", bad, err)
		}
	}
}
