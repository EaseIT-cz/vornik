package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/user"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/artifacts"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/spawn"
	"vornik.io/vornik/internal/storage"
)

// vornikctl memory reverify-delegated (design 22, "Amendment 2026-10-21
// (#482)", part 5; operator decisions A8 and A17). The operator-run
// backfill for chunks verified before the rule existed: every chunk an agent
// role in a companion-delegated task stamped verified by role-of-record is
// re-verified by provenance, and the rest de-verified. A dry run by default;
// --apply writes ONE audited event whose backup keeps every prior state;
// --restore puts an event's changes back. A CLI verb because only vornikctl
// may run git on an operator's request.

// Outcomes of one candidate artifact.
const (
	reverifyNotDelegated = "not-delegated"
	reverifyVerify       = "verify"
	reverifyDeverify     = "de-verify"
	// reverifyUnavailable: the candidate's provenance could not be examined
	// (no clone exists for its repository). It stays verified, labelled
	// provenance:unavailable: the backfill never removes trust it cannot
	// disprove (#506 C8).
	reverifyUnavailable = "unavailable"
)

type reverifyRow struct {
	Cand     persistence.VerificationCandidate
	Outcome  string
	Reason   string
	Path     string
	Result   memory.ProvenanceResult
	Sampled  bool
	SampleOK *bool
	ChunkID  string
}

// reverifyDeps is what the verb needs; tests supply fakes.
type reverifyDeps struct {
	Repo     persistence.MemoryVerificationRepository
	Verifier *memory.ProvenanceVerifier
	// Content returns an artifact's stored bytes, or an error when they
	// are no longer in the store.
	Content func(ctx context.Context, artifactID string) ([]byte, error)
}

type reverifyOptions struct {
	Project string
	Sample  int
}

type reverifyPlan struct {
	Rows  []reverifyRow
	Files map[string]string // eligible files at origin/main, path -> blob
	// FilesErr says why the clone could not be listed (no_clone, ...).
	FilesErr string
}

// candidatePaths are the paths a candidate's provenance is tried at: its
// source_name first when it is a repository path, then every eligible file
// at origin/main with the same base name (a legacy upload was named only by
// its file name). Only an exact blob match verifies, so a wrong file cannot
// verify; when two eligible files hold identical bytes the record names the
// first in sort order (code review 9a9a F9, F13).
func candidatePaths(sourceName string, files map[string]string) []string {
	var out []string
	if strings.Contains(sourceName, "/") {
		out = append(out, sourceName)
	}
	base := path.Base(sourceName)
	var byBase []string
	for p := range files {
		if path.Base(p) == base && p != sourceName {
			byBase = append(byBase, p)
		}
	}
	sort.Strings(byBase)
	return append(out, byBase...)
}

// planReverify decides every candidate of a project without writing.
func planReverify(ctx context.Context, d reverifyDeps, opt reverifyOptions) (*reverifyPlan, error) {
	plan := &reverifyPlan{}
	files, err := d.Verifier.OriginMainEligibleFiles(ctx, opt.Project)
	if err != nil {
		plan.FilesErr = err.Error()
	}
	plan.Files = files
	after := ""
	for {
		page, err := d.Repo.ListRoleOfRecordCandidates(ctx, opt.Project, after, 200)
		if err != nil {
			return nil, err
		}
		if len(page) == 0 {
			break
		}
		ids := make([]string, len(page))
		for i, c := range page {
			ids[i] = c.TaskID
		}
		delegated, err := d.Repo.CompanionDelegatedTasks(ctx, ids)
		if err != nil {
			return nil, err
		}
		for _, c := range page {
			after = c.ArtifactID
			if !delegated[c.TaskID] {
				plan.Rows = append(plan.Rows, reverifyRow{Cand: c, Outcome: reverifyNotDelegated})
				continue
			}
			plan.Rows = append(plan.Rows, decideReverify(ctx, d, opt, c, files))
		}
	}
	return plan, nil
}

