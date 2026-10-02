//go:build e2e_hermes

package hermes

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	_ "github.com/lib/pq"

	"vornik.io/vornik/internal/approval"
)

// Agent-administered Vornik's DoD lane (plan P8, design §1, §13). These
// scenarios extend the Hermes lane: the same daemon, Postgres, scripted
// broker agent and stubs. Assertions are on Vornik-side state and canaries,
// never on wording.

const (
	summaryMarker       = "[A1-SUMMARY]"
	hostileSchemaMarker = "hostile-schema"
	hostileSecretMarker = "hostile-secret"
	draftsMarker        = "[A1-DRAFTS]"
)

// agentScripts are the broker agent's fixed choices for the agent-defined
// workflows: the summary reads the bank through query_api and writes only
// aggregates; the hostile variants break the egress; the drafts workflow
// reads mail and proposes one draft.
func agentScripts() []Script {
	summary := map[string]any{"month": "2026-09", "total": -1325.39,
		"top_categories": []map[string]any{{"name": "rent", "amount": -1200}, {"name": "groceries", "amount": -106.4}, {"name": "subscriptions", "amount": -18.99}}}
	bankRead := ScriptStep{ToolSuffix: "query_api", Args: map[string]any{"provider": "bank", "method": "GET",
		"path": "accounts/main/transactions", "query": map[string]any{"from": "2026-09"}}}
	write := func(doc any) ScriptStep {
		return ScriptStep{ToolSuffix: "file_write", Args: map[string]any{"path": "artifacts/out/result.json", "content": mustJSON(doc)}}
	}
	hostileSchema := map[string]any{"month": "2026-09", "total": 1, "top_categories": []any{}, "raw_transactions": []string{"Landlord " + RawRecordCanary}}
	// The canary sits in a field it fits (a category name, up to 64), so the
	// schema passes and only the secret scan can refuse it.
	hostileSecret := map[string]any{"month": "2026-09", "total": 1, "top_categories": []map[string]any{{"name": BankKeyCanary, "amount": 1}}}
	draft := map[string]any{"action": "create_draft", "args": map[string]any{"to": "landlord@flat.example", "subject": "Re: Rent", "body": "Confirmed, the rent for October is paid."}}
	return []Script{
		{Marker: hostileSchemaMarker, Final: "Done.", Steps: []ScriptStep{bankRead, write(hostileSchema)}},
		{Marker: hostileSecretMarker, Final: "Done.", Steps: []ScriptStep{bankRead, write(hostileSecret)}},
		{Marker: summaryMarker, Final: "Summary written.", Steps: []ScriptStep{bankRead, write(summary)}},
		{Marker: draftsMarker, Final: "Draft proposed.", Steps: []ScriptStep{
			{ToolSuffix: "mail_search", Args: map[string]any{"query": "rent"}},
			{ToolSuffix: "file_write", Args: map[string]any{"path": "artifacts/out/propose-create_draft.json", "content": mustJSON(draft)}},
			write(map[string]any{"drafted": 1}),
		}},
	}
}

// requireLaneAgentImage fails a gating arm that would run against a mutable
// remote tag: the agent image must be built from this tree (review fe1e F2).
func requireLaneAgentImage(t *testing.T) {
	t.Helper()
	if os.Getenv("VORNIK_E2E_AGENT_IMAGE") == "" {
		t.Fatal("set VORNIK_E2E_AGENT_IMAGE to an agent image built from this tree " +
			"(podman build -f images/vornik-agent/Containerfile -t localhost/vornik-agent:lane .); " +
			"the lane never runs against a remote :latest")
	}
}

// slow scales the lane's waits on a slow host: VORNIK_E2E_SLOW=2 doubles
// them (review fe1e F4).
func slow(d time.Duration) time.Duration {
	if f, err := strconv.ParseFloat(os.Getenv("VORNIK_E2E_SLOW"), 64); err == nil && f > 1 {
		return time.Duration(float64(d) * f)
	}
	return d
}

// agentEnv is a temp user: HOME and XDG_CONFIG_HOME for connect and the
// bridge.
type agentEnv struct{ home, cfg string }

func newAgentEnv(t *testing.T) agentEnv {
	t.Helper()
	home := t.TempDir()
	return agentEnv{home: home, cfg: filepath.Join(home, ".config")}
}

func (e agentEnv) vars(extra ...string) []string {
	return cleanEnv(append([]string{"HOME=" + e.home, "XDG_CONFIG_HOME=" + e.cfg}, extra...)...)
}

// pairPhone prints a pairing code with the real command, as the operator
// does, and pairs the scripted phone with it.
func pairPhone(t *testing.T, s *stack) *Phone {
	t.Helper()
	cmd := exec.Command(s.ctl, "pair-device", "--label", "Lane phone")
	cmd.Env = cleanEnv("VORNIK_CONFIG="+s.cfgPath, "VORNIK_CONFIGS_DIR="+filepath.Join(filepath.Dir(s.cfgPath), "configs"),
		"VORNIK_DATA_DIR="+filepath.Join(s.dir, "data"))
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("pair-device: %v\n%s", err, out)
	}
	m := regexp.MustCompile(`Pairing code:\s+(\S+)`).FindSubmatch(out)
	if m == nil {
		t.Fatalf("no code in:\n%s", out)
	}
	p := NewPhone(s.apiURL)
	if err := p.Pair(strings.ReplaceAll(string(m[1]), "-", "")); err != nil {
		t.Fatalf("pair: %v", err)
	}
	return p
}

// bridgeSession is a running vornikctl agent mcp-bridge with the stdio client.
type bridgeSession struct {
	*StdioClient
	stderr *bytes.Buffer
	stop   func()
}

// startBridge runs exactly the command and arguments connect wrote into the
// harness's config file, with no Vornik environment, as the harness does:
// the entry must work on its own (DoD lane bring-up found it did not carry
// the daemon's URL).
func startBridge(t *testing.T, env agentEnv, configFile, entryName string) *bridgeSession {
	t.Helper()
	raw, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		MCPServers map[string]struct {
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("%s: %v", configFile, err)
	}
	e, ok := cfg.MCPServers[entryName]
	if !ok || e.Command == "" {
		t.Fatalf("%s has no %s entry:\n%s", configFile, entryName, raw)
	}
	cmd := exec.Command(e.Command, e.Args...)
	cmd.Env = env.vars()
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	b := &bridgeSession{StdioClient: NewStdioClient(in, out), stderr: &stderr, stop: func() { _ = in.Close(); _ = cmd.Wait() }}
	t.Cleanup(b.stop)
	return b
}

