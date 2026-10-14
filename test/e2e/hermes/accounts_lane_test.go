//go:build e2e_hermes

package hermes

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
	"vornik.io/vornik/internal/approval"
	"vornik.io/vornik/internal/testutil/testenv"
)

// accountsReceived requires the exact emitted target, canonical arguments and
// a native-ID-linked received object. CLI prose and offered schemas never enter.
func accountsReceived(e *callEvidence, name string, args map[string]any, field string) (map[string]any, error) {
	want, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	for _, c := range e.pairs() {
		full := c.Name
		if c.Namespace != "" {
			full = c.Namespace + "__" + c.Name
		}
		if full != name || c.ID == "" || c.Output == "" {
			continue
		}
		var actual any
		if json.Unmarshal([]byte(c.Args), &actual) != nil {
			continue
		}
		got, _ := json.Marshal(actual)
		if string(got) != string(want) {
			continue
		}
		if out := evidenceObject(c.Output, field); out != nil {
			return out, nil
		}
	}
	return nil, fmt.Errorf("no native-ID-linked received result for exact tool %s", name)
}
func TestAccountsEvidence(t *testing.T) {
	args := map[string]any{"account": map[string]any{"label": "work"}, "name": "mail-work"}
	want := "mcp__vornik-codex__add_mcp_server"
	encoded := mustJSON(args)
	makeEvidence := func(name, body string, link bool) *callEvidence {
		e := &callEvidence{}
		e.emit(nativeCall{ID: "native1", Name: name, Args: encoded})
		if link {
			e.received("native1", body)
		}
		return e
	}
	t.Run("positive", func(t *testing.T) {
		if _, err := accountsReceived(makeEvidence(want, `{"effect":"awaiting_approval"}`, true), want, args, "effect"); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("namespace-positive", func(t *testing.T) {
		e := &callEvidence{}
		e.emit(nativeCall{ID: "namespace1", Namespace: "mcp__vornik-codex", Name: "add_mcp_server", Args: encoded})
		e.received("namespace1", `{"effect":"awaiting_approval"}`)
		if _, err := accountsReceived(e, want, args, "effect"); err != nil {
			t.Fatal(err)
		}
	})
	for _, name := range []string{"wrong-target", "wrong-args", "wrong-namespace", "unlinked", "prose", "reset-history"} {
		t.Run(name, func(t *testing.T) {
			e := makeEvidence(want, `{"effect":"awaiting_approval"}`, true)
			expectedArgs := args
			switch name {
			case "wrong-target":
				e = makeEvidence("mcp__other-codex__add_mcp_server", `{"effect":"awaiting_approval"}`, true)
			case "wrong-args":
				e = makeEvidence(want, `{"effect":"awaiting_approval"}`, true)
				expectedArgs = map[string]any{"account": "personal"}
			case "wrong-namespace":
				e = &callEvidence{}
				e.emit(nativeCall{ID: "namespace1", Namespace: "mcp__other-codex", Name: "add_mcp_server", Args: encoded})
				e.received("namespace1", `{"effect":"awaiting_approval"}`)
			case "unlinked":
				e = makeEvidence(want, `{"effect":"awaiting_approval"}`, false)
			case "prose":
				e = makeEvidence(want, "The approval is waiting.", true)
			case "reset-history":
				e.reset()
				e.received("native1", `{"effect":"awaiting_approval"}`)
			}
			if _, err := accountsReceived(e, want, expectedArgs, "effect"); err == nil {
				t.Fatal("invalid model execution evidence accepted")
			}
		})
	}
}

const accountsWorkToken = "B11_WORK_CREDENTIAL_CANARY_c871"
const accountsPersonalToken = "B11_PERSONAL_CREDENTIAL_CANARY_730e"
const accountsWorkCalendarToken = "B11_WORK_CALENDAR_CANARY_883c"
const accountsPersonalCalendarToken = "B11_PERSONAL_CALENDAR_CANARY_07f0"

func accountsSafe(text string) string {
	return strings.NewReplacer(accountsWorkToken, "[work credential canary]", accountsPersonalToken, "[personal credential canary]", accountsWorkCalendarToken, "[work calendar credential canary]", accountsPersonalCalendarToken, "[personal calendar credential canary]").Replace(text)
}

type accountsClient struct {
	ns, home      string
	trace         strings.Builder
	invoke        func(string, map[string]any, string) map[string]any
	finalTrace    func() string
	sweepQuiet    map[string]string
	producerTrace *accountsHTTPTrace
}

func (c *accountsClient) call(tool string, args map[string]any, field string) map[string]any {
	return c.invoke(tool, args, field)
}
func (c *accountsClient) effect(t *testing.T, tool string, args map[string]any, want string) {
	t.Helper()
	out := c.call(tool, args, "effect")
	if out["effect"] != want {
		reason, _ := out["reason"].(string)
		category := "unclassified"
		for _, known := range []string{"not an approved write", "args_schema", "input", "egress", "standing", "not loaded", "not approved", "waiting for approval", "workflow ID", "required", "plain", "reference", "unknown"} {
			if strings.Contains(reason, known) {
				category = known
				break
			}
		}
		t.Fatalf("%s expected effect %s; refusal category=%s", tool, want, category)
	}
}
func accountsMailStub(label, token string) *MCPStub {
	s := NewMCPStub("mail-"+label, MCPTool{Name: "mail_search", Handle: func(json.RawMessage) (string, bool) {
		return `{"messages":[{"id":"synthetic-mail","subject":"Reply needed"}]}`, false
	}}, MCPTool{Name: "send", Handle: func(json.RawMessage) (string, bool) { return `{"sent":true}`, false }})
	s.Token = token
	return s
}
func accountsMailScripts() []Script {
	var scripts []Script
	for _, label := range []string{"work", "personal"} {
		proposal := map[string]any{"action": "send", "args": map[string]any{"to": "same@destination.example", "subject": "Reply", "body": "Confirmed."}}
		scripts = append(scripts, Script{Marker: "b11-mail-" + label, Final: "Reply proposed.", Steps: []ScriptStep{
			{ToolSuffix: "mail-" + label + "__mail_search", Args: map[string]any{"query": "reply"}},
			{ToolSuffix: "file_write", Args: map[string]any{"path": "artifacts/out/propose-send.json", "content": mustJSON(proposal)}},
			{ToolSuffix: "file_write", Args: map[string]any{"path": "artifacts/out/result.json", "content": `{"proposed":1}`}},
		}})
	}
	return scripts
}
func accountsApproveSetup(t *testing.T, p *Phone, label, value string) {
	t.Helper()
	pending := pendingOnPhone(t, p, 1, slow(time.Minute))
	if len(pending) == 0 {
		t.Fatal("account setup reached no paired-device request")
	}
	for _, req := range pending {
		page, err := p.get("/ui/approve/" + req.ID)
		if err != nil {
			t.Fatal(accountsSafe(err.Error()))
		}
		if label != "" && !strings.Contains(page, "your "+label+" account") {
			t.Fatalf("paired-device page omitted %s account", label)
		}
		entry := ""
		if value != "" && accountsCredentialRequest(req.Sentence, "MAIL_TOKEN_"+strings.ToUpper(label)) && strings.Contains(page, `name="value"`) && strings.Contains(page, `type="password"`) {
			entry = value
		}
		if value != "" && entry == "" {
			t.Fatal("mail credential request did not match named password form")
		}
		if err = p.Approve(req.ID, entry); err != nil {
			t.Fatal(accountsSafe(err.Error()))
		}
	}
	// Tool discovery after credential entry may file a separate approval.
	for quiet := time.Now(); time.Since(quiet) < slow(15*time.Second); {
		pending, err := p.Pending()
		if err != nil {
			t.Fatal("paired-device pending query failed")
		}
		if len(pending) == 0 {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		for _, req := range pending {
			page, err := p.get("/ui/approve/" + req.ID)
			if err != nil {
				t.Fatal("paired-device discovery page failed")
			}
			if label != "" && !strings.Contains(page, "your "+label+" account") {
				t.Fatal("discovery page omitted expected account")
			}
			if err := p.Approve(req.ID, ""); err != nil {
				t.Fatal("paired-device tool discovery approval failed")
			}
		}
		quiet = time.Now()
	}
}
func accountsDefineMail(t *testing.T, c *accountsClient, p *Phone) {
	roles := []map[string]any{}
	for _, label := range []string{"work", "personal"} {
		roles = append(roles, map[string]any{"name": "reader-" + label, "instructions": "Read the mailbox and propose a reply.", "tools": []string{"mcp__mail-" + label + "__mail_search", "file_write"}})
	}
	c.effect(t, "define_swarm", map[string]any{"slug": "comms", "roles": roles}, "applied")
	for _, label := range []string{"work", "personal"} {
		c.effect(t, "define_workflow", map[string]any{"project": "comms", "slug": "reply-" + label, "purpose": "Reply from the " + label + " account",
			"steps":  []map[string]any{{"name": "reply", "role": "reader-" + label, "instructions": "b11-mail-" + label + " Read mail and propose a reply."}},
			"inputs": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"run"}, "properties": map[string]any{"run": map[string]any{"enum": []string{"first", "covered", "same-key"}}}},
			"egress": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"proposed"}, "properties": map[string]any{"proposed": map[string]any{"type": "integer"}}},
			"proposes": []map[string]any{{"action": "send", "tool": "mcp__mail-" + label + "-write__send", "standing": map[string]any{"key": []string{"to"}, "max_days": 7, "max_uses": 20}, "args_schema": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"to", "subject", "body"}, "properties": map[string]any{
				"to": map[string]any{"type": "string", "pattern": "^[^@ ]+@[^@ ]+$", "maxLength": 100, "x-destination": true}, "subject": map[string]any{"type": "string", "maxLength": 100, "x-untrusted": true}, "body": map[string]any{"type": "string", "maxLength": 512, "x-untrusted": true},
			}}}},
		}, "awaiting_approval")
		accountsApproveSetup(t, p, "", "")
	}
}
func accountsDelegate(t *testing.T, c *accountsClient, label, run string) string {
	t.Helper()
	out := c.call("delegate", map[string]any{"workflow": c.ns + "--comms--reply-" + label, "inputs": map[string]any{"run": run}}, "task_id")
	id, _ := out["task_id"].(string)
	if id == "" {
		t.Fatal("actual client delegate returned no task ID")
	}
	for deadline := time.Now().Add(slow(3 * time.Minute)); time.Now().Before(deadline); {
		result := c.call("result", map[string]any{"task_id": id, "wait_seconds": 25}, "status")
		if result["complete"] == true {
			if result["status"] != "COMPLETED" {
				t.Fatal("account reply workflow did not complete successfully")
			}
			return id
		}
	}
	t.Fatal("actual client received no completed account result")
	return ""
}
func accountsSweep(t *testing.T, s *stack, c *accountsClient, tokens []string) {
	t.Helper()
	sw := NewSweep(tokens, nil)
	sw.AddText("actual client protocol/model/stdout/stderr", true, c.trace.String())
	if c.producerTrace != nil {
		sw.AddText("complete producer HTTP boundary", true, c.producerTrace.String())
	}
	if c.finalTrace != nil {
		sw.AddText("complete adapter transcript and stderr", true, c.finalTrace())
	}
	sw.AddDir("client home excluding only exact scoped key", true, accountsWithoutScopedKey(t, c.home, c.ns))
	quiet := map[string]string{"secret_redaction_audit": "this arm has no hostile producer"}
	for table, why := range c.sweepQuiet {
		quiet[table] = why
	}
	sweepVornik(t, s, sw, quiet)
	t.Logf("B11 canary sweep: %s", accountsSafe(sw.Report()))
	if violations := sw.Violations(); len(violations) > 0 {
		t.Fatalf("B11 canary violations: %s", accountsSafe(strings.Join(violations, "\n")))
	}
}
func accountsRunMail(t *testing.T, s *stack, c *accountsClient, p *Phone) {
	work := accountsMailStub("work", accountsWorkToken)
	personal := accountsMailStub("personal", accountsPersonalToken)
	mux := http.NewServeMux()
	mux.Handle("/mail-work", work)
	mux.Handle("/mail-personal", personal)
	base := fmt.Sprintf("http://127.0.0.1:%d", serveLoopback(t, mux))
	s.llm.SetScripts(accountsMailScripts()...)
	c.effect(t, "create_project", map[string]any{"slug": "comms", "purpose": "Two account mail"}, "applied")
	for _, label := range []string{"work", "personal"} {
		credential := "MAIL_TOKEN_" + strings.ToUpper(label)
		c.effect(t, "add_mcp_server", map[string]any{"project": "comms", "name": "mail-" + label, "url": base + "/mail-" + label, "auth": map[string]any{"mode": "static", "credential": credential}, "write_tools": []string{"send"}, "account": map[string]any{"label": label}}, "awaiting_approval")
		accountsApproveSetup(t, p, label, "")
		c.effect(t, "request_credential", map[string]any{"project": "comms", "name": credential, "purpose": "Read and send mail", "kind": "secret"}, "awaiting_approval")
		value := accountsWorkToken
		if label == "personal" {
			value = accountsPersonalToken
		}
		accountsApproveSetup(t, p, label, value)
	}
	accountsDefineMail(t, c, p)
	first := accountsDelegate(t, c, "work", "first")
	pending := pendingOnPhone(t, p, 1, slow(time.Minute))
	if len(pending) != 1 {
		t.Fatal("work proposal did not produce exactly one phone approval")
	}
	page, err := p.get("/ui/approve/" + pending[0].ID)
	if err != nil {
		t.Fatal(accountsSafe(err.Error()))
	}
	if !strings.Contains(page, "from your work account") || !strings.Contains(page, "mail-work") || !strings.Contains(page, "send") {
		t.Fatal("work phone page omitted send/connection/account")
	}
	if countTool(work, "send") != 0 || countTool(personal, "send") != 0 {
		t.Fatal("account send preceded phone approval")
	}
	if err = p.ApproveWithGrant(pending[0].ID, 7, 20); err != nil {
		t.Fatal(accountsSafe(err.Error()))
	}
	waitFor(t, "first work send", slow(time.Minute), func() bool { return countTool(work, "send") == 1 })
	db := laneDB(t, s)
	var grant string
	var uses int
	if err = db.QueryRow(`SELECT id,uses_left FROM broker_standing_grants WHERE namespace=$1 AND workflow_id=$2`, c.ns, c.ns+"--comms--reply-work").Scan(&grant, &uses); err != nil {
		t.Fatal(err)
	}
	second := accountsDelegate(t, c, "work", "covered")
	if second == first {
		t.Fatal("fresh work call unexpectedly reused task")
	}
	waitFor(t, "covered work send", slow(time.Minute), func() bool { return countTool(work, "send") == 2 })
	if pending := pendingOnPhone(t, p, 0, slow(3*time.Second)); len(pending) != 0 {
		t.Fatal("covered work write required phone approval")
	}
	var coveredUses int
	if err = db.QueryRow(`SELECT uses_left FROM broker_standing_grants WHERE id=$1`, grant).Scan(&coveredUses); err != nil || coveredUses != uses-1 {
		t.Fatal("covered send did not consume exactly one standing use")
	}
	personalTask := accountsDelegate(t, c, "personal", "same-key")
	pending = pendingOnPhone(t, p, 1, slow(time.Minute))
	if len(pending) != 1 {
		t.Fatal("personal account proposal not pending")
	}
	page, err = p.get("/ui/approve/" + pending[0].ID)
	if err != nil {
		t.Fatal(accountsSafe(err.Error()))
	}
	if !strings.Contains(page, "from your personal account") || !strings.Contains(page, "mail-personal") {
		t.Fatal("personal phone page omitted account/connection")
	}
	time.Sleep(slow(3 * time.Second))
	if countTool(personal, "send") != 0 || countTool(work, "send") != 2 {
		t.Fatal("work grant authorized personal account send")
	}
	var finalUses int
	if err = db.QueryRow(`SELECT uses_left FROM broker_standing_grants WHERE id=$1`, grant).Scan(&finalUses); err != nil || finalUses != coveredUses {
		t.Fatal("personal refusal changed work grant uses")
	}
	if countTool(work, "mail_search") != 2 || countTool(personal, "mail_search") != 1 || work.Refused() != 0 || personal.Refused() != 0 {
		t.Fatal("account read/authentication counters differ from work=2 personal=1 refused=0")
	}
	for _, want := range []struct{ task, status, approver string }{{first, "executed", ""}, {second, "executed", "grant:" + grant}, {personalTask, "pending", ""}} {
		var status, approver string
		waitFor(t, "persisted account action disposition", slow(time.Minute), func() bool {
			return db.QueryRow(`SELECT status,COALESCE(approver,'') FROM broker_actions WHERE task_id=$1 AND action_kind='send'`, want.task).Scan(&status, &approver) == nil && status == want.status
		})
		if status != want.status || (want.approver != "" && approver != want.approver) || (want.task == first && (approver == "" || strings.HasPrefix(approver, "grant:"))) || (want.task == personalTask && approver != "") {
			t.Fatal("persisted account action lifecycle or approver differs")
		}
	}
	if err = p.Reject(pending[0].ID); err != nil {
		t.Fatal(accountsSafe(err.Error()))
	}
	accountsAssertDevice(t, s, p, pending[0].ID, []string{accountsWorkToken, accountsPersonalToken})
	t.Log("B11 account isolation: work seed+covered sends=2, personal pending/send=0; same destination/key values; work grant unchanged by personal")
}
func TestAccountsA1_ProtocolLevel(t *testing.T) {
	requireLaneAgentImage(t)
	s, producerTrace := accountsStartStack(t)
	p := pairPhone(t, s)
	env := newAgentEnv(t)
	cmd := exec.Command(s.ctl, "agent", "connect", "claude-desktop", "--url", s.apiURL)
	cmd.Env = env.vars("VORNIK_API_KEY=" + s.adminKey)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("connect: %v %s", err, accountsSafe(string(out)))
	}
	b := startBridge(t, env, filepath.Join(env.cfg, "Claude", "claude_desktop_config.json"), "vornik-claudedesktop")
	if _, err := b.Call("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "B11 protocol stand-in"}}); err != nil {
		t.Fatal(err)
	}
	_ = b.Notify("notifications/initialized")
	c := &accountsClient{ns: "claudedesktop", home: env.home, producerTrace: producerTrace}
	defer accountsSweep(t, s, c, []string{accountsWorkToken, accountsPersonalToken})
	c.finalTrace = func() string { return b.Transcript() + "\n" + b.stderr.String() }
	c.invoke = func(tool string, args map[string]any, field string) map[string]any {
		text, isErr, err := b.Tool(tool, args)
		c.trace.WriteString(mustJSON(args) + "\n" + text + "\n")
		if err != nil || isErr {
			t.Fatalf("bridge %s failed: %v %s", tool, err, accountsSafe(text))
		}
		out := evidenceObject(text, field)
		if out == nil {
			t.Fatalf("bridge %s has no received %s", tool, field)
		}
		return out
	}
	t.Log("Actual connect-written bridge; Claude Desktop protocol stand-in, no vendor CLI claim")
	accountsRunMail(t, s, c, p)
}

