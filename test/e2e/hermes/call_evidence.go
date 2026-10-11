package hermes

import (
	"encoding/json"
	"io"
	"strings"
	"sync"
)

// callEvidence records model-originated calls and their native-ID-linked
// outputs received at the model endpoint. Offered schemas and echoed
// assistant history never originate a call. Reset scopes IDs to one CLI turn.
type nativeCall struct{ ID, Namespace, Name, Args, Output string }
type callEvidence struct {
	mu      sync.Mutex
	emitted map[string]nativeCall
	linked  []nativeCall
}

func (e *callEvidence) reset() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.emitted = map[string]nativeCall{}
	e.linked = nil
}
func (e *callEvidence) emit(c nativeCall) {
	if c.ID == "" || c.Name == "" || !json.Valid([]byte(c.Args)) {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.emitted == nil {
		e.emitted = map[string]nativeCall{}
	}
	e.emitted[c.ID] = c
}
func (e *callEvidence) received(id, out string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if c, ok := e.emitted[id]; ok {
		c.Output = out
		e.linked = append(e.linked, c)
	}
}
func (e *callEvidence) pairs() []nativeCall {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]nativeCall(nil), e.linked...)
}
func (e *callEvidence) chatInput(body []byte) {
	var req struct {
		Messages []struct {
			Role    string          `json:"role"`
			ID      string          `json:"tool_call_id"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &req) != nil {
		return
	}
	for _, m := range req.Messages {
		if m.Role == "tool" {
			e.received(m.ID, contentText(m.Content))
		}
	}
}
func (e *callEvidence) chatMessage(msg map[string]any) {
	b, _ := json.Marshal(msg)
	var m struct {
		Calls []struct {
			ID       string                           `json:"id"`
			Function struct{ Name, Arguments string } `json:"function"`
		} `json:"tool_calls"`
	}
	if json.Unmarshal(b, &m) != nil {
		return
	}
	for _, c := range m.Calls {
		e.emit(nativeCall{ID: c.ID, Name: c.Function.Name, Args: c.Function.Arguments})
	}
}
func (e *callEvidence) responseInput(input []map[string]any) {
	for _, it := range input {
		if it["type"] == "function_call_output" {
			id, _ := it["call_id"].(string)
			out, _ := it["output"].(string)
			if out == "" {
				b, _ := json.Marshal(it["output"])
				out = string(b)
			}
			e.received(id, out)
		}
	}
}
func (e *callEvidence) responseItem(it map[string]any) {
	if it["type"] != "function_call" {
		return
	}
	id, _ := it["call_id"].(string)
	ns, _ := it["namespace"].(string)
	name, _ := it["name"].(string)
	args, _ := it["arguments"].(string)
	e.emit(nativeCall{ID: id, Namespace: ns, Name: name, Args: args})
}

// modelCallParser observes bytes without buffering their delivery. Evidence
// is committed only after a bounded, well-formed response ends; parser errors
// discard evidence and never affect the response returned to Hermes.
type modelCallParser struct {
	e        *callEvidence
	stream   bool
	buf      []byte
	total    int
	bad      bool
	calls    map[[2]int]nativeCall
	complete []nativeCall
}

func newModelCallParser(e *callEvidence, stream bool) *modelCallParser {
	return &modelCallParser{e: e, stream: stream, calls: map[[2]int]nativeCall{}}
}
func (p *modelCallParser) feed(b []byte) {
	p.total += len(b)
	if p.total > 8<<20 {
		p.bad = true
		p.buf = nil
		return
	}
	if p.bad {
		return
	}
	p.buf = append(p.buf, b...)
	if !p.stream {
		return
	}
	for {
		idx := strings.IndexByte(string(p.buf), '\n')
		if idx < 0 {
			return
		}
		line := strings.TrimSpace(string(p.buf[:idx]))
		p.buf = p.buf[idx+1:]
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			continue
		}
		p.record([]byte(data), true)
	}
}
func (p *modelCallParser) record(b []byte, delta bool) {
	var resp struct {
		Choices []struct {
			Index   int             `json:"index"`
			Finish  string          `json:"finish_reason"`
			Message json.RawMessage `json:"message"`
			Delta   struct {
				Calls []struct {
					Index    int                              `json:"index"`
					ID       string                           `json:"id"`
					Function struct{ Name, Arguments string } `json:"function"`
				} `json:"tool_calls"`
			} `json:"delta"`
		} `json:"choices"`
	}
	if json.Unmarshal(b, &resp) != nil {
		p.bad = true
		return
	}
	for _, ch := range resp.Choices {
		if !delta {
			var msg map[string]any
			if json.Unmarshal(ch.Message, &msg) != nil {
				continue
			}
			temp := &callEvidence{}
			temp.chatMessage(msg)
			for _, c := range temp.emitted {
				p.complete = append(p.complete, c)
			}
			continue
		}
		for _, d := range ch.Delta.Calls {
			k := [2]int{ch.Index, d.Index}
			c := p.calls[k]
			c.ID += d.ID
			c.Name += d.Function.Name
			c.Args += d.Function.Arguments
			p.calls[k] = c
		}
		if ch.Finish == "tool_calls" {
			p.finishChoice(ch.Index, ch.Message)
		}
	}
}

func (p *modelCallParser) finishChoice(index int, message json.RawMessage) {
	// Some servers stream a completed message instead of deltas.
	// It has the same model-response provenance and native call IDs.
	var msg map[string]any
	temp := &callEvidence{}
	if json.Unmarshal(message, &msg) == nil {
		temp.chatMessage(msg)
	}
	for _, c := range temp.emitted {
		p.complete = append(p.complete, c)
	}
	for k, c := range p.calls {
		if k[0] != index {
			continue
		}
		if len(temp.emitted) == 0 {
			p.complete = append(p.complete, c)
		}
		delete(p.calls, k)
	}
}

func (p *modelCallParser) end() {
	if !p.stream {
		p.record(p.buf, false)
	} else if len(strings.TrimSpace(string(p.buf))) > 0 || len(p.calls) > 0 {
		p.bad = true
	}
	if p.bad {
		return
	}
	for _, c := range p.complete {
		p.e.emit(c)
	}
}

type observingModelBody struct {
	io.ReadCloser
	p    *modelCallParser
	done bool
}

func (b *observingModelBody) Read(dst []byte) (int, error) {
	n, err := b.ReadCloser.Read(dst)
	b.p.feed(dst[:n])
	if err == io.EOF && !b.done {
		b.done = true
		b.p.end()
	}
	return n, err
}