// verbResult is a mutating verb's answer.
type verbResult struct {
	Effect      string `json:"effect"`
	ApprovalURL string `json:"approval_url"`
	Reason      string `json:"reason"`
	Sentence    string `json:"sentence"`
}

func (b *bridgeSession) verb(t *testing.T, name string, args any) verbResult {
	t.Helper()
	text, isErr, err := b.Tool(name, args)
	if err != nil || isErr {
		t.Fatalf("%s: %v %s", name, err, text)
	}
	var r verbResult
	if err := json.Unmarshal([]byte(text), &r); err != nil {
		t.Fatalf("%s: %s", name, text)
	}
	return r
}

func (b *bridgeSession) mustEffect(t *testing.T, name string, args any, want string) verbResult {
	t.Helper()
	r := b.verb(t, name, args)
	if r.Effect != want {
		t.Fatalf("%s: effect %s, want %s (%s)", name, r.Effect, want, r.Reason)
	}
	return r
}

// approveAll lets the phone approve every pending request until none is
// left for a few seconds, entering a credential where the request names one
// in values. It returns how many it approved.
func approveAll(t *testing.T, p *Phone, values map[string]string) int {
	t.Helper()
	n, quiet := 0, time.Now()
	// Some requests are filed only after an earlier one is decided (a
	// server's tool list after its token is stored), so the phone waits
	// for a quiet spell before it stops.
	for time.Since(quiet) < slow(15*time.Second) {
		pending, err := p.Pending()
		if err != nil {
			t.Fatal(err)
		}
		if len(pending) == 0 {
			time.Sleep(500 * time.Millisecond)
			continue
		}
		for _, req := range pending {
			value := ""
			for name, v := range values {
				if strings.Contains(req.Sentence, name) {
					value = v
				}
			}
			if err := p.Approve(req.ID, value); err != nil {
				t.Fatalf("approve %s (%s): %v", req.ID, req.Sentence, err)
			}
			n++
		}
		quiet = time.Now()
	}
	return n
}

// waitResult reads a broker task's egress through the bridge.
func (b *bridgeSession) waitResult(t *testing.T, taskID string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(slow(4 * time.Minute))
	for time.Now().Before(deadline) {
		text, isErr, err := b.Tool("result", map[string]any{"task_id": taskID, "wait_seconds": 25})
		if err != nil {
			t.Fatal(err)
		}
		var r map[string]any
		_ = json.Unmarshal([]byte(text), &r)
		if r["complete"] == true || isErr {
			return r
		}
	}
	t.Fatalf("task %s did not finish", taskID)
	return nil
}

func (b *bridgeSession) delegate(t *testing.T, workflow string, inputs map[string]any) string {
	t.Helper()
	text, isErr, err := b.Tool("delegate", map[string]any{"workflow": workflow, "inputs": inputs})
	if err != nil || isErr {
		t.Fatalf("delegate %s: %v %s", workflow, err, text)
	}
	var out struct {
		TaskID string `json:"task_id"`
	}
	_ = json.Unmarshal([]byte(text), &out)
	if out.TaskID == "" {
		t.Fatalf("delegate %s: %s", workflow, text)
	}
	return out.TaskID
}