// accountsTarget selects the full offered target from the connect-written
// server. Receipt validation subsequently uses this exact name, never suffixes.
func accountsTarget(t *testing.T, tools []string, client, tool string) string {
	t.Helper()
	target, err := accountsFindTarget(tools, client, tool)
	if err != nil {
		t.Fatal(err)
	}
	return target
}
func accountsFindTarget(tools []string, client, tool string) (string, error) {
	target := "mcp__vornik_" + client + "__" + tool
	for _, name := range tools {
		if name == target {
			return target, nil
		}
	}
	return "", fmt.Errorf("canonical connected %s tool %s was not offered", client, tool)
}
func TestAccountsTargetSelection(t *testing.T) {
	for _, client := range []string{"hermes", "codex"} {
		want := "mcp__vornik_" + client + "__delegate"
		foreign := []string{"mcp__foreign_vornik_" + client + "__" + client + "__delegate", "mcp__vornik_other__" + client + "__delegate", "mcp__vornik_" + client + "__other__delegate"}
		for _, name := range foreign {
			if _, err := accountsFindTarget([]string{name}, client, "delegate"); err == nil {
				t.Fatal("foreign target accepted")
			}
		}
		if got, err := accountsFindTarget(append(foreign, want), client, "delegate"); err != nil || got != want {
			t.Fatal("canonical target rejected")
		}
	}
}

