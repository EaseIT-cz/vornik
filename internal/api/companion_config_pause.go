package api

import (
	"context"
	"sort"
	"strings"
	"time"

	"vornik.io/vornik/internal/controlplane"
	"vornik.io/vornik/internal/persistence"
)

// The agent's view of a config-apply pause (config-apply journal design
// §13.2, GitHub #302). A DRIFT row holds its projects' tasks QUEUED; before
// this, a broker key saw only QUEUED and the assistant reported a silent
// hang. Every field here is closed or Vornik-generated, so the same shape is
// safe for the broker view (broker design §5.2): no path, no failure text,
// no hash, no proposal id and no other project's name. Reading only: no
// tool resolves a row; that stays the admin-class door (§12.2).

// configPauseResolveTemplate is the one sentence an agent reads about who
// resolves a config-apply pause. It depends only on what this build ships,
// never on runtime state (review 20261013-9caf F1). <id> is the journal id.
const configPauseResolveTemplate = "Your project is paused until the operator decides about an interrupted config change (journal <id>). " +
	"They decide on their approver device (the card 'A config change was interrupted') or on the Vornik host with " +
	"vornikctl control-plane journal resolve <id> --keep (keep the files as they are now) or --restore (put back the version from before the change). " +
	"You cannot resolve this yourself; tell the person what to tap."

// configPauseReader is the optional half of the applier the pause view
// needs (satisfied by *controlplane.ApplyEngine), as journalResolver is for
// the resolve door.
type configPauseReader interface {
	PauseReasons(ctx context.Context) ([]controlplane.PauseReason, error)
}

// The production engine is the reader; a signature drift fails the build.
var _ configPauseReader = (*controlplane.ApplyEngine)(nil)

// pauseReasons reads every pause. With no reader wired (no journal on this
// daemon) nothing can be paused: no reasons, no fault. failed is true only
// when the read itself failed (logged; the gate fails open on the same read).
func (s *Server) pauseReasons(ctx context.Context) (reasons []controlplane.PauseReason, failed bool) {
	pr, wired := s.proposalApplier.(configPauseReader)
	if !wired || pr == nil {
		return nil, false
	}
	reasons, err := pr.PauseReasons(ctx)
	if err != nil {
		s.logger.Warn().Err(err).Msg("companion: config-apply pause read failed; pause omitted")
		return nil, true
	}
	return reasons, false
}

func pauseNames(r controlplane.PauseReason, project string) bool {
	for _, p := range r.Projects {
		if p == project || p == "*" {
			return true
		}
	}
	return false
}

// pauseView is the per-task `paused` list: nil when the task is terminal, its
// project is not paused, or the pause could not be read.
func (s *Server) pauseView(ctx context.Context, task *persistence.Task) []map[string]any {
	if task == nil || isTerminalStatus(task.Status) {
		return nil
	}
	reasons, _ := s.pauseReasons(ctx)
	var out []map[string]any
	for _, r := range reasons {
		if !pauseNames(r, task.ProjectID) {
			continue
		}
		out = append(out, map[string]any{
			"reason":      "config_apply_unresolved",
			"journal_id":  r.JournalID,
			"since":       r.Since.UTC().Format(time.RFC3339),
			"drift":       closedDrift(r.Drift),
			"files":       r.Files,
			"resolved_by": "operator",
			"resolve":     strings.ReplaceAll(configPauseResolveTemplate, "<id>", r.JournalID),
		})
	}
	return out
}

// addPauseView sets out["paused"] when the task's project is paused.
func (s *Server) addPauseView(ctx context.Context, task *persistence.Task, out map[string]any) {
	if p := s.pauseView(ctx, task); len(p) > 0 {
		out["paused"] = p
	}
}

// doctorPausedProjects is doctor's expansion: one entry per covered project
// per row, sorted by project then journal id; unavailable when the read
// failed, so "examined and clean" never reads like "not examined".
func (s *Server) doctorPausedProjects(ctx context.Context, key *persistence.APIKey) ([]map[string]any, bool) {
	reasons, failed := s.pauseReasons(ctx)
	out := []map[string]any{}
	if failed {
		return out, true
	}
	for _, r := range reasons {
		var projects []string
		for _, p := range r.Projects {
			if p == "*" {
				projects = s.coveredProjects(key)
				break
			}
			if keyCoversProject(key, p) {
				projects = append(projects, p)
			}
		}
		for _, p := range projects {
			out = append(out, map[string]any{
				"project": p, "journal_id": r.JournalID, "since": r.Since.UTC().Format(time.RFC3339),
				"drift": closedDrift(r.Drift), "files": r.Files,
			})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i]["project"] != out[j]["project"] {
			return out[i]["project"].(string) < out[j]["project"].(string)
		}
		return out[i]["journal_id"].(string) < out[j]["journal_id"].(string)
	})
	return out, false
}

// coveredProjects lists every registered project the key covers.
func (s *Server) coveredProjects(key *persistence.APIKey) []string {
	if s.projectRegistry == nil {
		return nil
	}
	var out []string
	for _, p := range s.projectRegistry.ListProjects() {
		if keyCoversProject(key, p.ID) {
			out = append(out, p.ID)
		}
	}
	return out
}

// closedDrift passes a declared drift class, else unknown: the view does not
// rely on the engine alone.
func closedDrift(d string) string {
	switch d {
	case controlplane.DriftEditedFile, controlplane.DriftChangedDependency:
		return d
	}
	return controlplane.DriftUnknown
}
