package configdrift

import "testing"

// Ack keys (drift design, slice C). An ack must keep silencing exactly the
// decision it recorded: a pure template REORDER must not re-key (it re-diffs
// every line and would re-open every ack at once), but a RE-INDENT must —
// indentation is structure in this corpus, and the same lines under another
// step or on_fail branch are a different change (matrix case 8).
func TestHunkKey_ReorderStableReindentNot(t *testing.T) {
	a := hunkKey([]string{"  retry: 3", "  timeout: 5m"})
	if b := hunkKey([]string{"  timeout: 5m", "  retry: 3"}); a != b {
		t.Error("a reorder re-keyed the hunk")
	}
	if c := hunkKey([]string{"    retry: 3", "    timeout: 5m"}); a == c {
		t.Error("a re-indent did not re-key the hunk")
	}
	if d := hunkKey([]string{"  retry: 3", "  retry: 3", "  timeout: 5m"}); a == d {
		t.Error("duplicates must count (multiset), or a doubled line inherits the old ack")
	}
	if e := hunkKey([]string{"  retry: 3\r", "  timeout: 5m"}); a != e {
		t.Error("CRLF must not re-key")
	}
}

// Keys are computed over the WHOLE set, never the bounded display subset: a
// new removal beyond the 20 displayed lines must still re-key (case 3).
func TestFindingKeys_UseTheUnboundedSets(t *testing.T) {
	f := newFixture(t)
	// A shared anchor, so the 30 template lines are a pure delete (a `d`
	// hunk), not a replace of an unrelated line.
	tmpl, dep := "anchor\n", "anchor\n"
	for i := 0; i < 30; i++ {
		tmpl += "line " + string(rune('a'+i%26)) + string(rune('0'+i/26)) + "\n"
	}
	f.write(".templates/workflows/w.md", tmpl)
	f.write("workflows/w.md", dep)
	ff := findFile(f.compare(), "workflows/w.md")
	if ff == nil || len(ff.Missing) != MaxHunkLines {
		t.Fatalf("display bound not applied: %+v", ff)
	}
	keys := ff.Keys()
	var hard []AckKey
	for _, k := range keys {
		if k.Class == AckHard {
			hard = append(hard, k)
		}
	}
	if len(hard) != 1 {
		t.Fatalf("hard keys %v, want one per hunk", hard)
	}
	all := make([]string, 0, 30)
	for i := 0; i < 30; i++ {
		all = append(all, "line "+string(rune('a'+i%26))+string(rune('0'+i/26)))
	}
	if hard[0].Key != hunkKey(all) {
		t.Error("the hard key was computed over the displayed subset, not the whole hunk")
	}
}

// One key per d HUNK, so a second, separate missed change in the same file
// is a second key (a new template fix re-opens the file without re-opening the
// first decision).
func TestFindingKeys_OnePerHunk(t *testing.T) {
	f := newFixture(t)
	f.write(".templates/workflows/w.md", "a\nnew one\nb\nc\nnew two\nd\n")
	f.write("workflows/w.md", "a\nb\nc\nd\n")
	ff := findFile(f.compare(), "workflows/w.md")
	n := 0
	for _, k := range ff.Keys() {
		if k.Class == AckHard {
			n++
		}
	}
	if n != 2 {
		t.Fatalf("%d hard keys, want 2 (one per hunk)", n)
	}
}

// Soft keys carry the regime: the same file's soft finding keys differently in
// exact and vague mode, so .origin appearing re-opens a vague ack (case 9).
func TestFindingKeys_SoftCarriesTheRegime(t *testing.T) {
	vague := newFixture(t)
	vague.write(".templates/workflows/w.md", "a\n")
	vague.write("workflows/w.md", "a\nmax_attempts: 3\n")
	vf := findFile(vague.compare(), "workflows/w.md")

	exact := newFixture(t)
	exact.origin("a\nmax_attempts: 3\n", "r0", "created")
	exact.write(".templates/workflows/w.md", "a\n")
	exact.write("workflows/w.md", "a\nmax_attempts: 3\n")
	ef := findFile(exact.compare(), "workflows/w.md")

	vk, ek := softKey(t, vf), softKey(t, ef)
	if vk.Regime != "vague" || ek.Regime != "exact" || vk == ek {
		t.Fatalf("vague %+v exact %+v", vk, ek)
	}
}

func softKey(t *testing.T, ff *FileFinding) AckKey {
	t.Helper()
	for _, k := range ff.Keys() {
		if k.Class == AckSoft {
			return k
		}
	}
	t.Fatalf("no soft key in %+v", ff)
	return AckKey{}
}

// Canonical: the deployed/template hash pair — either side changing re-keys
// (case 16).
func TestFindingKeys_CanonicalPair(t *testing.T) {
	f := newFixture(t)
	f.write(".templates/pricing.yaml", "m: 1\n")
	f.write("pricing.yaml", "m: 2\n")
	k1 := findFile(f.compare(), "pricing.yaml").Keys()[0]
	f.write("pricing.yaml", "m: 3\n")
	k2 := findFile(f.compare(), "pricing.yaml").Keys()[0]
	f.write(".templates/pricing.yaml", "m: 9\n")
	k3 := findFile(f.compare(), "pricing.yaml").Keys()[0]
	if k1.Class != AckCanonical || k1 == k2 || k2 == k3 {
		t.Fatalf("canonical keys %v %v %v", k1, k2, k3)
	}
}
