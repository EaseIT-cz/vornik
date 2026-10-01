package dispatcher

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/chat"
)

// 2026-09-28: 16 of the operator's last 42 Telegram replies opened with
// "[from your notes: not applicable.]", a marker the citation footer never
// defines. A replay on the real prompt and history showed the model copies it
// from its own earlier replies (11/12 with the marked history, 0/12 with the
// markers removed), so only well-formed markers survive, in history and in
// the reply (operator-profile-design.md, "Malformed citation markers are
// removed").
func TestSanitizeCitationMarkers(t *testing.T) {
	for in, want := range map[string]string{
		// Malformed: removed with the space after them.
		"[from your notes: not applicable.] Got it.":              "Got it.",
		"[from your notes: not applicable] Got it.":               "Got it.",
		"[From your notes: N/A] Got it.":                          "Got it.",
		"[from your profile: none] Scheduled.":                    "Scheduled.",
		"[from your profile: favourite_colour] Scheduled.":        "Scheduled.",
		"[overriding profile: mood — whatever] Fine.":             "Fine.",
		"Done. [from your notes: not applicable.]":                "Done.",
		"A.\n[from your notes: not applicable.] B.":               "A.\nB.",
		"[from your notes: not applicable.][from your notes] Ok.": "[from your notes] Ok.",
		// Well-formed: kept verbatim.
		"[from your notes] You prefer HTML.":                                  "[from your notes] You prefer HTML.",
		"[from your profile: tone] Bite my shiny metal ass.":                  "[from your profile: tone] Bite my shiny metal ass.",
		"[from your profile: preferred_channel] Sending HTML.":                "[from your profile: preferred_channel] Sending HTML.",
		"[overriding profile: verbosity — you asked for detail] Long answer.": "[overriding profile: verbosity — you asked for detail] Long answer.",
		// Shape only is checked by pattern; the key list decides (review 2026-09-28).
		"[from your profile: tone2] Hi.":         "Hi.",
		"[from your profile: time_zone] Prague.": "[from your profile: time_zone] Prague.",
		// No marker at all: untouched.
		"Plain reply with [brackets] and a [link](https://example.com).": "Plain reply with [brackets] and a [link](https://example.com).",
		"": "",
	} {
		assert.Equal(t, want, sanitizeCitationMarkers(in), "input %q", in)
	}
}

func TestProcess_MalformedCitationMarkersRemovedFromHistoryAndReply(t *testing.T) {
	srv, requests := hallucinationLoopServer(t, "[from your notes: not applicable.] Got it, the page is published.")
	agent := NewAgent(chat.NewClient(srv.URL, "k", "m"), nil, nil, nil, nil)

	userText := "why do replies start with [from your notes: not applicable]?"
	result := agent.Process(context.Background(), Request{
		Messages: []chat.Message{
			{Role: "user", Content: "schedule the brief"},
			{Role: "assistant", Content: "[from your notes: not applicable.] Scheduled."},
			{Role: "user", Content: userText},
		},
		Project: "p1",
	})
	require.NoError(t, result.Err)
	assert.Equal(t, "Got it, the page is published.", result.Text,
		"a malformed marker in the model's reply must not reach the channel")

	reqs := requests()
	require.Len(t, reqs, 1)
	var sawAssistant, sawUser bool
	for _, m := range reqs[0].Messages {
		switch {
		case m.Role == "assistant" && strings.Contains(m.Content, "Scheduled."):
			sawAssistant = true
			assert.Equal(t, "Scheduled.", m.Content, "a stored malformed marker must not seed the model again")
		case m.Role == "user" && strings.Contains(m.Content, "why do replies"):
			sawUser = true
			assert.Equal(t, userText, m.Content, "a user's own words are never altered")
		}
	}
	assert.True(t, sawAssistant && sawUser, "both history turns reach the model")
}

// The footer states the rule the replay showed wording alone cannot hold, so
// that the first malformed marker has no ambiguity to come from.
func TestCitationFooter_SaysNoMarkerWhenNothingApplied(t *testing.T) {
	f := operatorProfileCitationFooter
	assert.Contains(t, f, "no marker")
	assert.Contains(t, f, "not applicable")
}
