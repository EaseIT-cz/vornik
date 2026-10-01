package configdrift

import (
	"strings"
	"testing"
)

// Slice G (config-tree drift design, "a value the deployment never tuned"):
// a `replace` hunk used to be the ignored `c` class, so a template that
// changed `max_attempts: 3` to `5` was invisible on a deployment still at 3,
// whether the operator chose 3 or was never offered 5. In exact mode, a
// deployed value still equal to its .origin value was never tuned, so a
// template that changed it is a missed change.

const vfOrigin = "steps:\n  s:\n    role: coder\n    max_attempts: 3\n"

// Test 1: exact mode, untouched value, template changed it.
func TestValueFlip_UntunedValueTheTemplateChanged_IsHard(t *testing.T) {
	f := newFixture(t)
	f.origin(vfOrigin, "rev-old", "created")
	f.write(".templates/workflows/w.md", "steps:\n  s:\n    role: coder\n    max_attempts: 5\n")
	f.write("workflows/w.md", vfOrigin)
	ff := findFile(f.compare(), "workflows/w.md")
	if ff == nil || len(ff.Missing) != 1 || strings.TrimSpace(ff.Missing[0]) != "max_attempts: 5" {
		t.Fatalf("finding %+v, want the missed value max_attempts: 5", ff)
	}
	if hunks := ff.HardHunks(); len(hunks) != 1 {
		t.Fatalf("hard hunks %+v, want one", hunks)
	}
}

// Test 2: exact mode, the operator tuned the value; tuning wins.
func TestValueFlip_TunedValue_IsIgnored(t *testing.T) {
	f := newFixture(t)
	f.origin(vfOrigin, "rev-old", "created")
	f.write(".templates/workflows/w.md", "steps:\n  s:\n    role: coder\n    max_attempts: 5\n")
	f.write("workflows/w.md", "steps:\n  s:\n    role: coder\n    max_attempts: 4\n")
	if ff := findFile(f.compare(), "workflows/w.md"); ff != nil && len(ff.Missing) != 0 {
		t.Fatalf("a tuned value read as a missed change: %+v", ff)
	}
}

// Test 3: vague mode (no .origin) is unchanged — the accepted limit.
func TestValueFlip_VagueMode_IsIgnored(t *testing.T) {
	f := newFixture(t)
	f.write(".templates/workflows/w.md", "steps:\n  s:\n    role: coder\n    max_attempts: 5\n")
	f.write("workflows/w.md", vfOrigin)
	if ff := findFile(f.compare(), "workflows/w.md"); ff != nil && len(ff.Missing) != 0 {
		t.Fatalf("vague mode reported a value change: %+v", ff)
	}
}

// Test 4 (a guard against over-reporting): a `replace` whose deployed side is
// .origin text (rule 1 holds) but whose template side is unchanged relative
// to .origin (rule 2 fails). The template introduced nothing here, so there
// is nothing the deployment missed.
func TestValueFlip_TemplateUnchangedHere_IsIgnored(t *testing.T) {
	f := newFixture(t)
	f.origin("a\nq\nz\nx\n", "rev-old", "created")
	f.write(".templates/workflows/w.md", "a\nq\nz\n")
	f.write("workflows/w.md", "a\nx\nz\n")
	if ff := findFile(f.compare(), "workflows/w.md"); ff != nil && len(ff.Missing) != 0 {
		t.Fatalf("an unchanged template range was reported: %+v", ff)
	}
}

// Test 5: a documentation-only template change (a # line in front matter) is
// filtered like every hard hunk.
func TestValueFlip_FrontMatterCommentChange_IsFiltered(t *testing.T) {
	f := newFixture(t)
	orig := "---\n# old note\nid: w\n---\nbody\n"
	f.origin(orig, "rev-old", "created")
	f.write(".templates/workflows/w.md", "---\n# new note\nid: w\n---\nbody\n")
	f.write("workflows/w.md", orig)
	if ff := findFile(f.compare(), "workflows/w.md"); ff != nil && len(ff.Missing) != 0 {
		t.Fatalf("a documentation change was reported: %+v", ff)
	}
}

// Test 6: acknowledging the missed value silences it; a later template
// change of the same value re-opens it.
func TestValueFlip_AckSilencesAndANewFlipReopens(t *testing.T) {
	f := newFixture(t)
	f.origin(vfOrigin, "rev-old", "created")
	f.write(".templates/workflows/w.md", "steps:\n  s:\n    role: coder\n    max_attempts: 5\n")
	f.write("workflows/w.md", vfOrigin)
	k1 := hardKeyOf(t, f.compare(), "workflows/w.md")
	f.ack("workflows/w.md")
	if f.status("workflows/w.md", k1) != Acked {
		t.Fatal("the acked value flip is not suppressed")
	}
	f.write(".templates/workflows/w.md", "steps:\n  s:\n    role: coder\n    max_attempts: 7\n")
	k2 := hardKeyOf(t, f.compare(), "workflows/w.md")
	if k2 == k1 || f.status("workflows/w.md", k2) == Acked {
		t.Fatal("a new template value inherited the old acknowledgement")
	}
}

// Test 7: the approved-change exemption covers it as it covers a `d` hunk: an
// approved change that removed the line explains the template line.
func TestValueFlip_ExemptionAppliesLikeADHunk(t *testing.T) {
	f := newFixture(t)
	f.origin(vfOrigin, "rev-old", "created")
	tmpl := "steps:\n  s:\n    role: coder\n    max_attempts: 5\n"
	f.write(".templates/workflows/w.md", tmpl)
	f.write("workflows/w.md", vfOrigin)
	ff := findFile(f.compare(), "workflows/w.md")
	if ff == nil || len(ff.HardHunks()) != 1 {
		t.Fatalf("finding %+v, want one hard hunk", ff)
	}
	pre := "steps:\n  s:\n    role: coder\n    max_attempts: 5\n"
	got := ff.ExemptHunks([]ApprovedChange{{PreApply: []byte(pre), PostApply: []byte(vfOrigin)}})
	if !got[0] {
		t.Fatalf("an approved change that set the value was not exempt: %v", got)
	}
}

