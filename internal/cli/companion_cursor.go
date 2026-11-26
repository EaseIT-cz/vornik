package cli

// `vornikctl companion connect|doctor|disconnect cursor` (LLD 28 B7-B8,
// GitHub #621): connect generic's probe and key file, then the Cursor plugin
// embedded in this binary, installed into ~/.cursor/plugins/local. The key
// stays in the key file; the plugin's MCP entry runs this binary's bridge.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
	cursorcompanion "vornik.io/vornik/contrib/cursor-companion"
	"vornik.io/vornik/internal/harnessconfig"
	"vornik.io/vornik/internal/textutil"
)

const harnessCursor = "cursor"

// cursorPluginName is the plugin's manifest name and its directory under
// ~/.cursor/plugins/local.
const cursorPluginName = "vornik-companion"

// companionAllowUnscoped is connect cursor's --allow-unscoped: accept a key
// with no default repo scope (its notes are stored unscoped).
var companionAllowUnscoped bool

func init() {
	companionConnectCmd.Flags().BoolVar(&companionAllowUnscoped, "allow-unscoped", false,
		"cursor and opencode only: accept a key with no default repo scope (its notes are stored unscoped)")
}

func cursorPluginDir() (string, error) {
	home := userEnv().home
	if home == "" {
		return "", errors.New("no home directory: cannot find ~/.cursor")
	}
	return filepath.Join(home, ".cursor", "plugins", "local", cursorPluginName), nil
}

// requireCursorScope refuses an unscoped key unless --allow-unscoped: Cursor
// has no reliable session hook, so the key's default is the only scope its
// notes get (LLD 28 B7).
func requireCursorScope(id companionIdentity) error {
	if id.DefaultScope != "" || companionAllowUnscoped {
		return nil
	}
	return errors.New("this key has no default repo scope, so this assistant's notes would be stored unscoped: ask the operator to re-grant it with --repo-scope=github.com/ORG/REPO (or --repo-scope='*'), or pass --allow-unscoped")
}

// isVornikPlugin reports whether dir holds this plugin's manifest; only such
// a directory is replaced or removed.
func isVornikPlugin(dir string) bool {
	raw, err := os.ReadFile(filepath.Join(dir, ".cursor-plugin", "plugin.json"))
	if err != nil {
		return false
	}
	var m struct {
		Name string `json:"name"`
	}
	return json.Unmarshal(raw, &m) == nil && m.Name == cursorPluginName
}

// removeCursorPlugin removes dir when it is this plugin. A missing directory
// is not an error; a foreign one is.
func removeCursorPlugin(dir string) (removed bool, err error) {
	if _, statErr := os.Stat(dir); errors.Is(statErr, os.ErrNotExist) {
		return false, nil
	}
	if !isVornikPlugin(dir) {
		return false, fmt.Errorf("%s exists and is not the Vornik companion plugin; it is left alone", dir)
	}
	return true, os.RemoveAll(dir)
}

// installCursorPlugin writes the embedded plugin into dir, replacing an
// earlier copy, with mcp.json naming this binary's bridge for the entry.
func installCursorPlugin(dir string, entry harnessconfig.Entry) error {
	if _, err := removeCursorPlugin(dir); err != nil {
		return err
	}
	err := fs.WalkDir(cursorcompanion.Bundle, ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		dst := filepath.Join(dir, filepath.FromSlash(path))
		if d.IsDir() {
			return os.MkdirAll(dst, 0o755)
		}
		body, err := fs.ReadFile(cursorcompanion.Bundle, path)
		if err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(path, ".sh") {
			mode = 0o755
		}
		return os.WriteFile(dst, body, mode)
	})
	if err != nil {
		return err
	}
	mcp, err := json.MarshalIndent(map[string]any{
		"mcpServers": map[string]any{
			"vornik": map[string]any{"command": entry.Command, "args": entry.Args()},
		},
	}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "mcp.json"), append(mcp, '\n'), 0o644); err != nil {
		return err
	}
	// The session hook runs the same binary, for the same key name and URL.
	hookCmd := shellQuote(entry.Command) + " companion session-start --name " + shellQuote(entry.Namespace)
	if entry.URL != "" {
		hookCmd += " --url " + shellQuote(entry.URL)
	}
	hooks, err := json.MarshalIndent(map[string]any{
		"version": 1,
		"hooks":   map[string]any{"sessionStart": []map[string]string{{"command": hookCmd}}},
	}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "hooks", "hooks.json"), append(hooks, '\n'), 0o644)
}

