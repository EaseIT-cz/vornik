// Package agentbridge is the stdio-to-HTTP MCP bridge an agent harness runs
// as `vornikctl agent mcp-bridge` (agent-administered Vornik plan P6.1). The
// harness speaks newline-delimited JSON-RPC on the bridge's stdin/stdout;
// the bridge relays each message to the daemon's companion endpoint with
// the namespace's admin key, which it alone reads from a 0600 file. The key
// is never written to stdout or stderr. The daemon spawns nothing: the
// harness runs the bridge on its own side.
package agentbridge

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"vornik.io/vornik/internal/agentns"
)

// Config is one bridge's connection.
type Config struct {
	Endpoint string // the companion endpoint URL
	Key      string
	HTTP     *http.Client
	// PollEvery is how often the bridge lists the tools itself to notice a
	// change (agent-administered design §18.14); zero is DefaultPollEvery.
	PollEvery time.Duration
	// pollHook, when set, runs at the start of every poll: a test seam for a
	// poll defect, fixed before Run starts (review 20261003-195e F3).
	pollHook func()
}

// currentUID is os.Getuid; a test seam for the key file's owner check.
var currentUID = os.Getuid

// maxMessage bounds one JSON-RPC message.
const maxMessage = 16 << 20

// Run relays until stdin ends. A broken stdin is a clean exit.
func Run(ctx context.Context, in io.Reader, out, errw io.Writer, cfg Config) error {
	hc := cfg.HTTP
	if hc == nil {
		hc = &http.Client{Timeout: 10 * time.Minute}
	}
	b := &bridge{cfg: cfg, out: out, errw: errw}
	pollCtx, stopPoll := context.WithCancel(ctx)
	var polling sync.WaitGroup
	// The poll stops with the relay and is waited for, so nothing is
	// written after Run returns.
	defer func() {
		stopPoll()
		polling.Wait()
	}()
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64<<10), maxMessage)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var msg struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params struct {
				Cursor string `json:"cursor"`
			} `json:"params"`
		}
		if json.Unmarshal(line, &msg) != nil {
			b.writeError(json.RawMessage("null"), -32700, "parse error")
			continue
		}
		// The harness's own tool list, every page of it, seeds the hash the
		// poll compares against (design §18.14 round 2 F5; review 195e F2).
		harnessList := msg.Method == "tools/list"
		answered := false
		emit := func(r []byte) {
			c, ok := b.write(r)
			if !ok {
				// Dropped, so stdout stays protocol; said on stderr without
				// the content (review 20261002-00bc).
				b.logf("vornik bridge: dropped a reply that is not JSON")
				return
			}
			if sameID(c, msg.ID) {
				answered = true
				if harnessList {
					b.seenPage(c, msg.Params.Cursor)
				}
			}
			// After the initialize result is relayed, and only then, the
			// poll starts (design §18.14 round 2 F3).
			if v := negotiatedVersion(c); v != "" && b.initialized(v) {
				polling.Add(1)
				go func() {
					defer polling.Done()
					b.pollLoop(pollCtx, newPollClient())
				}()
			}
		}
		session, version := b.sessionHeaders()
		sess, err := post(ctx, hc, cfg, session, version, line, emit)
		b.setSession(sess)
		notification := len(msg.ID) == 0 || string(msg.ID) == "null"
		if err != nil {
			b.logf("vornik bridge: %v", err)
			// What already arrived was relayed; an error line is added only
			// when the request has no answer yet (review 20261002-3b9f F1).
			if !notification && !answered {
				b.writeError(msg.ID, -32000, "vornik: "+err.Error())
			}
		}
	}
	if err := sc.Err(); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("read stdin: %w", err)
	}
	return nil
}

// sameID reports whether a JSON-RPC message answers the request id.
func sameID(msg []byte, id json.RawMessage) bool {
	if len(id) == 0 {
		return false
	}
	var m struct {
		ID json.RawMessage `json:"id"`
	}
	return json.Unmarshal(msg, &m) == nil && bytes.Equal(bytes.TrimSpace(m.ID), bytes.TrimSpace(id))
}

// post sends one message and calls emit for each JSON-RPC message of the
// reply, as it arrives for an SSE reply (none for 202/204). It returns the
// session ID the server set.
func post(ctx context.Context, hc *http.Client, cfg Config, session, version string, body []byte, emit func([]byte)) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.Endpoint, bytes.NewReader(body))
	if err != nil {
		return "", errors.New("bad endpoint")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+cfg.Key)
	if session != "" {
		req.Header.Set("Mcp-Session-Id", session)
	}
	if version != "" {
		req.Header.Set("MCP-Protocol-Version", version)
	}
	resp, err := hc.Do(req)
	if err != nil {
		return "", errors.New("the daemon is not reachable")
	}
	defer func() { _ = resp.Body.Close() }()
	sess := resp.Header.Get("Mcp-Session-Id")
	if resp.StatusCode == http.StatusUnauthorized {
		return sess, errors.New("HTTP 401: this connection's key is not valid (disconnected?); run vornikctl agent connect again")
	}
	if resp.StatusCode >= 300 {
		return sess, fmt.Errorf("HTTP %d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
	}
	if strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		return sess, streamSSE(resp.Body, emit)
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxMessage))
	if err != nil {
		return sess, errors.New("the reply was cut off")
	}
	if len(bytes.TrimSpace(raw)) > 0 {
		emit(raw)
	}
	return sess, nil
}

