package registry

import "testing"

func TestForgeIssuePolicyScopes(t *testing.T) {
	p := &Project{Forge: ProjectForge{GitHub: ProjectGitHub{Repo: "O/R"}, Issues: ProjectForgeIssues{AllowedLabels: []string{"bug"}}}}
	if !p.ForgeIssueRepoAllowed("o/r") || p.ForgeIssueRepoAllowed("other/repo") {
		t.Fatal("repo scope")
	}
	if p.Forge.Issues.LabelsAllowed([]string{"bug"}) {
		t.Fatal("default writes enabled")
	}
	p.Forge.Issues.LabelsEnabled = true
	if !p.Forge.Issues.LabelsAllowed([]string{"bug"}) || p.Forge.Issues.LabelsAllowed([]string{"Bug"}) {
		t.Fatal("label scope")
	}
	merged := p.Forge.Issues.ManagedLabels([]string{"protected", "bug"}, []string{})
	if len(merged) != 1 || merged[0] != "protected" {
		t.Fatal(merged)
	}
	if (&Project{}).ForgeIssueRepoAllowed("o/r") {
		t.Fatal("empty scope")
	}
	p.Forge.Issues.Repos = []string{"../r"}
	if p.Forge.Issues.Validate() == nil {
		t.Fatal("invalid repo")
	}
	p.Forge.Issues.Repos = nil
	p.Forge.Issues.AllowedLabels = []string{""}
	if p.Forge.Issues.Validate() == nil {
		t.Fatal("invalid label")
	}
}

func TestForgeIssueManagedLabelsDeduplicates(t *testing.T) {
	p := ProjectForgeIssues{AllowedLabels: []string{"bug"}}
	merged := p.ManagedLabels([]string{"protected", "bug"}, []string{"bug", "bug", "protected"})
	if len(merged) != 2 || merged[0] != "protected" || merged[1] != "bug" {
		t.Fatal(merged)
	}
	var nilProject *Project
	if nilProject.ForgeIssueRepoAllowed("o/r") {
		t.Fatal("nil")
	}
	if (&Project{}).ForgeIssueRepoAllowed("../r") {
		t.Fatal("invalid")
	}
}
