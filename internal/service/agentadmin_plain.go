package service

import (
	"encoding/json"
	"strings"

	"vornik.io/vornik/internal/agentadmin"
	"vornik.io/vornik/internal/approverdevice"
	"vornik.io/vornik/internal/persistence"
)

// describeRequest reads the plain view the renderer put in the request's
// approved document (design §18.7). It never computes one here: what the
// page shows is what the device approves. A request filed before §18.7
// carries none and is shown as before.
func describeRequest(r persistence.AgentApprovalRequestRow) *approverdevice.Description {
	var pl struct {
		Change struct {
			Plain *agentadmin.PlainView `json:"plain"`
		} `json:"change"`
	}
	if json.Unmarshal(r.Rendered, &pl) != nil || pl.Change.Plain == nil {
		return nil
	}
	p := pl.Change.Plain
	return &approverdevice.Description{Summary: p.Summary, Level: p.Level, Reasons: p.Reasons}
}

// describeAction is a proposed write's description: always High (it acts in
// the user's accounts), grouped by the task that drafted it so one run's
// writes are reviewed together (approval fatigue, tier 1).
func describeAction(r persistence.AgentApprovalRequestRow) *approverdevice.Description {
	var pl actionPayload
	if json.Unmarshal(r.Rendered, &pl) != nil || pl.ActionID == "" {
		return nil
	}
	wf := pl.Workflow
	if i := strings.LastIndex(wf, "--"); i >= 0 {
		wf = wf[i+2:]
	}
	d := &approverdevice.Description{
		Summary: r.Sentence,
		Level:   agentadmin.LevelHigh,
		Reasons: []string{"it acts in your account on your behalf, exactly as shown below"},
	}
	// One task holds at most one write per action kind, so the group is the
	// workflow's action across runs (broker write-actions design, tier 1).
	if key := groupKey(pl.Workflow + "_" + pl.Action); key != "" && pl.Workflow != "" {
		d.Group, d.GroupTitle = key, wf+": "+strings.ReplaceAll(pl.Action, "_", " ")
	}
	return d
}

// groupKey maps an identifier to a page group key ([a-z0-9_] only).
func groupKey(id string) string {
	var b strings.Builder
	for _, c := range strings.ToLower(id) {
		switch {
		case c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '_':
			b.WriteRune(c)
		case c == '-':
			b.WriteRune('_')
		}
	}
	return b.String()
}
