package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode"

	"github.com/spf13/cobra"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/agentbridge"
	"vornik.io/vornik/internal/harnessconfig"
)

// `vornikctl agent connect <harness>` and `vornikctl agent disconnect <ns>`
// (agent-administered Vornik design §11, plan P6.2 and P6.5).

var (
	connectAcceptSharedUser bool
	connectDryRun           bool
	connectAPIKeyStdin      bool
	connectVornikctl        string
	connectPrint            bool
	connectClient           string
)

// stdinIsTerminal reports whether r is a terminal; a seam so a test can
// force it. Only an *os.File on a character device is one.
var stdinIsTerminal = func(r io.Reader) bool {
	f, ok := r.(*os.File)
	if !ok {
		return false
	}
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

var agentConnectCmd = &cobra.Command{
	Use:   "connect <hermes|claude-desktop|claude-code|codex|generic>",
	Short: "Let an AI assistant set up and use Vornik, with you approving on your phone",
	Long: `Connect an assistant to Vornik as an administering agent. It can then create
projects, workflows and connections to your services in its own namespace,
and run them; every credential and every widening of its reach is approved
by you on your phone, and the assistant never sees a credential.

Connect checks the daemon, mints the namespace's agent key (needs a paired
phone: vornikctl pair-device), stores the key in a file only you can read,
and adds one MCP server entry to the assistant's own config. That entry runs
'vornikctl agent mcp-bridge'; the assistant's config never holds the key.

For an assistant that can run shell commands (Claude Code, Codex), connect
also checks that Vornik runs as a different OS user than you: otherwise the
assistant's shell could read Vornik's files, and connect refuses unless you
pass --accept-shared-user.

For any other assistant, 'connect generic --print' mints the key the same
way, writes no assistant config, and prints the MCP entry to paste (the
stdio entry that runs the bridge, and the streamable-HTTP form). --client
names the assistant's kind (default generic); an assistant Vornik has no
adapter for is treated as one that can run shell commands.`,
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	// A refusal is printed once by connect itself (GitHub #75).
	SilenceErrors: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := newConnector(args[0], connectOptions{vornikctl: connectVornikctl, keyStdin: connectAPIKeyStdin})
		if err != nil {
			return err
		}
		if c.keyStdin && c.keyIn == nil {
			c.keyIn = cmd.InOrStdin()
		}
		c.print, c.clientKind = connectPrint, connectClient
		if c.print {
			c.info = cmd.ErrOrStderr()
		}
		return c.connect(cmd.OutOrStdout())
	},
}

var agentDisconnectCmd = &cobra.Command{
	Use:   "disconnect <namespace>",
	Short: "Revoke an assistant's Vornik key and remove what connect wrote",
	Long: `Revoke the namespace's agent key, remove the MCP server entry connect added to
the assistant's config, and delete the key file. The namespace's projects and
approvals stay, so connecting again resumes where it left off.`,
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		return disconnectAgent(ClientFromURL(), userEnv(), args[0], cmd.OutOrStdout())
	},
}

func init() {
	agentConnectCmd.Flags().StringVar(&agentNamespace, "namespace", "", "The agent namespace: 2 to 16 lowercase letters and digits (default: the harness name without dashes, e.g. claudecode)")
	agentConnectCmd.Flags().StringVar(&agentURL, "url", "", "Vornik's URL (default $VORNIK_API_URL, then "+DefaultAPIURL+")")
	agentConnectCmd.Flags().BoolVar(&connectAcceptSharedUser, "accept-shared-user", false, "Connect a shell-capable assistant although Vornik's files may be readable to it (see the printed table)")
	agentConnectCmd.Flags().BoolVar(&connectDryRun, "dry-run", false, "Check and print what would be done, change nothing")
	agentConnectCmd.Flags().BoolVar(&connectAPIKeyStdin, "api-key-stdin", false, "Read the operator's admin key from the first line of standard input (pipe it; a terminal is refused), instead of VORNIK_API_KEY or the stored sign-in")
	agentConnectCmd.Flags().BoolVar(&connectPrint, "print", false, "generic only: write no assistant config; print the MCP entry (JSON on standard output) to paste")
	agentConnectCmd.Flags().StringVar(&connectClient, "client", "", "generic only: the assistant's client kind, lowercase letters, digits and dashes (default generic)")
	agentConnectCmd.Flags().StringVar(&connectVornikctl, "vornikctl", "", "Absolute path of the vornikctl the assistant runs; symlinks are resolved and the resolved file is recorded (default: this vornikctl). It must be executable by you")
	agentDisconnectCmd.Flags().StringVar(&agentURL, "url", "", "Vornik's URL (default $VORNIK_API_URL, then "+DefaultAPIURL+")")
	agentCmd.AddCommand(agentConnectCmd, agentDisconnectCmd)
}