// TestA1_ProtocolLevel is the deterministic DoD gate (plan P8.2): Claude
// Desktop's stand-in drives §1 items 1-3 over vornikctl agent mcp-bridge,
// a scripted phone pairs, enters the credentials and approves, and the
// canary sweep (P8.5) checks nothing reached the harness.
func TestA1_ProtocolLevel(t *testing.T) {
	requireLaneAgentImage(t)
	s := startStack(t)
	s.llm.SetScripts(append(agentScripts(), digestScript(goodDigest()), replyScript())...)
	phone := pairPhone(t, s)

	// The one command the user runs.
	env := newAgentEnv(t)
	connect := exec.Command(s.ctl, "agent", "connect", "claude-desktop", "--url", s.apiURL)
	connect.Env = env.vars("VORNIK_API_KEY=" + s.adminKey)
	if out, err := connect.CombinedOutput(); err != nil {
		t.Fatalf("connect: %v\n%s", err, out)
	}
	ns := "claudedesktop"
	b := startBridge(t, env, filepath.Join(env.cfg, "Claude", "claude_desktop_config.json"), "vornik-"+ns)

	// 1. The guidance arrives over the connection; nothing is set up yet.
	init, err := b.Call("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "lane"}})
	if err != nil || !strings.Contains(string(init), "request_credential") {
		t.Fatalf("initialize: %s %v", init, err)
	}
	_ = b.Notify("notifications/initialized")
	if text, _, _ := b.Tool("describe_installation", nil); !strings.Contains(text, "how_to_work") {
		t.Fatalf("describe_installation: %s", text)
	}

	// 2. The financial summary.
	b.mustEffect(t, "create_project", map[string]any{"slug": "finance", "purpose": "Monthly money summary"}, "applied")
	b.mustEffect(t, "add_api", map[string]any{"project": "finance", "name": "bank", "base_url": s.bankURL,
		"auth": map[string]any{"credential": "BANK_KEY", "prefix": "Bearer "}, "methods": []string{"GET"}}, "awaiting_approval")
	// The API exists once the phone approves it; its credential is asked
	// for after that.
	if n := approveAll(t, phone, nil); n < 1 {
		t.Fatal("the phone saw no request for the API")
	}
	b.mustEffect(t, "request_credential", map[string]any{"project": "finance", "name": "BANK_KEY", "purpose": "Read the bank's transactions", "kind": "secret"}, "awaiting_approval")
	if n := approveAll(t, phone, map[string]string{"BANK_KEY": BankKeyCanary}); n < 1 {
		t.Fatal("the phone saw no credential request")
	}
	b.mustEffect(t, "define_swarm", map[string]any{"slug": "finance", "roles": []map[string]any{
		{"name": "reader", "instructions": "Read the bank and summarise.", "tools": []string{"query_api", "file_write"}}}}, "applied")
	slot := nextHourlySlot(time.Now().Add(3 * time.Minute))
	b.mustEffect(t, "define_workflow", map[string]any{"project": "finance", "slug": "summary", "purpose": "Monthly summary",
		"steps": []map[string]any{{"name": "sum", "role": "reader", "instructions": summaryMarker + " Summarise the month's spending by category."}},
		"inputs": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"month"},
			"properties": map[string]any{"month": map[string]any{"enum": []string{"previous", "hostile-schema", "hostile-secret"}}}},
		"egress": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
			"month": map[string]any{"type": "string", "maxLength": 7}, "total": map[string]any{"type": "number"},
			"top_categories": map[string]any{"type": "array", "maxItems": 5, "items": map[string]any{"type": "object", "additionalProperties": false,
				"properties": map[string]any{"name": map[string]any{"type": "string", "maxLength": 64}, "amount": map[string]any{"type": "number"}}}}}},
		"schedule": map[string]any{"cron": fmt.Sprintf("%d * * * *", slot.Minute()), "timezone": "UTC", "inputs": map[string]any{"month": "previous"}},
	}, "awaiting_approval")
	approveAll(t, phone, nil)

	wf := ns + "--finance--summary"
	task := b.delegate(t, wf, map[string]any{"month": "previous"})
	res := b.waitResult(t, task)
	out, _ := res["output"].(map[string]any)
	if out == nil || out["total"] == nil || len(out) != 3 {
		t.Fatalf("summary egress: %v", res)
	}

	// The hostile producer (P8.2 §4). A field outside the schema is
	// withheld as egress_schema. A credential the model writes into an
	// allowed field never arrives: the daemon's model channel redacts
	// credential-shaped values in the model's own tool calls before the
	// agent acts on them (bring-up 2026-10-02), and the egress-document scan
	// behind it would refuse it as egress_secret. Either way the result
	// carries no canary; the lane accepts either layer and says which acted.
	r := b.waitResult(t, b.delegate(t, wf, map[string]any{"month": "hostile-schema"}))
	if r["output"] != nil || !strings.Contains(mustJSON(r), "egress_schema") {
		t.Fatalf("hostile-schema: %v", r)
	}
	r = b.waitResult(t, b.delegate(t, wf, map[string]any{"month": "hostile-secret"}))
	got := mustJSON(r)
	switch {
	case strings.Contains(got, BankKeyCanary):
		t.Fatalf("hostile-secret: the credential reached the harness: %v", r)
	case strings.Contains(got, "egress_secret") && r["output"] == nil:
		t.Log("hostile-secret: withheld by the egress-document scan (egress_secret)")
	case strings.Contains(got, "[REDACTED:"):
		t.Log("hostile-secret: redacted in the model's tool call before the agent wrote it")
	default:
		t.Fatalf("hostile-secret: neither withheld nor redacted: %v", r)
	}

	// The scheduled run: its slot, its source, its key (P8.2 §2.7).
	scheduled := waitScheduledRun(t, b, wf, slot)
	if scheduled == task {
		t.Fatal("the scheduled run is the delegated task")
	}
	if r := b.waitResult(t, scheduled); r["output"] == nil {
		t.Fatalf("the scheduled run's egress: %v", r)
	}

	// 3. The mail assistant: a server with a static token and a proposable
	// write; the write waits for the phone.
	b.mustEffect(t, "create_project", map[string]any{"slug": "mail", "purpose": "Draft replies"}, "applied")
	b.mustEffect(t, "add_mcp_server", map[string]any{"project": "mail", "name": "mail", "url": s.agentMailURL,
		"auth": map[string]any{"mode": "static", "credential": "MAIL_TOKEN"}, "write_tools": []string{"drafts_create"}}, "awaiting_approval")
	approveAll(t, phone, nil)
	b.mustEffect(t, "request_credential", map[string]any{"project": "mail", "name": "MAIL_TOKEN", "purpose": "Read the mailbox and save drafts", "kind": "secret"}, "awaiting_approval")
	// The token, then the tool list the daemon reads with it.
	approveAll(t, phone, map[string]string{"MAIL_TOKEN": MailTokenCanary})
	b.mustEffect(t, "define_swarm", map[string]any{"slug": "mail", "roles": []map[string]any{
		{"name": "reader", "instructions": "Read mail.", "tools": []string{"mcp__mail__mail_search", "file_write"}}}}, "applied")
	b.mustEffect(t, "define_workflow", map[string]any{"project": "mail", "slug": "drafts", "purpose": "Draft replies",
		"steps":  []map[string]any{{"name": "draft", "role": "reader", "instructions": draftsMarker + " Draft a reply to the newest mail."}},
		"inputs": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{}},
		"egress": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"drafted": map[string]any{"type": "integer"}}},
		"proposes": []map[string]any{{"action": "create_draft", "tool": "mcp__mail-write__drafts_create", "args_schema": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
			"to":      map[string]any{"type": "string", "pattern": "^[^@ ]+@[^@ ]+$", "maxLength": 100},
			"subject": map[string]any{"type": "string", "maxLength": 200, "x-untrusted": true},
			"body":    map[string]any{"type": "string", "maxLength": 512, "x-untrusted": true}}}}},
	}, "awaiting_approval")
	approveAll(t, phone, nil)
	dr := b.waitResult(t, b.delegate(t, ns+"--mail--drafts", map[string]any{}))
	if countTool(s.agentMail, "drafts_create") != 0 {
		t.Fatal("a draft was created before the phone approved it")
	}
	if !strings.Contains(mustJSON(dr), "create_draft") {
		t.Fatalf("the proposal is not in the result: %v", dr)
	}
	approveAll(t, phone, nil)
	waitFor(t, "the approved draft", slow(2*time.Minute), func() bool { return countTool(s.agentMail, "drafts_create") == 1 })

	// The credentials arrived where they are used: each server was called,
	// with them (review fe1e F1: a check over zero calls proves nothing).
	assertBankCalledWithKey(t, s)
	if countTool(s.agentMail, "mail_search") == 0 {
		t.Fatal("the mail server was never read: the token was not exercised")
	}
	if s.agentMail.Refused() != 0 {
		t.Fatalf("the mail server refused %d requests: the token did not arrive", s.agentMail.Refused())
	}

	// 4. Refusals.
	if r := b.verb(t, "define_workflow", map[string]any{"project": "finance", "slug": "wide", "steps": []map[string]any{{"name": "a", "role": "reader", "instructions": "x"}},
		"egress": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{"a": map[string]any{"type": "string"}}}}); r.Effect != "refused" {
		t.Fatalf("an unbounded egress string: %+v", r)
	}
	if text, isErr, _ := b.Tool("delegate", map[string]any{"workflow": wf, "inputs": map[string]any{"month": BankKeyCanary}}); !isErr && !strings.Contains(text, "egress_secret") && !strings.Contains(text, "INPUT_REJECTED") {
		t.Fatalf("a credential in delegate's arguments was accepted: %s", text)
	}

	b.stop()
	assertDeviceApprovals(t, s, phone, 2)
	sweep := NewSweep([]string{BankKeyCanary, MailTokenCanary}, []string{RawRecordCanary, RawMailCanary})
	// What the harness received is the gated place. What it sent holds the
	// bank canary exactly once: the lane's own refusal probe above.
	var received, sent strings.Builder
	for _, l := range strings.Split(b.Transcript(), "\n") {
		switch {
		case strings.HasPrefix(l, "< "):
			received.WriteString(l + "\n")
		case strings.HasPrefix(l, "> "):
			sent.WriteString(l + "\n")
		}
	}
	if n := strings.Count(sent.String(), BankKeyCanary); n != 1 {
		t.Fatalf("the harness sent the bank canary %d times, want exactly the one refusal probe", n)
	}
	sweep.AddText("bridge: what the harness received (stdout)", true, received.String())
	// The bridge's stderr is normally empty; the pad keeps an empty stream
	// from reading as "examined nothing" while still sweeping all of it.
	sweep.AddText("bridge stderr", true, b.stderr.String()+" ")
	sweep.AddDir("the harness's home (key file excluded)", true, withoutKeys(t, env.home))
	sweepVornik(t, s, sweep, nil)
	t.Logf("canary sweep:\n%s", sweep.Report())
	if v := sweep.Violations(); len(v) != 0 {
		t.Fatalf("canary sweep:\n%s", strings.Join(v, "\n"))
	}
}

