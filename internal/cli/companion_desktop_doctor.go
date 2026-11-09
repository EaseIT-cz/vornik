package cli

// `vornikctl companion doctor --client claude-desktop`
// (25-claude-desktop-companion.md §B.5, GitHub #589): one line per check, each
// failure with its fix. It changes nothing.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"vornik.io/vornik/internal/agentbridge"
	"vornik.io/vornik/internal/harnessconfig"
)

// desktopServer is one mcpServers entry as Claude Desktop stores it.
type desktopServer struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

// doctorLine prints one check; it returns 1 for a failure.
func doctorLine(out io.Writer, ok bool, what, fix string) int {
	if ok {
		_, _ = fmt.Fprintf(out, "OK   %s\n", what)
		return 0
	}
	_, _ = fmt.Fprintf(out, "FAIL %s\n     fix: %s\n", what, fix)
	return 1
}

// entryURL is the --url the entry passes the bridge, if any.
func entryURL(args []string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == "--url" {
			return args[i+1]
		}
	}
	return ""
}

func runCompanionDoctor(ctx context.Context, out io.Writer) int {
	if ctx == nil {
		ctx = context.Background()
	}
	failed := 0
	connectFix := "run vornikctl companion connect claude-desktop --key-stdin"
	entryName := harnessconfig.Entry{Kind: harnessconfig.KindCompanion, Namespace: companionDeskName}.Name()

	// 1. The config file and our entry.
	var entry *desktopServer
	var servers map[string]desktopServer
	cfgPath, err := harnessconfig.Path(harnessconfig.ClaudeDesktop, userEnv().harnessEnv())
	if err == nil {
		raw, rerr := os.ReadFile(cfgPath)
		var cfg struct {
			MCPServers map[string]desktopServer `json:"mcpServers"`
		}
		switch {
		case rerr != nil:
			failed += doctorLine(out, false, "Claude Desktop config "+cfgPath+" does not exist", connectFix)
		case json.Unmarshal(raw, &cfg) != nil:
			failed += doctorLine(out, false, "Claude Desktop config "+cfgPath+" is not valid JSON", "fix the file by hand, then "+connectFix)
		default:
			servers = cfg.MCPServers
			if e, ok := servers[entryName]; ok {
				entry = &e
			}
			failed += doctorLine(out, entry != nil, fmt.Sprintf("entry %q in %s", entryName, cfgPath), connectFix)
		}
	} else {
		failed += doctorLine(out, false, "Claude Desktop config location: "+err.Error(), connectFix)
	}

	// 2. An npx bridge to the companion endpoint: the field report's first
	// failure (Node is not on many laptops).
	for name, s := range servers {
		if filepath.Base(s.Command) == "npx" && strings.Contains(strings.Join(s.Args, " "), "/api/v1/mcp/companion") {
			failed += doctorLine(out, false, fmt.Sprintf("entry %q runs npx to reach Vornik; npx needs Node, which many machines do not have", name),
				"remove that entry and "+connectFix+" (the built-in bridge needs no Node)")
		}
	}

	// 3. The entry's command.
	if entry != nil {
		fi, serr := os.Stat(entry.Command)
		ok := serr == nil && filepath.IsAbs(entry.Command) && fi.Mode().IsRegular() && fi.Mode().Perm()&0o111 != 0
		failed += doctorLine(out, ok, "the entry runs an executable vornikctl ("+entry.Command+")", connectFix+" (it writes this vornikctl's path)")
	}

	// 4. The key file.
	key := ""
	keyPath, kerr := companionKeyFile(companionDeskName)
	if kerr == nil {
		key, kerr = agentbridge.LoadKey(keyPath)
	}
	failed += doctorLine(out, kerr == nil, "key file "+keyPath+" exists, is readable only by you, and is yours", connectFix)

	// 5. The daemon, and the key against it.
	base := companionBaseURL()
	if entry != nil {
		if u := entryURL(entry.Args); u != "" {
			base = strings.TrimRight(u, "/")
		}
	}
	if err := agentbridge.CheckEndpoint(base); err != nil {
		failed += doctorLine(out, false, base+": "+err.Error(), "use an https:// URL for a Vornik on another machine")
	} else {
		failed += doctorLine(out, reachable(ctx, base), "Vornik answers at "+base, "check the URL and that the daemon runs (vornikctl doctor on its host)")
		if key != "" {
			id, perr := probeCompanion(ctx, base, key)
			switch {
			case errors.Is(perr, errCompanionUnauthorized):
				failed += doctorLine(out, false, "the key is accepted", "the key is wrong, revoked or not a companion key: ask the operator for a new one (vornikctl companion grant), then "+connectFix)
			case perr != nil:
				failed += doctorLine(out, false, "the key is accepted: "+perr.Error(), "check the URL and the key")
			case id.Admin:
				failed += doctorLine(out, false, "the key is a companion key", "this is an agent-admin key; use vornikctl agent connect claude-desktop")
			default:
				failed += doctorLine(out, true, fmt.Sprintf("the key works (project %s, repo scope %s)", orDash(id.Project), orDash(id.RepoScope)), "")
			}
		}
	}

	// 6. Information, not a failure: GUI apps do not read shell profiles.
	if strings.TrimSpace(os.Getenv("VORNIK_COMPANION_TOKEN")) != "" {
		_, _ = fmt.Fprintln(out, "INFO VORNIK_COMPANION_TOKEN is set in this shell; GUI apps do not read shell profiles, so Claude Desktop uses the key file, not this variable")
	}
	return failed
}

// reachable reports whether base answers /healthz.
func reachable(ctx context.Context, base string) bool {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/healthz", nil)
	if err != nil {
		return false
	}
	resp, err := (&http.Client{Timeout: companionDeskTimeout}).Do(req)
	if err != nil {
		return false
	}
	_ = resp.Body.Close()
	return resp.StatusCode < 500
}