// agentUser is the invoking user's side of a connection.
type agentUser struct {
	home, configDir string
	getenv          func(string) string
	uid             int
	gids            []int // primary and supplementary groups
}

func userEnv() agentUser {
	home, _ := os.UserHomeDir()
	cfg, _ := os.UserConfigDir()
	return agentUser{home: home, configDir: cfg, getenv: os.Getenv, uid: os.Getuid(), gids: userGroups()}
}

// userGroups is this process's primary and supplementary group ids.
func userGroups() []int {
	gids, _ := os.Getgroups()
	return append(gids, os.Getgid())
}

func (u agentUser) harnessEnv() harnessconfig.Env {
	return harnessconfig.Env{Home: u.home, ConfigDir: u.configDir, Getenv: u.getenv}
}

// ClientFromURL is ClientFromEnv with --url applied.
func ClientFromURL() *Client {
	c := ClientFromEnv()
	c.baseURL = agentBaseURL()
	return c
}

// keyAccess is what opening the daemon's store key as this user gave (plan
// P6 amendment F2).
type keyAccess int

const (
	keyReadable keyAccess = iota
	keyDenied
	keyAbsent
	// keyUnknown: the check could not be attempted (containerised daemon,
	// no Host block, or a relative path).
	keyUnknown
)

// classify turns an absent probe into readable for a same-UID,
// non-containerised daemon: store.key is created lazily, and the daemon
// will own it as the same user the agent's shell runs as (plan P6
// amendment, T6, operator decision D3). readable and denied always win.
func classify(probe keyAccess, host *hostView, uid int) keyAccess {
	if probe == keyAbsent && host != nil && !host.DaemonContainerized && host.DaemonUID == uid {
		return keyReadable
	}
	return probe
}

// refusalMessage is the one line printed when a readable store key refuses.
// "Vornik runs as your OS user" is said only when the UIDs match; a key read
// from another UID (group or world read) names the file instead.
func refusalMessage(harness string, sameUID bool, keyPath string) string {
	const tail = " To connect anyway, accept that: re-run with --accept-shared-user."
	if sameUID {
		return fmt.Sprintf("refused: %[1]s can run shell commands as you and Vornik runs as your OS user, so %[1]s could read Vornik's data.", harness) + tail
	}
	return fmt.Sprintf("refused: %[1]s can run shell commands as you and can read Vornik's key file (%[2]s), so it could read Vornik's data.", harness, keyPath) + tail
}

// probeKey opens path for reading and closes it at once; nothing is read.
func probeKey(path string) keyAccess {
	if strings.TrimSpace(path) == "" {
		return keyAbsent
	}
	f, err := os.Open(path)
	if err == nil {
		_ = f.Close()
		return keyReadable
	}
	if errors.Is(err, fs.ErrPermission) {
		return keyDenied
	}
	return keyAbsent
}

// sharedUserGate decides whether a harness may connect (design §3; plan P6
// amendments F1, F2, F8; review 6f6b F1). Only a shell-capable harness is
// gated, and the control is whether this user can read the daemon's store
// key: readable or unknown refuses without --accept-shared-user.
func sharedUserGate(class string, access keyAccess, accept bool) (bool, string) {
	if class != agentadmin.HarnessShellCapable {
		return true, "This assistant reaches Vornik only through its MCP connection."
	}
	switch access {
	case keyDenied:
		return true, "Vornik's store key is not readable to you, which indicates Vornik runs as another OS user. (This checks one file; it is evidence, not a test of every file.)"
	case keyReadable:
		if accept {
			return true, "Vornik's store key is readable to you, so this assistant's shell could read Vornik's secrets. Connecting anyway (--accept-shared-user)."
		}
		return false, "Vornik's store key is readable to you, so this assistant's shell could read Vornik's secrets, database and its own key. Run Vornik as a separate OS user, or pass --accept-shared-user to accept that."
	default:
		if accept {
			return true, "Whether Vornik's files are readable to you could not be checked (Vornik is on another machine, in a container, or the key you use is not an admin key). Connecting anyway (--accept-shared-user)."
		}
		return false, "Whether Vornik's files are readable to you could not be checked (Vornik is on another machine, in a container, or the key you use is not an admin key). Pass --accept-shared-user if Vornik runs as a separate OS user or on another machine."
	}
}