// shellQuote quotes s for a hook command line, which Cursor runs through a
// shell: single quotes, with each embedded quote closed and escaped.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// finishConnectCursor is connect's last step for cursor, after the key file
// is written.
func finishConnectCursor(out io.Writer, keyPath string, entry harnessconfig.Entry, id companionIdentity) error {
	dir, err := cursorPluginDir()
	if err != nil {
		return err
	}
	if err := installCursorPlugin(dir, entry); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "Key stored in %s (readable only by you).\n", keyPath)
	_, _ = fmt.Fprintf(out, "Cursor plugin %s %s installed in %s.\n", cursorPluginName, embeddedCursorVersion(), dir)
	_, _ = fmt.Fprintf(out, "The key works: project %s, repo scope %s.\n", orDash(id.Project), orDash(id.DefaultScope))
	_, _ = fmt.Fprintln(out, "Reload the Cursor window (Developer: Reload Window), then ask it to recall something from Vornik.")
	return nil
}

func runCompanionDisconnectCursor(out io.Writer) error {
	dir, err := cursorPluginDir()
	if err != nil {
		return err
	}
	removed, err := removeCursorPlugin(dir)
	if err != nil {
		return err
	}
	if removed {
		_, _ = fmt.Fprintf(out, "Cursor plugin removed from %s. Reload the Cursor window.\n", dir)
	} else {
		_, _ = fmt.Fprintf(out, "No Cursor plugin at %s.\n", dir)
	}
	return removeCompanionKeyFile(out, "")
}

func embeddedCursorVersion() string {
	return cursorManifestVersion(cursorcompanion.Bundle, ".cursor-plugin/plugin.json")
}

func cursorManifestVersion(fsys fs.FS, path string) string {
	raw, err := fs.ReadFile(fsys, path)
	if err != nil {
		return ""
	}
	var m struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(raw, &m) != nil {
		return ""
	}
	return m.Version
}

// runCompanionDoctorCursor checks the key and daemon as the generic doctor
// does, then the installed plugin: present, the embedded version, and an
// MCP entry whose command exists.
func runCompanionDoctorCursor(ctx context.Context, out io.Writer) int {
	if ctx == nil {
		ctx = context.Background()
	}
	connectFix := "run vornikctl companion connect cursor --key-stdin"
	failed, id, works := doctorKeyAndDaemon(ctx, out, companionBaseURL(), connectFix, harnessCursor)
	if works && id.DefaultScope == "" {
		_, _ = fmt.Fprintln(out, "WARN the key has no default repo scope; Cursor's notes are stored unscoped (re-grant with --repo-scope=github.com/ORG/REPO)")
	}
	dir, err := cursorPluginDir()
	if err != nil {
		_, _ = fmt.Fprintf(out, "FAIL %v\n", err)
		return failed + 1
	}
	if !isVornikPlugin(dir) {
		_, _ = fmt.Fprintf(out, "FAIL no Cursor plugin in %s: %s\n", dir, connectFix)
		return failed + 1
	}
	if got, want := cursorManifestVersion(os.DirFS(dir), ".cursor-plugin/plugin.json"), embeddedCursorVersion(); got != want {
		_, _ = fmt.Fprintf(out, "FAIL the installed plugin is %s, this vornikctl carries %s: %s\n", orDash(got), want, connectFix)
		failed++
	} else {
		_, _ = fmt.Fprintf(out, "OK   Cursor plugin %s installed in %s\n", got, dir)
	}
	var cfg struct {
		MCPServers map[string]struct {
			Command string `json:"command"`
		} `json:"mcpServers"`
	}
	raw, err := os.ReadFile(filepath.Join(dir, "mcp.json"))
	if err == nil {
		err = json.Unmarshal(raw, &cfg)
	}
	cmd := cfg.MCPServers["vornik"].Command
	if _, statErr := os.Stat(cmd); err != nil || cmd == "" || statErr != nil {
		_, _ = fmt.Fprintf(out, "FAIL the plugin's MCP entry does not name a vornikctl that exists (%s): %s\n", orDash(cmd), connectFix)
		failed++
	} else {
		_, _ = fmt.Fprintf(out, "OK   the MCP entry runs %s\n", cmd)
	}
	if hookBin := cursorHookBinary(dir); hookBin == "" {
		_, _ = fmt.Fprintf(out, "FAIL the plugin's session hook names no vornikctl: %s\n", connectFix)
		failed++
	} else if _, statErr := os.Stat(hookBin); statErr != nil {
		_, _ = fmt.Fprintf(out, "FAIL the plugin's session hook runs %s, which does not exist: %s\n", hookBin, connectFix)
		failed++
	} else {
		_, _ = fmt.Fprintf(out, "OK   the session hook runs %s\n", hookBin)
	}
	return failed
}

