package api

// Doctor check: git_config_composition.
//
// Process-spawn law S6-D4 (https://docs.vornik.io):
// every daemon git command reads no system config and a global config the
// daemon composes at startup from the operator's, keeping only an allowlist of
// keys. Program keys an agent could SELECT through work-tree content (filter,
// diff and merge drivers, textconv) and keys that would subvert the daemon's
// own guards (core.hooksPath) are dropped. The design's promise is that an
// operator is never surprised: this check lists every dropped key with its
// reason, and states the denominator (how many keys were examined).
//
//	ERROR    composition failed: daemon git reads an EMPTY global config, so a
//	         credential helper or ssh command the forge relies on is not used.
//	WARNING  keys were dropped (a global git-lfs, a signing key, an alias …).
//	OK       every operator key was kept.
//	SKIPPED  not wired (no composition ran in this process).

import (
	"fmt"
	"strings"
)

// GitConfigComposition is the daemon's composed git config, as the doctor
// reports it.
type GitConfigComposition struct {
	Path     string
	Examined int
	Kept     int
	Dropped  []DroppedGitConfigKey
	Err      string
}

// DroppedGitConfigKey is one operator key the composed config does not carry.
type DroppedGitConfigKey struct {
	Key    string
	Reason string
}

// SetGitConfigComposition wires the startup composition's result.
func (h *DoctorHandlers) SetGitConfigComposition(c *GitConfigComposition) {
	h.gitConfigComposition = c
}

func (h *DoctorHandlers) checkGitConfigComposition() DoctorCheck {
	const name = "git_config_composition"
	c := h.gitConfigComposition
	if c == nil {
		return DoctorCheck{Name: name, Status: "SKIPPED",
			Message: "the daemon's git config composition is not wired in this process; nothing was examined (this is NOT a statement that daemon git reads a safe config)"}
	}
	if c.Err != "" {
		return DoctorCheck{Name: name, Status: "ERROR",
			Message: "could not compose the daemon's git config (" + c.Err + "); daemon git reads an EMPTY global config, so no credential helper or ssh command is used — fix the operator's git config and restart the daemon"}
	}
	if len(c.Dropped) == 0 {
		return DoctorCheck{Name: name, Status: "OK",
			Message: fmt.Sprintf("examined %d key(s) of the operator's git config and kept all %d in %s", c.Examined, c.Kept, c.Path)}
	}
	parts := make([]string, 0, len(c.Dropped))
	for _, d := range c.Dropped {
		parts = append(parts, d.Key+" ("+d.Reason+")")
	}
	return DoctorCheck{Name: name, Status: "WARNING",
		Message: fmt.Sprintf("examined %d key(s) of the operator's git config: kept %d in %s, dropped %d that daemon git will not use: %s. "+
			"A driver a project needs (git-lfs) belongs in that project's own .git/config (`git lfs install --local`)",
			c.Examined, c.Kept, c.Path, len(c.Dropped), strings.Join(parts, "; "))}
}