// nextHourlySlot is the first whole minute at or after t.
func nextHourlySlot(t time.Time) time.Time {
	return t.UTC().Truncate(time.Minute).Add(time.Minute)
}

// waitScheduledRun polls list_my_setup until the workflow's recent_runs has
// the slot, and returns its task (P8.2 §2.7: by slot, not by existence).
func waitScheduledRun(t *testing.T, b *bridgeSession, wf string, slot time.Time) string {
	t.Helper()
	// The slot may already have passed while the delegated runs ran; the
	// list is still checked, for at least half a minute.
	// Up to the scheduler's catch-up window: a late tick still fires the
	// slot, and only that is a failure (review fe1e F6).
	deadline := slot.Add(15 * time.Minute)
	if min := time.Now().Add(30 * time.Second); deadline.Before(min) {
		deadline = min
	}
	for time.Now().Before(deadline) {
		text, _, err := b.Tool("list_my_setup", nil)
		if err != nil {
			t.Fatal(err)
		}
		var v struct {
			Projects []struct {
				Workflows []struct {
					ID       string `json:"id"`
					Schedule *struct {
						RecentRuns []struct {
							Slot   time.Time `json:"slot"`
							TaskID string    `json:"task_id"`
						} `json:"recent_runs"`
					} `json:"schedule"`
				} `json:"workflows"`
			} `json:"projects"`
		}
		_ = json.Unmarshal([]byte(text), &v)
		for _, p := range v.Projects {
			for _, w := range p.Workflows {
				if w.ID != wf || w.Schedule == nil {
					continue
				}
				for _, r := range w.Schedule.RecentRuns {
					if r.Slot.Equal(slot) && r.TaskID != "" {
						return r.TaskID
					}
				}
			}
		}
		time.Sleep(10 * time.Second)
	}
	t.Fatalf("no scheduled run for slot %s", slot)
	return ""
}

func countTool(s *MCPStub, tool string) int {
	n := 0
	for _, c := range s.Calls() {
		if c.Tool == tool {
			n++
		}
	}
	return n
}

// withoutKeys copies a home without its vornik key files: the key file is
// the one place the agent key is meant to be, and it is not a canary.
func withoutKeys(t *testing.T, home string) string {
	t.Helper()
	dst := t.TempDir()
	_ = filepath.WalkDir(home, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || strings.HasSuffix(path, ".key") {
			return nil
		}
		rel, _ := filepath.Rel(home, path)
		b, _ := os.ReadFile(path)
		_ = os.MkdirAll(filepath.Dir(filepath.Join(dst, rel)), 0o700)
		_ = os.WriteFile(filepath.Join(dst, rel), b, 0o600)
		return nil
	})
	return dst
}

