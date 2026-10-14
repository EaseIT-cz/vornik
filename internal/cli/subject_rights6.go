package cli

import (
	"bufio"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/datasubject"
	"vornik.io/vornik/internal/persistence/postgres"
	"vornik.io/vornik/internal/persistence/subjectstore"
	"vornik.io/vornik/internal/storage"
)

// `vornikctl subject restrict | lift-restriction | rectify` — GDPR increment 6
// (GitHub #343): Art 18 restriction and Art 16 rectification, through the same
// request ledger as access and erasure. Each verb previews, confirms, stores its
// report on the request with its hash, and actions the request only when the
// software carried out everything it planned.
//
// see LLD § https://docs.vornik.io §4.16

var (
	subjectRestrictGround  string
	subjectRestrictBy      string
	subjectLiftBy          string
	subjectLiftReason      string
	subjectLiftInformed    bool
	subjectLiftRequest     string
	subjectRectifyWrong    string
	subjectRectifyRight    string
	subjectRights6Yes      bool
	subjectRights6Out      string
	subjectRectifyReviewer = reviewRectificationOnTerminal
)

var subjectRestrictCmd = &cobra.Command{
	Use:   "restrict <request-id>",
	Short: "Impose an Article 18 restriction of processing for a verified request",
	Long: `Restrict processing of a subject's personal data (GDPR Art 18) for a verified
restriction request.

While the restriction is in force, memory records LINKED to the subject — or that
mention a knowledge-graph entity linked to the subject — are stored but not used:
they are excluded from every recall (including what agents and the reranker model
see), from memory listings and exports, from the knowledge-graph chunk lookup, and
from every model pass over memory (consolidation, titling, classification,
embedding, graph extraction).

WHAT IT DOES NOT REACH, and the report lists each with its row count: linked rows
of other tables (transcripts, the operator profile, identities, sessions, uploaded
files, extracted documents, knowledge-graph entities), and anything about the
subject that was never linked to them. Restrict those by other means. Operator
administration and the subject's own access, portability and erasure requests
still see restricted records.

The restriction lasts until 'subject lift-restriction', which requires that the
subject was informed first (Art 18(3)).`,
	Args: cobra.ExactArgs(1),
	RunE: runSubjectRestrict,
}

var subjectLiftRestrictionCmd = &cobra.Command{
	Use:   "lift-restriction <subject-id>",
	Short: "Lift a subject's Article 18 restriction (the subject must be informed first)",
	Long: `Lift the active Article 18 restriction on a subject.

Art 18(3) requires the subject who obtained the restriction to be informed BEFORE
it is lifted. The software cannot inform them: inform them first, then pass
--subject-informed, which records your affirmation and its time. The restriction
record is kept, not deleted, as evidence of how long the right was honoured.`,
	Args: cobra.ExactArgs(1),
	RunE: runSubjectLiftRestriction,
}

var subjectRectifyCmd = &cobra.Command{
	Use:   "rectify <request-id>",
	Short: "Correct inaccurate personal data (Article 16) for a verified request",
	Long: `Correct inaccurate personal data about a subject (GDPR Art 16).

Every case-insensitive occurrence of --incorrect in the memory records and
knowledge-graph entities linked to the subject is replaced with --correct. It is a
literal replacement, not a model rewrite. You review every proposed change; there
is no way to skip the review.

Conversation transcripts are NOT rewritten — they record what was said. The
correction is recorded against them as a supplementary statement instead (Art 16).
Rectification never deletes a record: one whose corrected text would duplicate
another is left for manual handling.

The inaccurate original is kept in the request's stored report, and the subject is
told so. Run once per phrasing of the inaccuracy.`,
	Args: cobra.ExactArgs(1),
	RunE: runSubjectRectify,
}