func accountsHermesClient(t *testing.T, s *stack) *accountsClient {
	t.Helper()
	ensureHermesImage(t)
	s.hermesModel = &LLMStub{Final: "Done."}
	var modelTrace accountsHTTPTrace
	s.hermesModelName = scriptedModel
	s.llamaURL = fmt.Sprintf("http://127.0.0.1:%d/v1", serveLoopback(t, accountsRecordingHandler(t, s.hermesModel, &modelTrace)))
	prepareHermesHome(t, s)
	cfgPath := filepath.Join(s.hermesHome, "config.yaml")
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, cfgPath, strings.Replace(string(raw), `cli: ["vornik", "memory"]`, `cli: ["mcp-vornik-hermes"]`, 1))
	ctl := buildStaticCtl(t, s)
	connect := exec.Command("podman", hermesRunArgs(t, "--rm", "--network", "host", "-v", s.hermesHome+":/opt/data:Z", "-v", ctl+":/usr/local/bin/vornikctl:ro,Z", "-e", "XDG_CONFIG_HOME=/opt/data/.config", "-e", "VORNIK_API_KEY="+s.adminKey, hermesImage, "/usr/local/bin/vornikctl", "agent", "connect", "hermes", "--url", s.apiURL)...)
	if err := connect.Run(); err != nil {
		t.Fatalf("Hermes connect failed: %v", err)
	}
	_ = exec.Command("podman", "unshare", "chown", "-R", "0:0", s.hermesHome).Run()
	c := &accountsClient{ns: "hermes", home: s.hermesHome}
	c.finalTrace = modelTrace.String
	turn := 0
	c.invoke = func(tool string, args map[string]any, field string) map[string]any {
		turn++
		marker := fmt.Sprintf("[B11-HERMES-%d]", turn)
		s.hermesModel.evidence.reset()
		s.hermesModel.SetScripts(Script{Marker: marker, Final: "Done.", Steps: []ScriptStep{{ToolSuffix: "hermes__" + tool, Args: args}}})
		uid, gid := hermesUser(t)
		if err := exec.Command("podman", "unshare", "chown", "-R", uid+":"+gid, filepath.Join(s.hermesHome, ".config")).Run(); err != nil {
			t.Fatalf("Hermes home ownership failed: %v", err)
		}
		cmd := exec.Command("podman", hermesRunArgs(t, "--rm", "--network", "host", "-v", s.hermesHome+":/opt/data:Z", "-v", ctl+":/usr/local/bin/vornikctl:ro,Z", "-e", "XDG_CONFIG_HOME=/opt/data/.config", hermesImage, "hermes", "-z", marker+" Perform this Vornik call and receive its result.")...)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		_ = exec.Command("podman", "unshare", "chown", "-R", "0:0", s.hermesHome).Run()
		c.trace.WriteString(stdout.String() + "\n" + stderr.String() + "\n")
		for _, p := range s.hermesModel.evidence.pairs() {
			c.trace.WriteString(p.Name + "\n" + p.Args + "\n" + p.Output + "\n")
		}
		if err != nil {
			t.Fatalf("Hermes turn %d failed: %v (output retained for sweep)", turn, err)
		}
		target := accountsTarget(t, s.hermesModel.ToolsSeen(), "hermes", tool)
		out, err := accountsReceived(&s.hermesModel.evidence, target, args, field)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	return c
}

