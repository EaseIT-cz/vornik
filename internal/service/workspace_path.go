package service

import (
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/spawn"
)

// resolveProjectWorkspacePath returns the effective base directory for
// per-project persistent workspaces: the configured
// runtime.project_workspace_path when set, otherwise
// $VORNIK_DATA_DIR/workspaces, otherwise "". Both the executor's
// staging guard and the dispatcher's create_task input-confinement
// gate derive their per-project uploads/ allow-list entry from this
// value — they MUST resolve it identically, or a channel upload the
// executor would happily stage gets rejected at create_task
// (incident-telegram-upload-input-roots-20260712).
func resolveProjectWorkspacePath(configured string) string {
	// One resolution, owned by config, so `vornikctl deps` and the dependency
	// cache derive the same workspace root the daemon uses (project
	// dependency provisioning design §8.2).
	return config.RuntimeConfig{ProjectWorkspacePath: configured}.ProjectWorkspaceDir()
}

// registerSpawnWorkspaceRoot registers the effective project workspace root
// with the process-spawn law's git kinds (internal/spawn, design "S1b-2, as
// built"): GitWorkspace and GitHTTPBackend refuse any directory outside a
// registered root, and with none registered they refuse everything. Called
// once from NewContainer, before anything that runs git is wired. An empty
// root (no workspace configured) registers nothing, so git stays refused —
// the executor runs no git without a workspace anyway.
func registerSpawnWorkspaceRoot(configured string) error {
	root := resolveProjectWorkspacePath(configured)
	if root == "" {
		return nil
	}
	return spawn.RegisterWorkspaceRoot(root)
}