// decideReverify runs the provenance rule for one companion-delegated
// candidate: verified at the first path whose blob matches, else de-verified
// with the first path's reason.
func decideReverify(ctx context.Context, d reverifyDeps, opt reverifyOptions, c persistence.VerificationCandidate, files map[string]string) reverifyRow {
	row := reverifyRow{Cand: c, Outcome: reverifyDeverify, Reason: memory.ProvReasonPathNotAllowed}
	content, cerr := d.Content(ctx, c.ArtifactID)
	paths := candidatePaths(c.SourceName, files)
	switch {
	case cerr != nil || content == nil:
		row.Reason = memory.ProvReasonContentUnavailable
		return row
	case len(paths) == 0:
		row.Path = c.SourceName
		return row
	}
	for i, p := range paths {
		res := d.Verifier.Verify(ctx, memory.ProvenanceRequest{ProjectID: opt.Project, RepoScope: c.RepoScope, Path: p, Content: content})
		if i == 0 || res.Verified() {
			row.Result, row.Path, row.Reason = res, p, res.Reason
		}
		if res.Verified() {
			row.Outcome, row.Reason = reverifyVerify, ""
			break
		}
	}
	if row.Outcome == reverifyDeverify && memory.ProvenanceUnexaminable(row.Reason) {
		row.Outcome = reverifyUnavailable
	}
	return row
}

func normalizeText(s string) string { return strings.Join(strings.Fields(s), " ") }

// sampleReverify runs the content sample: up to n candidates per outcome,
// checked against origin/main's text without using the blob ids.
func sampleReverify(ctx context.Context, d reverifyDeps, opt reverifyOptions, plan *reverifyPlan) {
	texts := map[string]string{}
	text := func(blob string) string {
		if t, ok := texts[blob]; ok {
			return t
		}
		b, err := d.Verifier.ReadOriginBlob(ctx, opt.Project, blob)
		if err != nil {
			b = nil
		}
		texts[blob] = normalizeText(string(b))
		return texts[blob]
	}
	taken := map[string]int{}
	for i := range plan.Rows {
		r := &plan.Rows[i]
		if r.Outcome == reverifyNotDelegated || taken[r.Outcome] >= opt.Sample {
			continue
		}
		chunks, err := d.Repo.ArtifactChunks(ctx, opt.Project, r.Cand.ArtifactID, 1)
		if err != nil || len(chunks) == 0 {
			continue
		}
		taken[r.Outcome]++
		r.Sampled, r.ChunkID = true, chunks[0].ID
		needle := normalizeText(chunks[0].Content)
		found := false
		if r.Outcome == reverifyVerify {
			found = needle != "" && strings.Contains(text(r.Result.OriginBlobSHA), needle)
		} else {
			for _, blob := range plan.Files {
				if needle != "" && strings.Contains(text(blob), needle) {
					found = true
					break
				}
			}
		}
		r.SampleOK = &found
	}
}