func TestAccountsA2_Hermes(t *testing.T) {
	requireLaneAgentImage(t)
	s, producerTrace := accountsStartStack(t)
	p := pairPhone(t, s)
	c := accountsHermesClient(t, s)
	c.producerTrace = producerTrace
	defer accountsSweep(t, s, c, []string{accountsWorkToken, accountsPersonalToken})
	accountsRunMail(t, s, c, p)
}

func accountsCodexClient(t *testing.T, s *stack) *accountsClient {
	t.Helper()
	bin, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal("certified Codex required")
	}
	verCmd := exec.Command(bin, "--version")
	verCmd.Env = testenv.Clean()
	ver, err := verCmd.Output()
	if err != nil || !strings.HasPrefix(strings.TrimSpace(string(ver)), certifiedCodex) {
		t.Fatal("Codex version differs from certified contract")
	}
	model := &ResponsesStub{Final: "Done."}
	var modelTrace accountsHTTPTrace
	url := fmt.Sprintf("http://127.0.0.1:%d/v1", serveLoopback(t, accountsRecordingHandler(t, model, &modelTrace)))
	env := newAgentEnv(t)
	home := filepath.Join(env.home, ".codex")
	if err := os.MkdirAll(home, 0700); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(home, "config.toml"), fmt.Sprintf("model = \"scripted\"\nmodel_provider = \"stub\"\n[model_providers.stub]\nname = \"stub\"\nbase_url = %q\nwire_api = \"responses\"\n", url))
	cmd := exec.Command(s.ctl, "agent", "connect", "codex", "--url", s.apiURL, "--accept-shared-user")
	cmd.Env = env.vars("VORNIK_API_KEY="+s.adminKey, "CODEX_HOME="+home)
	if err := cmd.Run(); err != nil {
		t.Fatalf("Codex connect failed: %v", err)
	}
	c := &accountsClient{ns: "codex", home: env.home}
	c.finalTrace = modelTrace.String
	turn := 0
	c.invoke = func(tool string, args map[string]any, field string) map[string]any {
		turn++
		marker := fmt.Sprintf("[B11-CODEX-%d]", turn)
		model.evidence.reset()
		model.mu.Lock()
		model.Scripts = []Script{{Marker: marker, Final: "Done.", Steps: []ScriptStep{{ToolSuffix: "codex__" + tool, Args: args}}}}
		model.mu.Unlock()
		cmd := exec.Command(bin, "exec", "--skip-git-repo-check", "--dangerously-bypass-approvals-and-sandbox", marker+" Perform this Vornik call and receive its result.")
		cmd.Dir = env.home
		cmd.Env = env.vars("CODEX_HOME=" + home)
		cmd.Stdin = strings.NewReader("")
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		err := cmd.Run()
		c.trace.WriteString(stdout.String() + "\n" + stderr.String() + "\n")
		for _, p := range model.evidence.pairs() {
			c.trace.WriteString(p.Namespace + "__" + p.Name + "\n" + p.Args + "\n" + p.Output + "\n")
		}
		if err != nil {
			t.Fatalf("Codex turn %d failed: %v (output retained for sweep)", turn, err)
		}
		target := accountsTarget(t, model.ToolsSeen(), "codex", tool)
		out, err := accountsReceived(&model.evidence, target, args, field)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	return c
}

