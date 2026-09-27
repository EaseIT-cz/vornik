package api

import (
	"go/ast"
	"go/parser"
	"go/token"
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