// capabilitiesView is the part of GET /api/v1/capabilities connect reads.
type capabilitiesView struct {
	Features map[string]bool `json:"features"`
	Host     *hostView       `json:"host"`
}

// hostView is the daemon's host block (admin-class keys only).
type hostView struct {
	DaemonUID           int    `json:"daemon_uid"`
	DaemonContainerized bool   `json:"daemon_containerized"`
	StoreKeyPath        string `json:"store_key_path"`
}

// agentRecord is <ns>.json beside the key file: what disconnect undoes. It
// holds no secret.
type agentRecord struct {
	Namespace  string `json:"namespace"`
	Harness    string `json:"harness"`
	KeyID      string `json:"key_id"`
	ProjectID  string `json:"project_id"`
	ConfigPath string `json:"config_path"`
}

type connector struct {
	harness, ns string
	client      *Client
	user        agentUser
	bridge      string // absolute path of vornikctl
	accept, dry bool
	probe       func(string) keyAccess
	// keyStdin: read the operator's admin key from keyIn (--api-key-stdin).
	keyStdin bool
	keyIn    io.Reader
	// print (--print, the generic harness only; GitHub #86): write no harness
	// file and print the MCP entry, as JSON on out, for the person to paste.
	// The human-readable lines then go to info, so out stays one JSON object.
	print bool
	info  io.Writer
	// clientKind is the kind the key is minted with: the harness name, or
	// --client for the generic harness.
	clientKind string
}

// newConnector is a variable so a test can run the cobra command against a
// fake daemon.
// connectOptions carries the connect flags into newConnector, so a test or
// another caller never depends on package globals (review b240 finding 6).
type connectOptions struct {
	vornikctl string // --vornikctl
	keyStdin  bool   // --api-key-stdin
}

var newConnector = func(harness string, opts connectOptions) (*connector, error) {
	exe := opts.vornikctl
	switch {
	case exe == "":
		var err error
		if exe, err = os.Executable(); err != nil {
			return nil, fmt.Errorf("find vornikctl's own path: %w", err)
		}
	case !filepath.IsAbs(exe):
		return nil, fmt.Errorf("--vornikctl must be an absolute path, got %q", exe)
	default:
		// Record the resolved file: what connect checks is what Hermes runs
		// (review b240 finding 5; design 24 states the choice).
		resolved, err := filepath.EvalSymlinks(exe)
		if err != nil {
			return nil, fmt.Errorf("--vornikctl %s: %w", exe, err)
		}
		exe = resolved
	}
	c := newConnectorFor(harness, agentNamespace, exe)
	c.keyStdin = opts.keyStdin
	return c, nil
}

// apiKeyStdinMax bounds the piped admin key.
const apiKeyStdinMax = 4096

// readAPIKey reads the admin key from the first line of r (GitHub #99,
// --api-key-stdin): in no argv, no environment, no history. A terminal is
// refused, because a typed key would echo.
func readAPIKey(r io.Reader) (string, error) {
	if r == nil || stdinIsTerminal(r) {
		return "", errors.New(`--api-key-stdin reads a piped key, and standard input is a terminal (a typed key would echo); pipe it instead: printf '%s\n' "$KEY" | vornikctl agent connect <assistant> --api-key-stdin`)
	}
	// Bounded read (review b240 finding 4): a key is short; a stream that is
	// not one is refused rather than read to its end.
	raw, err := io.ReadAll(io.LimitReader(r, apiKeyStdinMax+1))
	if err != nil {
		return "", fmt.Errorf("--api-key-stdin: read standard input: %w", err)
	}
	line, _, _ := strings.Cut(string(raw), "\n")
	if len(line) > apiKeyStdinMax {
		return "", fmt.Errorf("--api-key-stdin: the key is longer than %d bytes", apiKeyStdinMax)
	}
	key := strings.TrimSpace(line)
	if key == "" {
		return "", errors.New("--api-key-stdin: the first line of standard input is empty")
	}
	if strings.IndexFunc(key, unicode.IsControl) >= 0 {
		return "", errors.New("--api-key-stdin: the key contains a control character")
	}
	return key, nil
}

