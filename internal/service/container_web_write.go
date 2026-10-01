package service

import (
	"context"
	"encoding/base64"
	"strings"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/dispatcher"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/ui"
)

// webWriteComponents lazily builds — and memoizes on the container — the two
// shared pieces of the supervised web-write feature (LLD
// 2026-07-21-supervised-web-write-actions, Components 3/5):
//
//   - the pending-write store (persistence.WebWriteRepo over the daemon's raw
//     *sql.DB pool): web_submit(preview) inserts a pending row; the /inbox
//     approve surface lists + approves it; web_submit(submit) reads it back.
//   - the token-delivery store (dispatcher.WebWriteTokenStore): the operator-
//     chat-driven v1 channel by which the authenticated inbox approve hands the
//     freshly minted approval token to the submit path daemon-side (the assistant
//     never holds the token).
//
// Both must be the SAME instances across the dispatcher Agent and the UI server,
// so they are constructed exactly once here and reused by initDispatcher and
// initHTTPServer regardless of call order.
//
// Gate (mirrors how the scraper MCP is conditionally wired today, e.g.
// container_subsystems.go's block-notify hook): the scraper is reached over the
// MCP manager, so without c.mcpManager there is no scraper to write to; without
// c.DB there is no pending-write store. In either case the seams stay nil and
// web_submit degrades to its "not configured" HARD gate. The daemon-level
// web.writes tri-state toggle is enforced inside the tool itself — it is NOT a
// wiring gate here (an operator flipping web.writes on must not require a daemon
// rebuild), matching WithWebWritesConfig always being passed.
func (c *Container) webWriteComponents() (persistence.WebWriteRepo, *dispatcher.WebWriteTokenStore) {
	if c.webWriteRepo != nil && c.webWriteTokenStore != nil {
		return c.webWriteRepo, c.webWriteTokenStore
	}
	if c.mcpManager == nil || c.DB == nil {
		return nil, nil
	}
	c.webWriteRepo = persistence.NewWebWriteRepo(c.DB)
	c.webWriteTokenStore = dispatcher.NewWebWriteTokenStore()
	return c.webWriteRepo, c.webWriteTokenStore
}

// webWriteScreenshotMax bounds the screenshot attached to the Telegram
// alert (Telegram's photo limit is 10 MB; a form preview is far smaller).
const webWriteScreenshotMax = 5 << 20

// notifyWebWritePending sends the notify-only Telegram alert for a pending
// web-write (supervised web-write design, Components.4): project, target
// host, submission id, a deep link to its /inbox card, and the preview
// screenshot when there is one. It never carries a way to decide. It reads
// c.TelegramBot at call time and sends in the background, so web_submit
// never waits on Telegram. No bot: nothing.
func (c *Container) notifyWebWritePending(_ context.Context, a *persistence.WebWriteAction) {
	bot := c.TelegramBot
	if bot == nil || a == nil {
		return
	}
	sendWebWriteAlert(c.Logger, bot, c.Config.Server.PublicBaseURL, a)
}

// webWriteAlerter is the Telegram send the web-write alert needs.
type webWriteAlerter interface {
	NotifyWebWritePending(ctx context.Context, project, targetHost, submissionID, inboxURL string, screenshotJPEG []byte) error
}

// sendWebWriteAlert builds the alert from the row and sends it in the
// background. The returned channel closes when the send is over (tests).
func sendWebWriteAlert(logger zerolog.Logger, bot webWriteAlerter, publicBase string, a *persistence.WebWriteAction) <-chan struct{} {
	link := webWriteInboxLink(publicBase, a.SubmissionID)
	shot := webWriteScreenshotBytes(a.ScreenshotRef)
	project, host, id := a.ProjectID, a.TargetHost, a.SubmissionID
	return runAlert(logger.With().Str("submission_id", id).Logger(), "web-write", func(ctx context.Context) error {
		return bot.NotifyWebWritePending(ctx, project, host, id, link, shot)
	})
}

// webWriteInboxLink is the deep link to a pending web-write's /inbox card.
func webWriteInboxLink(publicBase, submissionID string) string {
	return brokerInboxURL(publicBase) + "#" + ui.WebWriteCardAnchor(submissionID)
}

// webWriteScreenshotBytes decodes the preview screenshot when the scraper
// returned it inline as a JPEG or PNG data URI of bounded size; anything
// else yields nil and the alert goes out as text.
func webWriteScreenshotBytes(ref string) []byte {
	for _, prefix := range []string{"data:image/jpeg;base64,", "data:image/png;base64,"} {
		if rest, ok := strings.CutPrefix(ref, prefix); ok {
			// DecodedLen over-counts by up to 3 bytes of padding; the
			// exact check after decoding enforces the limit.
			if base64.StdEncoding.DecodedLen(len(rest)) > webWriteScreenshotMax+3 {
				return nil
			}
			b, err := base64.StdEncoding.DecodeString(rest)
			if err != nil || len(b) > webWriteScreenshotMax {
				return nil
			}
			return b
		}
	}
	return nil
}
