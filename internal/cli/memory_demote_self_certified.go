package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/storage"
)

// vornikctl memory demote-self-certified (design 22, "Amendment 2026-11-15
// (#550 D1, D2, D5)"). Chunks served verified on their own producer's stamp
// are demoted to unverified by ONE audited event whose backup keeps every
// prior state, confidence included: companion self-certified deposits (D5,
// confidence capped at 0.3) and role stamps whose origin cannot be read (D2,
// no artifact and no task). A dry run by default; --restore puts an event's
// changes back. It replaces the #435 "Existing rows" SQL, which wrote no
// event and no backup. A CLI verb, like reverify-delegated: only vornikctl
// acts on the operator's request.

var (
	demoteProject            string
	demoteApply              bool
	demoteReason             string
	demoteActor              string
	demoteRestore            string
	demoteAllowSelfCertified bool
)

var memoryDemoteSelfCertifiedCmd = &cobra.Command{
	Use:   "demote-self-certified",
	Short: "Demote chunks verified on their own producer's stamp (dry run by default)",
	Long: `Two classes of chunk are served verified on a stamp their own producer
wrote, which no rule now grants:

  companion       a companion deposit (producer companion:) its companion
                  stamped verified; demoted with confidence capped at 0.3
  unknown_origin  a role-of-record stamp (validator = producer, an agent
                  role) on a chunk with no artifact and no task, so its
                  origin cannot be read; demoted with confidence unchanged

A dry run by default: it prints the denominator (verified chunks in the
project), each class's count with up to five chunk ids, and what it does not
read (superseded and refuted chunks).

--apply (with --reason) demotes both classes as ONE audited event; the prior
status, validator and confidence of every changed chunk are kept, and
--restore <event-id> puts them back where nothing has changed them since.

Always runs against the configured database. Run the dry run first.`,
	RunE: runMemoryDemoteSelfCertified,
}

func init() {
	f := memoryDemoteSelfCertifiedCmd.Flags()
	f.StringVarP(&demoteProject, "project", "p", "", "Project ID (required)")
	f.BoolVar(&demoteApply, "apply", false, "Write the demotion (or the restore); without it this is a dry run")
	f.StringVar(&demoteReason, "reason", "", "Why (required with --apply); recorded on the event")
	f.StringVar(&demoteActor, "actor", "", "Who runs it (default: the OS user)")
	f.StringVar(&demoteRestore, "restore", "", "Restore the prior state an earlier --apply event changed")
	f.BoolVar(&demoteAllowSelfCertified, "allow-self-certified", false,
		"With --restore: reinstate the self-certified stamps the event removed")
	_ = memoryDemoteSelfCertifiedCmd.MarkFlagRequired("project")
	memoryCmd.AddCommand(memoryDemoteSelfCertifiedCmd)
}

func runMemoryDemoteSelfCertified(cmd *cobra.Command, _ []string) error {
	if demoteApply && strings.TrimSpace(demoteReason) == "" {
		return fmt.Errorf("--apply needs --reason: it is recorded on the audited event")
	}
	ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Minute)
	defer cancel()
	cfg, _, err := config.Load()
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
	opt := demoteOptions{Project: demoteProject, Apply: demoteApply, Reason: demoteReason,
		Actor: demoteActorName(), Restore: demoteRestore, AllowSelfCertified: demoteAllowSelfCertified}
	ev, err := demoteSelfCertified(ctx, cmd.OutOrStdout(), repo, opt)
	if err != nil || ev == nil {
		return err
	}
	recordReverifyAudit(ctx, backend, ev, cmd.OutOrStdout())
	return nil
}

func demoteActorName() string {
	if demoteActor != "" {
		return demoteActor
	}
	return osActorName()
}

type demoteOptions struct {
	Project, Reason, Actor, Restore string
	Apply, AllowSelfCertified       bool
}

