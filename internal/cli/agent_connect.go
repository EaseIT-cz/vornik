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
)

var agentConnectCmd = &cobra.Command{
	Use:   "connect <hermes|claude-desktop|claude-code|codex>",
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
pass --accept-shared-user.`,
	Args:         cobra.ExactArgs(1),
	SilenceUsage: true,
	RunE: func(cmd *cobra.Command, args []string) error {
		c, err := newConnector(args[0])
		if err != nil {
			return err
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
	agentDisconnectCmd.Flags().StringVar(&agentURL, "url", "", "Vornik's URL (default $VORNIK_API_URL, then "+DefaultAPIURL+")")
	agentCmd.AddCommand(agentConnectCmd, agentDisconnectCmd)
}

// agentUser is the invoking user's side of a connection.
type agentUser struct {
	home, configDir string
	getenv          func(string) string
	uid             int
}

func userEnv() agentUser {
	home, _ := os.UserHomeDir()
	cfg, _ := os.UserConfigDir()
	return agentUser{home: home, configDir: cfg, getenv: os.Getenv, uid: os.Getuid()}
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
)

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
	Host     *struct {
		DaemonUID           int    `json:"daemon_uid"`
		DaemonContainerized bool   `json:"daemon_containerized"`
		StoreKeyPath        string `json:"store_key_path"`
	} `json:"host"`
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
}

func newConnector(harness string) (*connector, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("find vornikctl's own path: %w", err)
	}
	return newConnectorFor(harness, agentNamespace, exe), nil
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
	cfgPath, err := harnessconfig.Path(c.harness, c.user.harnessEnv())
	if err != nil {
		return err
	}
	keyPath, err := agentbridge.KeyPath(c.user.configDir, c.ns)
	if err != nil {
		return err
	}
	if err := agentbridge.CheckEndpoint(c.client.baseURL); err != nil {
		return err
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
	access := keyAbsent
	if caps.Host != nil && !caps.Host.DaemonContainerized {
		access = c.probe(caps.Host.StoreKeyPath)
	}
	ok, why := sharedUserGate(class, access, c.accept)
	printHarnessTable(out, c.harness, class, why)
	if caps.Host != nil && !caps.Host.DaemonContainerized && caps.Host.DaemonUID == c.user.uid {
		_, _ = fmt.Fprintf(out, "Note: Vornik runs as your OS user (UID %d).\n", c.user.uid)
	}
	if !ok {
		return errors.New("not connected")
	}
	entry := harnessconfig.Entry{Namespace: c.ns, Command: c.bridge, URL: c.client.baseURL}
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
	resp, err := c.client.Post("/api/v1/companion/grant", map[string]any{"clientKind": c.harness, "agentAdmin": true, "namespace": c.ns})
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
