package dispatcher

import (
	"fmt"
	"unicode/utf8"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/textutil"
)

// Tool-result compaction on send (dispatcher turn context design §6, GitHub
// #646). Every tool result of a turn used to be resent in full on every later
// call, and earlier turns' results rode along in history: 97% of the stored
// Telegram history bytes were tool results, and a call averaged 207 KB.
const (
	// compactKeepBatches is how many of the current turn's newest tool-call
	// batches are sent whole.
	compactKeepBatches = 2
	// compactOverRunes is the size above which an older tool result is
	// compacted. Refusals and errors are far below it.
	compactOverRunes = 6000
	// compactHeadRunes is how much of a compacted result is kept.
	compactHeadRunes = 1600
)

// compactionStat says what compactTurnToolResults shortened, for the log line
// that measures how often a shortened result is fetched again.
type compactionStat struct {
	results      int
	runesRemoved int
	tools        []string
}

// compactTurnToolResults returns a copy of msgs, the list one model call will
// be sent, in which large tool results that the model has already had are cut
// to their head plus a marker. turnStart is the index of the user message that
// opened the current turn: every result before it is a candidate, and in the
// turn the newest compactKeepBatches batches stay whole. A batch is an
// assistant message with tool calls plus the tool results that follow it.
// Only Content changes; msgs itself is never modified.
func compactTurnToolResults(msgs []chat.Message, turnStart int) ([]chat.Message, compactionStat) {
	var stat compactionStat
	if turnStart < 0 || turnStart > len(msgs) {
		turnStart = 0
	}
	// keepFrom is the index of the oldest batch the turn keeps whole.
	keepFrom := len(msgs)
	batches := 0
	for i := len(msgs) - 1; i >= turnStart && batches < compactKeepBatches; i-- {
		if msgs[i].Role == "assistant" && len(msgs[i].ToolCalls) > 0 {
			batches++
			keepFrom = i
		}
	}
	if batches < compactKeepBatches {
		// The turn has no batch old enough to compact; only history can be.
		keepFrom = turnStart
	}
	out := make([]chat.Message, len(msgs))
	copy(out, msgs)
	for i := 0; i < keepFrom; i++ {
		m := out[i]
		if m.Role != "tool" {
			continue
		}
		n := utf8.RuneCountInString(m.Content)
		if n <= compactOverRunes {
			continue
		}
		name := m.Name
		if name == "" {
			name = "a tool"
		}
		head := textutil.TruncateRunes(m.Content, compactHeadRunes)
		out[i].Content = fmt.Sprintf("[earlier tool result shortened: %s returned %d characters; showing the first %d. "+
			"Call the tool again if you need the rest.]\n\n%s", name, n, compactHeadRunes, head)
		stat.results++
		stat.runesRemoved += n - compactHeadRunes
		stat.tools = append(stat.tools, m.Name)
	}
	return out, stat
}

// turnStartIndex is the index of the last user message in msgs, the message
// that opens the turn about to run; 0 when there is none, so the whole list
// counts as the current turn.
func turnStartIndex(msgs []chat.Message) int {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return i
		}
	}
	return 0
}
