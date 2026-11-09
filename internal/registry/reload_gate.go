package registry

import (
	"fmt"
	"strings"
)

// ReloadRejectedError reports a reload refused because the staged tree could
// not be loaded in full. The previously active configuration is untouched and
// still serving.
//
// It carries the per-file diagnosis §11 produces, not just a count: an operator
// who is told "reload refused" and nothing else has to go and find out which
// file and why, which is the gap §11 closed and this must not reopen.
type ReloadRejectedError struct {
	Rejected []RejectedFile
}

func (e *ReloadRejectedError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "config reload REFUSED: %d file(s) in the tree could not be loaded, "+
		"so activating it would drop what they define. The running configuration is unchanged "+
		"and still serving.\n", len(e.Rejected))
	for _, r := range e.Rejected {
		fmt.Fprintf(&b, "\n  %s %s:\n%s", r.Kind, r.Path, indentLines(r.Error, "    "))
	}
	b.WriteString("\nFix the file(s) above and reload again. If a key is unknown to this " +
		"binary rather than misspelt, the config is ahead of the binary: deploy the binary first, " +
		"then the config (loader-validator agreement design §10).")
	return b.String()
}

func indentLines(s, prefix string) string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	for i, l := range lines {
		lines[i] = prefix + l
	}
	return strings.Join(lines, "\n") + "\n"
}

// ReloadQuarantinedError reports a reload that ACTIVATED while one or more
// project files were refused (loader-validator agreement design §15, GitHub
// #669 item 2): each running project whose new file did not decode keeps
// serving its last good definition, and every other change applied. It is a
// warning, not a failure, but it is never silent: callers log it at ERROR and
// the CLI exits non-zero.
type ReloadQuarantinedError struct {
	// Projects are the running projects carried over unchanged.
	Projects []string
	// Rejected are the refused project files, with §11's diagnosis.
	Rejected []RejectedFile
}

func (e *ReloadQuarantinedError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "config reload applied, but %d project file(s) could not be loaded; ", len(e.Rejected))
	if len(e.Projects) > 0 {
		fmt.Fprintf(&b, "these projects keep serving their previous configuration until their file is fixed: %s.\n", strings.Join(e.Projects, ", "))
	} else {
		b.WriteString("no running project was affected.\n")
	}
	for _, r := range e.Rejected {
		fmt.Fprintf(&b, "\n  %s %s:\n%s", r.Kind, r.Path, indentLines(r.Error, "    "))
	}
	return b.String()
}

// GateStagedRejections applies §15 to a staged tree that has refused files,
// on every reload path (Registry.Reload and the service's ConfigReloader
// loader, GitHub #683). A refused swarm or workflow discards the staged set
// and returns ReloadRejectedError. A refused project file carries the running
// project recorded from that path into the staged set unchanged, and the
// result is a *ReloadQuarantinedError for the caller to report once the
// reload activates. nil means nothing was refused. CheckStagedKeepsRunning
// must run after StripInvalidFromStaged.
func (r *Registry) GateStagedRejections() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.staged == nil || r.staged.index == nil || len(r.staged.index.Rejected) == 0 {
		return nil
	}
	rejected := r.staged.index.Rejected
	for _, rf := range rejected {
		if rf.Kind != "project" {
			r.staged = nil
			return &ReloadRejectedError{Rejected: rejected}
		}
	}
	q := &ReloadQuarantinedError{Rejected: rejected}
	if r.active == nil || r.active.index == nil {
		return q
	}
	for _, rf := range rejected {
		for _, src := range r.active.index.Sources {
			if src.Kind != "project" || src.Path != rf.Path || src.ShadowedBy != "" {
				continue
			}
			p, ok := r.active.projects[src.ID]
			if !ok {
				continue
			}
			if _, already := r.staged.projects[src.ID]; already {
				continue // another file now defines it; nothing to carry
			}
			r.staged.projects[src.ID] = p
			q.Projects = append(q.Projects, src.ID)
		}
	}
	return q
}

// CheckStagedKeepsRunning re-checks §12 after the carry-over and after
// StripInvalidFromStaged: when the staged tree had refused files, every
// project the running daemon serves must still be in the staged set, or the
// reload is refused (a renamed or moved refused file, a two-layer project, or
// a carried project the new tree no longer validates). nil otherwise.
func (r *Registry) CheckStagedKeepsRunning() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.staged == nil || r.staged.index == nil || len(r.staged.index.Rejected) == 0 || r.active == nil {
		return nil
	}
	for id := range r.active.projects {
		if _, ok := r.staged.projects[id]; !ok {
			rejected := r.staged.index.Rejected
			r.staged = nil
			return &ReloadRejectedError{Rejected: rejected}
		}
	}
	return nil
}
