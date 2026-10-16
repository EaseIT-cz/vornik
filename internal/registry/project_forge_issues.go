package registry

import (
	"strings"
	"vornik.io/vornik/internal/forge"
)

// ProjectForgeIssues grants task agents bounded issue access. All writes default off.
type ProjectForgeIssues struct {
	Repos         []string `yaml:"repos" since:"2026.10.6"`
	LabelsEnabled bool     `yaml:"labels_enabled" since:"2026.10.6"`
	AllowedLabels []string `yaml:"allowed_labels" since:"2026.10.6"`
	CloseEnabled  bool     `yaml:"close_enabled" since:"2026.10.6"`
	// WritesWithoutApproval is the operator's explicit acceptance that a
	// task agent, steered by untrusted issue text, may label and close
	// issues with no human approval. Every write is refused without it
	// (audit 2026-10-12; phone approval is design open question Q-FI1).
	// see LLD § https://docs.vornik.io "Issue triage writes: prompt injection and approval (2026-10-12)"
	WritesWithoutApproval bool `yaml:"writes_without_approval" since:"2026.10.6"`
}

// WritesEffective reports whether any issue write can actually happen: an
// opt-in AND the operator's acceptance of unapproved writes.
func (p ProjectForgeIssues) WritesEffective() bool {
	return p.WritesWithoutApproval && (p.LabelsEnabled || p.CloseEnabled)
}

// Validate checks configured repo and label scopes.
func (p ProjectForgeIssues) Validate() error {
	for _, repo := range p.Repos {
		if err := forge.ValidateIssueRepo(repo); err != nil {
			return err
		}
	}
	return forge.ValidateIssueLabels(p.AllowedLabels)
}

// ForgeIssueRepoAllowed never infers scope from task input or clone remotes.
func (p *Project) ForgeIssueRepoAllowed(repo string) bool {
	if p == nil || forge.ValidateIssueRepo(repo) != nil {
		return false
	}
	// One set for "the repositories this project is scoped to", shared with
	// the webhook repository scope (GitHub #407).
	for _, allowed := range p.ForgeRepos() {
		if strings.EqualFold(allowed, repo) {
			return true
		}
	}
	return false
}

// LabelsAllowed requires opt-in and exact membership for every requested label.
func (p ProjectForgeIssues) LabelsAllowed(labels []string) bool {
	if !p.LabelsEnabled || forge.ValidateIssueLabels(labels) != nil {
		return false
	}
	for _, l := range labels {
		found := false
		for _, a := range p.AllowedLabels {
			if l == a {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// ManagedLabels preserves labels beyond the grant while replacing managed labels.
func (p ProjectForgeIssues) ManagedLabels(current, desired []string) []string {
	out := []string{}
	for _, l := range current {
		managed := false
		for _, a := range p.AllowedLabels {
			if l == a {
				managed = true
				break
			}
		}
		if !managed {
			out = append(out, l)
		}
	}
	for _, l := range desired {
		found := false
		for _, a := range out {
			if a == l {
				found = true
				break
			}
		}
		if !found {
			out = append(out, l)
		}
	}
	return out
}