// demoteSelfCertified is the verb on a repository: the dry run, the apply or
// the restore. It returns the event it wrote, or nil.
func demoteSelfCertified(ctx context.Context, out io.Writer, repo persistence.MemoryVerificationRepository, opt demoteOptions) (*persistence.MemoryVerificationEvent, error) {
	if opt.Restore != "" {
		return demoteRestoreEvent(ctx, out, repo, opt)
	}
	s, err := repo.SurveySelfCertified(ctx, opt.Project)
	if err != nil {
		return nil, err
	}
	candidates := renderDemoteSurvey(out, opt.Project, s)
	if !opt.Apply {
		_, _ = fmt.Fprintln(out, "(dry run; nothing written. Re-run with --apply --reason to write it as one audited event.)")
		return nil, nil
	}
	if candidates == 0 {
		_, _ = fmt.Fprintln(out, "nothing to change; no event written")
		return nil, nil
	}
	ev, err := repo.DemoteSelfCertified(ctx, &persistence.MemoryVerificationEvent{
		ProjectID: opt.Project, Actor: opt.Actor, Reason: opt.Reason,
		Examined: s.VerifiedChunks, Unchanged: s.VerifiedChunks - candidates,
	}, candidates)
	if err != nil {
		return nil, err
	}
	_, _ = fmt.Fprintf(out, "event %s: examined %d, demoted %d chunks, skipped %d changed since the count; restore with --restore %s\n",
		ev.ID, ev.Examined, ev.Deverified, ev.Skipped, ev.ID)
	return ev, nil
}

// renderDemoteSurvey prints the dry run and returns the candidate count.
func renderDemoteSurvey(out io.Writer, project string, s *persistence.SelfCertifiedSurvey) int {
	_, _ = fmt.Fprintf(out, "project %s: %d verified chunks examined (superseded and refuted chunks are not read)\n", project, s.VerifiedChunks)
	total := 0
	for _, c := range s.Classes {
		total += c.Chunks
		effect := "demoted to unverified, confidence unchanged"
		if c.Class == persistence.SelfCertifiedCompanion {
			effect = fmt.Sprintf("demoted to unverified, confidence capped at %.1f", persistence.SelfCertifiedConfidenceCap)
		}
		_, _ = fmt.Fprintf(out, "  %-15s %d chunks: %s\n", c.Class, c.Chunks, effect)
		if len(c.Sample) > 0 {
			_, _ = fmt.Fprintf(out, "                  e.g. %s\n", strings.Join(c.Sample, ", "))
		}
	}
	_, _ = fmt.Fprintf(out, "  left alone      %d verified chunks\n", s.VerifiedChunks-total)
	return total
}

// demoteRestoreEvent is --restore, guarded like reverify-delegated's: putting
// a demote back reinstates self-certified stamps, so it needs
// --allow-self-certified, and the count is recorded on the event.
func demoteRestoreEvent(ctx context.Context, out io.Writer, repo persistence.MemoryVerificationRepository, opt demoteOptions) (*persistence.MemoryVerificationEvent, error) {
	n, err := repo.CountRestorableSelfCertified(ctx, opt.Restore)
	if err != nil {
		return nil, err
	}
	_, _ = fmt.Fprintf(out, "restoring event %s would reinstate %d self-certified stamps (validator = producer)\n", opt.Restore, n)
	if !opt.Apply {
		_, _ = fmt.Fprintln(out, "(dry run; nothing written. Re-run with --apply --reason to restore.)")
		return nil, nil
	}
	reason, err := restoreReason(n, opt.AllowSelfCertified, opt.Reason)
	if err != nil {
		return nil, err
	}
	ev, err := repo.RestoreVerificationEvent(ctx, opt.Restore, opt.Actor, reason)
	if err != nil {
		return nil, err
	}
	_, _ = fmt.Fprintf(out, "restore event %s: examined %d, restored %d, skipped %d changed since\n",
		ev.ID, ev.Examined, ev.Verified+ev.Deverified, ev.Skipped)
	return ev, nil
}
