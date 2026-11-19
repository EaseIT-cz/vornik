package agentloop

import (
	"regexp"
	"strconv"

	"vornik.io/vornik/internal/codespan"
)

// GitHub #612 (agent tool dispatch design, amendment 2026-11-17): a write
// argument that holds a tool call's closing frame carries the rest of the
// model's turn, merged into it by the provider's parser or written there by
// the model. The two tags are built from halves so this file holds no frame.
var mergedFrameRE = regexp.MustCompile(
	`<` + `/parameter>\s*<` + `/invoke>` + `|` + `<` + `/arg_value>\s*<` + `/tool_call>`)

// mergedToolCallFrame returns the first closing frame in text that lies
// outside fenced and inline code, and its byte offset; "" when there is none.
// A quoted frame is a document about one, not a merged turn.
func mergedToolCallFrame(text string) (string, int) {
	ms := mergedFrameRE.FindAllStringIndex(text, -1)
	if len(ms) == 0 {
		return "", 0
	}
	spans := codespan.Spans(text)
	for _, m := range ms {
		if !codespan.Inside(spans, m[0]) {
			return text[m[0]:m[1]], m[0]
		}
	}
	return "", 0
}

// mergedToolCallRefusal is the answer to a write that carries a frame, or "".
func mergedToolCallRefusal(content string) string {
	frame, at := mergedToolCallFrame(content)
	if frame == "" {
		return ""
	}
	return "ERROR: the content contains the end of a tool call (" + strconv.Quote(frame) + " at byte " + strconv.Itoa(at) +
		"), so the rest of the model's turn was merged into it; write the document again without it"
}