// laneDB opens the lane's database.
func laneDB(t *testing.T, s *stack) *sql.DB {
	t.Helper()
	db, err := sql.Open("postgres", fmt.Sprintf("host=127.0.0.1 port=%d user=vornik password=vornik dbname=vornik sslmode=disable", s.pgPort))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// assertDeviceApprovals checks every agent approval was decided by the
// phone's device with the hash its page showed, and that hash is the sha256
// of the stored rendering (review 69cc R4); every credential value is
// sealed (P8.2, F8).
func assertDeviceApprovals(t *testing.T, s *stack, phone *Phone, wantSecrets int) {
	t.Helper()
	db := laneDB(t, s)
	// The lane pairs exactly one device: the scripted phone (review fe1e F3).
	var laneDevice string
	var devices int
	if err := db.QueryRowContext(context.Background(), `SELECT count(*), COALESCE(min(id),'') FROM approver_devices WHERE revoked_at IS NULL`).Scan(&devices, &laneDevice); err != nil || devices != 1 {
		t.Fatalf("the lane pairs exactly one device: %d active, %v (review a507)", devices, err)
	}
	rows, err := db.QueryContext(context.Background(), `SELECT id, status, COALESCE(decided_by_device,''), rendered_sha256, rendered FROM agent_approval_requests WHERE kind <> 'device_enrollment'`)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	n := 0
	for rows.Next() {
		var id, status, device, sha, rendered string
		if err := rows.Scan(&id, &status, &device, &sha, &rendered); err != nil {
			t.Fatal(err)
		}
		n++
		if status != "approved" || device != laneDevice {
			t.Errorf("request %s: status %s, decided by %q, want the lane phone %q", id, status, device, laneDevice)
		}
		if want, err := approval.CanonicalSHA256([]byte(rendered)); err != nil || sha != want {
			t.Errorf("request %s: stored hash is not the rendering's (%v)", id, err)
		}
		if shown, ok := phone.Shown[id]; ok && shown != sha {
			t.Errorf("request %s: the phone was shown %s, the row has %s", id, shown, sha)
		}
	}
	if n == 0 {
		t.Fatal("no agent approval requests: the lane examined nothing")
	}
	// Every credential was entered on the device: stored encrypted, by the
	// device, and no stored ciphertext contains a canary in the clear.
	var total, unattributed, clear int
	err = db.QueryRowContext(context.Background(), `SELECT count(*),
		count(*) FILTER (WHERE created_by_device = ''),
		count(*) FILTER (WHERE position(convert_to($1, 'UTF8') in ciphertext) > 0 OR position(convert_to($2, 'UTF8') in ciphertext) > 0)
		FROM agent_secrets`, BankKeyCanary, MailTokenCanary).Scan(&total, &unattributed, &clear)
	if err != nil {
		t.Fatal(err)
	}
	if total != wantSecrets || unattributed != 0 || clear != 0 {
		t.Fatalf("agent_secrets: %d rows, %d not entered on a device, %d holding a canary in the clear", total, unattributed, clear)
	}
}

// sweepVornik adds the places inside Vornik: the daemon log (which carries
// the agent containers' output) and the text of every table an agent's
// traffic touches (P8.5; review 8941 F9, 69cc R1).
func sweepVornik(t *testing.T, s *stack, sw *Sweep, quiet map[string]string) {
	t.Helper()
	logb, _ := os.ReadFile(s.daemonLog)
	sw.AddText("daemon log (agent container output included)", false, string(logb))
	db := laneDB(t, s)
	read := func(table string) []string {
		rows, err := db.QueryContext(context.Background(), fmt.Sprintf(`SELECT row_to_json(t)::text FROM %s t`, table))
		if err != nil {
			t.Fatalf("sweep %s: %v", table, err)
		}
		defer func() { _ = rows.Close() }()
		var texts []string
		for rows.Next() {
			var s string
			_ = rows.Scan(&s)
			texts = append(texts, s)
		}
		return texts
	}
	for _, table := range []string{"tasks", "executions", "execution_step_outcomes", "step_prompts",
		"tool_audit_log", "agent_approval_requests", "control_plane_proposals", "broker_actions", "secret_redaction_audit"} {
		// A table this scenario does not write is expected empty, with
		// the reason, and swept the moment it is not.
		if why, ok := quiet[table]; ok {
			sw.AddRowsMustBeEmpty("table "+table, why, read(table))
			continue
		}
		sw.AddRows("table "+table, false, read(table))
	}
	// Written only when something opts in, which nothing here does: they
	// must stay empty, and are swept the moment they are not.
	sw.AddRowsMustBeEmpty("table llm_exchanges", "recording.llm_exchanges is a per-project opt-in the agent renderer never sets", read("llm_exchanges"))
	sw.AddRowsMustBeEmpty("table chat_audit_log", "the chat surface, which no agent uses and this lane does not drive", read("chat_audit_log"))
}

// hermesAgentRun runs one `hermes -z` with the agent connection only: the
// static vornikctl mounted where connect recorded it, the lane's Hermes home
// (which holds the key connect wrote, and nothing else of Vornik's), and no
// broker or memory token, so the plugin's own tools are not offered.
func hermesAgentRun(t *testing.T, s *stack, ctl, prompt string) hermesRun {
	t.Helper()
	// The lane chowns the home to itself after every run so it can sweep
	// it; Hermes's boot re-owns only a targeted set of paths, so the key's
	// directory is handed back to the Hermes user here, as it stays in a
	// real install.
	uid, gid := hermesUser(t)
	run(t, "podman", "unshare", "chown", "-R", uid+":"+gid, filepath.Join(s.hermesHome, ".config"))
	args := []string{"run", "--rm", "--network", "host",
		"-v", s.hermesHome + ":/opt/data:Z",
		"-v", ctl + ":/usr/local/bin/vornikctl:ro,Z",
		"-e", "XDG_CONFIG_HOME=/opt/data/.config",
		hermesImage, "hermes", "-z", prompt}
	cmd := exec.Command("podman", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	start := time.Now()
	err := cmd.Run()
	r := hermesRun{Prompt: prompt, Reply: strings.TrimSpace(stdout.String()), Stderr: stderr.String(), Elapsed: time.Since(start)}
	_ = exec.Command("podman", "unshare", "chown", "-R", "0:0", s.hermesHome).Run()
	if err != nil {
		t.Logf("hermes -z exited: %v\n%s", err, tail(r.Stderr, 3000))
	}
	return r
}

// buildStaticCtl builds vornikctl without cgo, so it runs inside the Hermes
// image whatever its libc.
func buildStaticCtl(t *testing.T, s *stack) string {
	t.Helper()
	out := filepath.Join(s.dir, "vornikctl-static")
	cmd := exec.Command("go", "build", "-o", out, "./cmd/vornikctl")
	cmd.Dir = repoRoot(t)
	cmd.Env = cleanEnv("CGO_ENABLED=0")
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("static vornikctl: %v\n%s", err, b)
	}
	return out
}

// a2Step is one scripted Hermes turn: its marker, and the admin calls the
// stub model makes through the vornik-hermes MCP entry.
// harnessScripts are the admin turns a harness's stub model makes, for the
// tools named <prefix><tool> and the workflows of namespace ns.
func harnessScripts(s *stack, prefix, ns string) []Script {
	tool := func(name string, args map[string]any) ScriptStep {
		return ScriptStep{ToolSuffix: prefix + name, Args: args}
	}
	return []Script{
		{Marker: "[A2-1]", Final: "Asked for the bank connection.", Steps: []ScriptStep{
			tool("describe_installation", map[string]any{}),
			tool("create_project", map[string]any{"slug": "finance", "purpose": "Monthly money summary"}),
			tool("add_api", map[string]any{"project": "finance", "name": "bank", "base_url": s.bankURL,
				"auth": map[string]any{"credential": "BANK_KEY", "prefix": "Bearer "}, "methods": []string{"GET"}}),
		}},
		{Marker: "[A2-2]", Final: "Asked for the bank key.", Steps: []ScriptStep{
			tool("request_credential", map[string]any{"project": "finance", "name": "BANK_KEY", "purpose": "Read the bank's transactions", "kind": "secret"}),
		}},
		{Marker: "[A2-3]", Final: "Asked to approve the summary.", Steps: []ScriptStep{
			tool("define_swarm", map[string]any{"slug": "finance", "roles": []map[string]any{
				{"name": "reader", "instructions": "Read the bank and summarise.", "tools": []string{"query_api", "file_write"}}}}),
			tool("define_workflow", map[string]any{"project": "finance", "slug": "summary", "purpose": "Monthly summary",
				"steps": []map[string]any{{"name": "sum", "role": "reader", "instructions": summaryMarker + " Summarise the month's spending by category."}},
				"inputs": map[string]any{"type": "object", "additionalProperties": false, "required": []string{"month"},
					"properties": map[string]any{"month": map[string]any{"enum": []string{"previous"}}}},
				"egress": map[string]any{"type": "object", "additionalProperties": false, "properties": map[string]any{
					"month": map[string]any{"type": "string", "maxLength": 7}, "total": map[string]any{"type": "number"},
					"top_categories": map[string]any{"type": "array", "maxItems": 5, "items": map[string]any{"type": "object", "additionalProperties": false,
						"properties": map[string]any{"name": map[string]any{"type": "string", "maxLength": 64}, "amount": map[string]any{"type": "number"}}}}}},
			}),
		}},
		{Marker: "[A2-4]", Steps: []ScriptStep{
			tool("delegate", map[string]any{"workflow": ns + "--finance--summary", "inputs": map[string]any{"month": "previous"}}),
			{ToolSuffix: prefix + "result", ArgsFrom: resultArgs},
			{ToolSuffix: prefix + "result", ArgsFrom: resultArgs},
		}, FinalFrom: func(last string) string { return "Summary: " + last }},
	}
}

// TestA2_Hermes is Hermes's DoD gate (plan P8.3), stub-model arm: Hermes is
// connected with vornikctl agent connect run inside its own image as the
// Hermes user, and drives the setup, the delegate and the result through
// the vornik-hermes MCP entry that connect wrote. The real-model arm is not
// a gate (P8.7).
func TestA2_Hermes(t *testing.T) {
	requireLaneAgentImage(t)
	s := startStack(t)
	s.llm.SetScripts(append(agentScripts(), digestScript(goodDigest()), replyScript())...)
	ensureHermesImage(t)
	s.hermesModel = &LLMStub{Final: "Done.", Scripts: harnessScripts(s, "hermes__", "hermes")}
	s.hermesModelName = scriptedModel
	s.llamaURL = fmt.Sprintf("http://127.0.0.1:%d/v1", serveLoopback(t, s.hermesModel))
	prepareHermesHome(t, s)
	// The agent connection's tools are toolset mcp-vornik-hermes in Hermes.
	cfgPath := filepath.Join(s.hermesHome, "config.yaml")
	raw, _ := os.ReadFile(cfgPath)
	writeFile(t, cfgPath, strings.Replace(string(raw), `cli: ["vornik", "memory"]`, `cli: ["mcp-vornik-hermes"]`, 1))
	phone := pairPhone(t, s)
	ctl := buildStaticCtl(t, s)

	// The one command, run as the user Hermes runs as: inside its image.
	connect := exec.Command("podman", "run", "--rm", "--network", "host",
		"-v", s.hermesHome+":/opt/data:Z", "-v", ctl+":/usr/local/bin/vornikctl:ro,Z",
		"-e", "XDG_CONFIG_HOME=/opt/data/.config", "-e", "VORNIK_API_KEY="+s.adminKey,
		hermesImage, "/usr/local/bin/vornikctl", "agent", "connect", "hermes", "--url", s.apiURL)
	out, err := connect.CombinedOutput()
	_ = exec.Command("podman", "unshare", "chown", "-R", "0:0", s.hermesHome).Run()
	if err != nil {
		t.Fatalf("connect inside the Hermes image: %v\n%s", err, out)
	}
	agents, _ := os.ReadDir(filepath.Join(s.hermesHome, ".config", "vornik", "agents"))
	var names []string
	for _, e := range agents {
		names = append(names, e.Name())
	}
	if strings.Join(names, ",") != "hermes.json,hermes.key" {
		t.Fatalf("the Hermes home holds %v, want only Hermes's own key and record (review 8941 F1)", names)
	}
	if b, _ := os.ReadFile(cfgPath); !strings.Contains(string(b), "vornik-hermes") || strings.Contains(string(b), "sk-vornik-") {
		t.Fatalf("Hermes config after connect:\n%s", b)
	}

	var transcript strings.Builder
	step := func(marker, prompt string) hermesRun {
		r := hermesAgentRun(t, s, ctl, marker+" "+prompt)
		transcript.WriteString(r.Reply + "\n" + r.Stderr + "\n")
		t.Logf("%s: reply %q; Hermes model saw tools %v; script failures %v; stderr tail:\n%s",
			marker, r.Reply, s.hermesModel.ToolsSeen(), s.hermesModel.Failures(), tail(r.Stderr, 1500))
		return r
	}
	step("[A2-1]", "Set up a monthly summary of my bank account.")
	if n := approveAll(t, phone, nil); n < 1 {
		t.Fatal("no request reached the phone after Hermes asked for the bank API")
	}
	step("[A2-2]", "Ask me for the bank key.")
	if n := approveAll(t, phone, map[string]string{"BANK_KEY": BankKeyCanary}); n < 1 {
		t.Fatal("no credential request reached the phone")
	}
	step("[A2-3]", "Define the summary.")
	approveAll(t, phone, nil)
	r := step("[A2-4]", "Run the summary for last month.")
	// The summary's total is in what Hermes received: its reply, or its
	// session store (Hermes replaces a repeated identical result with a
	// note, so the reply alone is not the place to look).
	if !strings.Contains(r.Reply, "-1325.39") && !dirContains(t, s.hermesHome, "-1325.39") {
		t.Fatalf("Hermes did not get the summary: %q\n%s", r.Reply, tail(r.Stderr, 2000))
	}
	assertBankCalledWithKey(t, s)

	// The bridge refuses its key file to any other user (review 8941 F2):
	// run directly in the image as a UID that is neither root nor the key's
	// owner. Which check refuses first (directory permissions, the open, or
	// the owner check) depends on the modes; each refuses.
	neg := exec.Command("podman", "run", "--rm", "--network", "host", "--user", "4242", "--entrypoint", "/usr/local/bin/vornikctl",
		"-v", s.hermesHome+":/opt/data:Z", "-v", ctl+":/usr/local/bin/vornikctl:ro,Z",
		"-e", "XDG_CONFIG_HOME=/opt/data/.config", hermesImage, "agent", "mcp-bridge", "--namespace", "hermes", "--url", s.apiURL)
	negOut, negErr := neg.CombinedOutput()
	_ = exec.Command("podman", "unshare", "chown", "-R", "0:0", s.hermesHome).Run()
	if refused := regexp.MustCompile(`belongs to another user|permission denied|no key at`).Match(negOut); negErr == nil || !refused {
		t.Fatalf("the bridge ran for a user that does not own the key: %v\n%s", negErr, negOut)
	}

	assertDeviceApprovals(t, s, phone, 1)
	sweep := NewSweep([]string{BankKeyCanary, MailTokenCanary}, []string{RawRecordCanary, RawMailCanary})
	sweep.AddText("Hermes replies and stderr", true, transcript.String())
	sweep.AddDir("the Hermes home (key file excluded)", true, withoutKeys(t, s.hermesHome))
	sweepVornik(t, s, sweep, harnessQuiet)
	t.Logf("canary sweep:\n%s", sweep.Report())
	if v := sweep.Violations(); len(v) != 0 {
		t.Fatalf("canary sweep:\n%s", strings.Join(v, "\n"))
	}
}

// hermesUser is the image's hermes user, as uid and gid strings.
func hermesUser(t *testing.T) (string, string) {
	t.Helper()
	id := func(flag string) string {
		out, err := exec.Command("podman", "run", "--rm", "--entrypoint", "id", hermesImage, flag, "hermes").Output()
		if err != nil {
			t.Fatalf("id %s hermes: %v", flag, err)
		}
		return strings.TrimSpace(string(out))
	}
	return id("-u"), id("-g")
}

// taskIDRE finds a task id in a tool result: Hermes wraps results in an
// untrusted_tool_result block, so the text is not bare JSON.
var taskIDRE = regexp.MustCompile(`"task_id\\?"\s*:\s*\\?"(task_[A-Za-z0-9_]+)`)

// resultArgs reads the task id the previous result named.
func resultArgs(last string) map[string]any {
	id := ""
	if m := taskIDRE.FindStringSubmatch(last); m != nil {
		id = m[1]
	}
	return map[string]any{"task_id": id, "wait_seconds": 25}
}

// TestA3_Codex is Codex's DoD gate (plan P8.4): codex-cli speaks only the
// Responses API, so its model is the scripted ResponsesStub, configured as
// a custom provider in the same config.toml connect then merges into. Codex
// runs on this host as this user, so connect's shared-user gate is
// exercised for real: refused, then accepted with --accept-shared-user (the
// lane's daemon is a test daemon). Codex runs with its own sandbox bypassed
// so its MCP calls are not held for an interactive approval; Vornik's
// approvals are the ones under test.
func TestA3_Codex(t *testing.T) {
	requireLaneAgentImage(t)
	codexBin, err := exec.LookPath("codex")
	if err != nil {
		t.Fatal("the Codex arm needs codex on PATH (plan P8.4)")
	}
	// The Responses contract the stub speaks was probed against this
	// version; a pass certifies only it (review fe1e F7).
	ver, _ := exec.Command(codexBin, "--version").Output()
	if !strings.HasPrefix(strings.TrimSpace(string(ver)), certifiedCodex) {
		t.Fatalf("codex is %q, the lane was probed against %s: re-probe the Responses contract "+
			"(design §13 as built) and update certifiedCodex", strings.TrimSpace(string(ver)), certifiedCodex)
	}
	t.Logf("certifying %s", strings.TrimSpace(string(ver)))
	s := startStack(t)
	s.llm.SetScripts(append(agentScripts(), digestScript(goodDigest()), replyScript())...)
	model := &ResponsesStub{Final: "Done.", Scripts: harnessScripts(s, "codex__", "codex")}
	modelURL := fmt.Sprintf("http://127.0.0.1:%d/v1", serveLoopback(t, model))
	phone := pairPhone(t, s)

	env := newAgentEnv(t)
	codexHome := filepath.Join(env.home, ".codex")
	if err := os.MkdirAll(codexHome, 0o700); err != nil {
		t.Fatal(err)
	}
	// The user's own Codex config, with a comment connect must keep.
	writeFile(t, filepath.Join(codexHome, "config.toml"), fmt.Sprintf(`# my codex settings
model = "scripted"
model_provider = "stub"

[model_providers.stub]
name = "stub"
base_url = %q
wire_api = "responses"
`, modelURL))

	connectArgs := []string{"agent", "connect", "codex", "--url", s.apiURL}
	refused := exec.Command(s.ctl, connectArgs...)
	refused.Env = env.vars("VORNIK_API_KEY="+s.adminKey, "CODEX_HOME="+codexHome)
	out, err := refused.CombinedOutput()
	// Readable, or not yet created (the store key appears on first use):
	// either way the gate refuses without the flag.
	if err == nil || !strings.Contains(string(out), "--accept-shared-user") {
		t.Fatalf("connect for a shell-capable harness under a shared user was not refused: %v\n%s", err, out)
	}
	accepted := exec.Command(s.ctl, append(connectArgs, "--accept-shared-user")...)
	accepted.Env = env.vars("VORNIK_API_KEY="+s.adminKey, "CODEX_HOME="+codexHome)
	if out, err := accepted.CombinedOutput(); err != nil {
		t.Fatalf("connect --accept-shared-user: %v\n%s", err, out)
	}
	if b, _ := os.ReadFile(filepath.Join(codexHome, "config.toml")); !strings.Contains(string(b), "# my codex settings") ||
		!strings.Contains(string(b), "[mcp_servers.vornik-codex]") || strings.Contains(string(b), "sk-vornik-") {
		t.Fatalf("config.toml after connect:\n%s", b)
	}

	var transcript strings.Builder
	step := func(marker, prompt string) string {
		cmd := exec.Command(codexBin, "exec", "--skip-git-repo-check", "--dangerously-bypass-approvals-and-sandbox", marker+" "+prompt)
		cmd.Dir = env.home
		cmd.Env = env.vars("CODEX_HOME=" + codexHome)
		cmd.Stdin = strings.NewReader("")
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		runErr := cmd.Run()
		transcript.WriteString(stdout.String() + "\n" + stderr.String() + "\n")
		t.Logf("%s: exit %v; model saw %d tools; failures %v; stdout tail:\n%s", marker, runErr, len(model.ToolsSeen()), model.Failures(), tail(stdout.String(), 800))
		return stdout.String()
	}
	step("[A2-1]", "Set up a monthly summary of my bank account.")
	if n := approveAll(t, phone, nil); n < 1 {
		t.Fatal("no request reached the phone after Codex asked for the bank API")
	}
	step("[A2-2]", "Ask me for the bank key.")
	if n := approveAll(t, phone, map[string]string{"BANK_KEY": BankKeyCanary}); n < 1 {
		t.Fatal("no credential request reached the phone")
	}
	step("[A2-3]", "Define the summary.")
	approveAll(t, phone, nil)
	if reply := step("[A2-4]", "Run the summary for last month."); !strings.Contains(reply, "-1325.39") && !dirContains(t, codexHome, "-1325.39") {
		t.Fatalf("Codex did not get the summary:\n%s", reply)
	}
	assertBankCalledWithKey(t, s)

	assertDeviceApprovals(t, s, phone, 1)
	sweep := NewSweep([]string{BankKeyCanary, MailTokenCanary}, []string{RawRecordCanary, RawMailCanary})
	sweep.AddText("codex exec output (stdout and stderr)", true, transcript.String())
	sweep.AddDir("the Codex home and user home (key file excluded)", true, withoutKeys(t, env.home))
	sweepVornik(t, s, sweep, harnessQuiet)
	t.Logf("canary sweep:\n%s", sweep.Report())
	if v := sweep.Violations(); len(v) != 0 {
		t.Fatalf("canary sweep:\n%s", strings.Join(v, "\n"))
	}
}

// dirContains reports whether any file under dir contains needle.
func dirContains(t *testing.T, dir, needle string) bool {
	t.Helper()
	found := false
	_ = filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || found || d.IsDir() {
			return nil
		}
		if b, err := os.ReadFile(path); err == nil && bytes.Contains(b, []byte(needle)) {
			found = true
		}
		return nil
	})
	return found
}