// renderReverify prints the denominator first, then the split, then the
// sample with what it does and does not measure.
func renderReverify(w io.Writer, opt reverifyOptions, plan *reverifyPlan) {
	type tally struct{ artifacts, chunks int }
	by := map[string]*tally{}
	reasons := map[string]*tally{}
	total := tally{}
	for _, r := range plan.Rows {
		total.artifacts++
		total.chunks += r.Cand.Chunks
		t := by[r.Outcome]
		if t == nil {
			t = &tally{}
			by[r.Outcome] = t
		}
		t.artifacts++
		t.chunks += r.Cand.Chunks
		if r.Outcome == reverifyDeverify {
			rt := reasons[r.Reason]
			if rt == nil {
				rt = &tally{}
				reasons[r.Reason] = rt
			}
			rt.artifacts++
			rt.chunks += r.Cand.Chunks
		}
	}
	get := func(k string) tally {
		if t := by[k]; t != nil {
			return *t
		}
		return tally{}
	}
	_, _ = fmt.Fprintf(w, "project %s: examined %d artifacts, %d chunks carrying a role-of-record stamp\n", opt.Project, total.artifacts, total.chunks)
	if plan.FilesErr != "" {
		_, _ = fmt.Fprintf(w, "the project's clone could not be read (%s): nothing can be provenance-verified\n", plan.FilesErr)
	} else {
		_, _ = fmt.Fprintf(w, "origin/main holds %d eligible files\n", len(plan.Files))
	}
	nd, v, dv, un := get(reverifyNotDelegated), get(reverifyVerify), get(reverifyDeverify), get(reverifyUnavailable)
	_, _ = fmt.Fprintf(w, "  not companion-delegated, left alone: %d artifacts, %d chunks\n", nd.artifacts, nd.chunks)
	_, _ = fmt.Fprintf(w, "  verified by provenance:              %d artifacts, %d chunks\n", v.artifacts, v.chunks)
	_, _ = fmt.Fprintf(w, "  kept, provenance unavailable:        %d artifacts, %d chunks (no clone of their repository; never de-verified)\n", un.artifacts, un.chunks)
	_, _ = fmt.Fprintf(w, "  de-verified:                         %d artifacts, %d chunks\n", dv.artifacts, dv.chunks)
	keys := make([]string, 0, len(reasons))
	for k := range reasons {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		_, _ = fmt.Fprintf(w, "    %-22s %d artifacts, %d chunks\n", k, reasons[k].artifacts, reasons[k].chunks)
	}
	sampled := map[string][2]int{}
	for _, r := range plan.Rows {
		if !r.Sampled || r.SampleOK == nil {
			continue
		}
		s := sampled[r.Outcome]
		s[0]++
		if *r.SampleOK {
			s[1]++
		}
		sampled[r.Outcome] = s
	}
	_, _ = fmt.Fprintf(w, "content sample (up to %d chunks per outcome; reads origin/main text, not the blob ids):\n", opt.Sample)
	s := sampled[reverifyVerify]
	_, _ = fmt.Fprintf(w, "  verified: %d of %d sampled chunks are text of their file at origin/main (a miss is a contradiction to read; a hit cannot prove blob equality)\n", s[1], s[0])
	s = sampled[reverifyDeverify]
	_, _ = fmt.Fprintf(w, "  de-verified: %d of %d sampled chunks have their text still on main in an eligible file (look at these: usually an older version of a design, correctly stale, possibly a design the rule could not tie to a path)\n", s[1], s[0])
	for _, r := range plan.Rows {
		if r.Sampled && r.SampleOK != nil {
			_, _ = fmt.Fprintf(w, "    sample %-10s chunk %s artifact %s path %q text-on-main=%v\n", r.Outcome, r.ChunkID, r.Cand.ArtifactID, r.Path, *r.SampleOK)
		}
	}
}

// applyReverify writes the plan as one audited event.
func applyReverify(ctx context.Context, d reverifyDeps, opt reverifyOptions, plan *reverifyPlan, actor, reason string) (*persistence.MemoryVerificationEvent, error) {
	ev := &persistence.MemoryVerificationEvent{ProjectID: opt.Project, Kind: persistence.MemoryVerificationKindBackfill,
		Actor: actor, Reason: reason}
	var changes []persistence.ArtifactVerificationChange
	for _, r := range plan.Rows {
		ev.Examined++
		switch r.Outcome {
		case reverifyNotDelegated:
			ev.Unchanged++
		case reverifyVerify:
			changes = append(changes, persistence.ArtifactVerificationChange{ArtifactID: r.Cand.ArtifactID,
				NewStatus: "verified", NewValidatorRole: r.Result.Role(),
				Provenance: memory.ProvenanceRecord(opt.Project, r.Cand.ArtifactID, r.Cand.RepoScope, r.Result)})
		case reverifyUnavailable:
			// Only the role-of-record stamp the plan saw is relabelled, so a
			// provenance stamp is never overwritten (code review fbe6 F1).
			changes = append(changes, persistence.ArtifactVerificationChange{ArtifactID: r.Cand.ArtifactID,
				NewStatus: "verified", NewValidatorRole: memory.ValidatorRoleProvenanceUnavailable,
				OnlyValidatorRole: r.Cand.ProducerRole,
				Provenance:        memory.ProvenanceRecord(opt.Project, r.Cand.ArtifactID, r.Cand.RepoScope, r.Result)})
		case reverifyDeverify:
			// Only the role-of-record stamp the plan saw is withdrawn: a
			// provenance stamp written between plan and apply survives
			// (code review 9a9a F4).
			changes = append(changes, persistence.ArtifactVerificationChange{ArtifactID: r.Cand.ArtifactID,
				NewStatus: "unverified", OnlyValidatorRole: r.Cand.ProducerRole})
		}
	}
	if len(changes) == 0 {
		return ev, nil // nothing to change: no event (code review 9a9a F6)
	}
	if err := d.Repo.ApplyVerificationChange(ctx, ev, changes); err != nil {
		return nil, err
	}
	return ev, nil
}

