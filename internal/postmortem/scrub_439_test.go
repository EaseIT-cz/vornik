package postmortem

// GitHub #439 (security audit 2026-10-17): the explainer must never call the
// provider with unscrubbed evidence. The route-level regression is
// TestPostMortem439_ProviderStubReceivesNoSecret (service); these pin the
// explainer's own fail-closed rule and the renderer's scrub-before-cut.

import (
	"context"
	"strings"
	"testing"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/report"
)

// passScrub is a scrubber that withholds nothing, for the tests of evidence
// assembly that predate #439 and assert on the raw text.
var passScrub report.LineScrubber = func(l string) (string, bool) { return l, false }

var planted439PM = "ghp_" + strings.Repeat("H6jK2mN8pR", 4)[:36]

func TestExplainer439_NoScrubberNoCall(t *testing.T) {
	chatStub := &stubChat{resp: makeChatResponse("x", 0, 0)}
	exp := &Explainer{
		Tasks:       &stubTaskRepo{task: &persistence.Task{ID: "task-1", ProjectID: "p1"}},
		PostMortems: &recordingPostMortemRepo{},
		Chat:        chatStub,
		Model:       "haiku",
		Logger:      zerolog.Nop(),
	}
	_, err := exp.Generate(context.Background(), "task-1", true)
	require.Error(t, err)
	assert.Empty(t, chatStub.captured, "no scrubber, no model call")
}

// A secret cut by the field limit must still be withheld: the renderer scrubs
// each field before it truncates it.
func TestRenderer439_ScrubsBeforeTheCut(t *testing.T) {
	scrub, err := report.NewLineScrubber("")
	require.NoError(t, err)
	le := strings.Repeat("x", 1490) + " " + planted439PM // the 1500-rune cut lands inside the token
	r := &Renderer{
		Tasks: &stubTaskRepo{task: &persistence.Task{ID: "t", ProjectID: "p", Status: persistence.TaskStatusFailed, LastError: &le}},
		Scrub: scrub,
	}
	res, err := r.Render(context.Background(), "t")
	require.NoError(t, err)
	assert.NotContains(t, res.Inputs.LastError, planted439PM[:8])
	assert.Equal(t, report.WithheldLine, res.Inputs.LastError)
}
