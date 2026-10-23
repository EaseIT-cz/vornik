package registry

import (
	"strings"
	"testing"
)

// §13.2's ratchet. A field added after per-key provenance shipped must declare
// the release that introduced it, or this fails — which is how the record stays
// true without anyone having to remember.
func TestProjectKeysCarryProvenance(t *testing.T) {
	missing := KeysWithoutProvenance()
	if len(missing) == 0 {
		return
	}
	t.Fatalf(`%d project config key(s) declare no `+"`since:`"+` tag and are not in the
pre-provenance baseline:

  %s

Add `+"`since:\"<release>\"`"+` to the struct field (loader-validator agreement
design §13.2). The baseline only ever shrinks — do NOT add the key to
internal/registry/project_keys_baseline.txt.`, len(missing), strings.Join(missing, "\n  "))
}

// The baseline must describe THIS binary's schema: a line naming a key that no
// longer exists is stale, and a stale baseline silently excuses a future field
// that happens to reuse the name.
func TestBaselineHasNoKeysTheSchemaDropped(t *testing.T) {
	live := map[string]bool{}
	for _, k := range ProjectConfigKeysSince() {
		live[k.Key] = true
	}
	var stale []string
	for k := range ProjectKeysPredatingProvenance() {
		if !live[k] {
			stale = append(stale, k)
		}
	}
	if len(stale) > 0 {
		t.Fatalf("baseline names %d key(s) the schema no longer has: %v — remove those lines", len(stale), stale)
	}
}

// A key that declares a release is reported as newer than an earlier one, and a
// key that predates tracking never is.
func TestProjectKeysNewerThan(t *testing.T) {
	got, err := ProjectKeysNewerThan("2026.9.5")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range got {
		if k.Since == "" {
			t.Fatalf("an untagged key was reported as newer: %+v", k)
		}
	}
	// An empty release means "everything with a declared since", which is what
	// `vornikctl` prints when the operator does not name one.
	all, err := ProjectKeysNewerThan("")
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range all {
		if k.Since == "" {
			t.Fatalf("untagged key in the unfiltered list: %+v", k)
		}
	}
	// #371: across a month boundary the comparison is numeric. Every tagged key
	// is newer than a release before all of them, so nothing may be dropped.
	early, err := ProjectKeysNewerThan("2026.9.0")
	if err != nil {
		t.Fatal(err)
	}
	if len(early) != len(all) {
		t.Fatalf("newer than 2026.9.0: %d keys, want all %d tagged keys", len(early), len(all))
	}
	if _, err := ProjectKeysNewerThan("dev+g44e675c12"); err == nil {
		t.Fatal("a dev+g build version was accepted as a release")
	}
}

// The two derivations must not drift: the same walk produces both.
func TestProjectConfigKeysSinceMatchesKeySet(t *testing.T) {
	plain := ProjectConfigKeys()
	withSince := ProjectConfigKeysSince()
	if len(plain) != len(withSince) {
		t.Fatalf("key set and provenance set disagree: %d vs %d", len(plain), len(withSince))
	}
	for i := range plain {
		if plain[i] != withSince[i].Key {
			t.Fatalf("order/content drift at %d: %q vs %q", i, plain[i], withSince[i].Key)
		}
	}
}
