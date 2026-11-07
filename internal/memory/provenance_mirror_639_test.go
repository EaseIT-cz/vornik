package memory

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
)

// #639 (design 22, "Amendment 2026-11-04 (#639)"): the repository mirror,
// against a real upstream and clone, with the store hooks faked. Before the
// change nothing brought a new version of a repository file into memory.

type mirrorFake struct {
	recs        map[string]*persistence.ChunkProvenance
	quarantined map[string]bool
	ingested    []MirrorFile
	blobReads   int
}

func (m *mirrorFake) hooks(project, scope string, paths ...string) MirrorHooks {
	return MirrorHooks{
		Projects: func() []MirrorProject {
			return []MirrorProject{{ProjectID: project, RepoScope: scope, Paths: paths}}
		},
		Provenance: func(_ context.Context, _, id string) (*persistence.ChunkProvenance, error) {
			return m.recs[id], nil
		},
		Quarantined: func(_ context.Context, _, id string) (bool, error) { return m.quarantined[id], nil },
		Ingest: func(_ context.Context, f MirrorFile) error {
			m.ingested = append(m.ingested, f)
			// The pipeline would write a record; a verified one here.
			m.recs[f.ArtifactID] = &persistence.ChunkProvenance{ArtifactID: f.ArtifactID, State: ProvenanceVerified}
			return nil
		},
	}
}

func newMirrorSweeper(t *testing.T, f *provFixture, m *mirrorFake, paths ...string) *ProvenanceSweeper {
	t.Helper()
	inner := f.v.Git
	f.v.Git = func(ctx context.Context, dir string, args ...string) ([]byte, error) {
		if len(args) >= 2 && args[0] == "cat-file" && args[1] == "blob" {
			m.blobReads++
		}
		return inner(ctx, dir, args...)
	}
	return &ProvenanceSweeper{Verifier: f.v, Logger: zerolog.Nop(), Mirror: m.hooks(f.project, f.scope, paths...)}
}

func newMirrorFake() *mirrorFake {
	return &mirrorFake{recs: map[string]*persistence.ChunkProvenance{}, quarantined: map[string]bool{}}
}

func ingestedPaths(m *mirrorFake) string {
	var out []string
	for _, f := range m.ingested {
		out = append(out, f.Path)
	}
	return strings.Join(out, ",")
}

func TestMirror_IngestsNewVersionsOnce_639(t *testing.T) {
	f := newProvFixture(t)
	f.upstreamCommit(t, "docs/public/a.md", "# A\n\nPage A.\n")
	f.upstreamCommit(t, "docs/public/sub/b.md", "# B\n")
	f.upstreamCommit(t, "docs/public/img.png", "not markdown")
	f.upstreamCommit(t, "docs/other/c.md", "# outside the prefix\n")
	m := newMirrorFake()
	s := newMirrorSweeper(t, f, m, "docs/public/")
	res := s.MirrorAll(context.Background())
	if got := ingestedPaths(m); got != "docs/public/a.md,docs/public/sub/b.md" {
		t.Fatalf("first pass ingested %q", got)
	}
	if res[0].Counts[MirrorIngested] != 2 || res[0].Counts[MirrorNotMarkdown] != 1 {
		t.Fatalf("counts %v", res[0].Counts)
	}
	if last := s.LastMirrors(); len(last) != 1 || last[0].ProjectID != f.project || last[0].Counts[MirrorIngested] != 2 || last[0].At.IsZero() {
		t.Fatalf("LastMirrors = %+v", last)
	}
	got := m.ingested[0]
	if want := MirrorArtifactID(f.project, f.scope, "docs/public/a.md", got.Blob); got.ArtifactID != want ||
		string(got.Content) != "# A\n\nPage A.\n" || got.RepoScope != f.scope {
		t.Fatalf("ingested file %+v, want id %s", got, want)
	}
	// A second pass: every version has a live record, so nothing is read.
	m.ingested, m.blobReads = nil, 0
	res = s.MirrorAll(context.Background())
	if len(m.ingested) != 0 || m.blobReads != 0 || res[0].Counts[MirrorUnchanged] != 2 {
		t.Fatalf("second pass ingested %q, read %d blobs, counts %v", ingestedPaths(m), m.blobReads, res[0].Counts)
	}
	// A changed file is a new version, with a new id.
	f.upstreamCommit(t, "docs/public/a.md", "# A\n\nPage A, version two.\n")
	s.MirrorAll(context.Background())
	if got := ingestedPaths(m); got != "docs/public/a.md" || m.ingested[0].ArtifactID == got {
		t.Fatalf("after a change ingested %q", got)
	}
}