// harnessQuiet are the tables the Hermes and Codex arms do not write: they
// drive the financial summary only; the write and the hostile producer are
// proven once, in A1 (review 69cc R6).
var harnessQuiet = map[string]string{
	"broker_actions":         "this arm proposes no write (the mail assistant's write is proven in A1)",
	"secret_redaction_audit": "this arm has no hostile producer (proven in A1)",
}

// TestA2_HermesRealModel is the non-gating real-model arm (plan P8.3, P8.7):
// Hermes on the pinned gpt-oss-20b, prompted only in plain language, with
// nothing but the guidance Vornik sends it. It proves "prompted only in
// plain language" (§1) when it passes, and records how far the model got
// when it does not; it never gates. VORNIK_E2E_REAL=1 runs it.
func TestA2_HermesRealModel(t *testing.T) {
	if os.Getenv("VORNIK_E2E_REAL") != "1" {
		t.Skip("the real-model arm runs with VORNIK_E2E_REAL=1 (non-gating)")
	}
	requireLaneAgentImage(t)
	s := startStack(t)
	// The broker agent stays scripted (lane design §3.2): whatever the model
	// names its workflow, the agent reads the bank and writes a summary.
	summary := agentScripts()[2]
	s.llm.SetScripts(digestScript(goodDigest()), replyScript())
	s.llm.Steps, s.llm.Final = summary.Steps, summary.Final
	startHermesModel(t, s)
	prepareHermesHome(t, s)
	cfgPath := filepath.Join(s.hermesHome, "config.yaml")
	raw, _ := os.ReadFile(cfgPath)
	writeFile(t, cfgPath, strings.Replace(string(raw), `cli: ["vornik", "memory"]`, `cli: ["mcp-vornik-hermes"]`, 1))
	phone := pairPhone(t, s)
	ctl := buildStaticCtl(t, s)
	connect := exec.Command("podman", "run", "--rm", "--network", "host",
		"-v", s.hermesHome+":/opt/data:Z", "-v", ctl+":/usr/local/bin/vornikctl:ro,Z",
		"-e", "XDG_CONFIG_HOME=/opt/data/.config", "-e", "VORNIK_API_KEY="+s.adminKey,
		hermesImage, "/usr/local/bin/vornikctl", "agent", "connect", "hermes", "--url", s.apiURL)
	if out, err := connect.CombinedOutput(); err != nil {
		t.Fatalf("connect: %v\n%s", err, out)
	}
	_ = exec.Command("podman", "unshare", "chown", "-R", "0:0", s.hermesHome).Run()

	start := time.Now()
	prompts := []string{
		"I want a monthly summary of my spending from my bank. The bank's API is at " + s.bankURL +
			" and it needs a key sent as a Bearer token; transactions are at accounts/main/transactions?from=YYYY-MM. Set it up with Vornik.",
		"I approved everything on my phone. Continue the setup, and ask me for the bank key the way Vornik wants.",
		"Done, I entered the key on my phone. Finish setting up the monthly summary.",
		"Run last month's summary now and tell me the total.",
	}
	approved := 0
	for i, p := range prompts {
		r := hermesAgentRun(t, s, ctl, p)
		t.Logf("turn %d (%s): %s", i+1, time.Since(start).Round(time.Second), tail(r.Reply, 600))
		approved += approveAll(t, phone, map[string]string{"BANK_KEY": BankKeyCanary, "KEY": BankKeyCanary})
	}
	authorized := 0
	for _, br := range s.bank.Requests() {
		if br.Authorized {
			authorized++
		}
	}
	got := dirContains(t, s.hermesHome, "-1325.39")
	t.Logf("real-model arm: %d phone approvals, %d authorized bank reads, summary reached Hermes: %v, wall time %s",
		approved, authorized, got, time.Since(start).Round(time.Second))
	sweep := NewSweep([]string{BankKeyCanary}, []string{RawRecordCanary})
	sweep.AddDir("the Hermes home (key file excluded)", true, withoutKeys(t, s.hermesHome))
	sweepVornik(t, s, sweep, realQuiet(t, s))
	t.Logf("canary sweep:\n%s", sweep.Report())
	if v := sweep.Violations(); len(v) != 0 {
		t.Fatalf("canary sweep (this one does gate: a leak is a leak):\n%s", strings.Join(v, "\n"))
	}
	if !got {
		t.Log("RESULT: the real model did not complete the setup in this run (non-gating; recorded)")
	}
}