// cursorHookBinary is the program the installed sessionStart hook runs: the
// first word of its command, single quotes removed as shellQuote wrote them.
func cursorHookBinary(dir string) string {
	var hooks struct {
		Hooks struct {
			SessionStart []struct {
				Command string `json:"command"`
			} `json:"sessionStart"`
		} `json:"hooks"`
	}
	raw, err := os.ReadFile(filepath.Join(dir, "hooks", "hooks.json"))
	if err != nil || json.Unmarshal(raw, &hooks) != nil || len(hooks.Hooks.SessionStart) == 0 {
		return ""
	}
	cmd := hooks.Hooks.SessionStart[0].Command
	if strings.HasPrefix(cmd, "'") {
		if end := strings.Index(cmd[1:], "' "); end >= 0 {
			return strings.ReplaceAll(cmd[1:1+end], `'\''`, "'")
		}
	}
	first, _, _ := strings.Cut(cmd, " ")
	return first
}

// companionSessionStartBudget bounds the whole digest: a session start must
// not wait on a slow daemon.
const companionSessionStartBudget = 8 * time.Second

// companionDigestMax bounds each part of the digest.
const companionDigestMax = 2000

var companionSessionStartCmd = &cobra.Command{
	Use:    "session-start",
	Short:  "Print the session digest a Cursor sessionStart hook adds to a conversation (used by the Cursor plugin)",
	Hidden: true,
	Long: `Print one JSON object, {"additional_context": "..."}: the outstanding and
finished delegations and the recent project memory, from the companion
endpoint with the key file connect wrote. Only tools the daemon offers are
called. It never fails: an error prints a short note that says to call
recent_memory, and exits 0 (LLD 28 B5).`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runCompanionSessionStart(cmd.Context(), cmd.OutOrStdout())
	},
}

func init() {
	companionSessionStartCmd.Flags().StringVar(&companionDeskName, "name", "default", "The companion key's name")
	companionSessionStartCmd.Flags().StringVar(&companionDeskURL, "url", "", "Vornik's URL (default $VORNIK_URL, then $VORNIK_API_URL, then "+DefaultAPIURL+")")
	companionCmd.AddCommand(companionSessionStartCmd)
}

func runCompanionSessionStart(ctx context.Context, out io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, companionSessionStartBudget)
	defer cancel()
	text, err := companionDigest(ctx)
	if err != nil {
		// The model's context gets a fixed note; the detail (an address, a
		// status) goes to stderr, where Cursor's hook log keeps it.
		_, _ = fmt.Fprintf(os.Stderr, "vornikctl companion session-start: %v\n", err)
		text = "Vornik: the session digest is unavailable; call Vornik's recent_memory tool to catch up. (vornikctl companion doctor --client cursor says why.)"
	}
	return json.NewEncoder(out).Encode(map[string]string{"additional_context": text})
}

func companionDigest(ctx context.Context) (string, error) {
	key, err := companionKeyForCLI()
	if err != nil {
		return "", errors.New("no companion key: run vornikctl companion connect cursor")
	}
	base := companionBaseURL()
	rpc := &companionRPC{hc: &http.Client{Timeout: companionSessionStartBudget}, base: base, endpoint: base + "/api/v1/mcp/companion", key: key}
	if err := rpc.call(ctx, "initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{},
		"clientInfo": map[string]any{"name": "vornikctl-cursor-session-start", "version": "1"}}, nil); err != nil {
		return "", err
	}
	var tools struct {
		Tools []struct {
			Name string `json:"name"`
		} `json:"tools"`
	}
	if err := rpc.call(ctx, "tools/list", map[string]any{}, &tools); err != nil {
		return "", err
	}
	offered := map[string]bool{}
	for _, t := range tools.Tools {
		offered[t.Name] = true
	}
	var b strings.Builder
	b.WriteString("Vornik session digest (from the Vornik plugin). Results are not included; fetch one with Vornik's result tool.\n")
	for _, part := range []struct {
		tool, title string
		args        map[string]any
	}{
		{"list", "Your delegations", map[string]any{"limit": 10}},
		{"recent_memory", "Recent project memory", map[string]any{"limit": 5}},
	} {
		if !offered[part.tool] {
			continue
		}
		text, err := companionToolText(ctx, rpc, part.tool, part.args)
		if err != nil {
			fmt.Fprintf(&b, "\n%s: unavailable (%v).\n", part.title, err)
			continue
		}
		fmt.Fprintf(&b, "\n%s:\n%s\n", part.title, textutil.TruncateRunes(text, companionDigestMax))
	}
	return b.String(), nil
}

func companionToolText(ctx context.Context, rpc *companionRPC, tool string, args map[string]any) (string, error) {
	var res struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
		IsError bool `json:"isError"`
	}
	if err := rpc.call(ctx, "tools/call", map[string]any{"name": tool, "arguments": args}, &res); err != nil {
		return "", err
	}
	if len(res.Content) == 0 {
		return "", errors.New("empty answer")
	}
	if res.IsError {
		return "", errors.New(textutil.TruncateRunes(res.Content[0].Text, 200))
	}
	return res.Content[0].Text, nil
}
