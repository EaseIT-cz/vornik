package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/datasubject"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/persistence/postgres"
	"vornik.io/vornik/internal/storage"
	"vornik.io/vornik/internal/subjectresolve"
)

// `vornikctl subject resolve-kg` — GDPR increment 4, the knowledge-graph binder,
// RESOLVED ON DEMAND (controller decision 2026-07-29).
//
// The rejected alternative was to materialise every KG-derived PERSON entity as
// a data-subject row, giving each named third party a standing subject. That
// would build a permanent register of identified people out of LLM extraction
// output — increasing the personal data held in order to make deletion
// possible. So resolution happens here, when a request has actually named
// someone, and never as a background sweep.
//
// PREVIEW, THEN NAME WHAT IS REAL. The binder's confidence ceiling is
// `possible`: it has false positives (an entity carrying a person's name that
// is not that person) and false negatives (a nickname the extractor never
// linked). Running it without an --entity shows candidates and writes nothing;
// only an entity id a human named is bound. On the erasure path an auto-linked
// false positive deletes a third party's data — a breach committed while
// honouring a right.

var (
	subjectKGNames    []string
	subjectKGProjects []string
	subjectKGEntities []string
	subjectKGAdopt    bool
	subjectKGRequest  string
)

var subjectResolveKGCmd = &cobra.Command{
	Use:   "resolve-kg <subject-id> --request <request-id>",
	Short: "Find and bind the knowledge-graph entities that denote this subject",
	Long: `Resolve a subject against the knowledge graph, so records that merely NAME
them are reachable by an access export or an erasure.

--request names the VERIFIED rights request this serves, and is required. A
search for a person's name across everything the deployment holds is itself
processing, lawful because a verified request requires it: an open (identity
not yet established), refused or closed request is refused, and so is a request
for a different subject. Every run is recorded in admin_audit BEFORE anything is
read (who, which request, how many names and how long — never the names); if
that record cannot be written, nothing is searched. Each bind is recorded after
it is written. The same rules govern the console's Resolve page.

Run with no --entity to PREVIEW. Nothing is written: you get every PERSON entity
whose canonical name or alias contains one of this subject's names, with how
many records mentions it, and who (if anyone) already holds it.

Re-run with --entity <id> for each entity that really is this person. Links are
written at 'possible' confidence — this binder is the widest net available and
the least reliable, and every report says so.

WHY YOU PICK, AND NOT THE DAEMON. A name search cannot tell one Jane Doe from
another, and an entity carrying a person's name may not be a person at all.
Linking a wrong entity to an erasure request destroys someone else's data while
honouring this subject's rights, so the judgement is yours and nothing is bound
without it.

An entity under an active Art 18 restriction is listed by id only, "held under
an active restriction — details withheld", and cannot be bound: it is stored,
not used.

--name adds a name to search for (at most 5, each 3–100 characters). Use it for a nickname or a maiden name the
extractor never linked to the canonical entity: that false negative is the known
limit of this binder, and you are the backstop for it.

COLLISIONS. An entity may already be bound to the placeholder subject the chat
path mints (display name 'kg:<entity-id>') when a shared note named this person.
--adopt moves that placeholder's links onto this subject and removes it, so one
person does not keep two subject rows. An entity bound to another IDENTIFIED
subject is refused outright and --adopt does not override it: two named people
claiming one entity is a question about people, and merging them would disclose
one person's data to another.`,
	Args: cobra.ExactArgs(1),
	RunE: runSubjectResolveKG,
}

func init() {
	subjectResolveKGCmd.Flags().StringArrayVar(&subjectKGNames, "name", nil,
		"extra name to search for (repeatable) — a nickname or former name the extractor never linked")
	subjectResolveKGCmd.Flags().StringArrayVar(&subjectKGProjects, "project", nil,
		"limit the search to these projects (repeatable); default is every project with a graph")
	subjectResolveKGCmd.Flags().StringArrayVar(&subjectKGEntities, "entity", nil,
		"bind this entity id to the subject (repeatable); without it the command only previews")
	subjectResolveKGCmd.Flags().BoolVar(&subjectKGAdopt, "adopt", false,
		"fold a placeholder 'kg:<entity-id>' subject into this one, moving its links")
	subjectResolveKGCmd.Flags().StringVar(&subjectKGRequest, "request", "",
		"the VERIFIED rights request this resolution serves (required; must be for <subject-id>)")
	subjectCmd.AddCommand(subjectResolveKGCmd)
}

