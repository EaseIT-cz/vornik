package ui

// One-line summaries for the settings page's collapsed sections
// (web-ui-design.md "Progressive disclosure across the console — 2026-11-13
// (#748)", B3): a closed section still says what it holds.

import (
	"fmt"
	"strings"
)

// OpenAll is true after a failed save: every section renders open so the
// field the error names is on screen (the error names a key, not a section).
func (d ProjectConfigFormData) OpenAll() bool { return d.Error != "" }

func plural(n int, one, many string) string {
	if n == 1 {
		return "1 " + one
	}
	return fmt.Sprintf("%d %s", n, many)
}

func countChipList(s string) int { return len(splitChipList(s)) }

// PermissionsSummary returns "2 secrets · 1 tool", or "defaults".
func (d ProjectConfigFormData) PermissionsSummary() string {
	var parts []string
	if n := countChipList(d.PermissionsSecrets); n > 0 {
		parts = append(parts, plural(n, "secret", "secrets"))
	}
	if n := countChipList(d.PermissionsAllowedTools); n > 0 {
		parts = append(parts, plural(n, "tool", "tools"))
	}
	return joinOr(parts, "defaults")
}

// RateLimitSummary returns "10 a minute · 100 an hour", or "no limit".
func (d ProjectConfigFormData) RateLimitSummary() string {
	var parts []string
	if d.RateLimitTasksPerMinute > 0 {
		parts = append(parts, fmt.Sprintf("%d a minute", d.RateLimitTasksPerMinute))
	}
	if d.RateLimitTasksPerHour > 0 {
		parts = append(parts, fmt.Sprintf("%d an hour", d.RateLimitTasksPerHour))
	}
	return joinOr(parts, "no limit")
}

// RetentionSummary returns the overridden thresholds, or "defaults".
func (d ProjectConfigFormData) RetentionSummary() string {
	var parts []string
	for _, f := range []struct {
		label string
		days  int
	}{
		{"LLM usage", d.RetentionTaskLLMUsageDays},
		{"tool audit", d.RetentionToolAuditDays},
		{"chat audit", d.RetentionChatAuditDays},
		{"tasks", d.RetentionTasksDays},
		{"executions", d.RetentionExecutionsDays},
		{"artifacts", d.RetentionArtifactsDays},
	} {
		if f.days > 0 {
			parts = append(parts, fmt.Sprintf("%s %dd", f.label, f.days))
		}
	}
	return joinOr(parts, "defaults")
}

// JudgeSummary returns "off", or "on · <model>".
func (d ProjectConfigFormData) JudgeSummary() string {
	if !d.JudgeEnabled {
		return "off"
	}
	if d.JudgeModel == "" {
		return "on · daemon default model"
	}
	return "on · " + d.JudgeModel
}

// TradingSummary returns "paper · kill switch off", or "not set".
func (d ProjectConfigFormData) TradingSummary() string {
	if d.TradingMode == "" {
		return "not set"
	}
	if d.TradingKillSwitch {
		return d.TradingMode + " · kill switch on"
	}
	return d.TradingMode + " · kill switch off"
}

// GitHubAppSummary returns "app 12 · installation 34", or "not configured".
func (d ProjectConfigFormData) GitHubAppSummary() string {
	if d.GitHubAppAppID == 0 {
		return "not configured"
	}
	s := fmt.Sprintf("app %d", d.GitHubAppAppID)
	if d.GitHubAppInstallationID != 0 {
		s += fmt.Sprintf(" · installation %d", d.GitHubAppInstallationID)
	}
	return s
}

// ToolCountLabel is a server row's tool count: "all 34 tools", "5 of 34
// tools", or, for "selected only" with nothing ticked, what the loader will
// actually do with it (no allowed_tools is the full catalogue).
func (r MCPRegistryRow) ToolCountLabel() string {
	n := len(r.Server.Tools)
	if n == 0 {
		return "no tools"
	}
	if r.AllowAllTools {
		return fmt.Sprintf("all %d tools", n)
	}
	k := 0
	for _, t := range r.Server.Tools {
		if r.AllowedTools[t.Name] {
			k++
		}
	}
	if k == 0 {
		return fmt.Sprintf("none selected: all %d tools", n)
	}
	return fmt.Sprintf("%d of %d tools", k, n)
}

func joinOr(parts []string, empty string) string {
	if len(parts) == 0 {
		return empty
	}
	return strings.Join(parts, " · ")
}