// A replace-only file with a slice G hunk is consistently exact
// (review-20260930-21d7).
func TestValueFlip_ReplaceOnlyFileIsExact(t *testing.T) {
	f := newFixture(t)
	f.origin(vfOrigin, "rev-old", "created")
	f.write(".templates/workflows/w.md", "steps:\n  s:\n    role: coder\n    max_attempts: 5\n")
	f.write("workflows/w.md", vfOrigin)
	ff := findFile(f.compare(), "workflows/w.md")
	if ff == nil || !ff.Exact || ff.OriginRevision != "rev-old" {
		t.Fatalf("finding %+v, want exact with origin revision rev-old", ff)
	}
}

// The accepted false positive of membership over position, pinned so nobody
// "fixes" it: a tuning that set a value to text .origin carries elsewhere (a
// duplicated line) reads as untuned. It surfaces as one hard hunk the
// operator can acknowledge in one command.
func TestValueFlip_AcceptedFalsePositive_IsHardAndAckable(t *testing.T) {
	f := newFixture(t)
	orig := "a:\n  enabled: true\nb:\n  enabled: false\n"
	f.origin(orig, "rev-old", "created")
	f.write(".templates/workflows/w.md", "a:\n  enabled: true\nb:\n  enabled: auto\n")
	f.write("workflows/w.md", "a:\n  enabled: true\nb:\n  enabled: true\n") // tuned b: false -> true
	r := f.compare()
	k := hardKeyOf(t, r, "workflows/w.md")
	f.ack("workflows/w.md")
	if f.status("workflows/w.md", k) != Acked {
		t.Fatal("the accepted false positive is not acknowledgeable")
	}
}

// Condition 1's quantifier: a multi-line replace where one deployed line is
// .origin text and another is not is tuning, and is ignored.
func TestValueFlip_MixedDeployedMembership_IsIgnored(t *testing.T) {
	f := newFixture(t)
	f.origin("s:\n  p: 1\n  q: 2\n", "rev-old", "created")
	f.write(".templates/workflows/w.md", "s:\n  p: 9\n  q: 8\n")
	f.write("workflows/w.md", "s:\n  p: 1\n  q: 7\n")
	if ff := findFile(f.compare(), "workflows/w.md"); ff != nil && len(ff.Missing) != 0 {
		t.Fatalf("a partly tuned range was reported: %+v", ff)
	}
}

// Documentation is filtered within the hunk: a front-matter # line and a
// real changed value in one replace range report only the value.
func TestValueFlip_MixedDocumentationInHunk_ReportsOnlyTheValue(t *testing.T) {
	f := newFixture(t)
	orig := "---\n# old note\nid: w\n---\nbody\n"
	f.origin(orig, "rev-old", "created")
	f.write(".templates/workflows/w.md", "---\n# new note\nid: w2\n---\nbody\n")
	f.write("workflows/w.md", orig)
	ff := findFile(f.compare(), "workflows/w.md")
	if ff == nil || len(ff.Missing) != 1 || ff.Missing[0] != "id: w2" {
		t.Fatalf("finding %+v, want only id: w2", ff)
	}
}

// A `d` hunk and a slice G hunk in one file enumerate as two hard hunks with
// distinct keys.
func TestValueFlip_CoexistsWithADHunk(t *testing.T) {
	f := newFixture(t)
	f.origin("a\nb: 3\n", "rev-old", "created")
	f.write(".templates/workflows/w.md", "x-new\na\nb: 5\n")
	f.write("workflows/w.md", "a\nb: 3\n")
	ff := findFile(f.compare(), "workflows/w.md")
	if ff == nil || len(ff.HardHunks()) != 2 {
		t.Fatalf("finding %+v, want two hard hunks", ff)
	}
	keys := map[AckKey]bool{}
	for _, k := range ff.Keys() {
		if k.Class == AckHard {
			keys[k] = true
		}
	}
	if len(keys) != 2 {
		t.Fatalf("hard keys %v, want two distinct", keys)
	}
}

// A G hunk ahead of a `d` hunk does not change the `d` hunk's key
// (review-20260930-6f47): keys hash hunk content, so an earlier hunk joining
// the list cannot re-key a later one.
func TestValueFlip_PrecedingGHunkDoesNotReKeyADHunk(t *testing.T) {
	dOnly := newFixture(t)
	dOnly.origin("b: 3\na\n", "rev-old", "created")
	dOnly.write(".templates/workflows/w.md", "b: 3\na\nz-new\n")
	dOnly.write("workflows/w.md", "b: 3\na\n")
	want := hardKeyOf(t, dOnly.compare(), "workflows/w.md")

	both := newFixture(t)
	both.origin("b: 3\na\n", "rev-old", "created")
	both.write(".templates/workflows/w.md", "b: 5\na\nz-new\n")
	both.write("workflows/w.md", "b: 3\na\n")
	ff := findFile(both.compare(), "workflows/w.md")
	if ff == nil || len(ff.HardHunks()) != 2 {
		t.Fatalf("finding %+v, want a G hunk and a d hunk", ff)
	}
	found := false
	for _, k := range ff.Keys() {
		if k == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("the d hunk re-keyed when a G hunk preceded it: keys %v, want %v among them", ff.Keys(), want)
	}
}