func init() {
	subjectRestrictCmd.Flags().StringVar(&subjectRestrictGround, "ground", "",
		"Art 18(1) ground (required):\n"+restrictionGroundHelp())
	subjectRestrictCmd.Flags().StringVar(&subjectRestrictBy, "by", "", "operator imposing the restriction (required)")
	subjectRestrictCmd.Flags().BoolVar(&subjectRights6Yes, "yes", false, "skip the confirmation prompt")
	subjectRestrictCmd.Flags().StringVar(&subjectRights6Out, "out", "", "also write the report here")

	subjectLiftRestrictionCmd.Flags().StringVar(&subjectLiftBy, "by", "", "operator lifting the restriction (required)")
	subjectLiftRestrictionCmd.Flags().StringVar(&subjectLiftReason, "reason", "", "why it is lifted (required)")
	subjectLiftRestrictionCmd.Flags().BoolVar(&subjectLiftInformed, "subject-informed", false,
		"affirm that the subject was informed BEFORE the lift (Art 18(3); required)")
	subjectLiftRestrictionCmd.Flags().StringVar(&subjectLiftRequest, "request", "",
		"the subject's own request to lift, when there is one")

	subjectRectifyCmd.Flags().StringVar(&subjectRectifyWrong, "incorrect", "", "the inaccurate text (required)")
	subjectRectifyCmd.Flags().StringVar(&subjectRectifyRight, "correct", "", "the accurate replacement (required)")
	subjectRectifyCmd.Flags().StringVar(&subjectRights6Out, "out", "", "also write the report here")

	subjectCmd.AddCommand(subjectRestrictCmd, subjectLiftRestrictionCmd, subjectRectifyCmd)
}

func restrictionGroundHelp() string {
	var b strings.Builder
	for _, g := range datasubject.RestrictionGrounds() {
		fmt.Fprintf(&b, "  %-10s %s — %s\n", g, g.Article(), g.Label())
	}
	return b.String()
}

// validateLiftFlags runs before anything touches the database, so a missing
// Art 18(3) affirmation is reported as itself.
func validateLiftFlags(by, reason string, informed bool) error {
	if strings.TrimSpace(by) == "" {
		return errors.New("--by is required")
	}
	if strings.TrimSpace(reason) == "" {
		return errors.New("--reason is required: it is what the subject would contest")
	}
	if !informed {
		return errors.New("lift refused under Art 18(3): the subject must be informed BEFORE the restriction is lifted; " +
			"inform them, then re-run with --subject-informed")
	}
	return nil
}

// liftAffirmationLine is what the record says about Art 18(3): the OPERATOR
// affirmed it. The software never established that the subject was informed.
func liftAffirmationLine(by string) string {
	return fmt.Sprintf("Art 18(3): operator %q affirmed the subject was informed before this lift "+
		"(recorded with the time; not verified by the software)", by)
}

// openSubjectDB opens the subject store for verb. restrict and lift-restriction
// need only the store and run on either driver (GitHub #355); a verb whose
// machinery is Postgres-only passes that machinery as pgOnly and is refused on
// SQLite by name. The returned *sql.DB is the backend's handle.
func openSubjectDB(ctx context.Context, verb, pgOnly string) (*subjectstore.Repository, *sql.DB, func(), error) {
	cfg, _, err := config.Load()
	if err != nil {
		return nil, nil, nil, fmt.Errorf("load config: %w", err)
	}
	backend, err := storage.Open(ctx, cfg.Database)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("open database: %w", err)
	}
	if pgOnly != "" {
		if _, err := requirePostgresSubjectVerb(backend, verb, pgOnly); err != nil {
			_ = backend.Close()
			return nil, nil, nil, err
		}
	}
	repo, err := subjectStoreFor(backend, verb)
	if err != nil {
		_ = backend.Close()
		return nil, nil, nil, err
	}
	return repo, backend.DB, func() { _ = backend.Close() }, nil
}

func confirmTyped(word string) bool {
	fmt.Printf("\nType '%s' to proceed: ", word)
	answer, _ := bufio.NewReader(os.Stdin).ReadString('\n')
	return strings.TrimSpace(answer) == word
}

// storeRequestReport stores the report on the request ALWAYS (an incomplete run's
// report is the record of what is outstanding), then actions the request only
// when complete — the contract writeErasureReport established.
func storeRequestReport(ctx context.Context, repo *subjectstore.Repository,
	req *datasubject.Request, report any, complete bool) error {
	blob, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	sum := sha256.Sum256(blob)
	hash := "sha256:" + hex.EncodeToString(sum[:])
	if err := repo.SaveRequestReport(ctx, req.ID, string(blob), hash); err != nil {
		return fmt.Errorf("store report: %w", err)
	}
	fmt.Printf("\nreport stored against request %s (hash %s)\n", req.ID, hash)
	if subjectRights6Out != "" {
		if err := os.WriteFile(subjectRights6Out, blob, 0o600); err != nil {
			return err
		}
		fmt.Printf("copy written to %s\n", subjectRights6Out)
	}
	if !complete {
		fmt.Println("The request is left un-actioned: marking it complete while work remains would " +
			"record a false completion in the ledger.")
		return nil
	}
	if err := req.Action(hash, time.Now().UTC()); err != nil {
		return err
	}
	if err := repo.SaveRequest(ctx, *req); err != nil {
		return err
	}
	fmt.Printf("request %s actioned\n", req.ID)
	return nil
}

