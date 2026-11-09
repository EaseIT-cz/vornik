package architecture

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// GitHub #676 (operations-api-contract-design.md §6.9): the admin UI writes
// the live config tree from fourteen functions, and none held configlock, the
// process's one config-write lock (#584, §6.6). This law keeps every UI write
// on one path under the lock, read from the syntax tree, not by pattern:
//
//   - every non-test function in internal/ui that calls
//     writeProjectConfigAtomic also calls configlock.LockAsWriter in its own
//     body (GitHub #681: the lock plus the config_writer fence, §6.10);
//   - os.WriteFile appears only in the functions named below (a temporary
//     validation copy, or the atomic writer itself), anywhere in the package.
var uiWriteFileAllowed = map[string]string{
	"validateProjectConfigEdit": "writes a temporary copy of the tree to validate an edit",
	"copyUIDir":                 "copies the tree into that temporary directory",
	"writeProjectConfigAtomic":  "the one live writer: the backup, before its temp-and-rename",
}

func TestUIConfigWrite676_EveryWriterHoldsTheLock(t *testing.T) {
	dir := filepath.Join(moduleRoot(t), "internal", "ui")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	var unlocked, strayWrites []string
	writers, files := 0, 0
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		files++
		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			writes, locks, writeFile := false, false, false
			ast.Inspect(fn.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				switch fun := call.Fun.(type) {
				case *ast.Ident:
					if fun.Name == "writeProjectConfigAtomic" {
						writes = true
					}
				case *ast.SelectorExpr:
					if pkg, ok := fun.X.(*ast.Ident); ok {
						switch {
						case pkg.Name == "configlock" && fun.Sel.Name == "LockAsWriter": // #681: Lock alone is not enough
							locks = true
						case pkg.Name == "os" && fun.Sel.Name == "WriteFile":
							writeFile = true
						}
					}
				}
				return true
			})
			if writes {
				writers++
				if !locks {
					unlocked = append(unlocked, name+": "+fn.Name.Name)
				}
			}
			if writeFile {
				if _, ok := uiWriteFileAllowed[fn.Name.Name]; !ok {
					strayWrites = append(strayWrites, name+": "+fn.Name.Name)
				}
			}
		}
	}
	sort.Strings(unlocked)
	sort.Strings(strayWrites)
	if len(unlocked) > 0 {
		t.Errorf("UI functions that write config without holding configlock: %v. Take configlock.LockAsWriter(r.Context()) before reading the file and hold it through applyConfigEdit (§6.9; #681, §6.10: it also refuses on a daemon that is not the config writer)", unlocked)
	}
	if len(strayWrites) > 0 {
		t.Errorf("os.WriteFile outside the atomic writer in internal/ui: %v. Write the live tree through writeProjectConfigAtomic (§6.9)", strayWrites)
	}
	t.Logf("UI config-write law: %d non-test files parsed, %d functions call writeProjectConfigAtomic", files, writers)
}