// openKGResolver wires the shared gates over the subject repository, the
// graph, the Art 18 screen and admin_audit — the same production
// implementations the console page is wired with.
func openKGResolver(ctx context.Context) (resolveKGDeps, func(), error) {
	cfg, _, err := config.Load()
	if err != nil {
		return resolveKGDeps{}, nil, fmt.Errorf("load config: %w", err)
	}
	backend, err := storage.Open(ctx, cfg.Database)
	if err != nil {
		return resolveKGDeps{}, nil, fmt.Errorf("open database: %w", err)
	}
	db, err := requirePostgresSubjectVerb(backend, "subject resolve-kg", "the knowledge-graph subject index")
	if err != nil {
		_ = backend.Close()
		return resolveKGDeps{}, nil, err
	}
	repo := postgres.NewDataSubjectRepository(db)
	deps := resolveKGDeps{
		svc: &subjectresolve.Service{
			Requests: repo,
			KG: &datasubject.KGResolver{
				Store: repo,
				Index: postgres.NewDataSubjectKGIndex(db),
			},
			Screen: persistence.NewSQLRestrictionScreen(db),
			Audit:  postgres.NewAdminAuditRepository(db),
		},
		subjects: repo,
		out:      os.Stdout,
		actor:    subjectresolve.Actor{Principal: cliPrincipal(), Source: "cli"},
	}
	return deps, func() { _ = backend.Close() }, nil
}

// resolveKGDeps is the command's seam: the shared gates (subjectresolve, the
// same functions the console page calls) plus the subject reader for the
// display name and where the operator's output goes.
type resolveKGDeps struct {
	svc      *subjectresolve.Service
	subjects interface {
		GetSubject(ctx context.Context, id string) (datasubject.Subject, error)
	}
	out   io.Writer
	actor subjectresolve.Actor
}

type resolveKGOpts struct {
	requestID, subjectID      string
	names, projects, entities []string
	adopt                     bool
}

func runSubjectResolveKG(_ *cobra.Command, args []string) error {
	// Generous: a resolve over every project's graph is a handful of indexed
	// queries per name, but an operator with many projects should not have a
	// timeout decide how much of their obligation gets discharged.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	deps, closeFn, err := openKGResolver(ctx)
	if err != nil {
		return err
	}
	defer closeFn()
	return runResolveKG(ctx, deps, resolveKGOpts{
		requestID: subjectKGRequest, subjectID: args[0], names: subjectKGNames,
		projects: subjectKGProjects, entities: subjectKGEntities, adopt: subjectKGAdopt,
	})
}

// runResolveKG applies the gates in the page's order (design §4.17.9): a
// verified request for THIS subject, valid names, the search on record in
// admin_audit before anything is read, then the screened preview or the
// screened binds, each bind audited after its write.
func runResolveKG(ctx context.Context, d resolveKGDeps, o resolveKGOpts) error {
	if strings.TrimSpace(o.requestID) == "" {
		return fmt.Errorf("--request <request-id> is required: resolution searches everything this " +
			"deployment holds for a person's name, which is lawful only for a VERIFIED rights request " +
			"(open one with `vornikctl subject request`, verify it with `vornikctl subject verify`)")
	}
	req, err := d.svc.Authorize(ctx, o.requestID)
	var notVerified *subjectresolve.NotVerifiedError
	switch {
	case errors.Is(err, subjectresolve.ErrRequestNotFound) || errors.As(err, &notVerified):
		return err // ids and a state only
	case err != nil:
		return fmt.Errorf("read request %s: %w", o.requestID, subjectresolve.RootCause(err))
	}
	if req.SubjectID != o.subjectID {
		return fmt.Errorf("request %s is for subject %s, not %s: nothing was searched", req.ID, req.SubjectID, o.subjectID)
	}
	names, err := subjectresolve.ValidateNames(o.names)
	if err != nil {
		return err
	}
	// Trimmed like the page's project filter (code review 2026-10-13).
	var projects []string
	for _, p := range o.projects {
		if p = strings.TrimSpace(p); p != "" {
			projects = append(projects, p)
		}
	}
	o.projects = projects
	subject, err := d.subjects.GetSubject(ctx, req.SubjectID)
	if err != nil {
		return fmt.Errorf("read subject %s: %w", req.SubjectID, subjectresolve.RootCause(err))
	}
	terms := subjectresolve.SearchTerms(subject.DisplayName, names)
	// The search is on record BEFORE anything is read; a bind re-runs the
	// candidate search, so it is a search too.
	if err := d.svc.AuditSearch(ctx, d.actor, req, o.projects, terms); err != nil {
		return fmt.Errorf("the search could not be recorded in admin_audit, so it was refused "+
			"(nothing was read): %w", subjectresolve.RootCause(err))
	}
	if len(o.entities) == 0 {
		return previewKGCandidates(ctx, d, req, names, o.projects)
	}
	return bindKGEntities(ctx, d, req, names, o)
}