// canExecute reports why the user (uid, gids) cannot run path, or nil: a
// regular file with an execute bit that applies to them, behind
// directories they can search, for the path as given and as resolved. It
// reads mode bits (so the uid is a seam), not ACLs, and cannot see a noexec
// mount (design 24, "Setup CLI gaps closed", GitHub #99).
func canExecute(path string, uid int, gids []int) error {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	fi, err := os.Stat(resolved)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return errors.New("it is not a regular file")
	}
	if !permits(fi, uid, gids) {
		return fmt.Errorf("no execute bit applies to uid %d", uid)
	}
	for _, p := range []string{path, resolved} {
		for dir := filepath.Dir(p); ; dir = filepath.Dir(dir) {
			di, err := os.Stat(dir)
			if err != nil {
				return err
			}
			if !permits(di, uid, gids) {
				return fmt.Errorf("directory %s is not searchable by uid %d", dir, uid)
			}
			if dir == filepath.Dir(dir) {
				break
			}
		}
	}
	return nil
}

// permits applies the execute (search, for a directory) bit of fi's owner,
// group or other class to uid; uid 0 needs any execute bit on a file.
func permits(fi os.FileInfo, uid int, gids []int) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok {
		return true
	}
	mode := fi.Mode().Perm()
	switch {
	case uid == 0:
		return fi.IsDir() || mode&0o111 != 0
	case int(st.Uid) == uid:
		return mode&0o100 != 0
	}
	for _, g := range gids {
		if int(st.Gid) == g {
			return mode&0o010 != 0
		}
	}
	return mode&0o001 != 0
}

// newConnectorFor defaults the namespace to the harness name without its
// dashes (a namespace is letters and digits only): claude-code becomes
// claudecode.
func newConnectorFor(harness, ns, exe string) *connector {
	if ns == "" {
		ns = strings.ReplaceAll(harness, "-", "")
	}
	return &connector{harness: harness, ns: ns, client: ClientFromURL(), user: userEnv(), bridge: exe,
		accept: connectAcceptSharedUser, dry: connectDryRun, probe: probeKey}
}

