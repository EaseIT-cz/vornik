package cli

// `vornikctl companion mcp-bridge | connect | disconnect | doctor`: a
// companion key in Claude Desktop and Cowork (25-claude-desktop-companion.md,
// GitHub #589). The agent-admin path (`vornikctl agent connect`) already had
// a stdio bridge, a key file only the user can read and a Claude Desktop
// config writer; these commands give a companion key the same, with the same
// helpers. The key never reaches stdout, stderr or the Claude config.

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/agentbridge"
	"vornik.io/vornik/internal/harnessconfig"
)

var (
	companionDeskName     string
	companionDeskURL      string
	companionDeskKeyStdin bool
	companionDoctorClient string
	// companionGenericPrint is connect generic's --print (design 25 §D2).
	companionGenericPrint bool
)

// companionDeskTimeout bounds each call connect and doctor make.
const companionDeskTimeout = 15 * time.Second

var companionMCPBridgeCmd = &cobra.Command{
	Use:   "mcp-bridge",
	Short: "Relay MCP over stdio to Vornik with a companion key (run by the assistant, not by you)",
	Long: `Relay newline-delimited MCP JSON-RPC between stdin/stdout and Vornik's
companion endpoint, authenticated with the companion key file
(<config dir>/vornik/companion/<name>.key, written by vornikctl companion connect).
VORNIK_COMPANION_TOKEN, when set, is used instead of the file (a manual run).

Claude Desktop runs this command from its MCP server entry. stdout carries the
protocol only; diagnostics go to stderr and never include the key.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return runCompanionMCPBridge(ctx, cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr())
	},
}

var companionConnectCmd = &cobra.Command{
	Use:   "connect <claude-desktop|cursor|opencode|generic>",
	Short: "Connect Claude Desktop (and Cowork), Cursor, opencode, or any other assistant, to Vornik with a companion key",
	Long: `Check a companion key against Vornik, then store it in a file only you can
read. For claude-desktop, also add one MCP server entry to Claude Desktop's
config that runs 'vornikctl companion mcp-bridge'. The Claude config never
holds the key.

For cursor, also install the Vornik plugin (MCP entry, an always-applied
rule, skills) into ~/.cursor/plugins/local/vornik-companion; its MCP entry
runs this binary's bridge, so no Cursor config holds the key. It refuses a
key with no default repo scope unless --allow-unscoped. Reload the Cursor
window after.

For opencode, also add a stdio entry that runs the bridge to opencode.json,
and install a plugin that adds Vornik's working rules and a session digest
to the system prompt, plus the companion skills (as skills/vornik-*), into
opencode's config directory ($XDG_CONFIG_HOME/opencode). A commented
opencode.json, or a plugin file that is not Vornik's, is refused before
anything is written. It refuses an unscoped key unless --allow-unscoped.

For any other assistant (Gemini CLI, ...), 'connect generic --print'
writes no assistant config. It prints the MCP entry to paste: a stdio form
that runs the bridge, and a streamable-HTTP form that reads the key from an
environment variable. It warns when the key has no default repo scope. The
key is never printed.

The key comes from --key-stdin, else VORNIK_COMPANION_TOKEN. connect does not
mint keys: an operator mints one with 'vornikctl companion grant'. An
agent-admin key is refused (use 'vornikctl agent connect').

Claude Code and Codex use their companion plugins instead.`,
	Example:      `  printf '%s\n' "$VORNIK_COMPANION_TOKEN" | vornikctl companion connect claude-desktop --key-stdin --url https://vornik.example`,
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runCompanionConnect(cmd.Context(), args[0], cmd.InOrStdin(), cmd.OutOrStdout())
	},
}

var companionDisconnectCmd = &cobra.Command{
	Use:          "disconnect <claude-desktop|cursor|opencode|generic>",
	Short:        "Remove the companion entry from Claude Desktop (if any) and delete its key file",
	Long:         `Remove what 'vornikctl companion connect' wrote. It revokes nothing on the daemon: the key belongs to the operator who minted it ('vornikctl companion keys').`,
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runCompanionDisconnect(args[0], cmd.OutOrStdout())
	},
}

var companionDoctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check a companion connection and print the fix for each failure",
	Long: `Check the key file, the daemon and the key itself, one line per check, each
failure with its fix; for claude-desktop, also the Claude Desktop entry. With
--client generic it also names the key the bridge uses and what it cannot see
(your assistant's own entry and environment). It changes nothing. Exit status
0 when every check passes, 1 otherwise.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		var failed int
		switch companionDoctorClient {
		case harnessconfig.ClaudeDesktop:
			failed = runCompanionDoctor(cmd.Context(), cmd.OutOrStdout())
		case harnessGeneric:
			failed = runCompanionDoctorGeneric(cmd.Context(), cmd.OutOrStdout())
		case harnessCursor:
			failed = runCompanionDoctorCursor(cmd.Context(), cmd.OutOrStdout())
		case harnessOpenCode:
			failed = runCompanionDoctorOpenCode(cmd.Context(), cmd.OutOrStdout())
		default:
			return fmt.Errorf("--client %q: the doctor checks claude-desktop, cursor, opencode, or generic", companionDoctorClient)
		}
		if failed > 0 {
			return fmt.Errorf("%d check(s) failed", failed)
		}
		return nil
	},
}

func init() {
	for _, c := range []*cobra.Command{companionMCPBridgeCmd, companionConnectCmd, companionDisconnectCmd, companionDoctorCmd} {
		c.Flags().StringVar(&companionDeskName, "name", "default", "The companion key's name (2 to 16 lowercase letters and digits)")
	}
	for _, c := range []*cobra.Command{companionMCPBridgeCmd, companionConnectCmd, companionDoctorCmd} {
		c.Flags().StringVar(&companionDeskURL, "url", "", "Vornik's URL (default $VORNIK_URL, then $VORNIK_API_URL, then "+DefaultAPIURL+")")
	}
	companionConnectCmd.Flags().BoolVar(&companionDeskKeyStdin, "key-stdin", false, "Read the companion key from standard input (piped)")
	companionConnectCmd.Flags().BoolVar(&companionGenericPrint, "print", false, "generic only: write no assistant config; print the MCP entry to paste")
	companionDoctorCmd.Flags().StringVar(&companionDoctorClient, "client", harnessconfig.ClaudeDesktop, "The client to check")
	companionCmd.AddCommand(companionMCPBridgeCmd, companionConnectCmd, companionDisconnectCmd, companionDoctorCmd)
}

// companionBaseURL is --url, then VORNIK_URL (the companion docs' variable),
// then VORNIK_API_URL, then the default.
func companionBaseURL() string {
	for _, u := range []string{companionDeskURL, os.Getenv("VORNIK_URL"), os.Getenv("VORNIK_API_URL")} {
		if u = strings.TrimSpace(u); u != "" {
			return strings.TrimRight(u, "/")
		}
	}
	return DefaultAPIURL
}

func companionKeyFile(name string) (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("no config directory: %w", err)
	}
	return agentbridge.CompanionKeyPath(dir, name)
}

// runCompanionMCPBridge resolves the key in the CLI (the environment variable
// for a manual run, else the file; a GUI app has no shell environment) and
// relays with the agent bridge's one implementation.
func runCompanionMCPBridge(ctx context.Context, in io.Reader, out, errw io.Writer) error {
	base := companionBaseURL()
	if err := agentbridge.CheckEndpoint(base); err != nil {
		return err
	}
	key := strings.TrimSpace(os.Getenv("VORNIK_COMPANION_TOKEN"))
	if key == "" {
		path, err := companionKeyFile(companionDeskName)
		if err != nil {
			return err
		}
		if key, err = agentbridge.LoadKey(path); err != nil {
			return err
		}
	}
	return agentbridge.Run(ctx, in, out, errw, agentbridge.Config{Endpoint: base + "/api/v1/mcp/companion", Key: key})
}

// companionIdentity is what connect and doctor learn from the endpoint.
type companionIdentity struct {
	Project   string
	RepoScope string // whoami's effective_repo_scope
	// DefaultScope is the key's own default_repo_scope: the only scope a
	// client with no SessionStart hook gives its notes (design 25 §D1).
	DefaultScope string
	// Instructions is what initialize returned: the working rules the daemon
	// sends every companion session.
	Instructions string
	Admin        bool // tools/list offers describe_installation: an agent-admin key
}

// errCompanionUnauthorized is a 401 from the companion endpoint.
var errCompanionUnauthorized = errors.New("the companion endpoint refused the key (401): it is wrong, revoked, or not a companion key")

// companionRPC posts one JSON-RPC call to the companion endpoint, keeping the
// session id the server returns.
type companionRPC struct {
	hc       *http.Client
	base     string
	endpoint string
	key      string
	session  string
}

func (c *companionRPC) call(ctx context.Context, method string, params any, out any) error {
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+c.key)
	if c.session != "" {
		req.Header.Set("Mcp-Session-Id", c.session)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return fmt.Errorf("cannot reach %s: %w", c.base, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if s := resp.Header.Get("Mcp-Session-Id"); s != "" {
		c.session = s
	}
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		return errCompanionUnauthorized
	case resp.StatusCode >= 300:
		return fmt.Errorf("%s answered %s", method, resp.Status)
	}
	var env struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("%s: not a JSON-RPC reply", method)
	}
	if env.Error != nil {
		return fmt.Errorf("%s: %s", method, env.Error.Message)
	}
	if out != nil {
		return json.Unmarshal(env.Result, out)
	}
	return nil
}

// probeCompanion runs initialize, tools/list and whoami against the endpoint
// with key. It is a client, not a relay: the bridge is agentbridge.Run.
func probeCompanion(ctx context.Context, base, key string) (companionIdentity, error) {
	var id companionIdentity
	rpc := &companionRPC{hc: &http.Client{Timeout: companionDeskTimeout}, base: base, endpoint: base + "/api/v1/mcp/companion", key: key}
	call := func(method string, params any, out any) error { return rpc.call(ctx, method, params, out) }
	var initRes struct {
		Instructions string `json:"instructions"`
	}
	if err := call("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "vornikctl-companion-connect", "version": "1"}}, &initRes); err != nil {
		return id, err
	}
	id.Instructions = initRes.Instructions
	var tools struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := call("tools/list", map[string]any{}, &tools); err != nil {
		return id, err
	}
	for _, t := range tools.Tools {
		if t.Name == "describe_installation" {
			// An agent-admin key: the caller refuses it, so whoami (and the
			// project it would name) is not needed.
			id.Admin = true
			return id, nil
		}
	}
	var res struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := call("tools/call", map[string]any{"name": "whoami", "arguments": map[string]any{}}, &res); err != nil {
		return id, err
	}
	// An unreadable whoami is an error: read as "no scope" it would warn a
	// healthy key (design 25 §D2, review a004 F3).
	var who struct {
		ProjectID    string `json:"project_id"`
		Scope        string `json:"effective_repo_scope"`
		DefaultScope string `json:"default_repo_scope"`
	}
	if len(res.Content) == 0 || json.Unmarshal([]byte(res.Content[0].Text), &who) != nil {
		return id, errors.New("whoami: the reply could not be read")
	}
	id.Project, id.RepoScope, id.DefaultScope = who.ProjectID, who.Scope, who.DefaultScope
	return id, nil
}

// readCompanionKey reads a piped key; a terminal is refused, a typed key would
// echo.
func readCompanionKey(r io.Reader) (string, error) {
	if r == nil || stdinIsTerminal(r) {
		return "", errors.New(`--key-stdin reads a piped key, and standard input is a terminal (a typed key would echo); pipe it instead: printf '%s\n' "$VORNIK_COMPANION_TOKEN" | vornikctl companion connect claude-desktop --key-stdin`)
	}
	line, err := bufio.NewReader(io.LimitReader(r, 4096)).ReadString('\n')
	if err != nil && !errors.Is(err, io.EOF) {
		return "", err
	}
	return strings.TrimSpace(line), nil
}

func companionEntry(name, base string) (harnessconfig.Entry, error) {
	exe, err := os.Executable()
	if err != nil {
		return harnessconfig.Entry{}, err
	}
	if exe, err = filepath.Abs(exe); err != nil {
		return harnessconfig.Entry{}, err
	}
	return harnessconfig.Entry{Kind: harnessconfig.KindCompanion, Namespace: name, Command: exe, URL: base}, nil
}

func requireConnectClient(client string) error {
	switch {
	case client != harnessconfig.ClaudeDesktop && client != harnessGeneric && client != harnessCursor && client != harnessOpenCode:
		return fmt.Errorf("%q: companion connect sets up claude-desktop, cursor, opencode, or generic (any other assistant); Claude Code and Codex use their companion plugins (contrib/claude-code-companion, contrib/codex-companion)", client)
	case client == harnessGeneric && !companionGenericPrint:
		return errors.New("connect generic writes no assistant config; pass --print to store the key and print the MCP entry to paste")
	case client != harnessGeneric && companionGenericPrint:
		return fmt.Errorf("--print is for generic (vornikctl companion connect generic --print); connect %s writes its own config", client)
	}
	return nil
}

func runCompanionConnect(ctx context.Context, client string, in io.Reader, out io.Writer) error {
	if err := requireConnectClient(client); err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	key := ""
	var err error
	if companionDeskKeyStdin {
		if key, err = readCompanionKey(in); err != nil {
			return err
		}
	} else {
		key = strings.TrimSpace(os.Getenv("VORNIK_COMPANION_TOKEN"))
	}
	if key == "" {
		return errors.New("no key: pipe it with --key-stdin, or set VORNIK_COMPANION_TOKEN (an operator mints one with vornikctl companion grant)")
	}
	base := companionBaseURL()
	if err := agentbridge.CheckEndpoint(base); err != nil {
		return err
	}
	keyPath, err := companionKeyFile(companionDeskName)
	if err != nil {
		return err
	}
	entry, err := companionEntry(companionDeskName, base)
	if err != nil {
		return err
	}
	// Check the key before writing anything, so a refusal leaves nothing.
	id, err := probeCompanion(ctx, base, key)
	if err != nil {
		return err
	}
	if id.Admin {
		return errors.New("this is an agent-admin key; use vornikctl agent connect claude-desktop")
	}
	if client == harnessCursor || client == harnessOpenCode {
		if err := requireCursorScope(id); err != nil {
			return err
		}
	}
	var ocCfg, ocDir string
	if client == harnessOpenCode {
		if ocCfg, ocDir, err = opencodeDir(); err != nil {
			return err
		}
		if err := preflightOpenCode(ocCfg, ocDir, entry); err != nil {
			return err
		}
	}
	if err := agentDir(filepath.Dir(keyPath), os.Getuid()); err != nil {
		return err
	}
	if err := writePrivate(keyPath, []byte(key+"\n")); err != nil {
		return err
	}
	if client == harnessGeneric {
		printGenericEntry(out, keyPath, entry, id)
		return nil
	}
	if client == harnessCursor {
		return finishConnectCursor(out, keyPath, entry, id)
	}
	if client == harnessOpenCode {
		return finishConnectOpenCode(out, keyPath, ocCfg, ocDir, entry, id)
	}
	cfgPath, err := harnessconfig.Path(harnessconfig.ClaudeDesktop, userEnv().harnessEnv())
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o700); err != nil {
		return err
	}
	if _, err := harnessconfig.Apply(cfgPath, func(b []byte) ([]byte, error) {
		return harnessconfig.Set(harnessconfig.ClaudeDesktop, b, entry)
	}, nil); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "Key stored in %s (readable only by you).\n", keyPath)
	_, _ = fmt.Fprintf(out, "Claude Desktop entry %q added to %s.\n", entry.Name(), cfgPath)
	_, _ = fmt.Fprintf(out, "The key works: project %s, repo scope %s.\n", orDash(id.Project), orDash(id.RepoScope))
	_, _ = fmt.Fprintln(out, "Restart Claude Desktop, then ask it to recall something from Vornik.")
	return nil
}

func orDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func runCompanionDisconnect(client string, out io.Writer) error {
	if client == harnessGeneric {
		return runCompanionDisconnectGeneric(out)
	}
	if client == harnessCursor {
		return runCompanionDisconnectCursor(out)
	}
	if client == harnessOpenCode {
		return runCompanionDisconnectOpenCode(out)
	}
	if client != harnessconfig.ClaudeDesktop {
		return fmt.Errorf("%q: companion disconnect removes claude-desktop, cursor, opencode, or generic", client)
	}
	entry := harnessconfig.Entry{Kind: harnessconfig.KindCompanion, Namespace: companionDeskName}
	cfgPath, err := harnessconfig.Path(harnessconfig.ClaudeDesktop, userEnv().harnessEnv())
	if err != nil {
		return err
	}
	removed := false
	if _, statErr := os.Stat(cfgPath); statErr == nil {
		if _, err := harnessconfig.Apply(cfgPath, func(b []byte) ([]byte, error) {
			o, found, rerr := harnessconfig.Remove(harnessconfig.ClaudeDesktop, b, entry)
			removed = found
			return o, rerr
		}, nil); err != nil {
			return err
		}
	}
	keyPath, err := companionKeyFile(companionDeskName)
	if err != nil {
		return err
	}
	keyRemoved := os.Remove(keyPath) == nil
	_, _ = fmt.Fprintf(out, "Claude Desktop entry %q removed: %v. Key file %s removed: %v.\n", entry.Name(), removed, keyPath, keyRemoved)
	_, _ = fmt.Fprintln(out, "The key itself is not revoked; the operator who minted it can (vornikctl companion keys).")
	return nil
}
