package architecture

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"vornik.io/vornik/internal/testutil/repowalk"
)

// secretEnvRead matches a read of a variable whose name a field or constant
// ending in "Env" holds: os.Getenv(p.IMAPPasswordEnv), os.LookupEnv(fooEnv).
var secretEnvRead = regexp.MustCompile(`os\.(?:Getenv|LookupEnv)\(\s*([A-Za-z_][A-Za-z0-9_.]*Env)\s*\)`)

// operatingSettingReads are the os.Getenv(<…>Env) calls that read an
// operating setting, not a secret (agent-administered Vornik design,
// amendment 2026-11-12 E, as built at 91a5d8087). Each is "file: argument".
var operatingSettingReads = map[string]bool{
	"internal/cli/account_keys.go: claimSecretEnv":              true, // a CLI's own input, read by vornikctl
	"internal/cli/profile.go: profileAPIKeyEnv":                 true, // a CLI's own input, read by vornikctl
	"internal/config/env_overrides.go: o.Env":                   true, // the VORNIK_* override table
	"internal/executor/call_project.go: interProjectEnabledEnv": true,
	"internal/imagemanifest/provenance.go: ObtainedPathEnv":     true,
	"internal/membench/guard.go: DenyDatabasesEnv":              true,
}

// TestSecretReadersGoThroughTheStore_626 pins the secret reader list (GitHub
// #626 E): a secret read by its variable name goes through
// secretstore.Value (the environment, then the operator credential store),
// so a value stored in the console reaches it. A new os.Getenv(<…>Env) in
// non-test code fails here unless it is added above as an operating setting,
// with its reason. It examines every non-test .go file under internal/ and
// cmd/ and reports how many it read.
func TestSecretReadersGoThroughTheStore_626(t *testing.T) {
	root := moduleRoot(t)
	var bad []string
	examined, found := 0, 0
	seen := map[string]bool{}
	for _, dir := range []string{"internal", "cmd"} {
		base := filepath.Join(root, dir)
		err := filepath.WalkDir(base, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() {
				if repowalk.SkipDir(base, path, d.Name(), "testdata") {
					return filepath.SkipDir
				}
				return nil
			}
			if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			examined++
			rel, _ := filepath.Rel(root, path)
			for _, m := range secretEnvRead.FindAllStringSubmatch(string(b), -1) {
				found++
				key := filepath.ToSlash(rel) + ": " + m[1]
				seen[key] = true
				if !operatingSettingReads[key] {
					bad = append(bad, key)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	t.Logf("examined %d non-test Go files; %d os.Getenv(<…>Env) reads, %d allowed as operating settings", examined, found, len(operatingSettingReads))
	sort.Strings(bad)
	for _, b := range bad {
		t.Errorf("%s reads a secret from the environment only; use secretstore.Value (or, for an operating setting, list it in operatingSettingReads with its reason)", b)
	}
	for key := range operatingSettingReads {
		if !seen[key] {
			t.Errorf("operatingSettingReads lists %q, which no longer exists; remove it", key)
		}
	}
	if examined < 100 {
		t.Fatalf("examined only %d files: the walk did not reach the tree", examined)
	}
}
