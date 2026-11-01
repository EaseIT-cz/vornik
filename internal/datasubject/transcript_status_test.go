package datasubject

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

// Slice 5d-1 (GitHub #344, design §15.14): a task status the live predicate
// does not classify would make 5d write a row a task is still reading, or
// defer one forever. This parses persistence's TaskStatus constants rather
// than listing them, so a new status fails here until it is classified.
func TestTranscriptStatuses_ClassifyEveryTaskStatus(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "../persistence/models.go", nil, 0)
	if err != nil {
		t.Fatalf("parse models.go: %v", err)
	}
	var all []string
	ast.Inspect(f, func(n ast.Node) bool {
		vs, ok := n.(*ast.ValueSpec)
		if !ok || vs.Type == nil {
			return true
		}
		if id, ok := vs.Type.(*ast.Ident); !ok || id.Name != "TaskStatus" {
			return true
		}
		for _, v := range vs.Values {
			if lit, ok := v.(*ast.BasicLit); ok {
				s, _ := strconv.Unquote(lit.Value)
				all = append(all, s)
			}
		}
		return true
	})
	if len(all) < 13 {
		t.Fatalf("found %d TaskStatus constants, want at least 13; the parse is broken", len(all))
	}
	class := map[string]string{}
	for _, s := range transcriptLiveStatuses {
		class[string(s)] = "live"
	}
	for _, s := range transcriptSettledStatuses {
		if class[string(s)] != "" {
			t.Errorf("%s is both live and settled", s)
		}
		class[string(s)] = "settled"
	}
	for _, s := range all {
		if class[s] == "" {
			t.Errorf("TaskStatus %s is neither live nor settled for transcript erasure", s)
		}
	}
	if class["COMPLETED"] != "settled" {
		t.Errorf("COMPLETED must be settled (O-5d-6)")
	}
}