func (c *connector) connect(out io.Writer) error {
	// The generic harness (GitHub #86; LLD 21 §D2 "The generic harness
	// contract") has no config file connect knows how to edit: the person
	// pastes the printed entry. --print is its way of connecting, and only its.
	generic := c.harness == harnessGeneric
	switch {
	case generic && !c.print:
		return errors.New("connect generic writes no harness file; pass --print to mint the key and print the MCP entry to paste")
	case !generic && c.print:
		return fmt.Errorf("--print is for the generic harness (vornikctl agent connect generic --print); connect %s edits %s's own config", c.harness, c.harness)
	case !generic && c.clientKind != "":
		return fmt.Errorf("--client is for the generic harness; connect %s mints a %s key", c.harness, c.harness)
	}
	info := out
	if c.print && c.info != nil {
		info = c.info
	}
	cfgPath := ""
	if !generic {
		var err error
		if cfgPath, err = harnessconfig.Path(c.harness, c.user.harnessEnv()); err != nil {
			return err
		}
	}
	keyPath, err := agentbridge.KeyPath(c.user.configDir, c.ns)
	if err != nil {
		return err
	}
	if err := agentbridge.CheckEndpoint(c.client.baseURL); err != nil {
		return err
	}
	if c.keyStdin {
		key, err := readAPIKey(c.keyIn)
		if err != nil {
			return err
		}
		c.client.apiKey = key
	}
	// The assistant runs this path at every start and for every approval;
	// one it cannot execute fails there, not here (GitHub #99).
	if err := canExecute(c.bridge, c.user.uid, c.user.gids); err != nil {
		return fmt.Errorf("the assistant would run vornikctl from %s, which you cannot execute: %v. Install an executable copy where you can run it and pass --vornikctl <path> (https://docs.vornik.io/guides/hermes-setup/)", c.bridge, err)
	}
	caps, err := c.capabilities()
	if err != nil {
		return err
	}
	if !caps.Features["companion-admin"] {
		return errors.New("this Vornik does not offer agent administration: agent_admin.enabled is false, the agent templates are not installed (make install-config-assets), or the daemon is older than this vornikctl")
	}
	class := agentadmin.HarnessClassOf(c.harness)
	// A containerised daemon's paths are not this host's, so its store key
	// is never probed: the check could not be made (plan P6 amendment F2).
	// A relative path would be probed against this process's cwd, so it is
	// unknown too (review 03a6 F3).
	access := keyUnknown
	if caps.Host != nil && !caps.Host.DaemonContainerized && filepath.IsAbs(caps.Host.StoreKeyPath) {
		access = classify(c.probe(caps.Host.StoreKeyPath), caps.Host, c.user.uid)
	}
	ok, why := sharedUserGate(class, access, c.accept)
	if !ok && access == keyReadable {
		why = refusalMessage(c.harness, caps.Host != nil && caps.Host.DaemonUID == c.user.uid, caps.Host.StoreKeyPath)
	}
	printHarnessTable(info, c.harness, class, why)
	if caps.Host != nil && !caps.Host.DaemonContainerized && caps.Host.DaemonUID == c.user.uid {
		_, _ = fmt.Fprintf(info, "Note: Vornik runs as your OS user (UID %d).\n", c.user.uid)
	}
	if !ok {
		return &exitCodeError{code: 1}
	}
	entry := harnessconfig.Entry{Namespace: c.ns, Command: c.bridge, URL: c.client.baseURL}
	if generic {
		if c.dry {
			_, _ = fmt.Fprintf(info, "Would mint the agent key for namespace %q, store it in %s, and print the MCP entry %q.\n", c.ns, keyPath, entry.Name())
			return nil
		}
		return c.connectGeneric(out, info, entry, keyPath)
	}
	// Refuse a harness file the edit cannot handle before anything is
	// minted.
	current, err := readIfExists(cfgPath)
	if err != nil {
		return err
	}
	if _, err := harnessconfig.Set(c.harness, current, entry); err != nil {
		return fmt.Errorf("%s: %w", cfgPath, err)
	}
	if c.dry {
		_, _ = fmt.Fprintf(out, "Would mint the agent key for namespace %q, store it in %s, and add %q to %s.\n", c.ns, keyPath, entry.Name(), cfgPath)
		return nil
	}
	grant, err := c.mint()
	if err != nil {
		return err
	}
	rec := agentRecord{Namespace: c.ns, Harness: c.harness, KeyID: grant.ID, ProjectID: grant.ProjectID, ConfigPath: cfgPath}
	undo := func(cause error) error {
		_ = removeAgentFiles(keyPath)
		if rerr := revokeAgentKey(c.client, rec); rerr != nil {
			return fmt.Errorf("%w; revoking the key just minted (%s) also failed: %v — revoke it in the UI", cause, rec.KeyID, rerr)
		}
		return cause
	}
	if err := writeAgentFiles(keyPath, grant.Secret, rec, c.user.uid); err != nil {
		return undo(err)
	}
	if _, err := harnessconfig.Apply(cfgPath, func(b []byte) ([]byte, error) { return harnessconfig.Set(c.harness, b, entry) }, nil); err != nil {
		if errors.Is(err, harnessconfig.ErrChanged) {
			err = fmt.Errorf("%s kept changing while it was edited; close %s and run connect again", cfgPath, c.harness)
		}
		return undo(err)
	}
	_, _ = fmt.Fprintf(out, "\nConnected %s as namespace %q.\n  key file: %s (only you can read it)\n  added %q to %s\n\nNext: restart %s, then ask it what it can do with Vornik.\n",
		c.harness, c.ns, keyPath, entry.Name(), cfgPath, c.harness)
	if c.harness == harnessconfig.ClaudeCode || c.harness == harnessconfig.Codex {
		_, _ = fmt.Fprintf(out, "Update the vornik-companion plugin first for its vornik-admin skill (the same guidance also reaches %s through Vornik itself).\n", c.harness)
	}
	return nil
}

// harnessGeneric is the harness name for an assistant with no adapter
// (GitHub #86): connect mints the key and prints the MCP entry.
const harnessGeneric = "generic"

// genericEntry is what connect generic --print prints: the stdio entry that
// runs the bridge (the key stays in the key file), and the streamable-HTTP
// form for a harness that speaks it, whose bearer the person reads from the
// key file themselves. The key is never printed.
type genericEntry struct {
	MCPServers     map[string]genericStdio `json:"mcpServers"`
	StreamableHTTP genericHTTP             `json:"streamable_http"`
}

type genericStdio struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

type genericHTTP struct {
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
	KeyFile string            `json:"key_file"`
	Note    string            `json:"note"`
}

