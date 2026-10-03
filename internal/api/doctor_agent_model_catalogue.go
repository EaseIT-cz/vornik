package api

import (
	"fmt"
	"strings"
)

// AgentModelFinding is one agent_admin.models entry that is not offered.
type AgentModelFinding struct {
	ID, Why string
}

// AgentModelCatalogueStatus is the catalogue as the daemon classified it
// against the live router and the pricing table, now.
type AgentModelCatalogueStatus struct {
	Examined int
	Offered  int
	Dropped  []AgentModelFinding
}

// SetAgentModelCatalogue wires the agent_model_catalogue check; nil leaves
// it SKIPPED.
func (h *DoctorHandlers) SetAgentModelCatalogue(fn func() AgentModelCatalogueStatus) {
	h.agentModels = fn
}

// checkAgentModelCatalogue names every agent_admin.models entry an agent
// role cannot choose, and why (agent-administered design §18.6 item 2: no
// route, a command-line provider, or remote without an exact pricing.yaml
// entry). It states how many entries it examined.
func (h *DoctorHandlers) checkAgentModelCatalogue() DoctorCheck {
	const name = "agent_model_catalogue"
	if h.agentModels == nil {
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "agent model catalogue not wired"}
	}
	st := h.agentModels()
	noun := "entries"
	if st.Examined == 1 {
		noun = "entry"
	}
	scope := fmt.Sprintf("%d catalogue %s examined (agent_admin.models), %d offered", st.Examined, noun, st.Offered)
	if st.Examined == 0 {
		return DoctorCheck{Name: name, Status: "OK", Message: scope + "; no model is offered to agent roles, which run on the global agent model"}
	}
	if len(st.Dropped) == 0 {
		return DoctorCheck{Name: name, Status: "OK", Message: scope}
	}
	items := make([]string, 0, len(st.Dropped))
	for _, d := range st.Dropped {
		items = append(items, d.ID+": "+d.Why)
	}
	return DoctorCheck{Name: name, Status: "WARNING", Message: scope + "; not offered to agent roles: " + strings.Join(items, "; ")}
}