var (
	reverifyProject            string
	reverifyApply              bool
	reverifyReason             string
	reverifyActor              string
	reverifySample             int
	reverifyNoFetch            bool
	reverifyRestore            string
	reverifyAcceptUnverifiable bool
)

var memoryReverifyDelegatedCmd = &cobra.Command{
	Use:   "reverify-delegated",
	Short: "Re-verify companion-delegated chunks by provenance; de-verify the rest (dry run by default)",
	Long: `Chunks an agent role wrote in a companion-delegated task were stamped
verified by role-of-record before design 22 amendment #482. This verb
re-verifies each such artifact by provenance (its stored bytes are the blob
at an eligible path on origin's default branch in the project's own clone)
and de-verifies the rest. The default branch is the project's
workspace.refresh_branch when set, checked against the clone's origin/HEAD,
else origin/HEAD.

A dry run by default: it prints the denominator (every artifact and chunk
examined), how many are left alone (not companion-delegated), verified and
de-verified (split by reason), and a content sample with what it measures.

--apply (with --reason) writes everything as ONE audited event; the prior
state of every changed chunk is kept, and --restore <event-id> puts it back
where nothing has changed it since. Before verifying it force-fetches the
clone's tracking ref of that branch from origin (--no-fetch skips it), so a
tracking ref pushed before the push guard existed is reset.

Always runs against the configured database. Run the dry run first.`,
	RunE: runMemoryReverifyDelegated,
}

func init() {
	f := memoryReverifyDelegatedCmd.Flags()
	f.StringVarP(&reverifyProject, "project", "p", "", "Project ID (required)")
	f.BoolVar(&reverifyApply, "apply", false, "Write the plan (or the restore); without it this is a dry run")
	f.StringVar(&reverifyReason, "reason", "", "Why (required with --apply); recorded on the event")
	f.StringVar(&reverifyActor, "actor", "", "Who runs it (default: the OS user)")
	f.IntVar(&reverifySample, "sample", 20, "Chunks per outcome in the content sample")
	f.BoolVar(&reverifyNoFetch, "no-fetch", false, "Do not fetch the default branch into the clone first: verify against the tracking ref as it stands")
	f.StringVar(&reverifyRestore, "restore", "", "Restore the prior state an earlier --apply event changed")
	f.BoolVar(&reverifyAcceptUnverifiable, "accept-unverifiable", false, "Apply although the clone or its fetch failed (every companion-delegated chunk is de-verified)")
	_ = memoryReverifyDelegatedCmd.MarkFlagRequired("project")
	memoryCmd.AddCommand(memoryReverifyDelegatedCmd)
}

func reverifyActorName() string {
	if reverifyActor != "" {
		return reverifyActor
	}
	if u, err := user.Current(); err == nil && u.Username != "" {
		return "operator:" + u.Username
	}
	return "operator"
}

