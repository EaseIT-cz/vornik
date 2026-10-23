package registry

import (
	_ "embed"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/version"
)

// Per-key release provenance — loader-validator agreement design §13.2.
//
// ProjectConfigKeys answers "would my config load under this binary?", which is
// the upgrade question. It cannot answer "what changed in 2026.9.4?", because
// the schema carried no record of which release introduced each key. A `since:`
// tag on the field does, read through the SAME config.WalkLeaves the key set is
// already derived from — so the record cannot be hand-maintained into drift,
// which is the whole reason to prefer it over a manifest generated at release
// time (a manifest exists only if the release step ran).

//go:embed project_keys_baseline.txt
var projectKeysBaselineFile string

// KeySince is one config key and the release that introduced it.
type KeySince struct {
	Key string `json:"key"`
	// Since is the release the field declares, or "" for a key that predates
	// per-key provenance. Empty is a STATEMENT — "older than tracking" — not a
	// gap, and it is only legitimate for a key in the committed baseline.
	Since string `json:"since,omitempty"`
}

// ProjectConfigKeysSince returns every project config key with the release that
// introduced it, sorted by key.
func ProjectConfigKeysSince() []KeySince {
	out := make([]KeySince, 0, 256)
	config.WalkLeaves(reflect.ValueOf(Project{}), func(key string, f reflect.StructField, _ reflect.Value) {
		out = append(out, KeySince{Key: key, Since: strings.TrimSpace(f.Tag.Get("since"))})
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// ProjectKeysPredatingProvenance is the committed baseline: the keys that
// existed before `since:` tags did.
//
// It exists so new fields can be REQUIRED to carry a release without
// back-tagging 193 existing ones — the same ratchet shape as the LLD anchor
// ceiling, and for the same reason: a rule enforced by a file is a rule, and a
// rule that depends on remembering is not.
//
// It only ever shrinks. Back-tagging an old field removes its line; nothing is
// ever added.
func ProjectKeysPredatingProvenance() map[string]bool {
	out := map[string]bool{}
	for _, line := range strings.Split(projectKeysBaselineFile, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out[line] = true
	}
	return out
}

// KeysWithoutProvenance returns keys that neither declare a `since:` nor appear
// in the baseline — i.e. fields added after provenance shipped and not tagged.
// Empty is the passing state.
func KeysWithoutProvenance() []string {
	baseline := ProjectKeysPredatingProvenance()
	var missing []string
	for _, k := range ProjectConfigKeysSince() {
		if k.Since == "" && !baseline[k.Key] {
			missing = append(missing, k.Key)
		}
	}
	sort.Strings(missing)
	return missing
}

// ProjectKeysNewerThan returns the keys introduced AFTER the given release.
// An empty release returns every key that declares one. A key with no `since:`
// predates tracking and is never "newer".
//
// Releases compare as calendar versions, field by field (version.CompareReleases;
// loader-validator agreement design, 2026-10-17 amendment, #371). Until then this
// compared strings, so "2026.10.0" sorted before "2026.9.8" and a query across a
// month boundary missed every newer key. The running daemon's own describe stamp
// (2026.10.6-115-g44e675c12) is a valid release: the build already contains the
// keys of the tag it follows.
//
// A release that does not parse (dev+g<rev>, unstamped, a bare hash) has no
// position in the order, so it is an error rather than a guess.
func ProjectKeysNewerThan(release string) ([]KeySince, error) {
	release = strings.TrimSpace(release)
	var after version.Release
	if release != "" {
		r, ok := version.ParseRelease(release)
		if !ok {
			return nil, fmt.Errorf("%q is not a calendar release: use one such as 2026.9.4, 2026.10.7-rc1 or a daemon version such as 2026.10.6-115-g44e675c12 (a dev+g<rev>, unstamped or bare-hash build has no release to compare against; run without --since for the full list)", release)
		}
		after = r
	}
	var out []KeySince
	for _, k := range ProjectConfigKeysSince() {
		if k.Since == "" {
			continue
		}
		if release == "" {
			out = append(out, k)
			continue
		}
		since, ok := version.ParseRelease(k.Since)
		if !ok {
			// TestSinceTagsNameAPublishedRelease makes this unreachable; it is
			// a backstop, not a branch anyone should meet.
			return nil, fmt.Errorf("config key %s declares since:%q, which is not a calendar release", k.Key, k.Since)
		}
		if version.CompareReleases(since, after) > 0 {
			out = append(out, k)
		}
	}
	return out, nil
}
