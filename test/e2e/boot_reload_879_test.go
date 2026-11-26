//go:build e2e_http
// +build e2e_http

package e2e_test

// GitHub #879: the control plane's unconfirmed-reload record (#859) lives in
// memory, on the assumption that a restarted daemon loads its config afresh
// from disk at boot. These tests restart the real binary to check that, and
// show what happens when the boot load hangs (control-plane apply/rollback
// design, amendment 2026-11-23).
//
// Found at: review (design review 20261121-160d, finding 3); the user
// observes it across a daemon restart, so the test is at e2e-lane.
//
// Each test boots its own daemon from the lane's Community binary, with its
// own config tree, data directory and SQLite file. testenv.Daemon builds
// its environment from an allowlist that holds no VORNIK_* variable, so the
// lane's daemon is untouched.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
	sqliterepo "vornik.io/vornik/internal/persistence/sqlite"
	"vornik.io/vornik/internal/testutil/testenv"
)

// daemon879 is one booted daemon process and the files it reads.
type daemon879 struct {
	work, cfgFile, configsTree, dbFile, base string
	cmd                                      *exec.Cmd
	exited                                   chan struct{}
	// env is added to the daemon's environment (#898: NOTIFY_SOCKET).
	env []string
}

// layout879 lays out a fresh config tree, config.yaml and database path for
// one test's daemon.
func layout879(t *testing.T) *daemon879 {
	t.Helper()
	work := t.TempDir()
	d := &daemon879{
		work:        work,
		cfgFile:     filepath.Join(work, "vornik.yaml"),
		configsTree: filepath.Join(work, "configs"),
		dbFile:      filepath.Join(work, "vornik.db"),
	}
	port := freePort()
	d.base = fmt.Sprintf("http://127.0.0.1:%d", port)
	if err := layoutConfigTree(d.configsTree, d.cfgFile, d.dbFile, work, port); err != nil {
		t.Fatal(err)
	}
	return d
}

