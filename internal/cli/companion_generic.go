package cli

// `vornikctl companion connect|disconnect|doctor generic` and
// `vornikctl companion instructions` (25-claude-desktop-companion.md §D,
// GitHub #842): a companion key in an assistant Vornik has no adapter for
// (Gemini CLI, opencode, ...). It reuses connect claude-desktop's probe, key
// file and bridge; it writes no assistant config, and the key is never
// printed.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/agentbridge"
	"vornik.io/vornik/internal/harnessconfig"
)

// The markers that delimit Vornik's section in an instructions file.
const (
	instructionsStart = "<!-- vornik:start -->"
	instructionsEnd   = "<!-- vornik:end -->"
)

var companionInstructionsWrite string

var companionInstructionsCmd = &cobra.Command{
	Use:   "instructions",
	Short: "Print how an assistant should work with Vornik, for its AGENTS.md (or write it there)",
	Long: `Print the working rules Vornik's companion endpoint sends every session,
read from the daemon with your companion key, preceded by what an assistant
without Vornik hooks needs: call recent_memory when a session starts, and
find Vornik's tools under the prefix your assistant gives them.

--write <file> puts the text between ` + instructionsStart + ` and
` + instructionsEnd + `, replacing an earlier copy and keeping the rest of the
file. A missing file is created; a file with unbalanced markers is refused.

The key comes from VORNIK_COMPANION_TOKEN, else the key file connect wrote.`,
	Args:         cobra.NoArgs,
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, _ []string) error {
		return runCompanionInstructions(cmd.Context(), companionInstructionsWrite, cmd.OutOrStdout())
	},
}

func init() {
	companionInstructionsCmd.Flags().StringVar(&companionInstructionsWrite, "write", "", "Write the text into this file between the vornik markers")
	companionInstructionsCmd.Flags().StringVar(&companionDeskName, "name", "default", "The companion key's name (2 to 16 lowercase letters and digits)")
	companionInstructionsCmd.Flags().StringVar(&companionDeskURL, "url", "", "Vornik's URL (default $VORNIK_URL, then $VORNIK_API_URL, then "+DefaultAPIURL+")")
	companionCmd.AddCommand(companionInstructionsCmd)
}

// printGenericEntry prints what connect generic --print gives the user: the
// two universal MCP entries, never the key, and the key's scope.
func printGenericEntry(out io.Writer, keyPath string, entry harnessconfig.Entry, id companionIdentity) {
	stdio := map[string]any{"mcpServers": map[string]any{entry.Name(): map[string]any{"command": entry.Command, "args": entry.Args()}}}
	httpForm := map[string]any{"mcpServers": map[string]any{entry.Name(): map[string]any{
		"url":     entry.URL + "/api/v1/mcp/companion",
		"headers": map[string]string{"Authorization": "Bearer ${VORNIK_COMPANION_TOKEN}"},
	}}}
	s, _ := json.MarshalIndent(stdio, "", "  ")
	h, _ := json.MarshalIndent(httpForm, "", "  ")
	_, _ = fmt.Fprintf(out, "Key stored in %s (readable only by you).\n", keyPath)
	_, _ = fmt.Fprintf(out, "The key works: project %s, repo scope %s.\n\n", orDash(id.Project), orDash(id.RepoScope))
	_, _ = fmt.Fprintf(out, "Paste one of these into your assistant's MCP settings.\n\n")
	_, _ = fmt.Fprintf(out, "1. Runs the bridge; the key stays in the file:\n%s\n\n", s)
	_, _ = fmt.Fprintf(out, "2. Streamable HTTP; your assistant reads the key from VORNIK_COMPANION_TOKEN.\n"+
		"   An app started from a desktop does not read your shell profile, so set the\n"+
		"   variable where the app can see it, or use form 1. Assistants spell the\n"+
		"   variable differently (opencode: {env:VORNIK_COMPANION_TOKEN}); see\n"+
		"   https://docs.vornik.io/guides/any-agent/.\n%s\n\n", h)
	if id.DefaultScope == "" {
		_, _ = fmt.Fprintln(out, "WARNING notes this assistant stores will have no repo scope: the key has no")
		_, _ = fmt.Fprintln(out, "        default and an assistant without Vornik hooks passes none. Ask the operator")
		_, _ = fmt.Fprintln(out, "        to re-grant it with --repo-scope=github.com/ORG/REPO.")
	}
	_, _ = fmt.Fprintln(out, "Then: vornikctl companion instructions --write AGENTS.md")
}

func runCompanionDisconnectGeneric(out io.Writer) error {
	return removeCompanionKeyFile(out, " Remove the entry from your assistant's MCP settings yourself.")
}

// removeCompanionKeyFile deletes the key file connect wrote. tail follows the
// removal line: generic names the entry the user pasted; an adapter that
// removed its own entry passes "".
func removeCompanionKeyFile(out io.Writer, tail string) error {
	keyPath, err := companionKeyFile(companionDeskName)
	if err != nil {
		return err
	}
	switch err := os.Remove(keyPath); {
	case errors.Is(err, os.ErrNotExist):
		_, _ = fmt.Fprintf(out, "No key file at %s: nothing to remove.\n", keyPath)
	case err != nil:
		return fmt.Errorf("remove %s: %w", keyPath, err)
	default:
		_, _ = fmt.Fprintf(out, "Key file %s removed.%s\n", keyPath, tail)
	}
	_, _ = fmt.Fprintln(out, "The key itself is not revoked; the operator who minted it can (vornikctl companion keys).")
	return nil
}

