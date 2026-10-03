package agentadmin

import (
	"fmt"
	"net"
	"net/url"
	"sort"
	"strings"

	"vornik.io/vornik/internal/netguard"
)

// A role's model (agent-administered design §18.6 item 2 in detail, GREEN at
// review a125). The operator lists the models agents may choose
// (agent_admin.models); whether a model is local or remote is computed from
// where the live router sends it, never declared, and a remote model's
// approval binds its destination, "<sub-provider>@<endpoint host>", for the
// whole namespace. A CLI-backed sub-provider cannot be bound to a
// destination (Vornik cannot see where its subprocess sends a call), so it
// is never offered.

// ModelSpec is one entry of agent_admin.models: the model id as routed and
// one plain line saying what it is good for.
type ModelSpec struct{ ID, GoodFor string }

// ModelRoute is where the live router sends a model id now: the
// sub-provider's name and its endpoint URL, or CLI for a subprocess-backed
// sub-provider (which has no endpoint Vornik can see).
type ModelRoute struct {
	SubProvider string
	Endpoint    string
	CLI         bool
}

// ModelResolver answers where the live router sends a model; false when the
// model has no route at all.
type ModelResolver func(model string) (ModelRoute, bool)

// ModelPricer answers a model's input and output price per million tokens,
// and whether the pricing table has an exact entry for it.
type ModelPricer func(model string) (in, out float64, known bool)

// Destination is where a role's content goes.
type Destination struct {
	SubProvider, Host string
	// Local is true only for an HTTP sub-provider whose endpoint host is a
	// loopback or private address (round 2 F6: an operator running such an
	// endpoint as a tunnel chose that route in their own config).
	Local bool
}

// String is the approval key, "<sub-provider>@<endpoint host>".
func (d Destination) String() string { return d.SubProvider + "@" + d.Host }

// Words names the destination in a sentence: "vertex at aiplatform.googleapis.com".
func (d Destination) Words() string { return d.SubProvider + " at " + d.Host }

// The reasons a model is not offered or may not run, the label values of
// vornik_agent_model_refusals_total (review 20261003-2ed0 B-extra).
const (
	RefusalOffCatalogue = "off_catalogue"
	RefusalNoRoute      = "no_route"
	RefusalCLIRoute     = "cli_route"
	RefusalNoEndpoint   = "no_endpoint"
	RefusalUnapproved   = "unapproved_destination"
)

// ModelRefusal is why a model may not run: a reason from the fixed set
// above and a sentence. The zero value means it may run.
type ModelRefusal struct{ Reason, Why string }

// DestinationOf classifies a route. why is set when the model cannot be
// offered: no route, a CLI-backed sub-provider, or no endpoint host (an
// empty endpoint included: never taken for local, review 2ed0 B1).
func DestinationOf(r ModelRoute, ok bool) (Destination, string) {
	d, refusal := destinationOf(r, ok)
	return d, refusal.Why
}

func destinationOf(r ModelRoute, ok bool) (Destination, ModelRefusal) {
	if !ok || r.SubProvider == "" {
		return Destination{}, ModelRefusal{RefusalNoRoute, "the chat router has no route for it"}
	}
	if r.CLI {
		return Destination{}, ModelRefusal{RefusalCLIRoute, fmt.Sprintf("the chat router sends it to %s, a command-line provider; Vornik cannot see where that sends a call", r.SubProvider)}
	}
	u, err := url.Parse(strings.TrimSpace(r.Endpoint))
	if err != nil || u.Hostname() == "" {
		return Destination{}, ModelRefusal{RefusalNoEndpoint, fmt.Sprintf("the chat router sends it to %s, whose endpoint has no host", r.SubProvider)}
	}
	host := strings.ToLower(u.Hostname())
	d := Destination{SubProvider: r.SubProvider, Host: host}
	if netguard.IsLocalHostname(host) {
		d.Local = true
	} else if ip := net.ParseIP(host); ip != nil && netguard.IsPrivateIP(ip) {
		d.Local = true
	}
	return d, ModelRefusal{}
}

// CatalogueModel is one model offered to agents, classified at load.
type CatalogueModel struct {
	ID, GoodFor string
	Dest        Destination
	// Priced is whether the pricing table has an exact entry; an unpriced
	// remote entry is never offered (round 2 F5).
	Priced                                  bool
	InputUSDPerMillion, OutputUSDPerMillion float64
}

// PriceLabel says the price as describe_installation shows it. An unpriced
// entry is always local (round 3 F4) and records no charge.
func (m CatalogueModel) PriceLabel() string {
	if !m.Priced {
		return "no charge recorded"
	}
	return fmt.Sprintf("$%s in, $%s out per million tokens", FormatUSD(m.InputUSDPerMillion), FormatUSD(m.OutputUSDPerMillion))
}

// Where says local or remote, as describe_installation shows it.
func (m CatalogueModel) Where() string {
	if m.Dest.Local {
		return "local"
	}
	return "remote: " + m.Dest.Words()
}