// connectGeneric mints the key, stores it and its record, and prints the
// entry. It writes no harness file.
func (c *connector) connectGeneric(out, info io.Writer, entry harnessconfig.Entry, keyPath string) error {
	grant, err := c.mint()
	if err != nil {
		return err
	}
	rec := agentRecord{Namespace: c.ns, Harness: c.harness, KeyID: grant.ID, ProjectID: grant.ProjectID}
	if err := writeAgentFiles(keyPath, grant.Secret, rec, c.user.uid); err != nil {
		_ = removeAgentFiles(keyPath)
		if rerr := revokeAgentKey(c.client, rec); rerr != nil {
			return fmt.Errorf("%w; revoking the key just minted (%s) also failed: %v — revoke it in the UI", err, rec.KeyID, rerr)
		}
		return err
	}
	doc := genericEntry{
		MCPServers: map[string]genericStdio{entry.Name(): {Command: entry.Command, Args: entry.Args()}},
		StreamableHTTP: genericHTTP{
			URL:     strings.TrimRight(c.client.baseURL, "/") + "/api/v1/mcp/companion",
			Headers: map[string]string{"Authorization": "Bearer <the contents of key_file>"},
			KeyFile: keyPath,
			Note:    "Prefer the stdio entry: the bridge reads the key file, so no config holds the key. Use streamable HTTP only if your assistant cannot run a command.",
		},
	}
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")
	if err := enc.Encode(doc); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(info, "\nConnected namespace %q (client kind %q).\n  key file: %s (only you can read it)\n\nPaste the printed entry into your assistant's MCP configuration, then restart it.\n",
		c.ns, c.kind(), keyPath)
	return nil
}

// kind is the client kind the key is minted with.
func (c *connector) kind() string {
	if c.clientKind != "" {
		return c.clientKind
	}
	return c.harness
}

func (c *connector) capabilities() (*capabilitiesView, error) {
	resp, err := c.client.Get("/api/v1/capabilities")
	if err != nil {
		return nil, fmt.Errorf("reach Vornik at %s: %w", c.client.baseURL, err)
	}
	if resp.StatusCode != http.StatusOK {
		return nil, ParseAPIError(resp)
	}
	defer func() { _ = resp.Body.Close() }()
	var v capabilitiesView
	if err := json.NewDecoder(resp.Body).Decode(&v); err != nil {
		return nil, fmt.Errorf("decode capabilities: %w", err)
	}
	return &v, nil
}

func (c *connector) mint() (*companionGrantOutput, error) {
	resp, err := c.client.Post("/api/v1/companion/grant", map[string]any{"clientKind": c.kind(), "agentAdmin": true, "namespace": c.ns})
	if err != nil {
		return nil, fmt.Errorf("grant: %w", err)
	}
	if resp.StatusCode != http.StatusCreated {
		err := ParseAPIError(resp)
		var apiErr *APIError
		if errors.As(err, &apiErr) {
			switch apiErr.Code {
			case "NO_APPROVER_DEVICE":
				return nil, errors.New("pair a phone first: vornikctl pair-device (it approves everything the assistant asks for)")
			case "NAMESPACE_TAKEN":
				return nil, fmt.Errorf("namespace %q is already connected: vornikctl agent disconnect %s first, or choose another --namespace", c.ns, c.ns)
			case "ADMIN_SCOPE_REQUIRED", "UNAUTHORIZED":
				return nil, errors.New("connect needs the operator's admin key (VORNIK_API_KEY, or vornikctl auth login)")
			}
		}
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	var out companionGrantOutput
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("decode grant: %w", err)
	}
	if out.Secret == "" || out.ID == "" {
		return nil, errors.New("the daemon returned no key")
	}
	return &out, nil
}

func printHarnessTable(out io.Writer, harness, class, why string) {
	_, _ = fmt.Fprintf(out, "%s is %s.\n", harness, map[string]string{
		agentadmin.HarnessMCPOnly:      "an MCP-only assistant",
		agentadmin.HarnessShellCapable: "an assistant that can run shell commands as you",
	}[class])
	for _, claim := range agentadmin.HarnessClaims(class) {
		_, _ = fmt.Fprintf(out, "  - %s\n", claim)
	}
	_, _ = fmt.Fprintf(out, "%s\n", why)
}

