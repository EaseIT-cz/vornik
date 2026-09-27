package service

// Slice 4 wiring — service-layer adapters for the workflow-proposal
// applier and rollbacker. Filesystem writer (two-tree discipline)
// and config-reload trigger. Kept here so internal/workflowapply stays
// free of filesystem / exec / git dependencies.

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/safepath"
	"vornik.io/vornik/internal/workflowapply"
)

// workflowApplierAdapter bridges *workflowapply.Applier (returns the
// typed persistence.WorkflowProposal) to the api package's
// WorkflowApplier interface (returns any).
type workflowApplierAdapter struct {
	a *workflowapply.Applier
}

func (w *workflowApplierAdapter) Apply(ctx context.Context, proposalID, appliedBy string) (any, error) {
	if w == nil || w.a == nil {
		return nil, fmt.Errorf("workflow applier not wired")
	}
	return w.a.Apply(ctx, proposalID, appliedBy)
}

// fsWorkflowWriter implements workflowapply.WorkflowWriter against the
// two-tree config discipline: writes to both source (operator's
// vornik checkout) and deployed (daemon's read-target) trees. The
// source path is returned; nothing commits it (process-spawn law, S3).
//
// Source-tree empty / not present is non-fatal: deployed-only
// deployments (production daemons without an operator checkout)
// get the file written to the deployed tree only.
type fsWorkflowWriter struct {
	sourceConfigDir   string // <root>/configs, contains workflows/
	deployedConfigDir string // ~/.config/vornik/configs, contains workflows/
}

func (w *fsWorkflowWriter) Write(_ context.Context, workflowID string, body []byte) (string, error) {
	if workflowID == "" {
		return "", fmt.Errorf("fsWorkflowWriter: empty workflowID")
	}
	if w.deployedConfigDir == "" {
		return "", fmt.Errorf("fsWorkflowWriter: deployedConfigDir not set")
	}
	// Defend against operator-supplied IDs containing path separators or
	// traversal. CleanPathComponent rejects "", ".", "..", and any separator —
	// tighter and less overbroad than the previous Contains("..") check.
	safeID, err := safepath.CleanPathComponent(workflowID)
	if err != nil {
		return "", fmt.Errorf("fsWorkflowWriter: invalid workflowID: %w", err)
	}

	deployedPath, err := w.writeToTree(w.deployedConfigDir, safeID, body)
	if err != nil {
		return "", fmt.Errorf("write deployed tree: %w", err)
	}

	// Source tree write is optional. When the source tree exists
	// AND has a workflows/ directory, we mirror the write so the
	// operator's checkout reflects the change (they commit it). Otherwise we skip
	// silently.
	if w.sourceConfigDir == "" {
		_ = deployedPath
		return "", nil
	}
	sourceWorkflowsDir := filepath.Join(w.sourceConfigDir, "workflows")
	if info, err := os.Stat(sourceWorkflowsDir); err != nil || !info.IsDir() {
		return "", nil
	}
	sourcePath, err := w.writeToTree(w.sourceConfigDir, safeID, body)
	if err != nil {
		return "", fmt.Errorf("write source tree: %w", err)
	}
	return sourcePath, nil
}

// ReadDeployed returns the deployed workflow file as it is now — the
// applier's pre-apply snapshot (config-drift slice E), read before Write
// replaces it. Same id guard as Write; a missing file is an error, not an
// empty snapshot, so the applier records nothing rather than a false one.
func (w *fsWorkflowWriter) ReadDeployed(_ context.Context, workflowID string) ([]byte, error) {
	if w.deployedConfigDir == "" {
		return nil, fmt.Errorf("fsWorkflowWriter: deployedConfigDir not set")
	}
	safeID, err := safepath.CleanPathComponent(workflowID)
	if err != nil {
		return nil, fmt.Errorf("fsWorkflowWriter: invalid workflowID: %w", err)
	}
	path, err := safepath.JoinUnder(filepath.Join(w.deployedConfigDir, "workflows"), safeID+".md")
	if err != nil {
		return nil, fmt.Errorf("workflowID escapes workflows directory: %w", err)
	}
	return os.ReadFile(path)
}

func (w *fsWorkflowWriter) writeToTree(configDir, workflowID string, body []byte) (string, error) {
	candidate, err := safepath.JoinUnder(filepath.Join(configDir, "workflows"), workflowID+".md")
	if err != nil {
		return "", fmt.Errorf("workflowID escapes workflows directory: %w", err)
	}
	// Best-effort atomic: write to <name>.md.tmp then rename.
	// os.Rename on the same filesystem is atomic on POSIX.
	tmp := candidate + ".tmp"
	if err := os.WriteFile(tmp, body, 0o644); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, candidate); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return candidate, nil
}

// configReloadAdapter bridges *config.ConfigReloader to
// workflowapply.ConfigReloadTrigger. The reloader's post-reload hook
// (installed by installConfigReloadBroadcast) handles the cross-
// instance NOTIFY automatically — we just call Reload() and the
// machinery downstream fires the broadcast.
type configReloadAdapter struct {
	reloader *config.ConfigReloader
}

func (a *configReloadAdapter) Reload() error {
	if a == nil || a.reloader == nil {
		return nil
	}
	return a.reloader.Reload()
}

// newWorkflowApplier wires the workflowapply.Applier out of the
// container's primitives. Returns nil if prerequisites are
// missing; the admin endpoint nil-checks and surfaces 503.
//
// The source-tree path is resolved from VORNIK_CONFIGS_SOURCE_DIR
// env (operator's vornik checkout root). When set, the writer mirrors
// every apply into it; nothing commits it — the daemon runs no git on a
// request (process-spawn law, S3). The operator commits the source tree
// with their own git.
func newWorkflowApplier(
	proposals persistence.WorkflowProposalRepository,
	reloader *config.ConfigReloader,
	deployedConfigDir string,
	logger *zerolog.Logger,
) *workflowapply.Applier {
	if proposals == nil || deployedConfigDir == "" {
		return nil
	}
	return workflowapply.NewApplier(
		proposals, newFSWorkflowWriter(deployedConfigDir),
		&configReloadAdapter{reloader: reloader},
		workflowapply.ApplierConfig{Logger: logger},
	)
}

// newFSWorkflowWriter is the two-tree writer both the applier and the
// rollbacker use, so a rollback restores to exactly the trees an apply wrote.
func newFSWorkflowWriter(deployedConfigDir string) *fsWorkflowWriter {
	return &fsWorkflowWriter{
		sourceConfigDir:   os.Getenv("VORNIK_CONFIGS_SOURCE_DIR"),
		deployedConfigDir: deployedConfigDir,
	}
}
