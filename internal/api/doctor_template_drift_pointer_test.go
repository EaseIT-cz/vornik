package api

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Matrix case 13: a registry row that fires, with config_template_drift
// WARNING, carries the pointer; a non-registry row does not.
func TestTemplateDriftPointer_AppendedToRegistryRowsOnly(t *testing.T) {
	checks := []DoctorCheck{
		{Name: "workflow_onfail_masking", Status: "WARNING", Message: "masking found"},
		{Name: "pricing_drift", Status: "WARNING", Message: "prices differ"},
		{Name: "config_template_drift", Status: "WARNING", Message: "drift"},
		{Name: "role_prompt_sanity", Status: "OK", Message: "fine"},
	}
	appendTemplateDriftPointer(checks)
	if !strings.Contains(checks[0].Message, "config_template_drift") {
		t.Errorf("registry row lacks the pointer: %q", checks[0].Message)
	}
	if strings.Contains(checks[1].Message, "config_template_drift") {
		t.Errorf("a non-registry row got the pointer: %q", checks[1].Message)
	}
	if strings.Contains(checks[3].Message, "config_template_drift") {
		t.Errorf("an OK registry row got the pointer: %q", checks[3].Message)
	}
}

// Matrix case 14: with config_template_drift SKIPPED — or OK — no row carries
// the pointer (it would point at a check that did not run, or found nothing).
func TestTemplateDriftPointer_NotWhenDriftIsSkippedOrOK(t *testing.T) {
	for _, st := range []string{"SKIPPED", "OK"} {
		checks := []DoctorCheck{
			{Name: "workflow_onfail_masking", Status: "WARNING", Message: "masking found"},
			{Name: "config_template_drift", Status: st},
		}
		appendTemplateDriftPointer(checks)
		if strings.Contains(checks[0].Message, "config_template_drift") {
			t.Errorf("drift %s: pointer appended: %q", st, checks[0].Message)
		}
	}
}

// Matrix case 15: every check either report builder calls is classified, so a
// new config-reading check cannot be added without a recorded decision. (The
// file is read relative to the package directory, where go test runs.)
func TestTemplateDriftPointer_EveryCheckIsClassified(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "doctor_handlers.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	seen := 0
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || (fn.Name.Name != "RunDoctor" && fn.Name.Name != "RunReportReadOnly") {
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || !strings.HasPrefix(sel.Sel.Name, "check") {
				return true
			}
			if id, ok := sel.X.(*ast.Ident); ok && id.Name == "h" {
				seen++
				if _, ok := doctorCheckConfigPointer[sel.Sel.Name]; !ok {
					t.Errorf("%s calls %s, which doctorCheckConfigPointer does not classify: "+
						"record whether its findings name deployed config content", fn.Name.Name, sel.Sel.Name)
				}
			}
			return true
		})
	}
	if seen < 40 {
		t.Fatalf("examined %d check calls in the report builders; the guard is not looking at the right functions", seen)
	}
}

// A SKIPPED registry row produced no finding about deployed content, so it
// gets no pointer — only a row that FIRED does (review 8330 F1).
func TestTemplateDriftPointer_NotOnASkippedRegistryRow(t *testing.T) {
	checks := []DoctorCheck{
		{Name: "workflow_onfail_masking", Status: "SKIPPED", Message: "no config dir"},
		{Name: "config_template_drift", Status: "WARNING"},
	}
	appendTemplateDriftPointer(checks)
	if strings.Contains(checks[0].Message, "config_template_drift") {
		t.Errorf("a SKIPPED registry row got the pointer: %q", checks[0].Message)
	}
}

// Each registry method's EMITTED name equals the name the map records, so a
// renamed check cannot silently drop out of the pointer (review 8330 F3). An
// empty handler makes every one of them report SKIPPED under its own name.
func TestTemplateDriftPointer_RegistryNamesMatchWhatTheChecksEmit(t *testing.T) {
	h := &DoctorHandlers{}
	emitted := map[string]string{
		"checkWorkflowOnFailMasking": h.checkWorkflowOnFailMasking().Name,
		"checkWorkflowMDShape":       h.checkWorkflowMDShape().Name,
		"checkRolePromptSanity":      h.checkRolePromptSanity().Name,
		"checkWorkflowSwarmCompat":   h.checkWorkflowSwarmCompat().Name,
		"checkDispatcherRole":        h.checkDispatcherRole(false).Name,
		"checkRoleLibrary":           h.checkRoleLibrary().Name,
	}
	for method, name := range doctorCheckConfigPointer {
		if name == "" {
			continue
		}
		got, ok := emitted[method]
		if !ok {
			t.Errorf("registry method %s is not exercised here; add it", method)
			continue
		}
		if got != name {
			t.Errorf("%s emits %q, the registry records %q", method, got, name)
		}
	}
}

