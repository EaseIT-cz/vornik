package hermes

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
)

// StdioClient speaks MCP over newline-delimited JSON-RPC, as Claude Desktop
// does to a stdio server: the lane's stand-in for it (agent-administered
// Vornik plan P8.2). It keeps a transcript of every line sent and read, for
// the canary sweep.
type StdioClient struct {
	in     io.Writer
	out    *bufio.Scanner
	mu     sync.Mutex
	nextID int
	log    bytes.Buffer
}

// NewStdioClient talks to a server whose stdin is in and stdout is out.
func NewStdioClient(in io.Writer, out io.Reader) *StdioClient {
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 64<<10), 16<<20)
	return &StdioClient{in: in, out: sc}
}

// Transcript is every line sent and received, in order.
func (c *StdioClient) Transcript() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.log.String()
}

// Notify sends a notification.
func (c *StdioClient) Notify(method string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.send(map[string]any{"jsonrpc": "2.0", "method": method})
}

func (c *StdioClient) send(msg any) error {
	b, err := json.Marshal(msg)
	if err != nil {
		return err
	}
	c.log.Write(append([]byte("> "), append(b, '\n')...))
	_, err = c.in.Write(append(b, '\n'))
	return err
}

// Call sends a request and returns its result, skipping server
// notifications; a JSON-RPC error is an error.
func (c *StdioClient) Call(method string, params any) (json.RawMessage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	id := c.nextID
	if err := c.send(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return nil, err
	}
	for c.out.Scan() {
		line := c.out.Bytes()
		c.log.Write(append([]byte("< "), append(append([]byte{}, line...), '\n')...))
		var resp struct {
			ID     *int            `json:"id"`
			Result json.RawMessage `json:"result"`
			Error  *struct {
				Code    int    `json:"code"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(line, &resp) != nil || resp.ID == nil || *resp.ID != id {
			continue // a notification, or not ours
		}
		if resp.Error != nil {
			return nil, fmt.Errorf("%s: JSON-RPC %d: %s", method, resp.Error.Code, resp.Error.Message)
		}
		return resp.Result, nil
	}
	if err := c.out.Err(); err != nil {
		return nil, err
	}
	return nil, errors.New("the server closed its output")
}

// Tool calls a tool and returns its text and whether it is a tool error.
func (c *StdioClient) Tool(name string, args any) (string, bool, error) {
	if args == nil {
		args = map[string]any{}
	}
	raw, err := c.Call("tools/call", map[string]any{"name": name, "arguments": args})
	if err != nil {
		return "", false, err
	}
	var res struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := json.Unmarshal(raw, &res); err != nil {
		return "", false, err
	}
	var text string
	for _, c := range res.Content {
		text += c.Text
	}
	return text, res.IsError, nil
}