func TestAccountsA3_Codex(t *testing.T) {
	requireLaneAgentImage(t)
	s, producerTrace := accountsStartStack(t)
	p := pairPhone(t, s)
	c := accountsCodexClient(t, s)
	c.producerTrace = producerTrace
	defer accountsSweep(t, s, c, []string{accountsWorkToken, accountsPersonalToken})
	accountsRunMail(t, s, c, p)
}

func accountsWithoutScopedKey(t *testing.T, home, ns string) string {
	t.Helper()
	dst := t.TempDir()
	key := filepath.Join(home, ".config", "vornik", "agents", ns+".key")
	if ns == "claudedesktop" {
		key = filepath.Join(home, ".config", "vornik", "agents", ns+".key")
	}
	if err := filepath.WalkDir(home, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || path == key {
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(home, path)
		if err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if err = os.MkdirAll(filepath.Dir(target), 0700); err != nil {
			return err
		}
		return os.WriteFile(target, data, 0600)
	}); err != nil {
		t.Fatal("client home sweep copy failed")
	}
	return dst
}

func TestAccountsUnexpectedKeySweep(t *testing.T) {
	home := t.TempDir()
	writeFile(t, filepath.Join(home, "unexpected.key"), accountsWorkToken)
	own := filepath.Join(home, ".config", "vornik", "agents", "codex.key")
	writeFile(t, own, accountsPersonalToken)
	sw := NewSweep([]string{accountsWorkToken, accountsPersonalToken}, nil)
	sw.AddDir("exact key exclusion control", true, accountsWithoutScopedKey(t, home, "codex"))
	if len(sw.Violations()) != 1 || !strings.Contains(sw.Violations()[0], accountsWorkToken) {
		t.Fatal("unexpected .key canary was excluded or scoped key was retained")
	}
}

