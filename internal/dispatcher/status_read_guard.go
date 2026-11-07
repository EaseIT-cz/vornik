package dispatcher

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/rs/zerolog"
)

// The status-read guard (GitHub #642; dispatcher-wait-for-data design,
// "Amendment 2026-11-04 (#642)"). A turn that polls a read-only status tool
// again and again learns nothing new: a task's state does not change within a
// turn worth waiting for, and the completion follow-up already tells the user
// when a task ends. On 2026-11-04 the dispatcher called list_tasks in runs of
// 21, 10, 9 and 10 and hit the iteration cap twice. Past the limits below, the
// call is not executed; the model gets a sentence telling it to answer.

// maxIdenticalStatusReads is how many times one status tool may run with the
// same arguments in one turn.
const maxIdenticalStatusReads = 2

// maxStatusReadsPerTool bounds one status tool's calls in a turn, whatever
// the arguments, so a model cannot poll by varying a filter.
const maxStatusReadsPerTool = 6

// statusReadTools are the read-only tools that report state the model might
// poll. A tool that changes state is never counted.
var statusReadTools = map[string]bool{
	"list_tasks":      true,
	"get_task_status": true,
	"list_executions": true,
	"list_reminders":  true,
}

// maxResearchCallsPerTurn bounds a turn's web and consult lookups
// (dispatcher turn-context design §7, GitHub #642): past it, the work belongs
// in a task. On 2026-11-04 one turn made about 30 web_fetch calls.
const maxResearchCallsPerTurn = 4

// researchToolParts are matched against an MCP tool's tool part only, never
// its server, so mcp__scraper__ical_events is not research (review 3cfe F1).
var researchToolParts = []string{"web_fetch", "web_search", "fetch_url", "browse", "scrape"}

// isResearchTool reports whether a tool is a web or consult lookup.
func isResearchTool(name string) bool {
	rest, ok := strings.CutPrefix(name, "mcp__")
	if !ok {
		return false
	}
	server, tool, found := strings.Cut(rest, "__")
	if !found {
		return false
	}
	if server == "consult" {
		return true
	}
	for _, part := range researchToolParts {
		if strings.Contains(tool, part) {
			return true
		}
	}
	return false
}

// statusReadGuard counts one turn's status reads and research calls. Not
// shared between turns. One guard, two classes: the one place that refuses
// tool calls inside a turn.
type statusReadGuard struct {
	identical map[string]int
	perTool   map[string]int
	research  int // never reset within a turn (review 3cfe F4)
}

func newStatusReadGuard() *statusReadGuard {
	return &statusReadGuard{identical: map[string]int{}, perTool: map[string]int{}}
}

// check records the call and reports whether it must be refused, with the
// text the model sees instead of the tool's answer.
func (g *statusReadGuard) check(tool, args string) (string, bool) {
	if g == nil {
		return "", false
	}
	if isResearchTool(tool) {
		g.research++
		if g.research <= maxResearchCallsPerTurn {
			return "", false
		}
		return fmt.Sprintf("Not run: this turn already made %d web or consult lookups. Work that needs more research "+
			"belongs in a task: call create_task with the user's request (in a project that has a research workflow), "+
			"tell the user what you scheduled, and stop.", maxResearchCallsPerTurn), true
	}
	if !statusReadTools[tool] {
		// A tool that may change state makes the next identical read a new
		// question: its answer can differ (review 0c05 F5). The per-tool
		// total still holds.
		g.identical = map[string]int{}
		return "", false
	}
	key := tool + "\x00" + canonicalArgs(args)
	g.identical[key]++
	g.perTool[tool]++
	if g.identical[key] <= maxIdenticalStatusReads && g.perTool[tool] <= maxStatusReadsPerTool {
		return "", false
	}
	return fmt.Sprintf("Not run: %s was already called %d times this turn, and its answer will not change "+
		"before you reply. I am not polling it again. If the user is waiting for a task, the completion "+
		"follow-up will tell them when it ends. Answer the user now with what you already know.",
		tool, g.perTool[tool]-1), true
}

// canonicalArgs renders JSON arguments with sorted keys and no whitespace,
// so key order or spacing cannot make one read look like another (review
// 0c05 F7). Arguments that are not a JSON object are compared as written,
// whitespace removed.
func canonicalArgs(args string) string {
	var v map[string]any
	if err := json.Unmarshal([]byte(args), &v); err == nil {
		if b, err := json.Marshal(v); err == nil {
			return string(b)
		}
	}
	return strings.Join(strings.Fields(args), "")
}

// logRefusedCall logs a refusal with a line per class, so the journal and the
// post-deploy audit can tell a status poll from a research budget (#642).
func logRefusedCall(logger zerolog.Logger, g *statusReadGuard, tool string) {
	if isResearchTool(tool) {
		logger.Warn().Str("tool", tool).Int("research_calls_this_turn", g.research).Msg("dispatcher: research budget reached")
		return
	}
	logger.Warn().Str("tool", tool).Int("calls_this_turn", g.perTool[tool]).Msg("dispatcher: repeated status read refused")
}
