package hermes

import (
	"io"
	"strings"
	"testing"
)

func TestCallEvidence_RequiresEmittedNativeCall(t *testing.T) {
	e := &callEvidence{}
	call := nativeCall{ID: "call_0", Name: "status", Args: `{"task_id":"held"}`}
	e.received("call_0", `{"progress":{"step_id":"sum"}}`)
	if len(e.pairs()) != 0 {
		t.Fatal("unlinked output accepted")
	}
	e.emit(call)
	e.received("wrong", `{"progress":{"step_id":"sum"}}`)
	if len(e.pairs()) != 0 {
		t.Fatal("wrong native ID accepted")
	}
	e.received("call_0", `{"progress":{"step_id":"sum"}}`)
	if got := e.pairs(); len(got) != 1 || got[0].Name != "status" {
		t.Fatalf("linked result missing: %v", got)
	}
	e.reset()
	e.received("call_0", `{"progress":{"step_id":"sum"}}`)
	if len(e.pairs()) != 0 {
		t.Fatal("call_0 from prior CLI invocation linked")
	}
}

func TestModelCallParser_ResponseProvenance(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		stream     bool
		want       int
	}{
		{"json", `{"choices":[{"message":{"tool_calls":[{"id":"c1","function":{"name":"vornik_delegate","arguments":"{\"workflow\":\"mail-digest\"}"}}]}}]}`, false, 1},
		{"stream", "data: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"c1\",\"function\":{\"name\":\"vornik_delegate\",\"arguments\":\"{\\\"workflow\\\":\"}}]}}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"mail-digest\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n", true, 1},
		{"incomplete", "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"id\":\"c1\",\"function\":{\"name\":\"vornik_delegate\",\"arguments\":\"{}\"}}]}}]}\n\n", true, 0},
		{"prose", `{"choices":[{"message":{"content":"I called vornik_delegate with call c1"}}]}`, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &callEvidence{}
			p := newModelCallParser(e, tc.stream)
			for _, b := range []byte(tc.body) {
				p.feed([]byte{b})
			}
			p.end()
			e.received("c1", `{"task_id":"task_1","reused":true}`)
			if got := len(e.pairs()); got != tc.want {
				t.Fatalf("got %d pairs, want %d", got, tc.want)
			}
		})
	}
}

func TestCallEvidence_RejectsForgedHistoryAndWrongResponse(t *testing.T) {
	e := &callEvidence{}
	e.chatInput([]byte(`{"tools":[{"function":{"name":"status"}}],"messages":[{"role":"assistant","tool_calls":[{"id":"fake","function":{"name":"status","arguments":"{}"}}]},{"role":"tool","tool_call_id":"fake","content":"{\"progress\":{}}"}]}`))
	if len(e.pairs()) != 0 {
		t.Fatal("offered schema or echoed client history originated evidence")
	}
	e.emit(nativeCall{ID: "real", Name: "delegate", Args: `{"max_age_seconds":600}`})
	e.responseInput([]map[string]any{{"type": "function_call_output", "call_id": "fake", "output": `{"reused":true}`}})
	if len(e.pairs()) != 0 {
		t.Fatal("wrong response ID linked")
	}
}

func TestModelCallParser_InvalidAndOversizeDiscardEvidence(t *testing.T) {
	for _, body := range []string{"data: invalid\n\n", string(make([]byte, (8<<20)+1))} {
		e := &callEvidence{}
		p := newModelCallParser(e, true)
		p.feed([]byte(body))
		p.end()
		e.received("c1", "{}")
		if len(e.pairs()) != 0 {
			t.Fatal("invalid response accepted")
		}
	}
}

func TestObservingModelBody_PreservesRejectedResponse(t *testing.T) {
	original := "data: invalid\n\n"
	e := &callEvidence{}
	body := &observingModelBody{ReadCloser: io.NopCloser(strings.NewReader(original)), p: newModelCallParser(e, true)}
	got, err := io.ReadAll(body)
	if err != nil || string(got) != original {
		t.Fatalf("parser rejection changed forwarded body: %q, %v", got, err)
	}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	if len(e.pairs()) != 0 {
		t.Fatal("malformed forwarding created evidence")
	}
}

func TestModelCallParser_StreamingCompletedMessageFallback(t *testing.T) {
	e := &callEvidence{}
	p := newModelCallParser(e, true)
	p.feed([]byte("data: {\"choices\":[{\"index\":0,\"message\":{\"tool_calls\":[{\"id\":\"full1\",\"function\":{\"name\":\"vornik_delegate\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"))
	p.end()
	e.received("full1", `{"task_id":"task_1"}`)
	if len(e.pairs()) != 1 {
		t.Fatal("completed streaming message.tool_calls fallback not recorded")
	}
}
