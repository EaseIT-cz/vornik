package service

import (
	"context"
	"errors"
	"os"
	"strings"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/spawn"
)

// memoryProvenanceWiring is the pipeline's half of design 22, "Amendment
// 2026-10-21 (#482): a companion-delegated chunk is verified only by
// provenance". CompanionDelegated is always set: without a verification
// repository it fails closed (every task counts as delegated), so a
// deployment that cannot answer the question never grants a role's authority
// to a companion's work.
type memoryProvenanceWiring struct {
	CompanionDelegated func(ctx context.Context, taskID string) (bool, error)
	Provenance         memory.ProvenanceChecker
	RecordProvenance   func(ctx context.Context, rec *persistence.ChunkProvenance) error
	Repo               persistence.MemoryVerificationRepository
	Verifier           *memory.ProvenanceVerifier
}

var errNoVerificationRepo = errors.New("memory verification repository not wired; task origin unknown")

func (c *Container) memoryProvenanceConfig() memoryProvenanceWiring {
	var repo persistence.MemoryVerificationRepository
	if c != nil && c.repos != nil {
		repo = c.repos.MemoryVerification
	}
	root := ""
	if c != nil && c.Config != nil {
		root = resolveProjectWorkspacePath(c.Config.Runtime.ProjectWorkspacePath)
	}
	w := newMemoryProvenanceWiring(repo, root)
	if w.Verifier != nil && c != nil {
		project := func(projectID string) *registry.Project {
			if c.Registry == nil {
				return nil
			}
			return c.Registry.GetProject(projectID)
		}
		wireProvenanceTrustRoot(w.Verifier, root, project)
		wireProvenanceAutoClones(w.Verifier, root, project, c.provenanceAutoClones(root))
	}
	return w
}

// provenanceAutoClones is the daemon's automatic-clone manager (#506 C1),
// built once: one daemon-only clone per repository scope under
// <workspace root's parent>/provenance, which is never under the workspace
// root (agents' worktrees) and is registered as a provenance root, not a
// workspace root. Nil when there is no workspace root to place it beside.
func (c *Container) provenanceAutoClones(workspaceRoot string) *memory.AutoClones {
	c.provenanceAutoOnce.Do(func() {
		dir := provenanceAutoRoot(workspaceRoot)
		if dir == "" {
			return
		}
		// A failure here leaves every repository without a clone, so its
		// documents are stored as before #482: say so loudly (code review
		// fbe6 F2) rather than leave provenance silently off.
		if err := os.MkdirAll(dir, 0o700); err != nil {
			c.Logger.Error().Err(err).Str("dir", dir).Msg("memory provenance: cannot create the automatic-clone root; no repository can be provenance-verified")
			return
		}
		if err := spawn.RegisterProvenanceRoot(dir); err != nil {
			c.Logger.Error().Err(err).Str("dir", dir).Msg("memory provenance: cannot register the automatic-clone root; no repository can be provenance-verified")
			return
		}
		auto := &memory.AutoClones{Root: dir, Run: memory.SpawnCloneRunner{},
			AppToken: c.provenanceAppToken, AppTokenByIdentity: c.provenanceAppTokenByIdentity}
		// #508 F1: self-hosted forges and the default-host choice. A forge
		// that cannot be configured (an unreadable token_file) leaves the
		// defaults in place and says so; it never disables provenance.
		if c.Config != nil {
			if err := memory.ConfigureAutoClones(auto, c.Config.Memory.Provenance, nil); err != nil {
				c.Logger.Error().Err(err).Msg("memory provenance: memory.provenance could not be applied; only the public default forges are cloned")
				auto.Forges, auto.ReplaceDefaultHosts = nil, false
			}
		}
		c.provenanceAuto = auto
	})
	return c.provenanceAuto
}

// provenanceAutoRoot places the automatic clones beside the workspace root.
func provenanceAutoRoot(workspaceRoot string) string { return memory.AutoCloneRoot(workspaceRoot) }

// wireProvenanceAutoClones gives the verifier its per-scope resolution (#506
// C1): the configured memory.provenance_clone for the project's own
// repository, else the scope's automatic clone (created on first use and
// fetched when stale). A configured clone that is refused is unsafe, never
// "no clone": only genuine absence counts as unexaminable (C4). A project's
// configured branch applies only to its own repository.
func wireProvenanceAutoClones(v *memory.ProvenanceVerifier, root string, project func(string) *registry.Project, auto *memory.AutoClones) {
	memory.WireForgeVerification(v, auto) // #508: forge origins and the forge-API fallback
	v.CloneFor = memory.ScopedCloneFor(func(projectID string) (string, string, bool) {
		p := project(projectID)
		if p == nil || p.Memory.ProvenanceClone == "" {
			return "", "", false
		}
		dir := daemonProvenanceClone(p.Memory.ProvenanceClone, root)
		return p.RepoScope, dir, dir == ""
	}, auto)
	v.DefaultBranchFor = func(projectID, scope string) string {
		if p := project(projectID); p != nil && scope != "" && strings.EqualFold(scope, p.RepoScope) {
			return p.TrustedBranch()
		}
		return ""
	}
}

