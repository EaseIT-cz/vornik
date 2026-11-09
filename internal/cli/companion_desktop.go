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
	Use:   "connect claude-desktop",
	Short: "Connect Claude Desktop (and Cowork) to Vornik with a companion key",
	Long: `Store a companion key in a file only you can read, add one MCP server entry
to Claude Desktop's config that runs 'vornikctl companion mcp-bridge', and
check the key against Vornik. The Claude config never holds the key.

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
	Use:          "disconnect claude-desktop",
	Short:        "Remove the companion entry from Claude Desktop and delete its key file",
	Long:         `Remove what 'vornikctl companion connect' wrote. It revokes nothing on the daemon: the key belongs to the operator who minted it ('vornikctl companion keys').`,
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		return runCompanionDisconnect(args[0], cmd.OutOrStdout())
	},
}

var companionDoctorCmd = &cobra.Command{
	Use:   "doctor",
	Short: "Check a companion connection in Claude Desktop and print the fix for each failure",
	Long: `Check the Claude Desktop entry, the key file, the daemon and the key itself,
one line per check, each failure with its fix. It changes nothing. Exit status
0 when every check passes, 1 otherwise.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		if companionDoctorClient != harnessconfig.ClaudeDesktop {
			return fmt.Errorf("--client %q: only claude-desktop is checked here", companionDoctorClient)
		}
		if failed := runCompanionDoctor(cmd.Context(), cmd.OutOrStdout()); failed > 0 {
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
	RepoScope string
	Admin     bool // tools/list offers describe_installation: an agent-admin key
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
	if err := call("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "vornikctl-companion-connect", "version": "1"}}, nil); err != nil {
		return id, err
	}
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
	if len(res.Content) > 0 {
		var who struct {
			ProjectID string `json:"project_id"`
			Scope     string `json:"effective_repo_scope"`
		}
		if json.Unmarshal([]byte(res.Content[0].Text), &who) == nil {
			id.Project, id.RepoScope = who.ProjectID, who.Scope
		}
	}
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

func requireClaudeDesktop(client string) error {
	if client != harnessconfig.ClaudeDesktop {
		return fmt.Errorf("%q: companion connect sets up claude-desktop; Claude Code and Codex use their companion plugins (contrib/claude-code-companion, contrib/codex-companion)", client)
	}
	return nil
}

func runCompanionConnect(ctx context.Context, client string, in io.Reader, out io.Writer) error {
	if err := requireClaudeDesktop(client); err != nil {
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
	if err := agentDir(filepath.Dir(keyPath), os.Getuid()); err != nil {
		return err
	}
	if err := writePrivate(keyPath, []byte(key+"\n")); err != nil {
		return err
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
	if err := requireClaudeDesktop(client); err != nil {
		return err
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