func runSubjectRestrict(_ *cobra.Command, args []string) error {
	ground := datasubject.RestrictionGround(subjectRestrictGround)
	if err := ground.Validate(); err != nil {
		return fmt.Errorf("%w\n\nPass --ground with one of:\n%s", err, restrictionGroundHelp())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	repo, _, closeFn, err := openSubjectDB(ctx, "subject restrict", "")
	if err != nil {
		return err
	}
	defer closeFn()

	req, err := repo.GetRequest(ctx, args[0])
	if err != nil {
		return err
	}
	res, err := datasubject.NewRestriction(req, ground, subjectRestrictBy, time.Now())
	if err != nil {
		return err
	}
	links, err := repo.ListLinks(ctx, req.SubjectID)
	if err != nil {
		return err
	}
	rep := datasubject.BuildRestrictionReport(res, links)
	printRestrictionReport(rep)
	// The knowledge-graph route covers chunks that mention an entity linked to
	// this subject (design §4.16.6). If no entity is bound — or the person's
	// entity is still held by a kg: placeholder — that route is empty; say so
	// BEFORE the confirmation, while the operator can still run resolve-kg.
	if ids, idErr := repo.ListIdentifiers(ctx, req.SubjectID); idErr == nil {
		warnIfKGUnresolved(ids, req)
	}

	if !subjectRights6Yes && !confirmTyped("restrict") {
		fmt.Println("Aborted. Nothing was restricted.")
		return nil
	}
	if err := repo.CreateRestriction(ctx, res); err != nil {
		return err
	}
	fmt.Printf("\nrestriction %s in force for subject %s from %s\n",
		res.ID, res.SubjectID, res.RestrictedAt.Format(time.RFC3339))
	// The restriction is in force before the report is stored: if storing fails,
	// the data is still protected and the operator re-runs nothing but the report.
	return storeRequestReport(ctx, repo, &req, rep, true)
}

func printRestrictionReport(rep datasubject.RestrictionReport) {
	fmt.Printf("Restriction for subject %s (request %s) under %s\n", rep.SubjectID, rep.RequestID, rep.Article)
	fmt.Printf("%d linked record(s) examined.\n", rep.RowsExamined)
	fmt.Println("\nWITHHELD from use while restricted:")
	for _, tc := range rep.Withheld {
		if tc.Reason != "" {
			// The residual of a withheld table (#350): what its withholding does not reach.
			fmt.Printf("  %-24s %d — %s\n", tc.Table, tc.Rows, tc.Reason)
			continue
		}
		fmt.Printf("  %-24s %d\n", tc.Table, tc.Rows)
	}
	if rep.SharedWithheld > 0 {
		fmt.Printf("  (%d of them also concern other people and are withheld whole)\n", rep.SharedWithheld)
	}
	if len(rep.NotWithheld) > 0 {
		fmt.Println("\nNOT withheld by the software — restrict these by other means:")
		for _, tc := range rep.NotWithheld {
			fmt.Printf("  %-24s %d — %s\n", tc.Table, tc.Rows, tc.Reason)
		}
	}
	fmt.Println("\nLimitations:")
	for _, l := range rep.Limitations {
		fmt.Printf("  - %s\n", l)
	}
}

func runSubjectLiftRestriction(_ *cobra.Command, args []string) error {
	if err := validateLiftFlags(subjectLiftBy, subjectLiftReason, subjectLiftInformed); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	repo, _, closeFn, err := openSubjectDB(ctx, "subject lift-restriction", "")
	if err != nil {
		return err
	}
	defer closeFn()

	if id := strings.TrimSpace(subjectLiftRequest); id != "" {
		liftReq, err := repo.GetRequest(ctx, id)
		if err != nil {
			return fmt.Errorf("--request %s: %w", id, err)
		}
		if liftReq.SubjectID != args[0] {
			return fmt.Errorf("--request %s belongs to subject %s, not %s", id, liftReq.SubjectID, args[0])
		}
	}
	res, err := repo.ActiveRestriction(ctx, args[0])
	if err != nil {
		return err
	}
	if err := res.Lift(subjectLiftBy, subjectLiftReason, subjectLiftInformed, subjectLiftRequest, time.Now()); err != nil {
		return err
	}
	if err := repo.SaveRestrictionLift(ctx, res); err != nil {
		return err
	}
	fmt.Printf("restriction %s on subject %s lifted at %s\n", res.ID, res.SubjectID, res.LiftedAt.Format(time.RFC3339))
	fmt.Println(liftAffirmationLine(res.LiftedBy))
	return nil
}

func runSubjectRectify(_ *cobra.Command, args []string) error {
	correction := datasubject.Correction{Incorrect: subjectRectifyWrong, Correct: subjectRectifyRight}
	if err := correction.Validate(); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Second)
	defer cancel()
	repo, db, closeFn, err := openSubjectDB(ctx, "subject rectify",
		"Art 16 rectification (the chunk redactor with re-embed and the entity rectifier)")
	if err != nil {
		return err
	}
	defer closeFn()

	req, err := repo.GetRequest(ctx, args[0])
	if err != nil {
		return err
	}
	links, err := repo.ListLinks(ctx, req.SubjectID)
	if err != nil {
		return err
	}
	items, err := repo.CollectItems(ctx, req.SubjectID, links)
	if err != nil {
		return err
	}
	plan, err := datasubject.PlanRectification(req, items, correction)
	if err != nil {
		return err
	}
	fmt.Printf("Rectification for subject %s (request %s): %d linked record(s) examined, %d contain the inaccurate text.\n",
		plan.SubjectID, plan.RequestID, plan.Examined, len(plan.Actions))
	for _, a := range plan.Actions {
		fmt.Printf("  %-24s %-24s %s\n", a.Treatment, a.Table, a.RowID)
	}
	if len(plan.Actions) == 0 {
		fmt.Println("\nNothing linked to this subject contains that text. The request is left open: try another " +
			"phrasing, or record the outcome with 'subject refuse'.")
		return nil
	}

	r := &datasubject.Rectifier{
		Chunks:   postgres.NewChunkRedactorRepository(db),
		Entities: postgres.NewEntityRectifierRepository(db),
		Approve:  subjectRectifyReviewer,
	}
	res, execErr := r.Execute(ctx, plan)
	if res == nil {
		return execErr
	}
	printRectificationResult(res)
	report := struct {
		Narrative string                           `json:"subject_narrative"`
		Plan      *datasubject.RectificationPlan   `json:"plan"`
		Result    *datasubject.RectificationResult `json:"result"`
	}{res.Narrative(), plan, res}
	if err := storeRequestReport(ctx, repo, &req, report, res.Complete()); err != nil {
		return err
	}
	return execErr
}

