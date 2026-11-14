package ui

import (
	"strings"

	"vornik.io/vornik/internal/chat"
	"vornik.io/vornik/internal/dispatcher"
)

// chatExchange is one question and its answer as the web chat shows it
// (web-chat-design.md §2.1): the person's message, a one-line summary of the
// tools the assistant used, the interim sentences it wrote between them, and
// the reply. The model's transcript keeps more (tool results, tool-call-only
// messages); the page never shows those.
type chatExchange struct {
	User    string
	Steps   string
	Interim []chatInterim
	Reply   string
	Mark    string // the reply's Art 50(2) mark, when it was marked this turn
}

// chatInterim is a sentence the assistant wrote between its tool calls, with
// its own mark when this turn delivered it (streaming does; §3.2).
type chatInterim struct {
	Text, Mark string
}

// buildChatExchanges folds a stored history into exchanges. A user message
// opens an exchange; an assistant message with tool calls adds its tools to
// the steps (and its text, if any, to the interim sentences); an assistant
// message without tool calls is the reply (a later one moves the earlier to
// the interim sentences); tool and system messages are skipped. Assistant
// text before any user message (the AI disclosure notice) is an exchange of
// its own with no user message. marks is keyed by history index.
func buildChatExchanges(history []chat.Message, marks map[int]string) []chatExchange {
	var out []chatExchange
	var tools []string
	cur := -1
	flush := func() {
		if cur >= 0 {
			out[cur].Steps = dispatcher.StepsSummary(tools)
		}
		tools = nil
	}
	for i, m := range history {
		switch m.Role {
		case "user":
			flush()
			out = append(out, chatExchange{User: messageText(m)})
			cur = len(out) - 1
		case "assistant":
			if cur < 0 {
				out = append(out, chatExchange{})
				cur = len(out) - 1
			}
			text := strings.TrimSpace(m.Content)
			if len(m.ToolCalls) > 0 {
				for _, tc := range m.ToolCalls {
					tools = append(tools, tc.Function.Name)
				}
				if text != "" {
					out[cur].Interim = append(out[cur].Interim, chatInterim{Text: text, Mark: marks[i]})
				}
				continue
			}
			if text == "" {
				continue
			}
			if out[cur].Reply != "" {
				out[cur].Interim = append(out[cur].Interim, chatInterim{Text: out[cur].Reply, Mark: out[cur].Mark})
			}
			out[cur].Reply = m.Content
			out[cur].Mark = marks[i]
		}
	}
	flush()
	return out
}

// messageText is a message's text: Content, or its text blocks when the
// message carries blocks (an image upload with a caption).
func messageText(m chat.Message) string {
	if m.Content != "" || len(m.Blocks) == 0 {
		return m.Content
	}
	var parts []string
	for _, b := range m.Blocks {
		if b.Text != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n")
}