// A check name is reported by exactly ONE method: two implementations of one
// check emit two rows under one name, and one of them is usually the wrong
// one. workflow_md_shape had two (checkWorkflowMDShape and
// checkWorkflowMdShape) until 2026-09-24 (found by the slice D review).
func TestDoctor_EachNamedCheckHasOneImplementation(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "doctor_handlers.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || (fn.Name.Name != "RunDoctor" && fn.Name.Name != "RunReportReadOnly") {
			continue
		}
		byName := map[string][]string{}
		ast.Inspect(fn, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok || !strings.HasPrefix(sel.Sel.Name, "check") {
				return true
			}
			if name := doctorCheckConfigPointer[sel.Sel.Name]; name != "" {
				byName[name] = append(byName[name], sel.Sel.Name)
			}
			return true
		})
		names := make([]string, 0, len(byName))
		for name := range byName {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if methods := byName[name]; len(methods) > 1 {
				t.Errorf("%s: check %q is reported by %d calls %v — one check, one implementation", fn.Name.Name, name, len(methods), methods)
			}
		}
	}
}

// The read-only report is a subset of the full one: a check wired only into
// RunReportReadOnly never runs where an operator looks. legacy_image_names
// shipped that way in 2026.10.2 and 2026.10.3 and stayed silent on a
// production tree that named the legacy image (EaseIT-cz migration design
// §5.2, as shipped).
func TestDoctor_ReadOnlyChecksAreAllInTheFullReport(t *testing.T) {
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, "doctor_handlers.go", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	calls := map[string]map[string]bool{"RunDoctor": {}, "RunReportReadOnly": {}}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || calls[fn.Name.Name] == nil {
			continue
		}
		ast.Inspect(fn, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && strings.HasPrefix(sel.Sel.Name, "check") {
				if id, ok := sel.X.(*ast.Ident); ok && id.Name == "h" {
					calls[fn.Name.Name][sel.Sel.Name] = true
				}
			}
			return true
		})
	}
	if len(calls["RunReportReadOnly"]) < 20 || len(calls["RunDoctor"]) < 40 {
		t.Fatalf("found %d read-only and %d full check calls; the guard is not looking at the right functions",
			len(calls["RunReportReadOnly"]), len(calls["RunDoctor"]))
	}
	var missing []string
	for c := range calls["RunReportReadOnly"] {
		if !calls["RunDoctor"][c] {
			missing = append(missing, c)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Errorf("RunReportReadOnly calls %v, which RunDoctor (vornikctl doctor, the UI) never runs", missing)
	}
	if !calls["RunDoctor"]["checkLegacyImageNames"] {
		t.Error("the full doctor report must run checkLegacyImageNames")
	}
}

// The containment above is sound only if those two are the only functions that
// assemble a doctor report; a third builder could hide a check from both
// (review 2666 F2). Every non-test file in this package is scanned.
func TestDoctor_OnlyTwoFunctionsBuildAReport(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	builders := map[string]bool{}
	scanned := 0
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		scanned++
		f, err := parser.ParseFile(token.NewFileSet(), path, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			fn, ok := d.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				as, ok := n.(*ast.AssignStmt)
				if !ok || len(as.Lhs) != 1 {
					return true
				}
				if sel, ok := as.Lhs[0].(*ast.SelectorExpr); ok && sel.Sel.Name == "Checks" {
					if call, ok := as.Rhs[0].(*ast.CallExpr); ok {
						if id, ok := call.Fun.(*ast.Ident); ok && id.Name == "append" {
							builders[path+":"+fn.Name.Name] = true
						}
					}
				}
				return true
			})
		}
	}
	want := map[string]bool{"doctor_handlers.go:RunDoctor": true, "doctor_handlers.go:RunReportReadOnly": true}
	if scanned < 50 {
		t.Fatalf("scanned %d files; the guard is not looking at this package", scanned)
	}
	for b := range builders {
		if !want[b] {
			t.Errorf("%s appends doctor checks: a third report builder; wire it into the containment test or fold it into RunDoctor", b)
		}
	}
	for b := range want {
		if !builders[b] {
			t.Errorf("%s no longer appends checks; update this guard", b)
		}
	}
}