func runMemoryReverifyDelegated(cmd *cobra.Command, _ []string) error {
	out := cmd.OutOrStdout()
	if reverifyApply && strings.TrimSpace(reverifyReason) == "" {
		return fmt.Errorf("--apply needs --reason: it is recorded on the audited event")
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Minute)
	defer cancel()
	cfg, cfgPath, err := config.Load()
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}
	backend, err := storage.Open(ctx, cfg.Database)
	if err != nil {
		return fmt.Errorf("open database: %w", err)
	}
	defer func() { _ = backend.Close() }()
	repo := backend.Repos.MemoryVerification
	if repo == nil {
		return fmt.Errorf("this database has no memory verification repository")
	}

	if reverifyRestore != "" {
		return runReverifyRestore(ctx, out, backend, repo)
	}
	return runReverifyBackfill(ctx, out, cfg, cfgPath, backend, repo)
}

// runReverifyRestore is --restore: the count of reinstated self-certified
// stamps first, then (with --apply) the restore as its own audited event.
func runReverifyRestore(ctx context.Context, out io.Writer, backend *storage.Backend, repo persistence.MemoryVerificationRepository) error {
	n, err := repo.CountRestorableSelfCertified(ctx, reverifyRestore)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "restoring event %s would reinstate %d role-of-record stamps (validator = producer); the hourly sweep does not see those\n", reverifyRestore, n)
	if !reverifyApply {
		_, _ = fmt.Fprintln(out, "(dry run; nothing written. Re-run with --apply --reason to restore.)")
		return nil
	}
	reason, err := restoreReason(n, reverifyAllowSelfCertified, reverifyReason)
	if err != nil {
		return err
	}
	ev, err := repo.RestoreVerificationEvent(ctx, reverifyRestore, reverifyActorName(), reason)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "restore event %s: examined %d, restored %d (verified %d), skipped %d changed since\n",
		ev.ID, ev.Examined, ev.Verified+ev.Deverified, ev.Verified, ev.Skipped)
	recordReverifyAudit(ctx, backend, ev, out)
	_, _ = fmt.Fprintln(out, "run the dry run again: the reinstated stamps are candidates again")
	return nil
}

