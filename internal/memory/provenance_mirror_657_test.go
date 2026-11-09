package memory

import (
	"context"
	"errors"
	"testing"
)

// GitHub #657 (design 22, amendment 2026-11-07). Found at: production (the
// #614 corpus audit of vornik-architect, 2026-11-06): two chunks of
// docs/public/benchmarks/evidence/README.md stayed in the corpus after the
// file was re-marked `disclosure: never`; the mirror skipped the new version
// and nothing retired the old one. Regression test at: integration, the real
// mirror pass against a real upstream and clone (provFixture), with the store
// hooks recorded. Lower because: production is the reference instance; the
// SQL half is repository_retire_document_657_test.go.

func withRetire(h MirrorHooks, err error) (MirrorHooks, *[]string) {
	var calls []string
	h.Retire = func(_ context.Context, _, _, path string) (int, error) {
		calls = append(calls, path)
		if err != nil {
			return 0, err
		}
		return 2, nil
	}
	return h, &calls
}

func TestMirror_RetiresAPathReMarkedNever_657(t *testing.T) {
	f := newProvFixture(t)
	f.upstreamCommit(t, "docs/public/evidence.md", "# Evidence\n\nMirrored while customer.\n")
	f.upstreamCommit(t, "docs/public/ok.md", "# Ok\n")
	m := newMirrorFake()
	s := newMirrorSweeper(t, f, m, "docs/public/")
	var calls *[]string
	s.Mirror, calls = withRetire(s.Mirror, nil)

	s.MirrorAll(context.Background())
	if len(*calls) != 0 {
		t.Fatalf("an ingestable version must never be retired: %v", *calls)
	}

	f.upstreamCommit(t, "docs/public/evidence.md", "---\ndisclosure: never\n---\n# Evidence\n")
	f.upstreamCommit(t, "docs/public/bad.md", "---\ndisclosure: [unclosed\n---\n# Bad\n")
	f.upstreamCommit(t, "docs/public/excl.md", "---\ndisclosure: customer\ndisclosureExclude:\n    - \"## Secret\"\n---\n# Excl\n")
	res := s.MirrorAll(context.Background())
	got := map[string]bool{}
	for _, p := range *calls {
		got[p] = true
	}
	for _, want := range []string{"docs/public/evidence.md", "docs/public/bad.md", "docs/public/excl.md"} {
		if !got[want] {
			t.Errorf("a %s version kept out must retire the path's chunks; calls %v", want, *calls)
		}
	}
	if got["docs/public/ok.md"] {
		t.Error("an unchanged ingestable path must not be retired")
	}
	if res[0].Counts[MirrorRetired] != 3 {
		t.Fatalf("counts %v, want retired 3", res[0].Counts)
	}
}

func TestMirror_AFailedRetireIsRetried_657(t *testing.T) {
	f := newProvFixture(t)
	f.upstreamCommit(t, "docs/public/evidence.md", "---\ndisclosure: never\n---\n# Evidence\n")
	m := newMirrorFake()
	s := newMirrorSweeper(t, f, m, "docs/public/")
	var calls *[]string
	s.Mirror, calls = withRetire(s.Mirror, errors.New("db down"))

	res := s.MirrorAll(context.Background())
	if res[0].Counts[MirrorFailed] != 1 || len(*calls) != 1 {
		t.Fatalf("a failed retire is a failed file: counts %v, calls %v", res[0].Counts, *calls)
	}
	s.Mirror, calls = withRetire(s.Mirror, nil)
	res = s.MirrorAll(context.Background())
	if len(*calls) != 1 || res[0].Counts[MirrorRetired] != 1 {
		t.Fatalf("the skip must not be remembered before a retire succeeded: counts %v, calls %v", res[0].Counts, *calls)
	}
	// Remembered now: the next pass reads nothing and retires nothing.
	s.Mirror, calls = withRetire(s.Mirror, nil)
	m.blobReads = 0
	s.MirrorAll(context.Background())
	if len(*calls) != 0 || m.blobReads != 0 {
		t.Fatalf("a retired, remembered version is not retired again: calls %v, reads %d", *calls, m.blobReads)
	}
}

// After a retire, re-marking the file customer (a new blob) ingests the new
// version (review 0c47 F5).
func TestMirror_ReMarkedCustomerIngestsAgain_657(t *testing.T) {
	f := newProvFixture(t)
	f.upstreamCommit(t, "docs/public/evidence.md", "---\ndisclosure: never\n---\n# Evidence\n")
	m := newMirrorFake()
	s := newMirrorSweeper(t, f, m, "docs/public/")
	var calls *[]string
	s.Mirror, calls = withRetire(s.Mirror, nil)
	s.MirrorAll(context.Background())
	if len(*calls) != 1 || len(m.ingested) != 0 {
		t.Fatalf("never-marked: calls %v, ingested %q", *calls, ingestedPaths(m))
	}
	f.upstreamCommit(t, "docs/public/evidence.md", "---\ndisclosure: customer\n---\n# Evidence\n\nPublic again.\n")
	res := s.MirrorAll(context.Background())
	if ingestedPaths(m) != "docs/public/evidence.md" || res[0].Counts[MirrorIngested] != 1 || len(*calls) != 1 {
		t.Fatalf("re-marked customer: ingested %q, counts %v, retire calls %v", ingestedPaths(m), res[0].Counts, *calls)
	}
}