// certifiedCodex is the codex-cli the Responses stub was probed against.
const certifiedCodex = "codex-cli 0.153."

// assertBankCalledWithKey: the bank was called on its transactions path, and
// every call carried the key the phone entered.
func assertBankCalledWithKey(t *testing.T, s *stack) {
	t.Helper()
	reqs := s.bank.Requests()
	hit := false
	for _, r := range reqs {
		if !r.Authorized {
			t.Fatalf("the bank saw a request without the key: %+v", r)
		}
		hit = hit || r.Path == "/v1/accounts/main/transactions"
	}
	if !hit {
		t.Fatalf("the bank's transactions were never read (%d requests): the key was not exercised", len(reqs))
	}
}

// realQuiet is harnessQuiet, plus the places the real model may not reach in
// a run it does not complete: they are then expected empty, said so.
func realQuiet(t *testing.T, s *stack) map[string]string {
	q := map[string]string{}
	for k, v := range harnessQuiet {
		q[k] = v
	}
	// Keyed on whether any task ran, not on bank activity (review a507).
	var tasks int
	_ = laneDB(t, s).QueryRowContext(context.Background(), `SELECT count(*) FROM tasks`).Scan(&tasks)
	if tasks == 0 {
		for _, tb := range []string{"tasks", "executions", "execution_step_outcomes", "step_prompts", "tool_audit_log"} {
			q[tb] = "the real model ran no workflow in this run"
		}
	}
	return q
}