func readIfExists(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

// agentDir makes the key directory 0700 and refuses one that is a symlink
// or not the user's own (review 6f6b F11).
func agentDir(dir string, uid int) error {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	fi, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
		return fmt.Errorf("%s is not a directory (a symlink?); remove it and run connect again", dir)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && int(st.Uid) != uid {
		return fmt.Errorf("%s belongs to another user", dir)
	}
	return os.Chmod(dir, 0o700)
}

// writePrivate writes path 0600 through a temporary file and a rename, so a
// reader never sees a partial file and a planted symlink is replaced, not
// followed.
func writePrivate(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if err := tmp.Chmod(0o600); err == nil {
		if _, err = tmp.Write(data); err == nil {
			err = tmp.Sync()
		}
		if cerr := tmp.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = os.Rename(name, path)
		}
		if err == nil {
			return nil
		}
		_ = os.Remove(name)
		return err
	}
	_ = tmp.Close()
	_ = os.Remove(name)
	return err
}

func recordPath(keyPath string) string { return strings.TrimSuffix(keyPath, ".key") + ".json" }

func writeAgentFiles(keyPath, secret string, rec agentRecord, uid int) error {
	if err := agentDir(filepath.Dir(keyPath), uid); err != nil {
		return err
	}
	raw, _ := json.MarshalIndent(rec, "", "  ")
	if err := writePrivate(recordPath(keyPath), append(raw, '\n')); err != nil {
		return err
	}
	return writePrivate(keyPath, []byte(secret+"\n"))
}

func removeAgentFiles(keyPath string) error {
	err1 := os.Remove(keyPath)
	err2 := os.Remove(recordPath(keyPath))
	for _, err := range []error{err1, err2} {
		if err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
	}
	return nil
}

func revokeAgentKey(client *Client, rec agentRecord) error {
	resp, err := client.Delete("/api/v1/projects/" + rec.ProjectID + "/keys/" + rec.KeyID)
	if err != nil {
		return err
	}
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK {
		_ = resp.Body.Close()
		return nil
	}
	return ParseAPIError(resp)
}

func disconnectAgent(client *Client, user agentUser, ns string, out io.Writer) error {
	keyPath, err := agentbridge.KeyPath(user.configDir, ns)
	if err != nil {
		return err
	}
	raw, err := os.ReadFile(recordPath(keyPath))
	if err != nil {
		return fmt.Errorf("no connection record for %q at %s: %w", ns, recordPath(keyPath), err)
	}
	var rec agentRecord
	if err := json.Unmarshal(raw, &rec); err != nil || rec.Namespace != ns {
		return fmt.Errorf("%s is not a connection record for %q", recordPath(keyPath), ns)
	}
	if err := revokeAgentKey(client, rec); err != nil {
		return fmt.Errorf("revoke the key: %w (nothing else was changed)", err)
	}
	_, _ = fmt.Fprintf(out, "Revoked %s's key for namespace %q.\n", rec.Harness, ns)
	for _, path := range disconnectPaths(rec, user) {
		removed := false
		_, err := harnessconfig.Apply(path, func(b []byte) ([]byte, error) {
			o, r, err := harnessconfig.Remove(rec.Harness, b, ns)
			removed = r
			return o, err
		}, nil)
		switch {
		case err != nil:
			_, _ = fmt.Fprintf(out, "Could not remove the entry from %s: %v. Remove \"vornik-%s\" by hand.\n", path, err, ns)
		case removed:
			_, _ = fmt.Fprintf(out, "Removed \"vornik-%s\" from %s.\n", ns, path)
		}
	}
	if err := removeAgentFiles(keyPath); err != nil {
		return err
	}
	_, _ = fmt.Fprintf(out, "Deleted %s. The namespace's projects and approvals are kept.\n", keyPath)
	return nil
}

// disconnectPaths is where the entry may be: the file connect wrote, and the
// file the harness reads now, when that differs (its home moved). Only files
// that exist are listed, so disconnect never creates one (review
// 20261002-d930 F1, F3).
func disconnectPaths(rec agentRecord, user agentUser) []string {
	var out []string
	cands := []string{rec.ConfigPath}
	if p, err := harnessconfig.Path(rec.Harness, user.harnessEnv()); err == nil {
		cands = append(cands, p)
	}
	seen := map[string]bool{}
	for _, p := range cands {
		if p == "" || seen[p] {
			continue
		}
		seen[p] = true
		if _, err := os.Lstat(p); err == nil {
			out = append(out, p)
		}
	}
	return out
}