// Captures complete model HTTP bodies, including offered schemas and unlinked
// history, independently of the strict received-result evidence matcher.
type accountsHTTPTrace struct {
	mu   sync.Mutex
	text strings.Builder
}

func (r *accountsHTTPTrace) String() string { r.mu.Lock(); defer r.mu.Unlock(); return r.text.String() }
func accountsRecordingHandler(t *testing.T, next http.Handler, trace *accountsHTTPTrace) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const limit = 2 << 20
		body, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
		if err != nil || len(body) > limit {
			t.Error("fixture request exceeds bounded capture or read failed")
			http.Error(w, "fixture read failed", 500)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		out := httptest.NewRecorder()
		bounded := &accountsBoundedResponse{ResponseRecorder: out, limit: limit}
		next.ServeHTTP(bounded, r)
		if bounded.overflow {
			t.Error("fixture response exceeds bounded capture")
			http.Error(w, "fixture response failed", 500)
			return
		}
		trace.mu.Lock()
		if trace.text.Len()+len(body)+out.Body.Len() > 32<<20 {
			trace.mu.Unlock()
			t.Error("fixture total trace exceeds bound")
			http.Error(w, "fixture trace failed", 500)
			return
		}
		trace.text.Write(body)
		trace.text.WriteString("\n")
		trace.text.Write(out.Body.Bytes())
		trace.text.WriteString("\n")
		trace.mu.Unlock()
		for k, vs := range out.Header() {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.WriteHeader(out.Code)
		_, _ = w.Write(out.Body.Bytes())
	})
}