// runReverifyBackfill is the dry run and, with --apply, the backfill.
func runReverifyBackfill(ctx context.Context, out io.Writer, cfg *config.Config, cfgPath string, backend *storage.Backend, repo persistence.MemoryVerificationRepository) error {
	root := cfg.Runtime.ProjectWorkspaceDir()
	if root == "" {
		return fmt.Errorf("no project workspace root is configured (runtime.project_workspace_path or VORNIK_DATA_DIR)")
	}
	if err := spawn.RegisterWorkspaceRoot(root); err != nil {
		return err
	}
	verifier := memory.NewProvenanceVerifier(root)
	// The trust root is the project's trusted branch and clone, as the
	// daemon resolves them: memory.provenance_branch else workspace.
	// refresh_branch, checked against the clone's origin/HEAD, and the
	// daemon-only memory.provenance_clone when set (GitHub #499).
	branch, clone, ownScope, err := configuredTrustRoot(cfg, cfgPath, reverifyProject)
	if err != nil {
		return err
	}
	verifier.DefaultBranch = func(string) string { return branch }
	cloneDir := ""
	if clone != "" {
		cloneDir = memory.DaemonCloneDir(clone, root)
		if cloneDir == "" {
			return fmt.Errorf("memory.provenance_clone %q does not resolve, or resolves into the workspace root %s", clone, root)
		}
		// A provenance root, never a workspace root (#506, review 28a5 F2).
		if err := spawn.RegisterProvenanceRoot(cloneDir); err != nil {
			return err
		}
		verifier.CloneDir = func(string) string { return cloneDir }
	}
	// #506 C8: the backfill resolves a clone per repository scope exactly as
	// the daemon does (memory.ScopedCloneFor): the configured clone for the
	// project's own repository, else the scope's automatic clone, created on
	// first use. A candidate whose repository has no clone is kept verified as
	// provenance:unavailable, never de-verified.
	var auto *memory.AutoClones
	if autoRoot := memory.AutoCloneRoot(root); autoRoot != "" {
		if err := os.MkdirAll(autoRoot, 0o700); err != nil {
			return err
		}
		if err := spawn.RegisterProvenanceRoot(autoRoot); err != nil {
			return err
		}
		auto = &memory.AutoClones{Root: autoRoot, Run: memory.SpawnCloneRunner{}}
		// #508 F1: the same forges and default-host choice as the daemon.
		// GitHub App tokens are the daemon's (it holds the registry's App
		// keys); the backfill clones a GitHub repository with a forge token
		// or the host's own credential helpers, as before #508.
		if err := memory.ConfigureAutoClones(auto, cfg.Memory.Provenance, nil); err != nil {
			return err
		}
	}
	verifier.CloneFor = memory.ScopedCloneFor(func(string) (string, string, bool) {
		if clone == "" {
			return "", "", false
		}
		return ownScope, cloneDir, cloneDir == ""
	}, auto)
	memory.WireForgeVerification(verifier, auto)
	verifier.DefaultBranchFor = func(_, scope string) string {
		if scope != "" && strings.EqualFold(scope, ownScope) {
			return branch
		}
		return ""
	}
	fetchOK := true
	switch {
	case clone == "":
		// Nothing to fetch by hand: the automatic clones fetch themselves
		// before each verification.
		_, _ = fmt.Fprintln(out, "no memory.provenance_clone configured: each repository is verified against its automatic clone")
	case !reverifyNoFetch:
		msg, ok := fetchOriginMain(ctx, verifier, reverifyProject)
		fetchOK = ok
		_, _ = fmt.Fprintln(out, msg)
	case reverifyApply:
		// Round-3 review C5: the tracking ref is trusted as it stands.
		_, _ = fmt.Fprintln(out, noFetchApplyWarning(clone))
	}
	artBackend, err := storage.OpenArtifactBackend(ctx, cfg.Storage)
	if err != nil {
		return fmt.Errorf("open artifact backend: %w", err)
	}
	defer func() { _ = artBackend.Close() }()
	store, err := artifacts.New(artifacts.WithBasePath(cfg.Storage.ArtifactsPath),
		artifacts.WithRepository(backend.Repos.Artifacts), artifacts.WithBackend(artBackend))
	if err != nil {
		return fmt.Errorf("open artifact store: %w", err)
	}
	d := reverifyDeps{Repo: repo, Verifier: verifier, Content: store.Retrieve}
	opt := reverifyOptions{Project: reverifyProject, Sample: reverifySample}
	plan, err := planReverify(ctx, d, opt)
	if err != nil {
		return err
	}
	sampleReverify(ctx, d, opt, plan)
	renderReverify(out, opt, plan)
	if !reverifyApply {
		_, _ = fmt.Fprintln(out, "(dry run; nothing written. Re-run with --apply --reason to write it as one audited event.)")
		return nil
	}
	// De-verifying because origin could not be read inverts the model: refuse
	// unless the operator says that is what they mean (code review 9a9a F2,
	// F8).
	// Since #506 C8 an unreadable clone no longer de-verifies (unexaminable
	// candidates are kept as provenance:unavailable), so only a failed fetch
	// of a configured clone refuses: it would verify against a stale branch.
	if !fetchOK && !reverifyAcceptUnverifiable {
		return fmt.Errorf("refusing --apply: the fetch of the configured provenance clone failed (%s), so its repository would be "+
			"verified against a stale branch; fix the fetch, or pass --accept-unverifiable if that is intended", plan.FilesErr)
	}
	ev, err := applyReverify(ctx, d, opt, plan, reverifyActorName(), reverifyReason)
	if err != nil {
		return err
	}
	if ev.ID == "" {
		_, _ = fmt.Fprintln(out, "nothing to change; no event written")
		return nil
	}
	_, _ = fmt.Fprintf(out, "event %s: examined %d, unchanged %d, verified %d chunks, de-verified %d chunks; restore with --restore %s\n",
		ev.ID, ev.Examined, ev.Unchanged, ev.Verified, ev.Deverified, ev.ID)
	recordReverifyAudit(ctx, backend, ev, out)
	return nil
}

