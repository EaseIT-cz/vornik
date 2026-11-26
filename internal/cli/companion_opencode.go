package cli

// `vornikctl companion connect|doctor|disconnect opencode` (LLD 29 B7, GitHub
// #841): connect generic's probe and key file, then the opencode bundle
// embedded in this binary, installed into opencode's config directory: the
// stdio bridge entry in opencode.json, the plugin with its guidance, the
// skills under vornik- names, and the deposit script. The key stays in the
// key file. Everything is checked before the key file is written, so a
// refusal leaves nothing behind.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	opencodecompanion "vornik.io/vornik/contrib/opencode-companion"
	"vornik.io/vornik/internal/harnessconfig"
)

const harnessOpenCode = harnessconfig.OpenCode

const (
	opencodePluginFile   = "plugin/vornik-companion.js"
	opencodeGuidanceFile = "plugin/vornik-companion-guidance.md"
	// opencodeSupportDir holds what has no opencode-defined place: the
	// deposit script the backlog-deposit skill runs.
	opencodeSupportDir = "vornik-companion"
)

// opencodeMarked are the files connect may replace and disconnect may remove
// only when they start with their marker (LLD 29 B7).
var opencodeMarked = map[string]string{
	opencodePluginFile:   "// vornik-companion",
	opencodeGuidanceFile: "<!-- vornik-companion",
}

// opencodeDir is opencode's config directory: the directory of the
// opencode.json harnessconfig edits.
func opencodeDir() (cfgPath, dir string, err error) {
	cfgPath, err = harnessconfig.Path(harnessconfig.OpenCode, userEnv().harnessEnv())
	if err != nil {
		return "", "", err
	}
	return cfgPath, filepath.Dir(cfgPath), nil
}

// isVornikFile reports whether path is absent or starts with marker.
func isVornikFile(path, marker string) (absent, ours bool) {
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return true, false
	}
	return false, err == nil && bytes.HasPrefix(raw, []byte(marker))
}

// opencodeSkills are the skill directories the bundle carries.
func opencodeSkills() []string {
	entries, err := fs.ReadDir(opencodecompanion.Bundle, "skills")
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			names = append(names, e.Name())
		}
	}
	return names
}

// preflightOpenCode refuses, before anything is written, a config connect
// cannot edit and a marked file that is someone else's.
func preflightOpenCode(cfgPath, dir string, entry harnessconfig.Entry) error {
	if raw, err := os.ReadFile(cfgPath); err == nil {
		if _, err := harnessconfig.Set(harnessconfig.OpenCode, raw, entry); err != nil {
			return fmt.Errorf("%s: %w", cfgPath, err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for rel, marker := range opencodeMarked {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		if absent, ours := isVornikFile(p, marker); !absent && !ours {
			return fmt.Errorf("%s exists and is not the Vornik companion's; it is left alone", p)
		}
	}
	return nil
}

// jsString is s as a JavaScript string literal (JSON's string syntax is a
// subset of JavaScript's).
func jsString(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

// opencodePlugin is the embedded plugin with the three constants set for
// this machine.
func opencodePlugin(entry harnessconfig.Entry) ([]byte, error) {
	raw, err := fs.ReadFile(opencodecompanion.Bundle, opencodePluginFile)
	if err != nil {
		return nil, err
	}
	js := string(raw)
	for from, to := range map[string]string{
		`const VORNIKCTL = "vornikctl";`: "const VORNIKCTL = " + jsString(entry.Command) + ";",
		`const KEY_NAME = "default";`:    "const KEY_NAME = " + jsString(entry.Namespace) + ";",
		`const VORNIK_URL = "";`:         "const VORNIK_URL = " + jsString(entry.URL) + ";",
	} {
		if strings.Count(js, from) != 1 {
			return nil, fmt.Errorf("the embedded plugin does not hold %q once", from)
		}
		js = strings.Replace(js, from, to, 1)
	}
	return []byte(js), nil
}

// writeBundleTree copies the embedded subtree src into dst, replacing dst.
func writeBundleTree(src, dst string) error {
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	return fs.WalkDir(opencodecompanion.Bundle, src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, filepath.FromSlash(path))
		out := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(out, 0o755)
		}
		body, err := fs.ReadFile(opencodecompanion.Bundle, path)
		if err != nil {
			return err
		}
		mode := os.FileMode(0o644)
		if strings.HasSuffix(path, ".sh") {
			mode = 0o755
		}
		return os.WriteFile(out, body, mode)
	})
}

// installOpenCode writes the plugin, guidance, skills and script, then the
// opencode.json entry last, so a failure part-way leaves no entry that runs
// without its plugin.
func installOpenCode(cfgPath, dir string, entry harnessconfig.Entry) error {
	if err := os.MkdirAll(filepath.Join(dir, "plugin"), 0o755); err != nil {
		return err
	}
	plugin, err := opencodePlugin(entry)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(opencodePluginFile)), plugin, 0o644); err != nil {
		return err
	}
	guidance, err := fs.ReadFile(opencodecompanion.Bundle, opencodeGuidanceFile)
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, filepath.FromSlash(opencodeGuidanceFile)), guidance, 0o644); err != nil {
		return err
	}
	for _, name := range opencodeSkills() {
		if err := writeBundleTree("skills/"+name, filepath.Join(dir, "skills", name)); err != nil {
			return err
		}
	}
	if err := writeBundleTree("scripts", filepath.Join(dir, opencodeSupportDir, "scripts")); err != nil {
		return err
	}
	_, err = harnessconfig.Apply(cfgPath, func(b []byte) ([]byte, error) {
		return harnessconfig.Set(harnessconfig.OpenCode, b, entry)
	}, nil)
	return err
}

