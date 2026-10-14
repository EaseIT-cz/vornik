package memory

// LAW: every function that reads memory-chunk CONTENT carries the GDPR Art 18
// restriction predicate, or is on the exemption list below with its reason.
//
// audit 2026-10-12 (security review of #343, "fail-open restriction predicate"):
// the first cut screened restricted chunks only in the Searcher, after the SQL,
// and left eight other content reads (embedding, titling, classification, graph
// extraction, listings, liveness) sending restricted text onward. A post-filter
// is a denylist of call sites; this test turns it into a rule with a published
// denominator, and a new content read fails here until someone decides about it.
//
// see LLD § https://docs.vornik.io §4.16.6

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// restrictionExempt lists content reads that deliberately do NOT carry the
// predicate. Key: "<path from repo root>:<func>". Every reason must say why
// reaching a restricted chunk is required or is not a use.
var restrictionExempt = map[string]string{
	// Erasure, redaction and rectification must reach restricted data: Art 18 does
	// not suspend Art 16/17, and an erasure that skipped restricted rows would
	// leave exactly the data the subject asked to remove.
	"internal/memory/repository.go:DeleteByExtractedDocument":              "erasure path (Art 17)",
	"internal/memory/repository.go:DeleteByArtifact":                       "erasure path (Art 17)",
	"internal/memory/project_cache_eviction.go:EvictProjectEmbeddingCache": "project deletion: derives cache keys of rows being deleted",
	"internal/persistence/postgres/chunk_redactor.go:LoadChunk":            "Art 16/17 rewrite of a linked row",
	"internal/persistence/postgres/chunk_redactor.go:RedactChunk":          "Art 16/17 rewrite of a linked row",
	"internal/persistence/subjectstore/repository.go:DeleteRow":            "Art 17 row deletion",
	"internal/memory/repository_correct.go:evictionCacheKeys":              "eviction (hard delete) derives cache keys of rows being deleted",
	"internal/memory/repository.go:CountChunksMissingTitle":                "a COUNT of rows with no title; returns a number, not content",
	"internal/memory/repository.go:DocumentVersionIsWhole":                 "ingest integrity check; compares stored text, sends nothing onward",
	"internal/memory/repository_correct.go:ChunkPreviewByID":               "operator correction/forget flow shows the row it is about to act on",
	"internal/memory/repository_correct.go:ListUnverifiedChunks":           "operator administration (vornikctl memory audit)",
	"internal/retention/memory_approval.go:MemoryApprovalRequests":         "operator approval of a deletion; the approver must see what is deleted",
}

// contentRead matches a SELECT whose text names the chunk's content or its
// derived title. content_hash / content_class are not content.
var contentRead = regexp.MustCompile(`(?is)\bSELECT\b.*\b(content|content_title)\b`)

func repoRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("no caller")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
}

func TestLaw_EveryChunkContentReadCarriesTheRestrictionPredicate(t *testing.T) {
	root := repoRoot(t)
	examined, guarded := 0, 0
	var missing, staleExempt []string
	seenExempt := map[string]bool{}

	for _, dir := range []string{"internal", "cmd"} {
		err := filepath.Walk(filepath.Join(root, dir), func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if info.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			rel, _ := filepath.Rel(root, path)
			if strings.HasSuffix(rel, "persistence/migrations.go") || strings.HasSuffix(rel, "sqlite/schema.go") {
				return nil
			}
			fset := token.NewFileSet()
			f, perr := parser.ParseFile(fset, path, nil, 0)
			if perr != nil {
				return nil
			}
			for _, d := range f.Decls {
				fn, ok := d.(*ast.FuncDecl)
				if !ok || fn.Body == nil {
					continue
				}
				var lits []string
				guards := false
				ast.Inspect(fn.Body, func(n ast.Node) bool {
					switch x := n.(type) {
					case *ast.BasicLit:
						if x.Kind == token.STRING {
							if s, err := strconv.Unquote(x.Value); err == nil {
								lits = append(lits, s)
							}
						}
					case *ast.SelectorExpr:
						if x.Sel.Name == "ChunkNotRestrictedSQL" {
							guards = true
						}
					case *ast.CallExpr:
						// Inside package persistence the predicate is called
						// unqualified (GitHub #345: SQLSubjectTextSearch lives
						// next to it), which is the same guard.
						if id, ok := x.Fun.(*ast.Ident); ok && id.Name == "ChunkNotRestrictedSQL" {
							guards = true
						}
					}
					return true
				})
				reads := false
				for _, s := range lits {
					if strings.Contains(s, "project_memory_chunks") && contentRead.MatchString(s) {
						reads = true
					}
				}
				if !reads {
					continue
				}
				examined++
				key := rel + ":" + fn.Name.Name
				if guards {
					guarded++
					continue
				}
				if _, ok := restrictionExempt[key]; ok {
					seenExempt[key] = true
					continue
				}
				missing = append(missing, key)
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	for k := range restrictionExempt {
		if !seenExempt[k] {
			staleExempt = append(staleExempt, k)
		}
	}
	sort.Strings(missing)
	sort.Strings(staleExempt)
	// The DENOMINATOR, published on every run so "clean" is never "examined nothing".
	t.Logf("chunk content reads examined: %d; carrying the predicate: %d; exempt: %d",
		examined, guarded, len(seenExempt))
	if examined < 10 {
		t.Fatalf("only %d content reads found — the scan is probably broken, and a vacuous law passes everything", examined)
	}
	for _, m := range missing {
		t.Errorf("reads chunk content without persistence.ChunkNotRestrictedSQL and is not exempt: %s", m)
	}
	for _, s := range staleExempt {
		t.Errorf("exemption names a content read that no longer exists (remove it): %s", s)
	}
}
