package persistence

// Goal origin: the creation SURFACE that wrote a task's goal, recorded on
// tasks.goal_origin when the row is created and never changed afterwards
// (knowledge-skill learning-loop design, amendment 2026-10-13; GitHub #347).
//
// It records a fact, not a verdict: which surface wrote the words. The
// verdict lives in GoalOriginIsFirstParty, so a policy change needs no
// backfill. Every task-creation site stamps one of these constants; a row
// with no value (every row written before migration 230, or a site that
// forgot) is treated as unknown, and the skill distiller refuses it.
//
// The value must only ever be a constant chosen by the creating code. It is
// never copied from a request field, a payload or a model's output, and no
// UPDATE statement names the column (goal_origin_sites_test.go enforces the
// first; repotest enforces the second).
const (
	// First-party: the words came from the key holder, the signed-in
	// operator, project config or the daemon itself.
	TaskGoalOriginREST      = "rest"      // POST /api/v1/projects/{id}/tasks
	TaskGoalOriginCompanion = "companion" // companion MCP delegate
	TaskGoalOriginUI        = "ui"        // the web UI's create-task form
	TaskGoalOriginAutonomy  = "autonomy"  // the autonomy manager's tick
	TaskGoalOriginDoctor    = "doctor"    // the project doctor's smoke run

	// Third-party: the words came from someone the daemon does not
	// authenticate as the operator, or from a model that may have read
	// anything.
	TaskGoalOriginBroker   = "broker"    // broker delegate / schedule fire
	TaskGoalOriginReminder = "reminder"  // a scheduled reminder firing
	TaskGoalOriginWebhook  = "webhook"   // an inbound signed webhook
	TaskGoalOriginGitHub   = "github"    // the GitHub channel (issues, PRs)
	TaskGoalOriginGitHubCI = "github_ci" // a GitHub CI outcome
	TaskGoalOriginChat     = "chat"      // the chat dispatcher (Telegram, Slack, email, web chat, voice)
	TaskGoalOriginA2A      = "a2a"       // an A2A caller

	// Derived: the goal was written by a parent's model or copied from
	// another task. Judged at the lineage root; a derived row that becomes a
	// root (OrphanChildren) is not first-party.
	TaskGoalOriginCheckpoint     = "checkpoint"
	TaskGoalOriginRoute          = "route"
	TaskGoalOriginDelegation     = "delegation"
	TaskGoalOriginSpawnProject   = "spawn_project"
	TaskGoalOriginCallProject    = "call_project"
	TaskGoalOriginFork           = "fork"
	TaskGoalOriginCounterfactual = "counterfactual"
)

var knownGoalOrigins = map[string]bool{
	TaskGoalOriginREST:           true,
	TaskGoalOriginCompanion:      true,
	TaskGoalOriginUI:             true,
	TaskGoalOriginAutonomy:       true,
	TaskGoalOriginDoctor:         true,
	TaskGoalOriginBroker:         true,
	TaskGoalOriginReminder:       true,
	TaskGoalOriginWebhook:        true,
	TaskGoalOriginGitHub:         true,
	TaskGoalOriginGitHubCI:       true,
	TaskGoalOriginChat:           true,
	TaskGoalOriginA2A:            true,
	TaskGoalOriginCheckpoint:     true,
	TaskGoalOriginRoute:          true,
	TaskGoalOriginDelegation:     true,
	TaskGoalOriginSpawnProject:   true,
	TaskGoalOriginCallProject:    true,
	TaskGoalOriginFork:           true,
	TaskGoalOriginCounterfactual: true,
}

// IsKnownGoalOrigin reports whether origin is one of the constants above.
// Empty (NULL) and any other string are unknown.
func IsKnownGoalOrigin(origin string) bool { return knownGoalOrigins[origin] }

// GoalOriginIsFirstParty is the closed allowlist of surfaces whose goal text
// is the operator's own. Everything else, unknown values included, is not.
func GoalOriginIsFirstParty(origin string) bool {
	switch origin {
	case TaskGoalOriginREST, TaskGoalOriginCompanion, TaskGoalOriginUI,
		TaskGoalOriginAutonomy, TaskGoalOriginDoctor:
		return true
	}
	return false
}