// streamSSE emits each event's data as soon as the event ends. An event
// still open when the stream breaks is emitted too, then the error is
// returned.
func streamSSE(body io.Reader, emit func([]byte)) error {
	r := bufio.NewReaderSize(body, 64<<10)
	var cur []string
	size := 0
	flush := func() {
		if len(cur) > 0 {
			emit([]byte(strings.Join(cur, "\n")))
			cur, size = nil, 0
		}
	}
	for {
		line, err := r.ReadString('\n')
		if line != "" {
			l := strings.TrimRight(line, "\r\n")
			switch {
			case l == "" && strings.HasSuffix(line, "\n"):
				flush()
			case strings.HasPrefix(l, "data:"):
				d := strings.TrimPrefix(strings.TrimPrefix(l, "data:"), " ")
				if size += len(d); size > maxMessage {
					return errors.New("an event is too large")
				}
				cur = append(cur, d)
			}
		}
		if err != nil {
			flush()
			if errors.Is(err, io.EOF) {
				return nil
			}
			return errors.New("the reply was cut off")
		}
	}
}

// negotiatedVersion is the protocolVersion of an initialize result, which
// every later request carries as MCP-Protocol-Version (review 6f6b F5).
func negotiatedVersion(msg []byte) string {
	var m struct {
		Result struct {
			ProtocolVersion string `json:"protocolVersion"`
		} `json:"result"`
	}
	if json.Unmarshal(msg, &m) != nil {
		return ""
	}
	return m.Result.ProtocolVersion
}

func (b *bridge) writeError(id json.RawMessage, code int, message string) {
	raw, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": id,
		"error": map[string]any{"code": code, "message": message}})
	_, _ = b.write(raw)
}

// LoadKey reads a namespace's key file. It must be a regular file the
// caller owns, readable by no one else.
func LoadKey(path string) (string, error) {
	// Review 6f6b F11: a symlink is refused, and the checks run on the
	// opened descriptor, so a swap between check and read cannot redirect
	// the bridge to another file.
	if li, err := os.Lstat(path); errors.Is(err, fs.ErrPermission) {
		// The directory belongs to someone else (a harness run as a
		// service user): reconnecting would not help (DoD lane bring-up,
		// 2026-10-02).
		return "", fmt.Errorf("cannot reach %s: permission denied; run vornikctl agent connect as the user the harness runs as", path)
	} else if err != nil {
		return "", fmt.Errorf("no key at %s; run vornikctl agent connect first", path)
	} else if li.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("%s is a symlink; run vornikctl agent connect again", path)
	}
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, fs.ErrPermission) {
		return "", fmt.Errorf("cannot read %s: permission denied; run vornikctl agent connect as the user the harness runs as", path)
	}
	if err != nil {
		return "", fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return "", fmt.Errorf("stat %s: %w", path, err)
	}
	if !fi.Mode().IsRegular() || fi.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s must be a file readable by you only (chmod 600)", path)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != currentUID() {
		// Plan P6 amendment F9: a harness run as a service user.
		return "", fmt.Errorf("%s belongs to another user; run vornikctl agent connect as the user the harness runs as", path)
	}
	b, err := io.ReadAll(io.LimitReader(f, 4096))
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	k := strings.TrimSpace(string(b))
	if k == "" {
		return "", fmt.Errorf("%s is empty", path)
	}
	return k, nil
}

// KeyPath is the namespace's key file under configDir (os.UserConfigDir):
// <configDir>/vornik/agents/<ns>.key. A namespace that is not valid is
// refused, so it cannot name another path.
func KeyPath(configDir, ns string) (string, error) {
	if !agentns.Valid(ns) {
		return "", fmt.Errorf("%q is not a namespace (2 to 16 lowercase letters and digits, starting with a letter)", ns)
	}
	return filepath.Join(configDir, "vornik", "agents", ns+".key"), nil
}

// CheckEndpoint refuses a URL the key would cross the network in cleartext
// to: https anywhere, plain http only to this machine.
func CheckEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return fmt.Errorf("%q is not a URL", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		h := u.Hostname()
		if h == "localhost" {
			return nil
		}
		if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() {
			return nil
		}
		return fmt.Errorf("%s is plain http to another machine; the key would cross the network in cleartext (use https)", raw)
	}
	return fmt.Errorf("%q is not an http(s) URL", raw)
}
