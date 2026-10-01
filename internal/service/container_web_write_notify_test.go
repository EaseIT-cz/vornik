package service

import (
	"context"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/ui"
)

// Supervised web-write design Components.4: the operator is told about a
// pending web-write on Telegram, with the preview screenshot when there is
// one. The scraper returns the screenshot as a data URI; only a bounded
// JPEG or PNG is attached, anything else falls back to text.
func TestWebWriteScreenshotBytes(t *testing.T) {
	jpeg := []byte{0xff, 0xd8, 0xff, 0xe0, 1, 2, 3}
	png := []byte{0x89, 'P', 'N', 'G', 1, 2}
	uri := func(mime string, b []byte) string {
		return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(b)
	}
	cases := []struct {
		ref  string
		want []byte
	}{
		{uri("image/jpeg", jpeg), jpeg},
		{uri("image/png", png), png},
		{"", nil},
		{"artifact_123", nil},                                           // not inline
		{uri("image/svg+xml", []byte("<svg/>")), nil},                   // not a photo type
		{"data:image/jpeg;base64,!!!notbase64", nil},                    // undecodable
		{uri("image/jpeg", make([]byte, webWriteScreenshotMax+1)), nil}, // too big
	}
	for _, c := range cases {
		got := webWriteScreenshotBytes(c.ref)
		if string(got) != string(c.want) {
			t.Errorf("webWriteScreenshotBytes(%.40q) = %d bytes, want %d", c.ref, len(got), len(c.want))
		}
	}
}

func TestWebWriteInboxLink(t *testing.T) {
	if got := webWriteInboxLink("https://v.example/", "ww_1"); got != "https://v.example/ui/inbox#"+ui.WebWriteCardAnchor("ww_1") {
		t.Fatalf("link = %q", got)
	}
	if got := webWriteInboxLink("", "ww_1"); got != "/ui/inbox#web-write-ww_1" {
		t.Fatalf("link = %q", got)
	}
}

// No Telegram bot: the hook does nothing.
func TestNotifyWebWritePending_NoBotIsANoOp(t *testing.T) {
	c := &Container{Logger: zerolog.Nop(), Config: &config.Config{}}
	c.notifyWebWritePending(context.Background(), &persistence.WebWriteAction{SubmissionID: "ww_1"})
	c.notifyWebWritePending(context.Background(), nil)
	if !strings.Contains(webWriteInboxLink("", "x"), "#web-write-x") {
		t.Fatal("anchor must match the inbox card id")
	}
}

type recordingWebWriteAlerter struct {
	project, host, id, link string
	shot                    []byte
	panics                  bool
}

func (r *recordingWebWriteAlerter) NotifyWebWritePending(_ context.Context, project, host, id, link string, shot []byte) error {
	if r.panics {
		panic("telegram client bug")
	}
	r.project, r.host, r.id, r.link, r.shot = project, host, id, link, shot
	return nil
}

// review-20260930-ea34 F4: the container passes the row's summary, the
// shared deep link and the decoded screenshot to the bot.
func TestSendWebWriteAlert_PassesSummaryLinkAndScreenshot(t *testing.T) {
	rec := &recordingWebWriteAlerter{}
	jpeg := []byte{0xff, 0xd8, 0xff, 1}
	a := &persistence.WebWriteAction{ProjectID: "p1", TargetHost: "claims.example", SubmissionID: "ww_9",
		ScreenshotRef: "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(jpeg)}
	<-sendWebWriteAlert(zerolog.Nop(), rec, "https://v.example", a)
	if rec.project != "p1" || rec.host != "claims.example" || rec.id != "ww_9" ||
		rec.link != "https://v.example/ui/inbox#"+ui.WebWriteCardAnchor("ww_9") || string(rec.shot) != string(jpeg) {
		t.Fatalf("alert = %+v", rec)
	}
}

// review-20260930-ea34 F1: a panic in the Telegram send is contained; it
// must never take the daemon down.
func TestRunAlert_ContainsAPanic(_ *testing.T) {
	<-sendWebWriteAlert(zerolog.Nop(), &recordingWebWriteAlerter{panics: true}, "", &persistence.WebWriteAction{SubmissionID: "x"})
	<-runAlert(zerolog.Nop(), "test", func(context.Context) error { panic("boom") })
}
