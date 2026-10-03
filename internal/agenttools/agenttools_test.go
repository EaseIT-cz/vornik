package agenttools

import (
	"reflect"
	"sort"
	"testing"
)

func TestIsBuiltin(t *testing.T) {
	for _, name := range []string{"file_read", "run_shell", "memory_search", "git_show", "tool_result_read", "query_api", "list_apis"} {
		if !IsBuiltin(name) {
			t.Errorf("IsBuiltin(%q) = false, want true", name)
		}
	}
	for _, name := range []string{"", "web_fetch", "mcp__scraper__web_fetch", "FILE_READ", "nope"} {
		if IsBuiltin(name) {
			t.Errorf("IsBuiltin(%q) = true, want false", name)
		}
	}
}

func TestIsMCPTool(t *testing.T) {
	if !IsMCPTool("mcp__scraper__web_fetch") {
		t.Error("mcp__scraper__web_fetch should be an MCP tool")
	}
	for _, name := range []string{"mcp__", "file_read", "", "mcp_scraper"} {
		if IsMCPTool(name) {
			t.Errorf("IsMCPTool(%q) = true, want false", name)
		}
	}
}

func TestNamesSortedAndComplete(t *testing.T) {
	names := Names()
	if len(names) != len(Tools) {
		t.Fatalf("Names() returned %d entries, want %d", len(names), len(Tools))
	}
	if !sort.StringsAreSorted(names) {
		t.Errorf("Names() not sorted: %v", names)
	}
	for _, n := range names {
		if Get(n) == nil {
			t.Errorf("Names() returned unknown tool %q", n)
		}
	}
}

func TestSetIsCopy(t *testing.T) {
	s := Set()
	want := map[string]bool{}
	for _, n := range Names() {
		want[n] = true
	}
	if !reflect.DeepEqual(s, want) {
		t.Fatal("Set() should equal the declared set")
	}
	// Mutating the returned map must not affect the package state.
	s["injected_tool"] = true
	if IsBuiltin("injected_tool") {
		t.Error("Set() returned a live reference, not a copy")
	}
}

// Operator ruling 2026-08-14: both are universal. They only read what the
// project already knows, so gating them buys no containment.
func TestAlwaysGranted_CoversTheReadOnlyBaseline(t *testing.T) {
	for _, want := range []string{"memory_search", "skill_fetch"} {
		if !IsAlwaysGranted(want) {
			t.Errorf("%s is not in the universal baseline", want)
		}
		if !IsBuiltin(want) {
			t.Errorf("%s is granted everywhere but is not a known built-in", want)
		}
	}
}

// The baseline is read-only by construction. A tool that writes, executes, or
// leaves the deployment must never be added here without the same argument
// being made again — this test is where that argument gets re-read.
func TestAlwaysGranted_ContainsNothingThatActs(t *testing.T) {
	for _, acting := range []string{
		"file_write", "file_edit", "run_shell", "backlog_deposit",
		"test_run", "lint_run", "typecheck_run", "query_api",
	} {
		if IsAlwaysGranted(acting) {
			t.Errorf("%s is granted to every role, but it acts rather than reads", acting)
		}
	}
}

// Callers append to the result; handing out the package's own slice would let
// one of them widen the baseline for the whole process.
func TestAlwaysGranted_ReturnsACopy(t *testing.T) {
	got := AlwaysGranted()
	if len(got) == 0 {
		t.Fatal("baseline is empty")
	}
	got[0] = "mutated"
	if AlwaysGranted()[0] == "mutated" {
		t.Fatal("AlwaysGranted handed out its backing array")
	}
}

// Agent-administered design §18.8 (2026-10-02): agent roles receive this
// baseline although their declared tools omit it, so the set is pinned
// exactly. A tool added to the baseline later fails here and forces the
// decision §18.8 records (what it reaches from an agent project) to be made
// again, instead of arriving in every agent role silently.
func TestAlwaysGranted_IsExactlyTheReviewedBaseline(t *testing.T) {
	got := AlwaysGranted()
	sort.Strings(got)
	want := []string{"memory_search", "skill_fetch"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("AlwaysGranted() = %v, want exactly %v; a change here needs §18.8 re-decided", got, want)
	}
	// The other way a tool reaches every role: exempt from the gate by design
	// (measured 2026-10-03; §18.8 first named tool_result_read as part of
	// AlwaysGranted, which it is not).
	if ex := ExemptNames(); !reflect.DeepEqual(ex, []string{"tool_result_read", "tool_search"}) {
		t.Fatalf("ExemptNames() = %v; a change here needs §18.8 re-decided", ex)
	}
}

// EveryRole is what every role can call beyond its declared tools.
func TestEveryRole_IsTheBaselineAndTheExemptTools(t *testing.T) {
	got := EveryRole()
	if !reflect.DeepEqual(got, []string{"memory_search", "skill_fetch", "tool_result_read", "tool_search"}) {
		t.Fatalf("EveryRole() = %v", got)
	}
}