// previewKGCandidates prints the screened proposals and writes nothing.
func previewKGCandidates(ctx context.Context, d resolveKGDeps, req datasubject.Request, names, projects []string) error {
	cands, err := d.svc.ScreenedCandidates(ctx, req.SubjectID, names, projects)
	if errors.Is(err, subjectresolve.ErrScreen) {
		return fmt.Errorf("candidates withheld: the processing-restriction check failed, and an " +
			"unscreened list is never shown")
	}
	if err != nil {
		// Root cause only: the resolver's wrapping quotes the searched name.
		return fmt.Errorf("knowledge-graph candidates could not be read: %w", subjectresolve.RootCause(err))
	}
	out := d.out
	if len(cands) == 0 {
		_, _ = fmt.Fprintf(out, "No PERSON entity matches this subject's names.\n\n")
		_, _ = fmt.Fprintf(out, "That is a real answer, not an error: the graph may hold nothing about them, or\n")
		_, _ = fmt.Fprintf(out, "they may appear only under a name it never linked. Try --name with a nickname,\n")
		_, _ = fmt.Fprintf(out, "a former name, or a shorter form. Nothing was written.\n")
		return nil
	}

	w := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(w, "ENTITY\tPROJECT\tNAME\tMATCHED ON\tRECORDS\tSTATE")
	plain := make([]datasubject.KGCandidate, 0, len(cands))
	withheld := 0
	for _, c := range cands {
		if c.Withheld {
			// Art 18: stored, not used. Id and project only — no name,
			// match, count, state or action.
			withheld++
			_, _ = fmt.Fprintf(w, "%s\t%s\t-\t-\t-\theld under an active restriction — details withheld\n",
				c.Entity.ID, c.Entity.ProjectID)
			continue
		}
		plain = append(plain, c.KGCandidate)
		_, _ = fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%d\t%s\n",
			c.Entity.ID, c.Entity.ProjectID, truncateName(c.Entity.CanonicalName),
			c.MatchedOn, c.MentionCount, describeKGState(c.KGCandidate))
	}
	_ = w.Flush()

	_, _ = fmt.Fprintf(out, "\nNOTHING WAS WRITTEN. These are candidates at 'possible' confidence — the widest\n")
	_, _ = fmt.Fprintf(out, "net this deployment has and the least reliable one. A name search cannot tell one\n")
	_, _ = fmt.Fprintf(out, "person from another of the same name, and an entity carrying a person's name may\n")
	_, _ = fmt.Fprintf(out, "not be a person at all.\n\n")
	if withheld > 0 {
		_, _ = fmt.Fprintf(out, "Entities held under an active Art 18 restriction: %d. They are shown by id\n"+
			"only and cannot be bound here.\n\n", withheld)
	}
	bindable := false
	for _, c := range plain {
		if c.State != datasubject.KGConflict {
			bindable = true
		}
	}
	if bindable {
		_, _ = fmt.Fprintf(out, "Bind the ones that really are this subject:\n\n")
		_, _ = fmt.Fprintf(out, "  vornikctl subject resolve-kg %s --request %s", req.SubjectID, req.ID)
		for _, n := range names {
			_, _ = fmt.Fprintf(out, " --name %q", n)
		}
		for _, p := range projects {
			_, _ = fmt.Fprintf(out, " --project %s", p)
		}
		for _, c := range plain {
			if c.State != datasubject.KGConflict {
				_, _ = fmt.Fprintf(out, " --entity %s", c.Entity.ID)
			}
		}
		_, _ = fmt.Fprintf(out, "\n")
	}
	if anyPlaceholder(plain) {
		_, _ = fmt.Fprintf(out, "\nAdd --adopt to fold the placeholder subject(s) above into this one.\n")
	}
	if anyConflict(plain) {
		_, _ = fmt.Fprintf(out, "\nEntities held by another identified subject are omitted from that line and\n")
		_, _ = fmt.Fprintf(out, "cannot be bound here. Reconcile the two subjects by hand first.\n")
	}
	return nil
}

