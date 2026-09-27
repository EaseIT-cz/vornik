package executor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"vornik.io/vornik/internal/agenttools"
	"vornik.io/vornik/internal/registry"
)

// Media routing LLD §4.2a (2026-09-25): staging follows what the role can
// read. extractTaskInputArtifacts skipped raw staging of an extracted document
// on the assumption the agent reads it with the document_* tools, and never
// checked the step's role could call them. T-8f69: a builtins-only reviewer
// made 17 calls to document_get_outline, each refused 403, and reviewed
// nothing (backlog P2, companion deposit of 2026-07-25).

const (
	toolOutline = "mcp__vornik__document_get_outline"
	toolRead    = "mcp__vornik__document_read_section"
)

func stagingSwarm(roles ...registry.SwarmRole) *registry.Swarm {
	return &registry.Swarm{ID: "s1", Roles: roles}
}

func TestExtractionAccess(t *testing.T) {
	cases := []struct {
		name      string
		allowed   []string
		role      string
		wantAble  bool
		wantCause string
	}{
		{"no allowlist is unrestricted, so able", nil, "r", true, ""},
		{"server wildcard", []string{"mcp__vornik__*"}, "r", true, ""},
		{"global mcp wildcard", []string{"mcp__*"}, "r", true, ""},
		{"both qualified", []string{toolOutline, toolRead}, "r", true, ""},
		{"both bare", []string{"document_get_outline", "document_read_section"}, "r", true, ""},
		{"outline only", []string{"file_read", toolOutline}, "r", false, "read_section_missing"},
		{"read only", []string{"file_read", toolRead}, "r", false, "outline_missing"},
		{"builtins only (T-8f69)", []string{"file_read", "grep", "memory_search"}, "r", false, "both_missing"},
		{"role missing from the swarm fails CLOSED", nil, "ghost", false, "unresolved_role"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sw := stagingSwarm(registry.SwarmRole{Name: "r", Permissions: registry.SwarmRolePermissions{AllowedTools: tc.allowed}})
			able, cause := extractionAccess(sw, tc.role)
			if able != tc.wantAble || cause != tc.wantCause {
				t.Fatalf("extractionAccess = (%v, %q), want (%v, %q)", able, cause, tc.wantAble, tc.wantCause)
			}
			// Agreement with the gate: for every RESOLVABLE role the decision is
			// exactly what /mcp/call's shared predicate says for both tools. The
			// one deliberate disagreement is the unresolvable role: the gate
			// fails open, staging fails closed.
			if tc.role == "r" {
				gate := agenttools.AllowlistAdmits(tc.allowed, toolOutline) && agenttools.AllowlistAdmits(tc.allowed, toolRead)
				if able != gate {
					t.Fatalf("executor says %v, the /mcp/call predicate says %v", able, gate)
				}
			}
		})
	}
	if able, cause := extractionAccess(nil, "r"); able || cause != "unresolved_role" {
		t.Fatalf("no swarm must fail closed, got (%v, %q)", able, cause)
	}
}

func TestDecideInputStaging(t *testing.T) {
	wf := func(declared bool, roles ...string) *registry.Workflow {
		w := &registry.Workflow{ID: "wf", RequireInputArtifacts: declared, Steps: map[string]registry.WorkflowStep{}}
		for i, r := range roles {
			w.Steps[string(rune('a'+i))] = registry.WorkflowStep{Type: "agent", Role: r}
		}
		return w
	}
	sw := stagingSwarm(
		registry.SwarmRole{Name: "researcher"},
		registry.SwarmRole{Name: "reviewer", Permissions: registry.SwarmRolePermissions{AllowedTools: []string{"file_read", "git_diff"}}},
		registry.SwarmRole{Name: "blind", Permissions: registry.SwarmRolePermissions{AllowedTools: []string{"current_time", toolOutline, toolRead}}},
	)

	d := decideInputStaging(&executionPlan{swarm: sw, workflow: wf(false, "researcher")})
	if d.force || len(d.unable) != 0 {
		t.Fatalf("every role can open extractions: nothing forced, got %+v", d)
	}
	d = decideInputStaging(&executionPlan{swarm: sw, workflow: wf(false, "researcher", "reviewer")})
	if !d.force || d.unable["reviewer"] != "both_missing" || len(d.unable) != 1 {
		t.Fatalf("one unable role must force staging and be named, got %+v", d)
	}
	d = decideInputStaging(&executionPlan{swarm: sw, workflow: wf(true, "researcher")})
	if !d.force || len(d.unable) != 0 {
		t.Fatalf("a declared require_input_artifacts still forces, with no unable role, got %+v", d)
	}
	d = decideInputStaging(&executionPlan{swarm: sw, workflow: wf(false, "blind")})
	if len(d.noReader) != 1 || d.noReader[0] != "blind" {
		t.Fatalf("a role with none of the four file readers must be recorded, got %+v", d)
	}
	if d.force {
		t.Fatal("a role that can open extractions forces nothing, file reader or not")
	}

	// A plan step runs whichever swarm roles its lead picks, none declared in
	// the workflow: every swarm role is a candidate.
	adaptive := &registry.Workflow{ID: "adaptive", Steps: map[string]registry.WorkflowStep{
		"plan": {Type: "plan", Role: "researcher"},
	}}
	d = decideInputStaging(&executionPlan{swarm: sw, workflow: adaptive})
	if !d.force || d.unable["reviewer"] != "both_missing" {
		t.Fatalf("an adaptive workflow must consider every swarm role, got %+v", d)
	}
}