func TestMirror_SkipsNeverMarkedOversizeAndSymlinks_639(t *testing.T) {
	f := newProvFixture(t)
	f.upstreamCommit(t, "docs/public/hidden.md", "---\ndisclosure: never\n---\n# Hidden\n")
	f.upstreamCommit(t, "docs/public/big.md", strings.Repeat("x", mirrorMaxFileBytes+1))
	if err := os.Symlink("hidden.md", filepath.Join(f.upstream, "docs/public/link.md")); err != nil {
		t.Fatal(err)
	}
	f.upstreamCommit(t, "docs/public/ok.md", "# Ok\n")
	m := newMirrorFake()
	s := newMirrorSweeper(t, f, m, "docs/public/")
	res := s.MirrorAll(context.Background())
	c := res[0].Counts
	if ingestedPaths(m) != "docs/public/ok.md" || c[MirrorNeverMarked] != 1 || c[MirrorTooLarge] != 1 || c[MirrorNotRegular] != 1 {
		t.Fatalf("ingested %q, counts %v", ingestedPaths(m), c)
	}
	// The skipped versions are not read again (review 385f F1), and a hit
	// counts unchanged, so never_marked counts first encounters (87b2 N3).
	m.blobReads = 0
	res = s.MirrorAll(context.Background())
	if m.blobReads != 0 || res[0].Counts[MirrorNeverMarked] != 0 || res[0].Counts[MirrorUnchanged] != 3 {
		t.Fatalf("second pass read %d blobs, counts %v", m.blobReads, res[0].Counts)
	}
}

func TestMirror_QuarantineAndTerminalRecords_639(t *testing.T) {
	f := newProvFixture(t)
	for _, n := range []string{"q", "stable", "unreach", "pending"} {
		f.upstreamCommit(t, "docs/public/"+n+".md", "# "+n+"\n")
	}
	m := newMirrorFake()
	s := newMirrorSweeper(t, f, m, "docs/public/")
	id := func(name string) string {
		blob := provGit(t, f.clone, "rev-parse", "refs/remotes/origin/main:docs/public/"+name+".md")
		return MirrorArtifactID(f.project, f.scope, "docs/public/"+name+".md", blob)
	}
	m.quarantined[id("q")] = true
	m.recs[id("stable")] = &persistence.ChunkProvenance{State: ProvenanceUnverified, Reason: ProvReasonPathNotAllowed}
	m.recs[id("unreach")] = &persistence.ChunkProvenance{State: ProvenanceUnverified, Reason: ProvReasonCommitUnreachable}
	m.recs[id("pending")] = &persistence.ChunkProvenance{State: ProvenanceUnverified, Reason: ProvReasonPendingStamp}
	res := s.MirrorAll(context.Background())
	c := res[0].Counts
	if ingestedPaths(m) != "docs/public/unreach.md" || c[MirrorQuarantined] != 1 || c[MirrorTerminal] != 1 || c[MirrorUnchanged] != 1 {
		t.Fatalf("ingested %q, counts %v", ingestedPaths(m), c)
	}
}

