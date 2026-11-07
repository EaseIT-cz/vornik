package dispatcher

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// TurnStatusReporter receives coarse progress for an in-flight turn so the
// originating channel can show the human that work is happening.
//
// Implemented in the service container, which resolves the channel by name and
// forwards to it when it is a conversation.TurnStatusChannel. The dispatcher
// deliberately does not know about channels.
//
// Every call is best-effort and must not block the turn: this fires from inside
// the tool loop, and a slow status update would make the very latency it is
// reporting worse.
type TurnStatusReporter interface {
	ReportTurnStatus(ctx context.Context, channel, sessionID, status string)
}

// SetTurnStatusReporter binds the status sink. Late-bound like the other
// channel-facing seams, because channels are constructed after the agent.
func (a *Agent) SetTurnStatusReporter(r TurnStatusReporter) {
	a.turnStatusMu.Lock()
	a.turnStatus = r
	a.turnStatusMu.Unlock()
}

// TurnStatusReporterBound reports whether a status sink is bound. It exists for
// the container's wiring test (#642); production code does not need it.
func (a *Agent) TurnStatusReporterBound() bool {
	a.turnStatusMu.RLock()
	defer a.turnStatusMu.RUnlock()
	return a.turnStatus != nil
}

// reportTurnStatus forwards one status line for the turn described by req.
// No-op when nothing is bound or the request has no originating channel — API
// and A2A callers have no surface to show it on.
func (a *Agent) reportTurnStatus(ctx context.Context, req Request, status string) {
	if a == nil || req.OriginatingChannel == "" || req.OriginatingSessionID == "" {
		return
	}
	a.turnStatusMu.RLock()
	reporter := a.turnStatus
	a.turnStatusMu.RUnlock()
	if reporter == nil {
		return
	}
	reporter.ReportTurnStatus(ctx, req.OriginatingChannel, req.OriginatingSessionID, status)
}

// toolStatusLine renders a tool call as something a non-engineer can read.
//
// The tool NAME is the only thing surfaced, never its arguments: a status line
// is visible to everyone in a shared channel, and arguments routinely carry the
// substance of the request (file paths, search terms, a person's name).
func toolStatusLine(toolName string) string {
	name := strings.TrimSpace(toolName)
	if name == "" {
		return "working on it…"
	}
	if phrase, ok := friendlyToolPhrases[name]; ok {
		return phrase
	}
	// An MCP tool (mcp__<server>__<tool>) names its server, as the stream
	// markers did before #642 removed them.
	if rest, ok := strings.CutPrefix(name, "mcp__"); ok {
		if server, tool, found := strings.Cut(rest, "__"); found && server != "" {
			return "using " + server + "/" + tool + "…"
		}
		return "using " + rest + "…"
	}
	// Unknown tools degrade to the bare name rather than to something invented.
	return "running `" + name + "`…"
}

// turnProgress builds the one status line a turn shows (conversation-channel
// design, update 2026-11-05, GitHub #642): the current activity, the elapsed
// time and every activity so far with a count. It replaced a status line per
// tool call, which piled up above the answer on Telegram.
type turnProgress struct {
	start  time.Time
	order  []string
	counts map[string]int
}

func newTurnProgress(start time.Time) *turnProgress {
	return &turnProgress{start: start, counts: map[string]int{}}
}

// note records a tool call and returns the line to show now.
func (p *turnProgress) note(toolName string) string {
	return p.noteAt(toolName, time.Now())
}

func (p *turnProgress) noteAt(toolName string, now time.Time) string {
	phrase := strings.TrimSuffix(toolStatusLine(toolName), "…")
	if p.counts[phrase] == 0 {
		p.order = append(p.order, phrase)
	}
	p.counts[phrase]++
	parts := make([]string, 0, len(p.order))
	for _, ph := range p.order {
		if n := p.counts[ph]; n > 1 {
			parts = append(parts, fmt.Sprintf("%s ×%d", ph, n))
		} else {
			parts = append(parts, ph)
		}
	}
	elapsed := now.Sub(p.start).Round(time.Second)
	return fmt.Sprintf("⏳ %s · %s · so far: %s", phrase, elapsed, strings.Join(parts, ", "))
}

// friendlyToolPhrases covers the built-in dispatcher tools. An unlisted tool is
// not a bug — MCP catalogues are per-deployment — it just reads less warmly.
var friendlyToolPhrases = map[string]string{
	"memory_search":           "searching memory…",
	"memory_forget":           "forgetting a memory chunk…",
	"create_task":             "scheduling the job…",
	"list_tasks":              "checking your tasks…",
	"get_task_status":         "checking the job status…",
	"wait_for_task":           "waiting for the job…",
	"cancel_task":             "cancelling the job…",
	"retry_task":              "retrying the job…",
	"list_artifacts":          "looking through the results…",
	"read_artifact":           "reading the result…",
	"send_artifact":           "sending the file…",
	"send_email":              "sending the email…",
	"render_document":         "rendering the document…",
	"set_reminder":            "setting the reminder…",
	"cancel_reminder":         "cancelling the reminder…",
	"list_projects":           "looking at your projects…",
	"switch_project":          "switching project…",
	"query_api":               "calling an external service…",
	"list_apis":               "checking what services are available…",
	"web_submit":              "submitting the form…",
	"update_operator_profile": "updating your profile…",
	"report_problem":          "filing the bug report…",
}