// End to end through the real staging function: an extracted, within-cap
// document is staged for a workflow with an unable role, not staged over the
// cap, and not staged when every role can read the extraction.
func TestInputStaging_FollowsTheRole(t *testing.T) {
	dir := t.TempDir()
	small := filepath.Join(dir, "cv.pdf")
	big := filepath.Join(dir, "book.epub")
	if err := os.WriteFile(small, []byte("tiny"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(big, make([]byte, 4096), 0o600); err != nil {
		t.Fatal(err)
	}
	payload := []byte(`{"context":{"inputFiles":["` + small + `","` + big + `"],
		"inputExtractions":[{"extracted_document_id":"d1"},{"extracted_document_id":"d2"}]}}`)

	able := inputStagingDecision{}
	if got := extractTaskInputArtifacts(payload, 0, able.force, 1024); got != nil {
		t.Fatalf("every role able: extracted documents stay unstaged, got %v", got)
	}
	unable := inputStagingDecision{force: true, unable: map[string]string{"reviewer": "both_missing"}}
	got := extractTaskInputArtifacts(payload, 0, unable.force, 1024)
	if len(got) != 1 || got[0]["name"] != "cv.pdf" {
		t.Fatalf("unable role: the within-cap document is staged and the over-cap one is not, got %v", got)
	}

	m := NewMetrics(prometheus.NewRegistry())
	e := &Executor{metrics: m}
	e.observeInputStaging("task-1", payload, got, unable)
	if v := testutil.ToFloat64(m.InputStagingRoleUnableTotal.WithLabelValues("staged", "both_missing")); v != 1 {
		t.Errorf("staged/both_missing = %v, want 1", v)
	}
	if v := testutil.ToFloat64(m.InputStagingRoleUnableTotal.WithLabelValues("over_cap", "both_missing")); v != 1 {
		t.Errorf("over_cap/both_missing = %v, want 1", v)
	}
	noReader := inputStagingDecision{force: true, unable: map[string]string{"blind": "unresolved_role"}, noReader: []string{"blind"}}
	e.observeInputStaging("task-2", payload, got, noReader)
	if v := testutil.ToFloat64(m.InputStagingNoFileReaderTotal); v != 1 {
		t.Errorf("no_file_reader = %v, want 1 (one staged file, one role without a reader)", v)
	}
	if v := testutil.ToFloat64(m.InputStagingRoleUnableTotal.WithLabelValues("staged", "unresolved_role")); v != 1 {
		t.Errorf("staged/unresolved_role = %v, want 1", v)
	}
	// Nil-safe: an executor with no metrics must not panic.
	(&Executor{}).observeInputStaging("task-3", payload, got, unable)
}

// The prompt, per step. An over-cap document for a role that cannot open
// extractions must not tell it to call tools it lacks (the T-8f69 trigger was
// the ATTACHED DOCUMENTS preamble); a role that can, gets today's text.
func TestAttachedBlock_OverCapUnableRoleIsToldThePlainTruth(t *testing.T) {
	files := []string{"/store/x/book.epub"}
	ext := []map[string]any{{"extracted_document_id": "d1", "artifact_id": "a1", "title": "Book"}}

	unable := buildAttachedFilesBlockForRole(files, ext, nil, false)
	for _, banned := range []string{"document_read_section", "mcp__vornik__document_get_outline"} {
		if strings.Contains(unable, banned) {
			t.Errorf("an unable role's prompt must not name %q:\n%s", banned, unable)
		}
	}
	for _, want := range []string{"cannot open the file", "memory_search returns matching passages", "say that you could not read the file"} {
		if !strings.Contains(unable, want) {
			t.Errorf("an unable role's prompt must say %q:\n%s", want, unable)
		}
	}

	capable := buildAttachedFilesBlockForRole(files, ext, nil, true)
	if !strings.Contains(capable, "mcp__vornik__document_get_outline") || !strings.Contains(capable, boundedReadContract) {
		t.Errorf("a capable role keeps today's text and the paging contract:\n%s", capable)
	}
}

// Two steps, one unable reviewer and one capable researcher, over a within-cap
// extracted file that was staged because of the reviewer. Both see it under
// ATTACHED FILES at its staged path; only the capable role is told the
// document_read_section route, and its block carries the paging contract
// exactly once (round 3 F1: staging moved the file out of the only block that
// carried it). Neither is told the file is not staged.
func TestAttachedBlock_StagedAndExtractedKeepsThePagingContract(t *testing.T) {
	files := []string{"/store/x/cv.pdf", "/store/x/notes.txt"}
	ext := []map[string]any{{"extracted_document_id": "d1", "artifact_id": "a1"}, {}}
	staged := map[string]string{"cv.pdf": "/app/workspace/artifacts/in/cv.pdf", "notes.txt": "/app/workspace/artifacts/in/notes.txt"}

	capable := buildAttachedFilesBlockForRole(files, ext, staged, true)
	unable := buildAttachedFilesBlockForRole(files, ext, staged, false)
	for name, block := range map[string]string{"capable": capable, "unable": unable} {
		if !strings.Contains(block, "/app/workspace/artifacts/in/cv.pdf") {
			t.Errorf("%s: the staged file must be listed at its staged path:\n%s", name, block)
		}
		if strings.Contains(block, "NOT staged") {
			t.Errorf("%s: a staged file must never be called unstaged:\n%s", name, block)
		}
	}
	if !strings.Contains(capable, "readable with document_read_section") {
		t.Errorf("the capable role's line must carry the route:\n%s", capable)
	}
	if strings.Count(capable, "readable with document_read_section") != 1 {
		t.Errorf("only the extracted file carries the route, not the plain one:\n%s", capable)
	}
	if n := strings.Count(capable, boundedReadContract); n != 1 {
		t.Errorf("the paging contract must appear exactly once, got %d:\n%s", n, capable)
	}
	if strings.Contains(unable, "document_read_section") {
		t.Errorf("the unable role must not be told a route it cannot call:\n%s", unable)
	}

	plain := buildAttachedFilesBlockForRole([]string{"/store/x/notes.txt"}, nil, map[string]string{"notes.txt": "/app/workspace/artifacts/in/notes.txt"}, true)
	if strings.Contains(plain, boundedReadContract) {
		t.Errorf("a block with no extracted entry carries no contract:\n%s", plain)
	}
}

// The capable-role ATTACHED DOCUMENTS text is byte-identical to before §4.2a:
// the paging sentence became a shared constant, and the output must not move
// by a character (review-20260925 implementation, suggestion 3). The expected
// strings are the pre-change literals, copied verbatim.
func TestAttachedBlock_CapableRoleTextIsUnchanged(t *testing.T) {
	const before = "## ATTACHED DOCUMENTS (already in project memory)\n" +
		"These documents have been extracted into structured text + indexed into project memory at task-creation time. The raw binary is NOT staged in the container — access the content via mcp__vornik__document_get_outline / document_read_section / document_get_metadata (use the extracted_document_id below), or via memory_search for cross-document queries. Do NOT attempt to file_read these documents — there is no staged file path.\n" +
		"Read in bounded slices: document_read_section takes offset_chars + limit_chars, and returns next_offset with has_more — page with those rather than pulling a section whole. A raw read of a 600 KB EPUB or a 30 MB PDF blows the context window of every model in the fallback chain, which is why there is no staged path to read.\n"
	got := buildAttachedFilesBlockForRole([]string{"/store/x/book.epub"},
		[]map[string]any{{"extracted_document_id": "d1", "artifact_id": "a1"}}, nil, true)
	if !strings.HasPrefix(got, before) {
		t.Fatalf("capable-role text changed:\n got: %q\nwant prefix: %q", got, before)
	}
	if got != buildAttachedFilesBlockStaged([]string{"/store/x/book.epub"},
		[]map[string]any{{"extracted_document_id": "d1", "artifact_id": "a1"}}, nil) {
		t.Fatal("the legacy entry point must equal the capable-role builder")
	}
}
