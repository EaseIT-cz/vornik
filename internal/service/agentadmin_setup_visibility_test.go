package service

import (
	"context"
	"encoding/json"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/persistence"
)

// Agent-administered design §18.14 ("Customer-zero findings: what the agent
// cannot see", GREEN at review be5a; round 2 governs): list_my_setup says
// which model each role runs on and where that sends its work, and which
// model destinations the namespace has approved; describe_installation
// says which daemon build answers.

// setupProject is one project of a list_my_setup answer, by id.
func setupProject(t *testing.T, v SetupView, id string) SetupProject {
	t.Helper()
	for _, p := range v.Projects {
		if p.ID == id {
			return p
		}
	}
	t.Fatalf("project %s not in list_my_setup: %+v", id, v.Projects)
	return SetupProject{}
}

// §18.14 finding 1 and round 2 F6, F7, F8: a role with a model and one
// without appear with and without model; each says where it runs; the
// role_details names equal roles, which is unchanged; a live destination is
// listed with exactly {destination, approved_at}; a withdrawn one is absent
// and the role on it says it needs approval again.
func TestListSetup_RoleDetailsAndApprovedDestinations(t *testing.T) {
	f, _ := modelFixture(t)
	ctx := context.Background()
	worker := agentadmin.RoleInput{Name: "worker", Instructions: "Do the worker job.", Tools: []string{"file_read", "file_write"}}
	res := f.do(agentadmin.VerbDefineSwarm, swarmRoles(modelRole("drafter", "qwen3:35b"), modelRole("critic", "google/gemini-pro"), worker))
	if res.Effect != agentadmin.EffectAwaiting {
		t.Fatalf("remote model: %+v", res)
	}
	f.approve(res)

	v, err := f.svc.ListSetup(ctx, f.key)
	if err != nil {
		t.Fatal(err)
	}
	p := setupProject(t, v, "hermes--finance")
	names := []string{}
	got := map[string]SetupRole{}
	for _, r := range p.RoleDetails {
		names = append(names, r.Name)
		got[r.Name] = r
	}
	if !reflect.DeepEqual(names, p.Roles) {
		t.Fatalf("role_details names %v, roles %v", names, p.Roles)
	}
	roles := append([]string(nil), p.Roles...)
	sort.Strings(roles)
	if !reflect.DeepEqual(roles, []string{"critic", "drafter", "worker"}) {
		t.Fatalf("roles changed shape: %v", p.Roles)
	}
	// tools is the GRANTED list: define_swarm adds file_write to every role
	// (§18.1), so a role asked with file_read alone shows both.
	want := map[string]SetupRole{
		"drafter": {Name: "drafter", Model: "qwen3:35b", Tools: []string{"file_read", "file_write"}, RunsOn: "local"},
		"critic":  {Name: "critic", Model: "google/gemini-pro", Tools: []string{"file_read", "file_write"}, RunsOn: "approved:" + vertexDest},
		"worker":  {Name: "worker", Tools: []string{"file_read", "file_write"}, RunsOn: "default"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("role_details:\n got %+v\nwant %+v", got, want)
	}
	// With and without model on the wire.
	raw, _ := json.Marshal(p.RoleDetails)
	var wire []map[string]any
	_ = json.Unmarshal(raw, &wire)
	for _, r := range wire {
		_, hasModel := r["model"]
		if (r["name"] == "worker") == hasModel {
			t.Errorf("role %v: model present = %v", r["name"], hasModel)
		}
		for _, k := range []string{"name", "tools", "runs_on"} {
			if _, ok := r[k]; !ok {
				t.Errorf("role %v has no %s", r["name"], k)
			}
		}
	}

	if len(v.ApprovedModelDestinations) != 1 || v.ApprovedModelDestinations[0].Destination != vertexDest || v.ApprovedModelDestinations[0].ApprovedAt.IsZero() {
		t.Fatalf("approved_model_destinations: %+v", v.ApprovedModelDestinations)
	}
	raw, _ = json.Marshal(v.ApprovedModelDestinations[0])
	var entry map[string]any
	_ = json.Unmarshal(raw, &entry)
	keys := []string{}
	for k := range entry {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if !reflect.DeepEqual(keys, []string{"approved_at", "destination"}) {
		t.Fatalf("an approved destination carries %v", keys)
	}

	// The operator withdraws the destination: it is no longer listed, and
	// the role on it says what it needs.
	if ok, err := f.c.repos.AgentGrants.MarkModelDestinationRemoved(ctx, "hermes", vertexDest, time.Now().UTC()); err != nil || !ok {
		t.Fatalf("withdraw: %v %v", ok, err)
	}
	v, err = f.svc.ListSetup(ctx, f.key)
	if err != nil {
		t.Fatal(err)
	}
	if len(v.ApprovedModelDestinations) != 0 {
		t.Fatalf("a withdrawn destination is listed: %+v", v.ApprovedModelDestinations)
	}
	raw, _ = json.Marshal(v)
	var whole map[string]any
	_ = json.Unmarshal(raw, &whole)
	if _, ok := whole["approved_model_destinations"]; !ok {
		t.Error("approved_model_destinations is absent when empty; it must be an empty list")
	}
	for _, r := range setupProject(t, v, "hermes--finance").RoleDetails {
		if r.Name == "critic" && r.RunsOn != "needs_approval:"+vertexDest {
			t.Fatalf("critic after withdrawal: %q", r.RunsOn)
		}
	}
}

// withdrawingGrants withdraws a destination right after the Nth
// ListModelDestinations read: a withdrawal landing in the middle of one
// list_my_setup (review 20261003-195e F1).
type withdrawingGrants struct {
	persistence.AgentGrantRepository
	reads, after int
	ns, dest     string
}

func (w *withdrawingGrants) ListModelDestinations(ctx context.Context, ns string) ([]persistence.AgentModelDestinationApproval, error) {
	rows, err := w.AgentGrantRepository.ListModelDestinations(ctx, ns)
	w.reads++
	if w.reads == w.after {
		_, _ = w.MarkModelDestinationRemoved(ctx, w.ns, w.dest, time.Now().UTC())
	}
	return rows, err
}

// Review 20261003-195e F1 (design §18.14): one list_my_setup never
// contradicts itself. A destination withdrawn between the reads one answer
// makes must not show as approved in runs_on while absent from
// approved_model_destinations (or the reverse): both come from the same
// rows. Checked for an approved and a withdrawn destination, with the
// withdrawal landing after each read in turn.
func TestListSetup_RunsOnAgreesWithApprovedDestinations(t *testing.T) {
	for after := 1; after <= 3; after++ {
		f, _ := modelFixture(t)
		ctx := context.Background()
		res := f.do(agentadmin.VerbDefineSwarm, swarmRoles(modelRole("critic", "google/gemini-pro")))
		f.approve(res)
		w := &withdrawingGrants{AgentGrantRepository: f.c.repos.AgentGrants, after: after, ns: "hermes", dest: vertexDest}
		f.c.repos.AgentGrants = w
		for call := 0; call < 2; call++ { // during the withdrawal, then after it
			v, err := f.svc.ListSetup(ctx, f.key)
			if err != nil {
				t.Fatal(err)
			}
			listed := map[string]bool{}
			for _, d := range v.ApprovedModelDestinations {
				listed[d.Destination] = true
			}
			for _, r := range setupProject(t, v, "hermes--finance").RoleDetails {
				approvedOn := strings.TrimPrefix(r.RunsOn, "approved:")
				if strings.HasPrefix(r.RunsOn, "approved:") != listed[approvedOn] {
					t.Fatalf("withdrawal after read %d, call %d: %s runs_on %q, approved_model_destinations %v",
						after, call, r.Name, r.RunsOn, v.ApprovedModelDestinations)
				}
			}
		}
		if w.reads < after {
			t.Fatalf("only %d reads; the withdrawal after read %d never landed", w.reads, after)
		}
	}
}

// §18.14 finding 2 and review be5a: describe_installation carries the
// daemon's version, read on every call, not when the service was built.
func TestDescribe_DaemonVersionReadEachCall(t *testing.T) {
	f, _ := modelFixture(t)
	ctx := context.Background()
	f.c.SetVersion("2026.10.3")
	caps, err := f.svc.Describe(ctx, f.key)
	if err != nil || caps.DaemonVersion != "2026.10.3" {
		t.Fatalf("daemon_version %q, %v", caps.DaemonVersion, err)
	}
	f.c.SetVersion("2026.10.4")
	if caps, _ = f.svc.Describe(ctx, f.key); caps.DaemonVersion != "2026.10.4" {
		t.Fatalf("daemon_version after an upgrade: %q", caps.DaemonVersion)
	}
	raw, _ := json.Marshal(caps)
	var m map[string]any
	_ = json.Unmarshal(raw, &m)
	if m["daemon_version"] != "2026.10.4" {
		t.Fatalf("on the wire: %v", m["daemon_version"])
	}
}