func (w *accountsBoundedResponse) WriteString(s string) (int, error) { return w.Write([]byte(s)) }

type accountsBoundedResponse struct {
	*httptest.ResponseRecorder
	limit    int
	overflow bool
}

func (w *accountsBoundedResponse) Write(p []byte) (int, error) {
	if w.Body.Len()+len(p) > w.limit {
		w.overflow = true
		return 0, fmt.Errorf("fixture response limit")
	}
	return w.ResponseRecorder.Write(p)
}

func accountsStartStack(t *testing.T) (*stack, *accountsHTTPTrace) {
	t.Helper()
	trace := &accountsHTTPTrace{}
	s := startStackWith(t, func(cfg string) string {
		var parsed struct {
			Chat struct {
				Endpoint string `yaml:"endpoint"`
			} `yaml:"chat"`
		}
		if yaml.Unmarshal([]byte(cfg), &parsed) != nil {
			t.Fatal("fixture endpoint config parse failed")
		}
		target, err := url.Parse(parsed.Chat.Endpoint)
		if err != nil || target.Hostname() != "127.0.0.1" || target.Path != "/v1" {
			t.Fatal("fixture endpoint is not isolated localhost /v1")
		}
		base := *target
		base.Path = ""
		proxy := httputil.NewSingleHostReverseProxy(&base)
		replacement := fmt.Sprintf("http://127.0.0.1:%d/v1", serveLoopback(t, accountsRecordingHandler(t, proxy, trace)))
		return strings.ReplaceAll(cfg, parsed.Chat.Endpoint, replacement)
	}, nil)
	return s, trace
}

