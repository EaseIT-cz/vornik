//go:build e2e_hermes

package hermes

// H-rules and H-hints — Vornik's engineering guidance reaches Hermes (design
// 24, "Engineering guidance from Vornik", claims 5 to 7; GitHub #686).
//
// H-rules: a new Hermes session's prompt, as the model received it, carries
// the daemon's working-with-Vornik rules (the vornik.rules section, read
// through MCP initialize with the memory key).
//
// H-hints: Hermes commits with git. (a) The tool result the model received
// carries the hint marker; (b) the model then starts the review with
// vornik_companion_delegate, and the review task's input artifact holds the
// bytes of the file it named; (c) at the next session the daemon received
// the outcome commit.review, followed; (d) with commit.review muted in the
// project, a new session's commit delivers no marker.
//
// The memory key here is granted the review and ingest workflows (the
// operator decision of the same design); the main lane's key stays
// --no-delegate, so its H1 tool list is unchanged. Named TestHermesLane* so
// the release gate's -run selector includes it.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/testutil/testenv"
)

const (
	guidanceProject = "hermes-dev"
	// hintMarker is hints.MARKER without the closing bracket; the request
	// body carries it JSON-encoded, and it holds nothing JSON escapes.
	hintMarker = "[Vornik hint: guidance from Vornik, not part of this tool"
	workDir    = "/opt/data/work"
	promptRule = "[GUIDE0] Say hello in one short sentence."
	promptHint = "[GUIDE1] Using the terminal, run exactly this command: " + guideCommit +
		" Then follow any Vornik hint the result gives you, naming the file change.diff."
	guideCommit = "mkdir -p " + workDir + " && cd " + workDir + " && git init -q . && " +
		"git -c user.email=e2e@vornik.test -c user.name=e2e commit --allow-empty -qm lane-commit && " +
		"git show --stat > change.diff"
)