func printRectificationResult(r *datasubject.RectificationResult) {
	fmt.Printf("\nCorrected: %d record(s) (originals kept in the request report).\n", len(r.Corrected))
	for _, c := range r.Corrected {
		fmt.Printf("  CORRECTED %s/%s\n", c.Table, c.RowID)
	}
	for _, s := range r.Statements {
		fmt.Printf("  STATEMENT %s/%s — transcript not rewritten; supplementary statement recorded\n", s.Table, s.RowID)
	}
	for _, m := range r.Manual {
		fmt.Printf("  MANUAL    %s/%s — %s\n", m.Table, m.RowID, m.Reason)
	}
	for _, d := range r.Deferred {
		fmt.Printf("  DEFERRED  %s/%s — %s\n", d.Table, d.RowID, d.Reason)
	}
	for _, f := range r.Failed {
		fmt.Printf("  FAILED    %s/%s — %s\n", f.Table, f.RowID, f.Err)
	}
	fmt.Println("\nNot re-derived by this command: an entity's embedding is cleared and not recomputed, and " +
		"knowledge-graph mentions and edges extracted from a corrected record keep the old text until it is re-extracted.")
}

// reviewRectificationOnTerminal is the mandatory review: a literal replacement can
// match an unintended place, which only a reader catches.
func reviewRectificationOnTerminal(p datasubject.RectificationProposal) (bool, error) {
	fmt.Printf("\n────────────────────────────────────────────────────────────\n")
	fmt.Printf("REVIEW CORRECTION  %s/%s\n", p.Table, p.RowID)
	fmt.Printf("\n--- BEFORE ---\n%s\n", p.Before)
	fmt.Printf("\n--- AFTER ----\n%s\n", p.After)
	fmt.Printf("\nCheck the replacement only touched what is about this subject. Apply? [y/N/q]: ")
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil {
		return false, fmt.Errorf("read review decision: %w", err)
	}
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true, nil
	case "q", "quit":
		return false, fmt.Errorf("review abandoned by the operator")
	default:
		return false, nil
	}
}