// start boots the daemon on d's files. The log is appended to, so a restart
// keeps the first boot's lines.
func (d *daemon879) start(t *testing.T) {
	t.Helper()
	log, err := os.OpenFile(filepath.Join(d.work, "daemon.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(e2eBin)
	cmd.Dir = d.work
	cmd.Env = testenv.Daemon(filepath.Join(d.work, "data"),
		append([]string{"VORNIK_CONFIG=" + d.cfgFile, "VORNIK_CONFIGS_DIR=" + d.configsTree}, d.env...)...)
	for _, kv := range cmd.Env {
		switch k, _, _ := strings.Cut(kv, "="); {
		case !strings.HasPrefix(k, "VORNIK_"), k == "VORNIK_DATA_DIR", k == "VORNIK_CONFIG", k == "VORNIK_CONFIGS_DIR":
		default:
			t.Fatalf("the test daemon inherited %s; its environment must name only its own tree", k)
		}
	}
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	d.cmd, d.exited = cmd, make(chan struct{})
	exited := d.exited
	go func() { _ = cmd.Wait(); _ = log.Close(); close(exited) }()
	t.Cleanup(func() { d.stop(t) })
}

// stop sends SIGTERM and waits for the exit, killing after 15 s.
func (d *daemon879) stop(t *testing.T) {
	t.Helper()
	select {
	case <-d.exited:
		return
	default:
	}
	_ = d.cmd.Process.Signal(syscall.SIGTERM)
	select {
	case <-d.exited:
	case <-time.After(15 * time.Second):
		_ = d.cmd.Process.Kill()
		<-d.exited
	}
}

func (d *daemon879) waitHealthy(t *testing.T) {
	t.Helper()
	if err := waitHealthy(d.base, 120*time.Second); err != nil {
		t.Fatalf("the daemon never became healthy: %v\n%s", err, d.log())
	}
}

func (d *daemon879) log() string {
	b, _ := os.ReadFile(filepath.Join(d.work, "daemon.log"))
	return string(b)
}

func (d *daemon879) do(t *testing.T, method, path, body string) (int, string) {
	t.Helper()
	var r io.Reader
	if body != "" {
		r = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, d.base+path, r)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := (&http.Client{Timeout: 60 * time.Second}).Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// restoreReloadCheck runs the doctor and returns control_plane_restore_reload.
func (d *daemon879) restoreReloadCheck(t *testing.T) (status, message string) {
	t.Helper()
	code, body := d.do(t, http.MethodPost, "/api/v1/doctor", "")
	if code != http.StatusOK {
		t.Fatalf("doctor: HTTP %d %s", code, body)
	}
	var rep struct {
		Checks []struct{ Name, Status, Message string }
	}
	if err := json.Unmarshal([]byte(body), &rep); err != nil {
		t.Fatalf("doctor body: %v (%s)", err, body)
	}
	for _, c := range rep.Checks {
		if c.Name == "control_plane_restore_reload" {
			return c.Status, c.Message
		}
	}
	t.Fatalf("doctor has no control_plane_restore_reload check: %s", body)
	return "", ""
}

func (d *daemon879) hasProject(t *testing.T, id string) bool {
	t.Helper()
	code, body := d.do(t, http.MethodGet, "/api/v1/projects", "")
	if code != http.StatusOK {
		t.Fatalf("projects: HTTP %d %s", code, body)
	}
	return strings.Contains(body, `"`+id+`"`)
}

// projectFiles879 renders the e2e template's project, swarm and workflow for
// id, keyed by their paths under the configs tree.
func projectFiles879(id string) map[string]string {
	r := strings.NewReplacer("{{.projectId}}", id, "{{.displayName}}", "E2E "+id, "{{.topic}}", "restarts")
	return map[string]string{
		filepath.Join("projects", id+".yaml"):   r.Replace(e2eProjectTmpl),
		filepath.Join("swarms", id+"-swarm.md"): r.Replace(e2eSwarmTmpl),
		filepath.Join("workflows", id+"-wf.md"): r.Replace(e2eWorkflowTmpl),
	}
}

// TestBootReload879_RestartForgetsTheRecordAndReadsTheDisk: a daemon whose
// restoring reload was refused reports it; after a restart it reports no
// record, and serves what the files held when it booted.
func TestBootReload879_RestartForgetsTheRecordAndReadsTheDisk(t *testing.T) {
	d := layout879(t)
	d.start(t)
	d.waitHealthy(t)
	if status, msg := d.restoreReloadCheck(t); status != "OK" {
		t.Fatalf("a fresh daemon: control_plane_restore_reload = %s %q, want OK", status, msg)
	}

	// A malformed swarm in the live tree: every reload from here is refused,
	// the boot registry keeps serving.
	broken := filepath.Join(d.configsTree, "swarms", "e2e-879-broken.md")
	if err := os.WriteFile(broken, []byte("---\nswarmId: [unclosed\n---\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Apply a proposal: its forward reload is refused, the writes are
	// reversed, and the restoring reload is refused as well.
	const applied = "e2e-879-applied"
	proposal := `{"raw":{"projectId":"` + applied + `","displayName":"E2E 879","topic":"restarts"}}`
	sid := seedSession879(t, d.dbFile, &persistence.ProjectWizardSession{
		ID:                sessionID("879"),
		OperatorID:        e2eOperatorID,
		Transcript:        []byte("[]"),
		CurrentProposal:   []byte(proposal),
		SuggestedTemplate: e2eTemplateSlug,
		ReadyToCommit:     true,
	})
	code, body := d.do(t, http.MethodPost, "/api/v1/projects/wizard/"+sid+"/commit", "")
	if code != http.StatusOK {
		t.Fatalf("commit: HTTP %d %s", code, body)
	}
	var cr struct {
		ProposalID string `json:"proposal_id"`
	}
	if err := json.Unmarshal([]byte(body), &cr); err != nil || cr.ProposalID == "" {
		t.Fatalf("commit body: %v (%s)", err, body)
	}
	if code, b := d.do(t, http.MethodPost, "/api/v1/operator/proposals/"+cr.ProposalID+"/decide",
		`{"decision":"approve","actor":"e2e-reviewer"}`); code != http.StatusOK {
		t.Fatalf("approve: HTTP %d %s", code, b)
	}
	code, body = d.do(t, http.MethodPost, "/api/v1/operator/proposals/"+cr.ProposalID+"/apply", `{"actor":"e2e-reviewer"}`)
	if code != http.StatusUnprocessableEntity || !strings.Contains(body, "APPLY_FAILED") {
		t.Fatalf("apply over a tree that refuses every reload: HTTP %d %s, want 422 APPLY_FAILED", code, body)
	}
	if _, err := os.Stat(filepath.Join(d.configsTree, "projects", applied+".yaml")); !os.IsNotExist(err) {
		t.Fatalf("the refused apply's project file was not reversed: err=%v", err)
	}
	status, msg := d.restoreReloadCheck(t)
	if status != "ERROR" || !strings.Contains(msg, cr.ProposalID) || !strings.Contains(msg, "rejected") {
		t.Fatalf("after a refused restoring reload: control_plane_restore_reload = %s %q, want ERROR naming %s, rejected",
			status, msg, cr.ProposalID)
	}

	// Down: fix the tree and add a project the first process never saw.
	d.stop(t)
	if err := os.Remove(broken); err != nil {
		t.Fatal(err)
	}
	const later = "e2e-879-later"
	for rel, body := range projectFiles879(later) {
		if err := os.WriteFile(filepath.Join(d.configsTree, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Up again, same binary, config and database.
	d.start(t)
	d.waitHealthy(t)
	if status, msg := d.restoreReloadCheck(t); status != "OK" || strings.Contains(msg, cr.ProposalID) {
		t.Fatalf("after a restart: control_plane_restore_reload = %s %q, want OK not naming %s (the record is per process)",
			status, msg, cr.ProposalID)
	}
	if !d.hasProject(t, later) {
		t.Fatalf("after a restart: project %s, written while the daemon was down, is not loaded", later)
	}
	if !d.hasProject(t, e2eStaticProject) {
		t.Fatalf("after a restart: the boot project %s is not loaded", e2eStaticProject)
	}
}

// TestBootReload879_AHungBootLoadHangsTheBoot: a config file whose read
// blocks stops the boot there. No bound fires: the process stays alive and
// answers nothing until the read completes, then loads what it read.
func TestBootReload879_AHungBootLoadHangsTheBoot(t *testing.T) {
	const hung = "e2e-879-hung"
	d := layout879(t)
	h := d.startWithHungProject(t, hung)

	// The boot is now blocked in the read. For 5 s: alive, and no answer.
	client := &http.Client{Timeout: time.Second}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case <-d.exited:
			t.Fatalf("the daemon exited while its boot load was blocked:\n%s", d.log())
		default:
		}
		if resp, err := client.Get(d.base + "/healthz"); err == nil {
			resp.Body.Close()
			t.Fatalf("the daemon answered /healthz (%d) while its boot load was blocked", resp.StatusCode)
		}
		time.Sleep(250 * time.Millisecond)
	}

	h.release(t)
	d.waitHealthy(t)
	if !d.hasProject(t, hung) {
		t.Fatalf("after the blocked read completed: project %s is not loaded", hung)
	}
}

// hungProject879 is a project whose file is a named pipe the daemon's boot is
// blocked reading.
type hungProject879 struct {
	d     *daemon879
	pipe  string
	body  string
	write *os.File
}

// startWithHungProject lays out project id with its project file as a named
// pipe, boots the daemon, and returns once the boot has opened the pipe: the
// boot is then blocked in the read.
func (d *daemon879) startWithHungProject(t *testing.T, id string) *hungProject879 {
	t.Helper()
	files := projectFiles879(id)
	projectRel := filepath.Join("projects", id+".yaml")
	for rel, body := range files {
		if rel == projectRel {
			continue
		}
		if err := os.WriteFile(filepath.Join(d.configsTree, rel), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The project file is a named pipe: a read of it blocks until a writer
	// writes and closes.
	pipe := filepath.Join(d.configsTree, projectRel)
	if err := syscall.Mkfifo(pipe, 0o644); err != nil {
		t.Fatal(err)
	}
	d.start(t)

	// Opening the pipe for writing returns once the daemon has opened it for
	// reading: the boot has reached the load.
	opened := make(chan *os.File, 1)
	go func() {
		w, err := os.OpenFile(pipe, os.O_WRONLY, 0)
		if err != nil {
			close(opened)
			return
		}
		opened <- w
	}()
	var w *os.File
	select {
	case w = <-opened:
	case <-d.exited:
		t.Fatalf("the daemon exited before it read the pipe:\n%s", d.log())
	case <-time.After(120 * time.Second):
		t.Fatalf("the daemon never opened the pipe in 120 s:\n%s", d.log())
	}
	if w == nil {
		t.Fatal("opening the pipe for writing failed")
	}
	t.Cleanup(func() { _ = w.Close() })
	return &hungProject879{d: d, pipe: pipe, body: files[projectRel], write: w}
}

// release lets the blocked boot finish: a regular file takes the pipe's place
// for every later read, and the blocked read gets the same project.
func (h *hungProject879) release(t *testing.T) {
	t.Helper()
	tmp := filepath.Join(h.d.work, "hung-project.yaml")
	if err := os.WriteFile(tmp, []byte(h.body), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, h.pipe); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(h.write, h.body); err != nil {
		t.Fatal(err)
	}
	if err := h.write.Close(); err != nil {
		t.Fatal(err)
	}
}

// seedSession879 inserts a wizard session into the database at dbFile, as
// seedSession does for the lane's daemon.
func seedSession879(t *testing.T, dbFile string, s *persistence.ProjectWizardSession) string {
	t.Helper()
	db, err := sql.Open("sqlite", dbFile+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	defer db.Close()
	if err := sqliterepo.NewProjectWizardSessionRepository(db).Insert(context.Background(), s); err != nil {
		t.Fatalf("seed session: %v", err)
	}
	return s.ID
}