func runCompanionDoctorGeneric(ctx context.Context, out io.Writer) int {
	if ctx == nil {
		ctx = context.Background()
	}
	connectFix := "run vornikctl companion connect generic --print --key-stdin"
	failed, id, works := doctorKeyAndDaemon(ctx, out, companionBaseURL(), connectFix, harnessGeneric)
	if works && id.DefaultScope == "" {
		_, _ = fmt.Fprintln(out, "WARN the key has no default repo scope; notes from an assistant without Vornik hooks are stored unscoped (re-grant with --repo-scope=github.com/ORG/REPO)")
	}
	if strings.TrimSpace(os.Getenv("VORNIK_COMPANION_TOKEN")) != "" {
		_, _ = fmt.Fprintln(out, "INFO VORNIK_COMPANION_TOKEN is set in this shell: the bridge uses it instead of the key file. An assistant started from a desktop does not read shell profiles.")
	} else {
		_, _ = fmt.Fprintln(out, "INFO VORNIK_COMPANION_TOKEN is not set here: the bridge uses the key file.")
	}
	_, _ = fmt.Fprintln(out, "INFO your assistant's own MCP entry and environment are not visible from here; check them in the assistant.")
	return failed
}

// companionKeyForCLI is the key a CLI command uses: the variable, else the
// key file connect wrote (the bridge's order).
func companionKeyForCLI() (string, error) {
	if k := strings.TrimSpace(os.Getenv("VORNIK_COMPANION_TOKEN")); k != "" {
		return k, nil
	}
	path, err := companionKeyFile(companionDeskName)
	if err != nil {
		return "", err
	}
	return agentbridge.LoadKey(path)
}

func runCompanionInstructions(ctx context.Context, writeTo string, out io.Writer) error {
	if ctx == nil {
		ctx = context.Background()
	}
	key, err := companionKeyForCLI()
	if err != nil {
		return fmt.Errorf("no companion key (set VORNIK_COMPANION_TOKEN or run vornikctl companion connect generic --print): %w", err)
	}
	base := companionBaseURL()
	if err := agentbridge.CheckEndpoint(base); err != nil {
		return err
	}
	id, err := probeCompanion(ctx, base, key)
	if err != nil {
		return err
	}
	if id.Admin {
		return errors.New("this is an agent-admin key; companion instructions need a companion key")
	}
	text := companionInstructionsText(id)
	if writeTo == "" {
		_, err := io.WriteString(out, text)
		return err
	}
	if err := writeInstructionsSection(writeTo, text); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "Vornik's section written to %s.\n", writeTo)
	return nil
}

// companionInstructionsText is the daemon's guidance with the lines an
// assistant without hooks needs in front of it.
func companionInstructionsText(id companionIdentity) string {
	var b strings.Builder
	b.WriteString("## Vornik (companion)\n\n")
	b.WriteString("- When a session starts, call Vornik's `recent_memory` to catch up on what is known.\n")
	b.WriteString("- Your assistant shows Vornik's tools under a prefix (Gemini CLI: `mcp_vornik_recall`; opencode: `vornik_recall`): call the tool whose name ends in Vornik's tool name, such as `recall`.\n")
	if id.DefaultScope == "" {
		b.WriteString("- Pass `repo_scope` (this repository's remote, such as `github.com/ORG/REPO`) to `remember`, `recall` and `delegate`: this key has no default.\n")
	}
	b.WriteString("\n")
	b.WriteString(strings.TrimSpace(id.Instructions))
	b.WriteString("\n")
	return b.String()
}

// writeInstructionsSection puts text between the markers in path, replacing
// an earlier copy and keeping the rest; it refuses unbalanced markers.
func writeInstructionsSection(path, text string) error {
	if strings.Contains(text, instructionsStart) || strings.Contains(text, instructionsEnd) {
		return errors.New("the daemon's instructions contain a vornik marker; writing them would leave a file the next run refuses")
	}
	old, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	mode := os.FileMode(0o644)
	if st, serr := os.Stat(path); serr == nil {
		mode = st.Mode().Perm()
	}
	// A marker counts only alone on its line, so a file that quotes the
	// markers (in a code block, say) is refused, not rewritten (review 76ae F3).
	lines := strings.SplitAfter(string(old), "\n")
	start, end, starts, ends := -1, -1, 0, 0
	for i, l := range lines {
		switch strings.TrimSpace(l) {
		case instructionsStart:
			starts, start = starts+1, i
		case instructionsEnd:
			ends, end = ends+1, i
		}
	}
	quoted := strings.Count(string(old), instructionsStart) != starts || strings.Count(string(old), instructionsEnd) != ends
	section := instructionsStart + "\n" + text + instructionsEnd + "\n"
	var updated string
	switch {
	case quoted:
		return fmt.Errorf("%s mentions a vornik marker inside a line; vornik's section must have each marker alone on its line. Fix it by hand, then run again", path)
	case starts == 0 && ends == 0:
		s := string(old)
		if s != "" && !strings.HasSuffix(s, "\n") {
			s += "\n"
		}
		if s != "" {
			s += "\n"
		}
		updated = s + section
	case starts == 1 && ends == 1 && start < end:
		updated = strings.Join(lines[:start], "") + section + strings.Join(lines[end+1:], "")
	default:
		return fmt.Errorf("%s has %d %q and %d %q lines; fix them by hand (one of each, in order), then run again", path, starts, instructionsStart, ends, instructionsEnd)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".vornik-instructions-*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.WriteString(updated); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Chmod(tmp.Name(), mode); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
