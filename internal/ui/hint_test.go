package ui

// Standing explanations on the reporting pages sit behind an ⓘ that opens on
// hover, keyboard focus or a tap (operator 2026-11-01; web-ui-design.md
// "Explanations behind an ⓘ — 2026-11-01", B). Found at: production (the
// operator reading the overview and the trading panel), so the pages are
// rendered here and the touch path runs in headless Chromium
// (hint_toggle_test.go).

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

func TestHintPartialRendersIconAndPop(t *testing.T) {
	srv := NewServer()
	tpl, err := srv.templates.Clone()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tpl.New("hint-test").Parse(`{{template "hintOpen" "What this shows"}}Over the last {{.}} days.{{template "hintClose"}}`); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := tpl.ExecuteTemplate(&buf, "hint-test", 7); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{
		`class="vk-hint"`, `<button type="button"`, `class="vk-hint-btn"`, `aria-label="What this shows"`,
		`aria-expanded="false"`, `data-on-click="hint-toggle"`, `class="vk-hint-pop" role="tooltip"`, "Over the last 7 days.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("hint lacks %q:\n%s", want, out)
		}
	}
}

// moved are the standing explanations the as-built record labelled
// (review-20261031-17fb), by their opening words: 36, less the adoption
// page's machine-level note, which a recorded decision keeps visible, and
// the two that carry a chart's colour legend.
var moved = []struct{ file, opening string }{
	{"admin_chat_audit.html", "One row per dispatcher turn. Each row captures t"},
	{"admin_health_cluster.html", "Leader-election lease ownership (daemon_leader_l"},
	{"admin_health_runtime.html", "Voice (STT / TTS) provider health + artifact sto"},
	{"admin_health_runtime.html", "STT (Whisper) + TTS (Piper) + ffmpeg. A row's re"},
	{"admin_health_runtime.html", "Active FileBackend for artifact blob I/O — picke"},
	{"admin_memory_audit.html", "Per-call audit trail for memory-search retrieval"},
	{"dashboard.html", "Calls Vornik answered itself, with no provider c"},
	{"insights_adoption.html", "From memory_retrieval_audit and task_llm_usage,"},
	{"insights_adoption.html", "Scored on distinct capabilities used, not event"},
	{"insights_adoption.html", "A capability no team has used is one the custome"},
	{"insights_adoption.html", "Machine actors (autonomy, background workers, cr"},
	{"insights_quality.html", "Only workflows declaring qualityScoring are scor"},
	{"insights_quality.html", "These executions ran under a scoring policy and"},
	{"insights_quality.html", "Mean covers scored executions only. Awaiting-evi"},
	{"insights_tool_budget.html", "Actual tool-calls per execution, from the most r"},
	{"insights_tool_budget.html", "Factors scale the role's configured tool-iterati"},
	{"insights_tool_budget.html", "Per-role over/under-provisioning signals mined b"},
	{"insights_trends.html", "Daily task throughput and success rate over the"},
	{"insights_trends.html", "Bar height = tasks created that day. Hover a bar"},
	{"insights_trends.html", "Bar height = verdicts recorded that day. Hover f"},
	{"memory_index.html", "Cross-project, cross-channel preferences + notes"},
	{"memory_index.html", "Background pipeline turning project_memory_chunk"},
	{"memory_operators.html", "Per-operator preferences + accumulated notes the"},
	{"memory_project.html", "Cumulative volume by stage across the recent epo"},
	{"memory_project.html", "Which quality gate is rejecting most for this pr"},
	{"memory_project.html", "One row per ingest pipeline run. Active epochs a"},
	{"memory_project.html", "Tombstones from memory_eviction_audit — every ha"},
	{"memory_project.html", "Audit trail for corpus_epochs_active mutations."},
	{"spend.html", "Workflow steps are the executor's per-step LLM c"},
	{"spend.html", "High iter/step = tool loop running long. High in"},
	{"trading_dashboard.html", "Paper fills are optimistic: no slippage or rejec"},
	{"trading_dashboard.html", "The engine decides only from the tools the"},
	{"trading_dashboard.html", "The book is marked to market (realized and unrea"},
}

// kept are explanations that must stay visible: an empty state, a warning
// and a sample line.
var kept = []struct{ file, opening string }{
	{"insights_adoption.html", "No credentials to show"},
	{"memory_operator_detail.html", "Removes every profile field"},
	{"trading_dashboard.html", "unknown risk"},
	{"insights_adoption.html", "Task activity per"},
	{"memory_project.html", "One stacked bar per ingest epoch"},
	{"memory_project.html", "Sample of up to 500 embedded chunks"},
}

var (
	hintTagRE   = regexp.MustCompile(`<[^<>]*>`)
	hintSpaceRE = regexp.MustCompile(`\s+`)
)

// flatSource is a template with its tags removed and whitespace collapsed,
// so an explanation that wraps or holds inline tags matches by its words.
// Template actions survive: they hold no angle brackets.
func flatSource(t *testing.T, file string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("templates", file))
	if err != nil {
		t.Fatal(err)
	}
	return hintSpaceRE.ReplaceAllString(hintTagRE.ReplaceAllString(string(b), ""), " ")
}

// insideHint reports whether the text at i follows a hintOpen with no
// hintClose between.
func insideHint(src string, i int) bool {
	open := strings.LastIndex(src[:i], `{{template "hintOpen"`)
	return open >= 0 && !strings.Contains(src[open:i], `{{template "hintClose"}}`)
}

func TestStandingExplanationsSitBehindAHint(t *testing.T) {
	if len(moved) != 33 {
		t.Fatalf("the design moves 33 explanations, the table has %d", len(moved))
	}
	for _, m := range moved {
		src := flatSource(t, m.file)
		i := strings.Index(src, m.opening)
		if i < 0 {
			t.Errorf("%s: %q not found", m.file, m.opening)
			continue
		}
		if !insideHint(src, i) {
			t.Errorf("%s: %q is still on the page, not behind an ⓘ", m.file, m.opening)
		}
	}
	for _, k := range kept {
		src := flatSource(t, k.file)
		i := strings.Index(src, k.opening)
		if i < 0 || insideHint(src, i) {
			t.Errorf("%s: %q must stay visible (found=%v)", k.file, k.opening, i >= 0)
		}
	}
}

func TestOverviewCacheTileHasAHint(t *testing.T) {
	src := flatSource(t, "dashboard.html")
	i := strings.Index(src, "Calls Vornik answered itself")
	if i < 0 || !insideHint(src, i) {
		t.Fatal("the cache tile's explanation is not behind an ⓘ")
	}
}

func TestStylesheetCarriesTheHintClasses(t *testing.T) {
	css, err := os.ReadFile(filepath.Join("static", "tailwind.css"))
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range []string{".vk-hint", ".vk-hint-btn", ".vk-hint-pop", ".vk-hint-layer"} {
		if !bytes.Contains(css, []byte(c)) {
			t.Errorf("tailwind.css lacks %s: run make ui-css", c)
		}
	}
}