func finishConnectOpenCode(out io.Writer, keyPath, cfgPath, dir string, entry harnessconfig.Entry, id companionIdentity) error {
	if err := installOpenCode(cfgPath, dir, entry); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "Key stored in %s (readable only by you).\n", keyPath)
	_, _ = fmt.Fprintf(out, "opencode entry %q added to %s.\n", entry.Name(), cfgPath)
	_, _ = fmt.Fprintf(out, "Plugin %s %s, its guidance and %d skills installed in %s.\n", opencodePluginFile, embeddedOpenCodeVersion(), len(opencodeSkills()), dir)
	_, _ = fmt.Fprintf(out, "The key works: project %s, repo scope %s.\n", orDash(id.Project), orDash(id.DefaultScope))
	_, _ = fmt.Fprintln(out, "Start a new opencode session, then ask it to recall something from Vornik.")
	return nil
}

func runCompanionDisconnectOpenCode(out io.Writer) error {
	cfgPath, dir, err := opencodeDir()
	if err != nil {
		return err
	}
	if _, statErr := os.Stat(cfgPath); statErr == nil {
		entry := harnessconfig.Entry{Kind: harnessconfig.KindCompanion, Namespace: companionDeskName}
		removed := false
		if _, err := harnessconfig.Apply(cfgPath, func(b []byte) ([]byte, error) {
			o, found, rerr := harnessconfig.Remove(harnessconfig.OpenCode, b, entry)
			removed = found
			return o, rerr
		}, nil); err != nil {
			return err
		}
		if removed {
			_, _ = fmt.Fprintf(out, "opencode entry %q removed from %s.\n", entry.Name(), cfgPath)
		}
	}
	for rel, marker := range opencodeMarked {
		p := filepath.Join(dir, filepath.FromSlash(rel))
		switch absent, ours := isVornikFile(p, marker); {
		case absent:
		case !ours:
			_, _ = fmt.Fprintf(out, "%s is not the Vornik companion's; left alone.\n", p)
		default:
			if err := os.Remove(p); err != nil {
				return err
			}
		}
	}
	for _, name := range opencodeSkills() {
		if err := os.RemoveAll(filepath.Join(dir, "skills", name)); err != nil {
			return err
		}
	}
	if err := os.RemoveAll(filepath.Join(dir, opencodeSupportDir)); err != nil {
		return err
	}
	// The directories connect may have created go when empty; a user's own
	// plugin or skill keeps them.
	for _, d := range []string{"plugin", "skills"} {
		_ = os.Remove(filepath.Join(dir, d))
	}
	_, _ = fmt.Fprintf(out, "The Vornik plugin, guidance and skills removed from %s.\n", dir)
	return removeCompanionKeyFile(out, "")
}

var (
	opencodeVersionRe   = regexp.MustCompile(`(?m)^const VERSION = "([^"]+)";`)
	opencodeTestedRe    = regexp.MustCompile(`(?m)^const TESTED_OPENCODE = "([^"]+)";`)
	opencodeVornikctlRe = regexp.MustCompile(`(?m)^const VORNIKCTL = ("(?:[^"\\]|\\.)*");`)
)

func firstMatch(re *regexp.Regexp, raw []byte) string {
	if m := re.FindSubmatch(raw); m != nil {
		return string(m[1])
	}
	return ""
}