// CatalogueFinding is a catalogue entry that is not offered, and why; the
// doctor reports each one.
type CatalogueFinding struct{ ID, Why string }

// ModelGrant is one remote destination a define_swarm approval records for
// the namespace, with the model and role that asked for it, so the apply
// can re-resolve the model and refuse if the route moved (round 3 F2).
type ModelGrant struct {
	Destination string `json:"destination"`
	Model       string `json:"model"`
	Role        string `json:"role"`
}

// BuildCatalogue classifies the operator's catalogue against the live
// router and the pricing table. Entries with no route, a CLI-backed route,
// a duplicate id, or a remote route without an exact price are dropped,
// each with a finding.
func BuildCatalogue(specs []ModelSpec, resolve ModelResolver, price ModelPricer) (map[string]CatalogueModel, []CatalogueFinding) {
	out := map[string]CatalogueModel{}
	var findings []CatalogueFinding
	for _, s := range specs {
		id := strings.TrimSpace(s.ID)
		if id == "" {
			findings = append(findings, CatalogueFinding{ID: s.ID, Why: "the entry has no id"})
			continue
		}
		if _, dup := out[id]; dup {
			findings = append(findings, CatalogueFinding{ID: id, Why: "listed twice; the first entry is used"})
			continue
		}
		var rt ModelRoute
		ok := false
		if resolve != nil {
			rt, ok = resolve(id)
		}
		dest, why := DestinationOf(rt, ok)
		if why != "" {
			findings = append(findings, CatalogueFinding{ID: id, Why: why})
			continue
		}
		m := CatalogueModel{ID: id, GoodFor: strings.TrimSpace(s.GoodFor), Dest: dest}
		if price != nil {
			m.InputUSDPerMillion, m.OutputUSDPerMillion, m.Priced = price(id)
		}
		if !m.Priced && !dest.Local {
			findings = append(findings, CatalogueFinding{ID: id, Why: fmt.Sprintf("it is remote (%s) and pricing.yaml has no exact entry for it, so its spend could not be counted", dest.Words())})
			continue
		}
		out[id] = m
	}
	return out, findings
}

// SortedModels returns the catalogue in id order.
func SortedModels(cat map[string]CatalogueModel) []CatalogueModel {
	out := make([]CatalogueModel, 0, len(cat))
	for _, m := range cat {
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// RunModelRefusal is the run-time judgement of a model an agent role runs
// on (round 2 F2, round 3 F1, review a125): the model must still be in the
// operator's catalogue, and the live router must send it to a local
// destination or to one the namespace approved. It returns the destination
// it judged and the refusal (zero when the model may run).
func RunModelRefusal(model string, specs []ModelSpec, resolve ModelResolver, approved func(destination string) bool) (Destination, ModelRefusal) {
	listed := false
	for _, s := range specs {
		if strings.TrimSpace(s.ID) == model {
			listed = true
			break
		}
	}
	if !listed {
		return Destination{}, ModelRefusal{RefusalOffCatalogue, fmt.Sprintf("the model %q is not in the operator's catalogue (agent_admin.models)", model)}
	}
	var rt ModelRoute
	ok := false
	if resolve != nil {
		rt, ok = resolve(model)
	}
	dest, refusal := destinationOf(rt, ok)
	if refusal.Reason != "" {
		refusal.Why = fmt.Sprintf("the model %q cannot run for an agent role: %s", model, refusal.Why)
		return dest, refusal
	}
	if dest.Local || (approved != nil && approved(dest.String())) {
		return dest, ModelRefusal{}
	}
	return dest, ModelRefusal{RefusalUnapproved, fmt.Sprintf("the model %q now sends the role's work to %s, which no approver device has approved for this namespace; define the swarm again to ask", model, dest.String())}
}

// What list_my_setup says a role runs on (design §18.14 finding 1, round 2
// F6, GREEN at review be5a).
const (
	// runsOnDefault: the role names no model and runs on the operator's
	// default, which may be remote.
	runsOnDefault = "default"
	// runsOnLocal: the live router sends the role's model to a loopback or
	// private endpoint.
	runsOnLocal = "local"
)

// RoleRunsOn says where a role's model sends its work now, with the same
// judgement the executor makes before every attempt (RunModelRefusal), so
// list_my_setup and the run cannot disagree: default, local,
// approved:<destination>, needs_approval:<destination>, or, for a model the
// run-time check refuses for another reason (off the catalogue, no route, a
// command-line provider, no endpoint host), unavailable:<reason>.
func RoleRunsOn(model string, specs []ModelSpec, resolve ModelResolver, approved func(destination string) bool) string {
	if strings.TrimSpace(model) == "" {
		return runsOnDefault
	}
	dest, refusal := RunModelRefusal(model, specs, resolve, approved)
	switch refusal.Reason {
	case "":
		if dest.Local {
			return runsOnLocal
		}
		return "approved:" + dest.String()
	case RefusalUnapproved:
		return "needs_approval:" + dest.String()
	default:
		return "unavailable:" + refusal.Reason
	}
}