func accountsAssertDevice(t *testing.T, s *stack, p *Phone, rejected string, tokens []string) {
	t.Helper()
	if len(tokens) != 2 {
		t.Fatal("device proof expects exactly two credential canaries")
	}
	db := laneDB(t, s)
	var device string
	var n int
	if db.QueryRow(`SELECT count(*),COALESCE(min(id),'') FROM approver_devices WHERE revoked_at IS NULL`).Scan(&n, &device) != nil || n != 1 {
		t.Fatal("account fixture did not pair exactly one device")
	}
	rows, err := db.Query(`SELECT id,status,COALESCE(decided_by_device,''),rendered_sha256,rendered FROM agent_approval_requests WHERE kind<>'device_enrollment'`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	examined, rejectedSeen := 0, false
	for rows.Next() {
		var id, status, by, sha, rendered string
		if rows.Scan(&id, &status, &by, &sha, &rendered) != nil {
			t.Fatal("approval row scan failed")
		}
		examined++
		want := "approved"
		if id == rejected {
			want = "rejected"
			rejectedSeen = true
		}
		actual, err := approval.CanonicalSHA256([]byte(rendered))
		shown, ok := p.Shown[id]
		if status != want || by != device || err != nil || actual != sha || !ok || shown != sha {
			t.Fatal("account approval status/device/rendered/shown proof differs")
		}
	}
	if rows.Err() != nil || examined == 0 || (rejected != "" && !rejectedSeen) {
		t.Fatal("account approval proof denominator incomplete")
	}
	var total, wrong, clear int
	if db.QueryRow(`SELECT count(*),count(*) FILTER(WHERE created_by_device<>$1),count(*) FILTER(WHERE position(convert_to($2,'UTF8') in ciphertext)>0 OR position(convert_to($3,'UTF8') in ciphertext)>0) FROM agent_secrets`, device, tokens[0], tokens[1]).Scan(&total, &wrong, &clear) != nil || total != 2 || wrong != 0 || clear != 0 {
		t.Fatal("account credentials are not two encrypted phone-attributed rows")
	}
}

func TestAccountsHTTPCapture(t *testing.T) {
	trace := &accountsHTTPTrace{}
	handler := accountsRecordingHandler(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Fixture", "preserved")
		w.WriteHeader(202)
		_, _ = io.WriteString(w, accountsPersonalToken)
	}), trace)
	body := mustJSON(map[string]any{"tools": accountsWorkToken, "system": accountsWorkCalendarToken, "unlinked_output": accountsPersonalCalendarToken})
	result := httptest.NewRecorder()
	handler.ServeHTTP(result, httptest.NewRequest("POST", "http://fixture/v1", strings.NewReader(body)))
	if result.Code != 202 || result.Header().Get("X-Fixture") != "preserved" || result.Body.String() != accountsPersonalToken {
		t.Fatal("recording changed HTTP response")
	}
	sw := NewSweep([]string{accountsWorkToken, accountsPersonalToken, accountsWorkCalendarToken, accountsPersonalCalendarToken}, nil)
	sw.AddText("complete raw HTTP capture", true, trace.String())
	if len(sw.Violations()) != 4 {
		t.Fatal("raw schemas/system/unlinked/response canaries were not all examined")
	}
	bounded := &accountsBoundedResponse{ResponseRecorder: httptest.NewRecorder(), limit: 8}
	if _, err := io.WriteString(bounded, "oversized-content"); err == nil || !bounded.overflow || bounded.Body.Len() != 0 {
		t.Fatal("WriteString bypassed response capture cap")
	}
}

func accountsCredentialRequest(sentence, name string) bool {
	_, rest, ok := strings.Cut(sentence, "add the credential ")
	if !ok || !strings.HasPrefix(rest, name) {
		return false
	}
	rest = strings.TrimPrefix(rest, name)
	if rest == "" {
		return true
	}
	ch := rest[0]
	return !((ch >= 'A' && ch <= 'Z') || (ch >= 'a' && ch <= 'z') || (ch >= '0' && ch <= '9') || ch == '_' || ch == '-')
}
func TestAccountsCredentialRequest(t *testing.T) {
	name := "MAIL_TOKEN_WORK"
	if !accountsCredentialRequest("May I add the credential "+name+" for your work account?", name) {
		t.Fatal("exact credential rejected")
	}
	for _, suffix := range []string{"_EXTRA", "X", "-extra", "9"} {
		if accountsCredentialRequest("May I add the credential "+name+suffix+" for your work account?", name) {
			t.Fatal("credential suffix matched")
		}
	}
}