// wireProvenanceTrustRoot points the verifier at each project's trusted
// branch and clone, as the registry holds them now.
func wireProvenanceTrustRoot(v *memory.ProvenanceVerifier, root string, project func(string) *registry.Project) {
	// The trust root is the project's trusted branch: memory.
	// provenance_branch, else workspace.refresh_branch, which the
	// verifier checks against the clone's origin/HEAD (security finding
	// 2026-10-22; GitHub #499).
	v.DefaultBranch = func(projectID string) string {
		if p := project(projectID); p != nil {
			return p.TrustedBranch()
		}
		return ""
	}
	// The clone is the daemon-only memory.provenance_clone when one is
	// configured (GitHub #499, operator decision A18), else the
	// project's workspace clone.
	workspaceClone := v.CloneDir
	v.CloneDir = func(projectID string) string {
		p := project(projectID)
		if p == nil || p.Memory.ProvenanceClone == "" {
			return workspaceClone(projectID)
		}
		return daemonProvenanceClone(p.Memory.ProvenanceClone, root)
	}
}

// daemonProvenanceClone resolves a configured memory.provenance_clone for
// the verifier (design 22, part 8g). The clone is daemon-only: a path that
// resolves (symlinks followed on both sides) into the agent-mounted
// workspace root is refused, because an agent could then write the objects
// the verifier reads; a refused or unresolvable clone yields "", which the
// verifier reports as no clone (unverified), never as the workspace clone.
// An accepted clone is registered with the process-spawn law so its git
// reads are allowed.
func daemonProvenanceClone(clone, workspaceRoot string) string {
	dir := memory.DaemonCloneDir(clone, workspaceRoot)
	if dir == "" {
		return ""
	}
	// A provenance root, never a workspace root: GitHTTPBackend, the
	// daemon's git serving path for agents, must not reach it (#506, review
	// 28a5 F2).
	if err := spawn.RegisterProvenanceRoot(dir); err != nil {
		return ""
	}
	return dir
}

// provenanceClones lists the projects' configured daemon-only clones, for
// the sweeper's fetch (GitHub #499).
func (c *Container) provenanceClones() []memory.ProvenanceClone {
	if c == nil || c.Registry == nil {
		return nil
	}
	var out []memory.ProvenanceClone
	for _, p := range c.Registry.ListProjects() {
		if p != nil && p.Memory.ProvenanceClone != "" {
			out = append(out, memory.ProvenanceClone{ProjectID: p.ID, Dir: p.Memory.ProvenanceClone, RepoScope: p.RepoScope})
		}
	}
	// The automatic clones (#506 C1), listed by scope with no project.
	if c.provenanceAuto != nil {
		out = append(out, c.provenanceAuto.List()...)
	}
	return out
}

func newMemoryProvenanceWiring(repo persistence.MemoryVerificationRepository, workspaceRoot string) memoryProvenanceWiring {
	w := memoryProvenanceWiring{Repo: repo}
	w.CompanionDelegated = func(ctx context.Context, taskID string) (bool, error) {
		if repo == nil {
			return true, errNoVerificationRepo
		}
		m, err := repo.CompanionDelegatedTasks(ctx, []string{taskID})
		if err != nil {
			return true, err
		}
		d, ok := m[taskID]
		if !ok {
			// The repository answers every id; an absent one is an answer
			// it did not give, so it counts as delegated (code review 0780 F2).
			return true, nil
		}
		return d, nil
	}
	if repo == nil {
		return w
	}
	w.RecordProvenance = repo.UpsertProvenance
	if workspaceRoot != "" {
		w.Verifier = memory.NewProvenanceVerifier(workspaceRoot)
		w.Provenance = w.Verifier
	}
	return w
}

// provenanceCloneStatus feeds the memory_provenance_clone doctor check: each
// configured clone, whether the verifier accepts it, and its last fetch.
func (c *Container) provenanceCloneStatus() []api.ProvenanceCloneStatus {
	root := ""
	if c != nil && c.Config != nil {
		root = resolveProjectWorkspacePath(c.Config.Runtime.ProjectWorkspacePath)
	}
	// Keyed by clone directory, which both kinds have: an automatic clone
	// has no project (#506 C7).
	last := map[string]memory.ProvenanceFetchResult{}
	if c != nil && c.memoryProvenanceSweeper != nil {
		for _, r := range c.memoryProvenanceSweeper.LastFetches() {
			last[r.Dir] = r
		}
	}
	var out []api.ProvenanceCloneStatus
	for _, cl := range c.provenanceClones() {
		st := api.ProvenanceCloneStatus{ProjectID: cl.ProjectID, Usable: memory.DaemonCloneDir(cl.Dir, root) != ""}
		if cl.ProjectID == "" {
			// An automatic clone: listed by its repository, and usable by
			// construction (it lives under the provenance root).
			st.ProjectID, st.Usable = "automatic clone of "+cl.RepoScope, true
		}
		if r, ok := last[cl.Dir]; ok {
			st.Fetched, st.FetchOutcome, st.FetchError, st.FetchedAt = true, r.Outcome, r.Error, r.At
		}
		out = append(out, st)
	}
	return out
}
