package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/datasubject"
)

// `vornikctl subject extend` records an Art 12(3) extension (GitHub #344
// slice 5d-1c; art17 redact design §15.16). Request.Extend had no caller, so
// no extension could be recorded, and the escalation pass names this command.
//
// see LLD § https://docs.vornik.io §15.16

var (
	subjectExtendReason     string
	subjectExtendNotifiedAt string
)

var subjectExtendCmd = &cobra.Command{
	Use:   "extend <request-id>",
	Short: "Record an Article 12(3) extension the subject was told of",
	Long: `Extend a request's deadline by up to two further months (Art 12(3)).

The extension is lawful only where it is necessary given the complexity and
number of requests, and only if the subject was told of it, with the reasons,
within one month of receipt of the request. Tell the subject first; then record
it here with the date you told them. A notice date later than the first month
(the end of the same date next month, or of that month's last day) is
refused: the extension cannot be claimed for a deadline already missed.

--notified-at takes RFC 3339 (2026-10-30T14:00:00Z) or a date (2026-10-30, read
as 00:00 UTC).`,
	Args: cobra.ExactArgs(1),
	RunE: runSubjectExtend,
}

func init() {
	subjectExtendCmd.Flags().StringVar(&subjectExtendReason, "reason", "",
		"why the extension is necessary: the complexity or number of requests (required)")
	subjectExtendCmd.Flags().StringVar(&subjectExtendNotifiedAt, "notified-at", "",
		"when the subject was told of the extension and its reasons (required)")
	subjectCmd.AddCommand(subjectExtendCmd)
}

// requestStore is what extending a request needs.
type requestStore interface {
	GetRequest(ctx context.Context, id string) (datasubject.Request, error)
	SaveRequest(ctx context.Context, req datasubject.Request) error
}

func runSubjectExtend(_ *cobra.Command, args []string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	repo, closeFn, err := openSubjectRepo(ctx)
	if err != nil {
		return err
	}
	defer closeFn()
	req, err := extendRequest(ctx, repo, args[0], subjectExtendReason, subjectExtendNotifiedAt, time.Now().UTC())
	if err != nil {
		return err
	}
	fmt.Printf("request %s extended: deadline %s (subject told %s)\n", req.ID,
		req.Deadline().UTC().Format(time.RFC3339), req.ExtensionNotifiedAt.UTC().Format(time.RFC3339))
	return nil
}

func extendRequest(ctx context.Context, repo requestStore, id, reason, notifiedAt string, now time.Time) (datasubject.Request, error) {
	at, err := parseNoticeDate(notifiedAt)
	if err != nil {
		return datasubject.Request{}, err
	}
	req, err := repo.GetRequest(ctx, id)
	if err != nil {
		return datasubject.Request{}, err
	}
	if err := req.Extend(reason, at, now); err != nil {
		return datasubject.Request{}, err
	}
	if err := repo.SaveRequest(ctx, req); err != nil {
		return datasubject.Request{}, err
	}
	return req, nil
}

func parseNoticeDate(s string) (time.Time, error) {
	if strings.TrimSpace(s) == "" {
		return time.Time{}, fmt.Errorf("--notified-at is required: the extension is lawful only if the subject was told of it")
	}
	return parseFlagTime("--notified-at", s)
}

// parseFlagTime reads a time flag: RFC 3339 (2026-10-30T14:00:00Z) or a date
// (2026-10-30, read as 00:00 UTC). Shared by --notified-at and --received-at.
func parseFlagTime(flag, s string) (time.Time, error) {
	s = strings.TrimSpace(s)
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("%s %q is neither RFC 3339 nor YYYY-MM-DD", flag, s)
}