func embeddedOpenCodeVersion() string {
	raw, _ := fs.ReadFile(opencodecompanion.Bundle, opencodePluginFile)
	return firstMatch(opencodeVersionRe, raw)
}

// runCompanionDoctorOpenCode checks the key and daemon as the generic doctor
// does, then the opencode.json entry, the plugin and its version, the binary
// both name, and (a warning only) the opencode version against the tested one.
func runCompanionDoctorOpenCode(ctx context.Context, out io.Writer) int {
	if ctx == nil {
		ctx = context.Background()
	}
	connectFix := "run vornikctl companion connect opencode --key-stdin"
	failed, id, works := doctorKeyAndDaemon(ctx, out, companionBaseURL(), connectFix, harnessOpenCode)
	if works && id.DefaultScope == "" {
		_, _ = fmt.Fprintln(out, "WARN the key has no default repo scope; opencode's notes are stored unscoped (re-grant with --repo-scope=github.com/ORG/REPO)")
	}
	cfgPath, dir, err := opencodeDir()
	if err != nil {
		_, _ = fmt.Fprintf(out, "FAIL %v\n", err)
		return failed + 1
	}
	name := harnessconfig.Entry{Kind: harnessconfig.KindCompanion, Namespace: companionDeskName}.Name()
	var cfg struct {
		MCP map[string]struct {
			Command []string `json:"command"`
		} `json:"mcp"`
	}
	var cmd string
	if raw, err := os.ReadFile(cfgPath); err == nil && json.Unmarshal(raw, &cfg) == nil {
		if c := cfg.MCP[name].Command; len(c) > 0 {
			cmd = c[0]
		}
	}
	if _, statErr := os.Stat(cmd); cmd == "" || statErr != nil {
		_, _ = fmt.Fprintf(out, "FAIL %s has no entry %q naming a vornikctl that exists: %s\n", cfgPath, name, connectFix)
		failed++
	} else {
		_, _ = fmt.Fprintf(out, "OK   the opencode entry %q runs %s\n", name, cmd)
	}
	pluginPath := filepath.Join(dir, filepath.FromSlash(opencodePluginFile))
	plugin, perr := os.ReadFile(pluginPath)
	if perr != nil || !bytes.HasPrefix(plugin, []byte(opencodeMarked[opencodePluginFile])) {
		_, _ = fmt.Fprintf(out, "FAIL no Vornik plugin at %s: %s\n", pluginPath, connectFix)
		return failed + 1
	}
	if got, want := firstMatch(opencodeVersionRe, plugin), embeddedOpenCodeVersion(); got != want {
		_, _ = fmt.Fprintf(out, "FAIL the installed plugin is %s, this vornikctl carries %s: %s\n", orDash(got), want, connectFix)
		failed++
	} else {
		_, _ = fmt.Fprintf(out, "OK   plugin %s installed at %s\n", got, pluginPath)
	}
	var bin string
	_ = json.Unmarshal([]byte(firstMatch(opencodeVornikctlRe, plugin)), &bin)
	if _, statErr := os.Stat(bin); bin == "" || statErr != nil {
		_, _ = fmt.Fprintf(out, "FAIL the plugin's digest runs %s, which does not exist: %s\n", orDash(bin), connectFix)
		failed++
	} else {
		_, _ = fmt.Fprintf(out, "OK   the plugin's digest runs %s\n", bin)
	}
	if _, gerr := os.Stat(filepath.Join(dir, filepath.FromSlash(opencodeGuidanceFile))); gerr != nil {
		_, _ = fmt.Fprintf(out, "FAIL the plugin's guidance is missing: %s\n", connectFix)
		failed++
	}
	warnOpenCodeVersion(ctx, out, firstMatch(opencodeTestedRe, plugin))
	return failed
}

// warnOpenCodeVersion warns, never fails, when the opencode on the PATH is
// not the version the plugin was tested with (LLD 29 B12): the plugin relies
// on an experimental hook a later opencode may rename.
func warnOpenCodeVersion(ctx context.Context, out io.Writer, tested string) {
	bin, err := exec.LookPath("opencode")
	if err != nil || tested == "" {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	raw, err := exec.CommandContext(ctx, bin, "--version").Output()
	got := strings.TrimSpace(string(raw))
	if err != nil || got == "" {
		_, _ = fmt.Fprintf(out, "WARN could not read %s --version\n", bin)
		return
	}
	if got != tested {
		_, _ = fmt.Fprintf(out, "WARN opencode is %s; the plugin was tested with %s. If Vornik's rules stop reaching opencode's model, report it (the plugin uses an experimental hook)\n", got, tested)
	}
}