// The cap counts ingests, not files read: never-marked files sorted ahead of
// a mirrorable one do not stop it in the same pass (review 385f F1).
func TestMirror_CapCountsIngestsOnly_639(t *testing.T) {
	f := newProvFixture(t)
	for i := 0; i < mirrorMaxFilesPerPass+50; i++ {
		provWrite(t, f.upstream, fmt.Sprintf("docs/public/a%03d.md", i), fmt.Sprintf("---\ndisclosure: never\n---\n# %d\n", i))
	}
	provWrite(t, f.upstream, "docs/public/z.md", "# Z\n")
	f.upstreamCommit(t, "docs/public/zz.md", "# ZZ\n")
	m := newMirrorFake()
	res := newMirrorSweeper(t, f, m, "docs/public/").MirrorAll(context.Background())
	if ingestedPaths(m) != "docs/public/z.md,docs/public/zz.md" || res[0].Counts[MirrorNeverMarked] != mirrorMaxFilesPerPass+50 {
		t.Fatalf("ingested %q, counts %v", ingestedPaths(m), res[0].Counts)
	}
	// With more mirrorable files than the cap, the rest wait for the next
	// pass and are counted.
	for i := 0; i < mirrorMaxFilesPerPass+5; i++ {
		provWrite(t, f.upstream, fmt.Sprintf("docs/public/m%03d.md", i), fmt.Sprintf("# m %d\n", i))
	}
	f.upstreamCommit(t, "docs/public/mzz.md", "# last\n")
	m.ingested = nil
	res = newMirrorSweeper(t, f, m, "docs/public/").MirrorAll(context.Background())
	if len(m.ingested) != mirrorMaxFilesPerPass || res[0].Pending == 0 || res[0].Counts[MirrorCapped] != res[0].Pending {
		t.Fatalf("ingested %d, pending %d, counts %v", len(m.ingested), res[0].Pending, res[0].Counts)
	}
}

func TestMirror_RefusedCloneIngestsNothing_639(t *testing.T) {
	f := newProvFixture(t)
	f.upstreamCommit(t, "docs/public/a.md", "# A\n")
	m := newMirrorFake()
	s := newMirrorSweeper(t, f, m, "docs/public/")
	s.Mirror.Projects = func() []MirrorProject {
		return []MirrorProject{{ProjectID: f.project, RepoScope: "github.com/someone/else", Paths: []string{"docs/public/"}}}
	}
	res := s.MirrorAll(context.Background())
	if len(m.ingested) != 0 || res[0].Counts[MirrorNoClone] != 1 {
		t.Fatalf("a clone whose origin is not the scope was mirrored: %q %v", ingestedPaths(m), res[0].Counts)
	}
}

func TestMirrorArtifactID_639(t *testing.T) {
	a := MirrorArtifactID("p", "s", "docs/a.md", "b1")
	if a != MirrorArtifactID("p", "s", "docs/a.md", "b1") || a == MirrorArtifactID("p", "s", "docs/a.md", "b2") ||
		!strings.HasPrefix(a, MirrorArtifactPrefix+"_") {
		t.Fatalf("id %q is not deterministic per version", a)
	}
}

// Nested prefixes list a file once (review 3cdc F6).
func TestMirror_NestedPrefixesCountOnce_639(t *testing.T) {
	f := newProvFixture(t)
	f.upstreamCommit(t, "docs/public/sub/b.md", "# B\n")
	m := newMirrorFake()
	res := newMirrorSweeper(t, f, m, "docs/public/", "docs/public/sub/").MirrorAll(context.Background())
	if len(m.ingested) != 1 || res[0].Counts[MirrorIngested] != 1 || res[0].Counts[MirrorUnchanged] != 0 {
		t.Fatalf("ingested %q, counts %v", ingestedPaths(m), res[0].Counts)
	}
}

// The disclosure filter fails closed (security review 2026-11-05): a file
// whose front matter does not parse is skipped, not mirrored, because its
// mark cannot be read; and a file that names disclosureExclude sections is
// skipped, because mirroring it whole would serve the excluded sections
// (stripping them would change its bytes, so it could never verify).
func TestMirror_DisclosureFilterFailsClosed_639(t *testing.T) {
	f := newProvFixture(t)
	f.upstreamCommit(t, "docs/public/broken.md", "---\ndisclosure: never\nsources: [unterminated\n---\n# Broken\n")
	f.upstreamCommit(t, "docs/public/partial.md", "---\ndisclosure: customer\ndisclosureExclude:\n    - \"## Secret\"\n---\n# P\n\n## Secret\n\nx\n")
	f.upstreamCommit(t, "docs/public/ok.md", "# Ok\n")
	m := newMirrorFake()
	res := newMirrorSweeper(t, f, m, "docs/public/").MirrorAll(context.Background())
	c := res[0].Counts
	if ingestedPaths(m) != "docs/public/ok.md" || c[MirrorBadFrontMatter] != 1 || c[MirrorHasExclusions] != 1 {
		t.Fatalf("ingested %q, counts %v", ingestedPaths(m), c)
	}
}
