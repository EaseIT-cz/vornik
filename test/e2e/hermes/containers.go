package hermes

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	vruntime "vornik.io/vornik/internal/runtime"
)

// reapMaxAge bounds how long a lane container may live. The longest lane runs
// about 16 minutes, so a lane container older than this is a leak from a run
// whose t.Cleanup never ran (go test -timeout, SIGKILL, a host crash).
const reapMaxAge = 6 * time.Hour

const (
	laneLabelKey = "vornik-e2e-lane"
	laneLabelVal = "hermes"
	runLabelKey  = "vornik-e2e-run"
)

// runSuffix names this test process's containers. One stack per process (lane
// design, "Containers are named per run"): two stacks in one process would
// share it.
var runSuffix = newRunSuffix()

func newRunSuffix() string {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		panic("hermes e2e: no randomness for the run suffix: " + err.Error())
	}
	return hex.EncodeToString(b)
}

func pgContainerName() string    { return "vornik-e2e-pg-" + runSuffix }
func llamaContainerName() string { return "vornik-e2e-llama-" + runSuffix }

// laneLabels are the podman run arguments that mark a container as this
// lane's and this run's.
func laneLabels() []string {
	return []string{"--label", laneLabelKey + "=" + laneLabelVal, "--label", runLabelKey + "=" + runSuffix}
}

// podmanContainer is the part of `podman ps --format json` the reaper reads.
// Created is Unix seconds.
type podmanContainer struct {
	ID      string
	State   string
	Created int64
	Names   []string
	Labels  map[string]string
}

// taskOwner answers whether a task is one of this lane's daemons' tasks. A
// non-nil error means "cannot tell".
type taskOwner func(projectID, taskID string) (bool, error)

// foreignContainers names the running containers that are not this lane's.
// The dedicated-host memory override is refused while any exist (lane
// design, "Memory guardrails"). A container is the lane's when it carries
// vornik-e2e-lane=hermes, or when it is an agent container (vornik.managed)
// whose vornik.taskId owns reports as a task of this lane's daemons
// (amendment W8-AJ: the lane's own agents carry the daemon's labels, not the
// lane's). Production's agent containers carry the same daemon labels, so
// vornik.managed alone never exempts one; an unknown task, a missing task
// id, a nil owns or an owns error leaves the container foreign (fail closed).
func foreignContainers(in []podmanContainer, owns taskOwner) []string {
	var out []string
	for _, c := range in {
		if c.State != "running" || c.Labels[laneLabelKey] == laneLabelVal {
			continue
		}
		name := c.ID
		if len(c.Names) > 0 {
			name = c.Names[0]
		}
		if c.Labels[vruntime.LabelManaged] == vruntime.LabelValueTrue && owns != nil {
			if task := c.Labels[vruntime.LabelTaskID]; task != "" {
				mine, err := owns(c.Labels[vruntime.LabelProjectID], task)
				if err == nil && mine {
					continue
				}
				if err != nil {
					name += " (task owner unknown: " + err.Error() + ")"
				}
			}
		}
		out = append(out, name)
	}
	return out
}

// laneDaemon is one running lane daemon the owner may ask.
type laneDaemon struct{ apiURL, adminKey string }

var (
	laneDaemonsMu sync.Mutex
	laneDaemons   = map[*laneDaemon]struct{}{}
)

// registerLaneDaemon records a running lane daemon (startStackWith, once its
// project guard passed) and returns its removal, for t.Cleanup.
func registerLaneDaemon(d laneDaemon) func() {
	p := &d
	laneDaemonsMu.Lock()
	laneDaemons[p] = struct{}{}
	laneDaemonsMu.Unlock()
	return func() {
		laneDaemonsMu.Lock()
		delete(laneDaemons, p)
		laneDaemonsMu.Unlock()
	}
}

func registeredLaneDaemons() []laneDaemon {
	laneDaemonsMu.Lock()
	defer laneDaemonsMu.Unlock()
	out := make([]laneDaemon, 0, len(laneDaemons))
	for p := range laneDaemons {
		out = append(out, *p)
	}
	return out
}

// laneTaskOwner asks each lane daemon for the task, with the lane's admin
// key: GET /api/v1/projects/{project}/tasks/{task}. 200 naming the same task
// is the lane's; 404 from every daemon is not; anything else (a transport
// error, another status, a reply naming another task, an id that is not a
// plain path segment) is "cannot tell".
func laneTaskOwner(client *http.Client, daemons []laneDaemon) taskOwner {
	return func(projectID, taskID string) (bool, error) {
		for _, id := range []string{projectID, taskID} {
			if id == "" || strings.ContainsAny(id, "/?#%") || id == "." || id == ".." {
				return false, fmt.Errorf("label value %q is not a plain id", id)
			}
		}
		for _, d := range daemons {
			mine, err := askLaneDaemon(client, d, projectID, taskID)
			if err != nil || mine {
				return mine, err
			}
		}
		return false, nil
	}
}

func askLaneDaemon(client *http.Client, d laneDaemon, projectID, taskID string) (bool, error) {
	req, err := http.NewRequest(http.MethodGet, d.apiURL+"/api/v1/projects/"+projectID+"/tasks/"+taskID, nil)
	if err != nil {
		return false, err
	}
	req.Header.Set("Authorization", "Bearer "+d.adminKey)
	resp, err := client.Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	switch resp.StatusCode {
	case http.StatusNotFound:
		return false, nil
	case http.StatusOK:
		var got struct {
			TaskID string `json:"taskId"`
		}
		if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&got); err != nil {
			return false, fmt.Errorf("lane daemon %s: task %s: %w", d.apiURL, taskID, err)
		}
		if got.TaskID != taskID {
			return false, fmt.Errorf("lane daemon %s answered task %q for %q", d.apiURL, got.TaskID, taskID)
		}
		return true, nil
	default:
		return false, fmt.Errorf("lane daemon %s: task %s: HTTP %d", d.apiURL, taskID, resp.StatusCode)
	}
}

// reaperListArgs lists the lane's containers by label, never by name prefix.
func reaperListArgs() []string {
	return []string{"ps", "-a", "--filter", "label=" + laneLabelKey + "=" + laneLabelVal, "--format", "json"}
}

// reapCandidates picks the lane containers to remove: those that have exited,
// and any older than reapMaxAge. A container younger than reapMaxAge that has
// not exited (a live sibling run, or one still starting) is never returned.
// podman ps has no "created before" filter, so the cutoff is computed here.
func reapCandidates(in []podmanContainer, now time.Time) []string {
	cutoff := now.Add(-reapMaxAge).Unix()
	var ids []string
	for _, c := range in {
		if c.State == "exited" || c.Created < cutoff {
			ids = append(ids, c.ID)
		}
	}
	return ids
}