// fetchOriginMain force-updates the clone's tracking ref from origin. The
// daemon fetches only a configured daemon-only clone (the hourly sweep,
// GitHub #499); a workspace clone is fetched here, by the operator.
func fetchOriginMain(ctx context.Context, v *memory.ProvenanceVerifier, projectID string) (string, bool) {
	dir := v.CloneDir(projectID)
	if dir == "" {
		return "fetch skipped: no clone for this project", false
	}
	if _, err := v.Git(ctx, dir, "remote", "get-url", "origin"); err != nil {
		return "fetch skipped: the clone has no origin remote", false
	}
	branch, ok := v.TrustedBranch(ctx, projectID)
	if !ok {
		return "fetch skipped: the project's default branch cannot be resolved (no origin/HEAD, or it disagrees with workspace.refresh_branch); nothing will verify", false
	}
	refspec := "+refs/heads/" + branch + ":refs/remotes/origin/" + branch
	if _, err := v.Git(ctx, dir, "fetch", "--quiet", "origin", refspec); err != nil {
		return "fetch failed (verifying against the clone's current origin/" + branch + "): " + err.Error(), false
	}
	return "fetched origin/" + branch + " into the project's clone", true
}

// configuredTrustRoot reads the project's trusted branch (memory.
// provenance_branch, else workspace.refresh_branch; "" = none configured)
// and its memory.provenance_clone ("" = the workspace clone) from the config
// tree, as the daemon's registry holds them.
func configuredTrustRoot(cfg *config.Config, cfgPath, projectID string) (string, string, string, error) {
	configsDir := resolveConfigsDir(cfgPath)
	if configsDir == "" {
		return "", "", "", fmt.Errorf("could not locate configs/ directory (set VORNIK_CONFIGS_DIR): the project's default branch is part of the trust root")
	}
	reg := registry.New()
	reg.SetAgentModelCatalogue(cfg.AgentAdmin.ModelIDs())
	if err := reg.Load(configsDir); err != nil {
		return "", "", "", fmt.Errorf("load registry from %s: %w", configsDir, err)
	}
	p := reg.GetProject(projectID)
	if p == nil {
		return "", "", "", fmt.Errorf("project %q is not in the registry at %s", projectID, configsDir)
	}
	return p.TrustedBranch(), p.Memory.ProvenanceClone, p.RepoScope, nil
}

// recordReverifyAudit mirrors the event into admin_audit. The event row is
// the audited record (written in the change's transaction); this copy puts
// it on the operator's audit trail and is best-effort.
func recordReverifyAudit(ctx context.Context, backend *storage.Backend, ev *persistence.MemoryVerificationEvent, out io.Writer) {
	if backend == nil || backend.Repos == nil || backend.Repos.AdminAudit == nil || ev == nil {
		return
	}
	after, _ := json.Marshal(map[string]any{"project_id": ev.ProjectID, "reason": ev.Reason, "examined": ev.Examined,
		"unchanged": ev.Unchanged, "verified": ev.Verified, "deverified": ev.Deverified, "skipped": ev.Skipped,
		"restores_event_id": ev.RestoresEventID})
	entry := &persistence.AdminAuditEntry{
		Principal: ev.Actor, Source: "vornikctl", Action: "memory.verification." + ev.Kind, Target: ev.ID,
		After: string(after),
	}
	if err := backend.Repos.AdminAudit.Insert(ctx, entry); err != nil {
		_, _ = fmt.Fprintf(out, "warning: the admin audit copy was not written (%v); the event row %s is the record\n", err, ev.ID)
	}
}