// guidanceFiles are the extra configs: the review and ingest workflows, the
// companion swarm on the scripted model, and the hermes-dev project.
func guidanceFiles(t *testing.T) map[string]string {
	t.Helper()
	root := repoRoot(t)
	read := func(rel string) string {
		b, err := os.ReadFile(filepath.Join(root, rel))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	swarm := strings.NewReplacer(
		"{{.projectId}}", guidanceProject,
		"{{.displayName}}", "Hermes dev (e2e)",
		"{{.defaultModel}}", scriptedModel,
		"ghcr.io/easeit-cz/vornik-agent:latest", agentImage(),
	).Replace(read("configs/project-templates/companion/swarm.md.tmpl"))
	return map[string]string{
		"workflows/companion-architectural-review.md": read("configs/workflows/companion-architectural-review.md"),
		"workflows/companion-rag-ingest.md":           read("configs/workflows/companion-rag-ingest.md"),
		"swarms/" + guidanceProject + "-swarm.md":     swarm,
		"projects/" + guidanceProject + ".yaml": `projectId: "` + guidanceProject + `"
displayName: "Hermes dev (e2e)"
swarmId: "` + guidanceProject + `-swarm"
defaultWorkflowId: "companion-architectural-review"
defaultPriority: 50
`,
	}
}

func guidanceScripts() []Script {
	return []Script{
		{Marker: "[GUIDE0]", Final: "Hello."},
		{Marker: "[GUIDE1]", Steps: []ScriptStep{
			{ToolSuffix: "terminal", Args: map[string]any{"command": guideCommit}},
			{ToolSuffix: "vornik_companion_delegate", Args: map[string]any{
				"workflow": "companion-architectural-review", "prompt": "Review this change.", "files": []any{"change.diff"}}},
		}, Final: "Committed, and the review is running."},
	}
}

func pgScalar(t *testing.T, sql string) string {
	t.Helper()
	out, err := exec.Command("podman", "exec", pgContainerName(), "psql", "-U", "vornik", "-d", "vornik", "-tAc", sql).Output()
	if err != nil {
		t.Fatalf("psql %q: %v", sql, err)
	}
	return strings.TrimSpace(string(out))
}

// asHermes hands the plugin's hint state and the work directory to Hermes's
// user before a run: runHermes chowns the home back to the host user after
// each run, and Hermes's own start fixes ownership on a targeted list that
// does not include them.
func asHermes(t *testing.T, s *stack) func() {
	return func() {
		uid, gid := hermesUser(t)
		for _, d := range []string{"vornik-companion", "work"} {
			p := filepath.Join(s.hermesHome, d)
			if _, err := os.Stat(p); err != nil {
				continue
			}
			if out, err := exec.Command("podman", "unshare", "chown", "-R", uid+":"+gid, p).CombinedOutput(); err != nil {
				t.Fatalf("hand %s to Hermes's user: %v\n%s", d, err, out)
			}
		}
	}
}

func TestHermesLaneGuidance(t *testing.T) {
	s := startStackWith(t, nil, guidanceFiles(t))
	s.memoryKey = grantKey(t, s.ctl, s.apiURL, s.adminKey, "--project", guidanceProject, "--client", "hermes",
		"--memory-all", "--workflows", "companion-architectural-review,companion-rag-ingest", "--budget-usd", "5")
	l := &laneRun{t: t, s: s, attempts: map[string]int{}, replies: map[string]string{}}
	startHermesModel(t, s, guidanceScripts()...)
	prepareHermesHome(t, s)
	// The commit needs Hermes's terminal (lane design §7 records why the
	// main lane offers only vornik and memory).
	cfg := filepath.Join(s.hermesHome, "config.yaml")
	b, err := os.ReadFile(cfg)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, cfg, strings.Replace(string(b), `cli: ["vornik", "memory"]`, `cli: ["vornik", "memory", "terminal", "file"]`, 1))
	watch := modelWatch(s)

	// H-rules (claim 5).
	lines := strings.SplitN(api.CompanionGuidance(), "\n", 4)
	rule := strings.TrimSpace(lines[2]) // the first sentence under the heading
	if len(rule) < 20 {
		t.Fatalf("H-rules: the guidance's first sentence is %q", rule)
	}
	rule = rule[:40]
	l.step("H-rules", promptRule, false, func(hermesRun) error {
		if !watch.WatchSeen() {
			return fmt.Errorf("no request to the model carried the standing rules (%q)", rule)
		}
		return nil
	}, asHermes(t, s), func() { watch.Watch(rule) })

	// H-hints (a) and (b) (claim 6).
	l.step("H-hints", promptHint, false, func(hermesRun) error {
		if !watch.WatchSeen() {
			return fmt.Errorf("(a) no tool result the model received carried the hint marker")
		}
		diff, err := os.ReadFile(filepath.Join(s.hermesHome, "work", "change.diff"))
		if err != nil {
			return fmt.Errorf("(b) the commit's change.diff is missing: %v", err)
		}
		sum := sha256.Sum256(diff)
		n, _ := strconv.Atoi(pgScalar(t, fmt.Sprintf(
			`SELECT count(*) FROM artifacts WHERE project_id = '%s' AND content_hash_sha256 = '%s'`,
			guidanceProject, hex.EncodeToString(sum[:]))))
		if n == 0 {
			return fmt.Errorf("(b) no input artifact in %s holds the bytes of change.diff", guidanceProject)
		}
		return nil
	}, asHermes(t, s), func() { watch.Watch(hintMarker) })

	// (c) the next session uploads the outcome (claim 7).
	l.step("H-hints-outcome", promptRule, false, func(hermesRun) error {
		got := pgScalar(t, fmt.Sprintf(
			`SELECT fired_total || '/' || followed_total FROM companion_hint_windows WHERE project_id = '%s' AND hint_id = 'commit.review'`,
			guidanceProject))
		if got == "" || strings.HasPrefix(got, "0/") || strings.HasSuffix(got, "/0") {
			return fmt.Errorf("(c) commit.review fired/followed on the daemon = %q, want a followed firing", got)
		}
		return nil
	}, asHermes(t, s))

	// (d) muted for the project, the hint is not delivered (claim 7).
	proj := filepath.Join(filepath.Dir(s.cfgPath), "configs", "projects", guidanceProject+".yaml")
	pb, err := os.ReadFile(proj)
	if err != nil {
		t.Fatal(err)
	}
	writeFile(t, proj, string(pb)+"companion:\n  mute: [\"commit.review\"]\n")
	reload := exec.Command(s.ctl, "config", "reload")
	reload.Env = testenv.Clean("VORNIK_API_URL="+s.apiURL, "VORNIK_API_KEY="+s.adminKey)
	if out, err := reload.CombinedOutput(); err != nil {
		t.Fatalf("H-hints-mute: config reload: %v\n%s", err, out)
	}
	if err := os.RemoveAll(filepath.Join(s.hermesHome, "work")); err != nil {
		t.Fatal(err)
	}
	l.step("H-hints-mute", promptHint, false, func(hermesRun) error {
		if watch.WatchSeen() {
			return fmt.Errorf("(d) a muted hint still reached the model")
		}
		return nil
	}, asHermes(t, s), func() { watch.Watch(hintMarker) })
}

// modelWatch is the request-body watcher of whichever model the lane runs.
func modelWatch(s *stack) interface {
	Watch(string)
	WatchSeen() bool
} {
	if s.hermesModel != nil {
		return s.hermesModel
	}
	return s.hermesRec
}
