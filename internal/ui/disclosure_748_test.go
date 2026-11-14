package ui

// Progressive disclosure (web-ui-design.md "Progressive disclosure across the
// console — 2026-11-13 (#748)", B1 and B3). Found at: production; the operator
// opened a project's settings page and met 70 tool checkboxes for 8 MCP
// servers before the Save button (measured in §A). These tests render the real
// template, so they sit at the level a user sees: the page's HTML.

import (
	"bytes"
	"fmt"
	"regexp"
	"strings"
	"testing"
)

func renderNamed(t *testing.T, name string, data any) string {
	t.Helper()
	var buf bytes.Buffer
	if err := NewServer().templates.ExecuteTemplate(&buf, name, data); err != nil {
		t.Fatalf("render %s: %v", name, err)
	}
	return buf.String()
}

func TestDisclosure748_PartialRendersADetailsWithItsSummary(t *testing.T) {
	s := NewServer()
	tpl, err := s.templates.Clone()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tpl.New("disc-748").Parse(`{{template "discOpen" (dict "ID" "rl" "Title" "Rate limit" "Summary" .S "Open" .O)}}<input name="x">{{template "discClose"}}`); err != nil {
		t.Fatal(err)
	}
	render := func(sum string, open bool) string {
		var buf bytes.Buffer
		if err := tpl.ExecuteTemplate(&buf, "disc-748", map[string]any{"S": sum, "O": open}); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	closed := render("10 a minute", false)
	for _, want := range []string{`<details class="vk-disc" id="rl">`, `class="vk-disc-sum"`, `>Rate limit<`, `class="vk-disc-meta">10 a minute<`, `class="vk-disc-body"`, `<input name="x">`, `</details>`} {
		if !strings.Contains(closed, want) {
			t.Errorf("closed disclosure lacks %q:\n%s", want, closed)
		}
	}
	if !strings.Contains(closed, `<details class="vk-disc" id="rl"><summary`) {
		t.Errorf("a disclosure not asked to open renders open:\n%s", closed)
	}
	if open := render("", true); !strings.Contains(open, `<details class="vk-disc" id="rl" open>`) || strings.Contains(open, "vk-disc-meta") {
		t.Errorf("open disclosure with no summary renders wrong:\n%s", open)
	}
}

// settingsWithServers is the acceptance fixture: 3 subscribed servers with 34
// tools each, every tool with a description.
func settingsWithServers(n, tools int) ProjectConfigFormData {
	d := ProjectConfigFormData{
		Title: "Project Config", CurrentPage: "projects", ProjectID: "demo",
		WorkflowOptions: []string{"adaptive"}, AutonomyModes: []string{"event", "cron"},
		TimezoneOptions: []string{"UTC"},
	}
	for i := 0; i < n; i++ {
		srv := MCPRegistryServer{Name: fmt.Sprintf("srv%d", i), Transport: "http", URL: fmt.Sprintf("http://srv%d.local/mcp", i), Reachable: i != 1, Error: "dial refused"}
		for j := 0; j < tools; j++ {
			srv.Tools = append(srv.Tools, MCPRegistryTool{Name: fmt.Sprintf("tool_%02d", j), Description: fmt.Sprintf("Describes tool %d of server %d", j, i)})
		}
		row := MCPRegistryRow{Server: srv, Subscribed: true, AllowAllTools: i != 2, AllowedTools: map[string]bool{}}
		if i == 2 {
			for j := 0; j < 5; j++ {
				row.AllowedTools[fmt.Sprintf("tool_%02d", j)] = true
			}
		}
		d.MCPRegistryRows = append(d.MCPRegistryRows, row)
	}
	return d
}

// closedDetailsBodies returns the HTML inside each closed <details>, outside
// its <summary>: what a user does not see until they open it.
func closedDetailsBodies(html string) string {
	var hidden strings.Builder
	depth, start := 0, -1
	re := regexp.MustCompile(`<details\b[^>]*>|</details>`)
	for _, m := range re.FindAllStringIndex(html, -1) {
		tag := html[m[0]:m[1]]
		if strings.HasPrefix(tag, "<details") {
			if depth == 0 && !regexp.MustCompile(`\sopen[\s>]`).MatchString(tag) {
				start = m[1]
			}
			if start >= 0 || depth > 0 {
				depth++
			}
			continue
		}
		if depth > 0 {
			depth--
			if depth == 0 && start >= 0 {
				hidden.WriteString(html[start:m[0]])
				start = -1
			}
		}
	}
	return hidden.String()
}

func TestSettings748_ToolCheckboxesAndDescriptionsAreBehindADisclosure(t *testing.T) {
	body := renderNamed(t, "project_config_form.html", settingsWithServers(3, 34))
	hidden := closedDetailsBodies(body)
	if got := strings.Count(body, `name="mcpAllowedTool_`); got != 3*34 {
		t.Fatalf("every tool checkbox still renders (the form posts them): got %d, want %d", got, 3*34)
	}
	if got := strings.Count(hidden, `name="mcpAllowedTool_`); got != 3*34 {
		t.Errorf("tool checkboxes outside a closed disclosure: %d of %d are hidden", got, 3*34)
	}
	for _, desc := range []string{"Describes tool 0 of server 0", "Describes tool 33 of server 2"} {
		if strings.Contains(body, desc) && !strings.Contains(hidden, desc) {
			t.Errorf("tool description %q is on the page outside a closed disclosure", desc)
		}
	}
	for _, url := range []string{"http://srv0.local/mcp"} {
		if !strings.Contains(hidden, url) {
			t.Errorf("server URL %q is not behind the row's disclosure", url)
		}
	}
	first := strings.Index(body, `name="mcpAllowedTool_`)
	for _, field := range []string{`name="displayName"`, `name="swarmId"`, `name="autonomy_enabled"`, `name="budget_dailySoftUsd"`, `name="mcpSubscribe_srv0"`} {
		if i := strings.Index(body, field); i < 0 || i > first {
			t.Errorf("%s must render before the first tool checkbox (at %d, first tool at %d)", field, i, first)
		}
	}
	for _, label := range []string{"all 34 tools", "5 of 34 tools"} {
		if !strings.Contains(body, label) {
			t.Errorf("the server row lacks its tool count %q", label)
		}
	}
	if strings.Count(body, "Without subscription, the project is invisible to this server.") > 1 {
		t.Error("the subscribe explanation repeats per server; it belongs once, in the section's hint")
	}
}

func TestSettings748_RareSectionsCollapseWithASummaryAndOpenOnError(t *testing.T) {
	d := settingsWithServers(1, 3)
	d.RateLimitTasksPerMinute, d.RateLimitTasksPerHour = 10, 100
	d.JudgeEnabled, d.JudgeModel = true, "small-model"
	body := renderNamed(t, "project_config_form.html", d)
	cases := []struct{ id, summary string }{
		{"permissions", "defaults"},
		{"rate-limit", "10 a minute · 100 an hour"},
		{"retention", "defaults"},
		{"judge", "on · small-model"},
		{"trading", "not set"},
		{"github-app", "not configured"},
	}
	for _, c := range cases {
		re := regexp.MustCompile(`<details class="vk-disc" id="` + c.id + `">[\s\S]*?class="vk-disc-meta">([^<]*)<`)
		m := re.FindStringSubmatch(body)
		if m == nil {
			t.Errorf("section %s is not a closed shared disclosure", c.id)
			continue
		}
		if m[1] != c.summary {
			t.Errorf("section %s summary: got %q, want %q", c.id, m[1], c.summary)
		}
	}
	for _, id := range []string{"identity", "routing", "autonomy", "budget", "chat", "mcp-servers"} {
		if !strings.Contains(body, `id="`+id+`"`) {
			t.Errorf("section %s has no anchor for the section nav", id)
		}
		if !strings.Contains(body, `href="#`+id+`"`) {
			t.Errorf("the section nav does not link to %s", id)
		}
	}
	d.Error = "Validation failed: budget.dailyHardUsd must be ≥ dailySoftUsd"
	withErr := renderNamed(t, "project_config_form.html", d)
	for _, c := range cases {
		if !strings.Contains(withErr, `<details class="vk-disc" id="`+c.id+`" open>`) {
			t.Errorf("with a save error, section %s must render open", c.id)
		}
	}
}

func TestSettings748_Summaries(t *testing.T) {
	var d ProjectConfigFormData
	check := func(name, got, want string) {
		t.Helper()
		if got != want {
			t.Errorf("%s: got %q, want %q", name, got, want)
		}
	}
	check("permissions empty", d.PermissionsSummary(), "defaults")
	d.PermissionsSecrets, d.PermissionsAllowedTools = "A\nB", "file_read"
	check("permissions set", d.PermissionsSummary(), "2 secrets · 1 tool")
	check("rate empty", d.RateLimitSummary(), "no limit")
	d.RateLimitTasksPerHour = 100
	check("rate hour only", d.RateLimitSummary(), "100 an hour")
	check("retention empty", d.RetentionSummary(), "defaults")
	d.RetentionTasksDays, d.RetentionToolAuditDays = 60, 30
	check("retention set", d.RetentionSummary(), "tool audit 30d · tasks 60d")
	check("judge off", d.JudgeSummary(), "off")
	d.JudgeEnabled = true
	check("judge default model", d.JudgeSummary(), "on · daemon default model")
	check("trading unset", d.TradingSummary(), "not set")
	d.TradingMode, d.TradingKillSwitch = "paper", true
	check("trading", d.TradingSummary(), "paper · kill switch on")
	check("github unset", d.GitHubAppSummary(), "not configured")
	d.GitHubAppAppID, d.GitHubAppInstallationID = 12, 34
	check("github", d.GitHubAppSummary(), "app 12 · installation 34")

	row := MCPRegistryRow{Server: MCPRegistryServer{Tools: []MCPRegistryTool{{Name: "a"}, {Name: "b"}, {Name: "c"}}}, AllowAllTools: true}
	check("all tools", row.ToolCountLabel(), "all 3 tools")
	row.AllowAllTools, row.AllowedTools = false, map[string]bool{"a": true, "gone": true}
	check("selected", row.ToolCountLabel(), "1 of 3 tools")
	check("no tools", MCPRegistryRow{}.ToolCountLabel(), "no tools")
	row.AllowedTools = map[string]bool{"gone": true}
	check("selected but none: the loader reads it as all", row.ToolCountLabel(), "none selected: all 3 tools")
}