// bindKGEntities binds each named entity through the shared screen-then-bind,
// reporting each outcome and auditing each AFTER its write. One failure does
// not abandon the rest: a partly-discharged request the operator can see is
// better than an all-or-nothing that stops at the first refusal.
func bindKGEntities(ctx context.Context, d resolveKGDeps, req datasubject.Request, names []string, o resolveKGOpts) error {
	out := d.out
	var failures, auditFailures []string
	choice := "confirm"
	if o.adopt {
		choice = "adopt"
	}
	for _, entityID := range o.entities {
		res := d.svc.BindEntity(ctx, req, entityID, names, o.projects, o.adopt)
		if res.OK {
			_, _ = fmt.Fprintf(out, "bound %s: %s\n", entityID, strings.TrimPrefix(res.Page, "ok: "))
		} else {
			// The page text may name another subject (a conflict); the
			// operator may read it, the audit row and the error may not.
			_, _ = fmt.Fprintf(out, "REFUSED %s: %s\n", entityID, strings.TrimPrefix(res.Page, "refused: "))
			failures = append(failures, entityID)
		}
		if err := d.svc.AuditDecision(ctx, d.actor, subjectresolve.ActionBindEntity, req, map[string]any{
			"subject_id": req.SubjectID, "kind": "kg", "id": entityID, "choice": choice, "outcome": res.Audited,
		}); err != nil {
			_, _ = fmt.Fprintf(out, "AUDIT WRITE FAILED for %s (%v) — the outcome above stands; record it by hand\n",
				entityID, subjectresolve.RootCause(err))
			auditFailures = append(auditFailures, entityID)
		}
	}
	var errs []string
	if len(failures) > 0 {
		errs = append(errs, fmt.Sprintf("%d of %d entities were not bound: %s",
			len(failures), len(o.entities), strings.Join(failures, ", ")))
	}
	if len(auditFailures) > 0 {
		errs = append(errs, fmt.Sprintf("AUDIT WRITE FAILED for %s", strings.Join(auditFailures, ", ")))
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	_, _ = fmt.Fprintf(out, "\nLinks are recorded at 'possible' confidence and marked as shared rows: an access\n")
	_, _ = fmt.Fprintf(out, "export LISTS them and withholds their content under Art 15(4), and an erasure\n")
	_, _ = fmt.Fprintf(out, "treats them under the shared-record rule for the request's Art 17(1) ground.\n")
	return nil
}

// describeKGState renders a candidate's binding state for the preview.
func describeKGState(c datasubject.KGCandidate) string {
	switch c.State {
	case datasubject.KGBoundHere:
		return "already bound here"
	case datasubject.KGBoundToPlaceholder:
		return fmt.Sprintf("placeholder %s — needs --adopt", c.BoundSubjectID)
	case datasubject.KGConflict:
		return fmt.Sprintf("HELD BY %s (%q) — resolve by hand", c.BoundSubjectID, c.BoundSubjectName)
	default:
		return "free"
	}
}

func anyPlaceholder(cands []datasubject.KGCandidate) bool {
	for _, c := range cands {
		if c.State == datasubject.KGBoundToPlaceholder {
			return true
		}
	}
	return false
}

func anyConflict(cands []datasubject.KGCandidate) bool {
	for _, c := range cands {
		if c.State == datasubject.KGConflict {
			return true
		}
	}
	return false
}

// truncateName keeps the preview table readable without hiding which person a
// row is about — the first 48 characters of a name are always the deciding part.
// Cut on runes, never bytes (code review 2026-10-13: a byte cut split a rune).
func truncateName(s string) string {
	const limit = 48
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit-1]) + "…"
}
