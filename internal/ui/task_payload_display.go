package ui

import (
	"encoding/json"
	"fmt"
	"unicode/utf8"
)

// brokerInputDisplayBytes is how much of one broker input the task page
// shows. A document input may be 256 KiB (broker design §18.1); the full
// text stays in the task row and in the step's staged file.
const brokerInputDisplayBytes = 1024

// displayTaskPayload returns the payload to show as the task page's "Raw
// payload": unchanged, except that each string in context.broker_inputs
// longer than brokerInputDisplayBytes is cut to its head and a note of its
// size (review 20261003-6fec item 3). A payload that is not a JSON object,
// or has no broker inputs, is returned as is.
func displayTaskPayload(raw []byte) []byte {
	var p map[string]any
	if json.Unmarshal(raw, &p) != nil {
		return raw
	}
	ctx, _ := p["context"].(map[string]any)
	inputs, _ := ctx["broker_inputs"].(map[string]any)
	changed := false
	for k, v := range inputs {
		s, ok := v.(string)
		if !ok || len(s) <= brokerInputDisplayBytes {
			continue
		}
		head := s[:brokerInputDisplayBytes]
		for len(head) > 0 && !utf8.ValidString(head) {
			head = head[:len(head)-1]
		}
		inputs[k] = fmt.Sprintf("%s\n[... %d bytes, shown %d]", head, len(s), len(head))
		changed = true
	}
	if !changed {
		return raw
	}
	out, err := json.Marshal(p)
	if err != nil {
		return raw
	}
	return out
}
