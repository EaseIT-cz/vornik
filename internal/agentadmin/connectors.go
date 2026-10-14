package agentadmin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"vornik.io/vornik/internal/agentns"
)

// Connectors (agent-administered Vornik design §19.14, build decisions
// §19.14.8; GitHub #314): a recipe that also carries a vetted connector
// block, which a paired approver device may install by itself. The block
// fixes every server URL (the person never types an endpoint); the device
// fills only enum and cron variables and the schedule's timezone. A
// connector whose vetting is stale, or that needs an OAuth sign-in, is
// demoted: it still loads as a recipe for agents, and the device does not
// offer it.

// RecipeConnector is a recipe's connector block.
type RecipeConnector struct {
	Title   string                     `yaml:"title" json:"title"`
	Servers map[string]ConnectorServer `yaml:"servers" json:"servers"`
	Vetted  ConnectorVetting           `yaml:"vetted" json:"vetted"`
}

// ConnectorServer is one need's vetted server.
type ConnectorServer struct {
	URL       string `yaml:"url" json:"url"`
	Transport string `yaml:"transport" json:"transport,omitempty"`
}

// ConnectorVetting records who vetted the connector, when, and which recipe
// version (§19.14.1 rule 4).
type ConnectorVetting struct {
	By            string `yaml:"by" json:"by"`
	At            string `yaml:"at" json:"at"`
	RecipeVersion int    `yaml:"recipe_version" json:"recipe_version"`
}

// connectorFile is the operator's vetting file beside a recipe (design
// §19.14.11): operator-owned, never shipped, so config-deploy never writes
// it and the shipped recipe.yaml keeps following the template.
const connectorFile = "connector.yaml"

// applyOperatorConnector reads the recipe's operator vetting file, when
// there is one, as the recipe's connector block, under the same rules.
// A recipe that also carries an inline block is refused: two vettings.
func applyOperatorConnector(fsys fs.FS, file string, r *Recipe) error {
	body, err := fs.ReadFile(fsys, file)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("connector: %w", err)
	}
	if r.Connector != nil {
		return fmt.Errorf("connector: the recipe carries a connector block and %s vets it too; keep one", connectorFile)
	}
	var c RecipeConnector
	dec := yaml.NewDecoder(bytes.NewReader(body))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil {
		return fmt.Errorf("connector: %s does not match the connector fields: %v", connectorFile, err)
	}
	for _, s := range c.Servers {
		if strings.Contains(s.URL, "{{") || strings.Contains(s.URL, "}}") {
			return fmt.Errorf("connector: %s: a server URL may not name a variable", connectorFile)
		}
	}
	r.Connector, r.demoted = &c, ""
	return checkConnector(r)
}

// checkConnector applies §19.14.1 rules 1 to 3 (§19.14.8 items 1 to 3) to a
// recipe's connector block, refusing the catalogue, and records a rule-4 or
// OAuth demotion. A recipe without the block passes.
func checkConnector(r *Recipe) error {
	c := r.Connector
	if c == nil {
		return nil
	}
	if err := checkOneLine("connector title", c.Title, true); err != nil {
		return err
	}
	if err := checkOneLine("connector vetted.by", c.Vetted.By, true); err != nil {
		return err
	}
	if err := checkOneLine("connector vetted.at", c.Vetted.At, true); err != nil {
		return err
	}
	if c.Vetted.RecipeVersion < 1 {
		return errors.New("connector vetted.recipe_version must be an integer from 1")
	}
	if err := checkConnectorServers(r); err != nil {
		return err
	}
	if err := checkConnectorVariables(r); err != nil {
		return err
	}
	// An OAuth need is offered since §19.14.10 (install, sign in, then a
	// second confirm); only a stale vetting demotes.
	if c.Vetted.RecipeVersion != r.Version {
		r.demoted = fmt.Sprintf("the connector was vetted for recipe version %d, and the recipe is version %d; vet it again", c.Vetted.RecipeVersion, r.Version)
	}
	return nil
}

// checkConnectorServers is rule 1: one vetted server per need and no other,
// each a plain URL over streamable HTTP; a need's literal URL is its
// server's; a url variable names one server URL.
func checkConnectorServers(r *Recipe) error {
	c := r.Connector
	needs := map[string]bool{}
	for _, n := range r.Needs {
		needs[n.Name] = true
		s, ok := c.Servers[n.Name]
		if !ok {
			return fmt.Errorf("connector: the need %s has no vetted server; a connector fixes every server", n.Name)
		}
		if err := checkURL(s.URL); err != nil {
			return fmt.Errorf("connector: server %s: %v", n.Name, err)
		}
		if s.Transport != "" && s.Transport != TransportStreamableHTTP {
			return fmt.Errorf("connector: server %s: the transport must be %s; an install carries no other", n.Name, TransportStreamableHTTP)
		}
		if !varRefRe.MatchString(n.URL) && n.URL != s.URL {
			return fmt.Errorf("connector: the need %s names %s, and its vetted server is %s", n.Name, n.URL, s.URL)
		}
	}
	for name := range c.Servers {
		if !needs[name] {
			return fmt.Errorf("connector: the server %s is not a need of the recipe", name)
		}
	}
	return nil
}

// varUse is where a recipe references a variable.
type varUse struct {
	needs                          []string
	cron, timezone, scheduleInputs bool
}

// variableUses maps each referenced variable to where it is used.
func variableUses(r *Recipe) map[string]*varUse {
	out := map[string]*varUse{}
	use := func(value string) *varUse {
		m := varRefRe.FindStringSubmatch(value)
		if m == nil {
			return nil
		}
		if out[m[1]] == nil {
			out[m[1]] = &varUse{}
		}
		return out[m[1]]
	}
	for _, n := range r.Needs {
		if u := use(n.URL); u != nil {
			u.needs = append(u.needs, n.Name)
		}
	}
	if d := r.ScheduleDefault; d != nil {
		if u := use(d.Cron); u != nil {
			u.cron = true
		}
		if u := use(d.Timezone); u != nil {
			u.timezone = true
		}
		for _, v := range d.Inputs {
			if s, ok := v.(string); ok {
				if u := use(s); u != nil {
					u.scheduleInputs = true
				}
			}
		}
	}
	return out
}

// checkConnectorVariables is rule 2 with rule 1's binding: a url variable is
// bound to one vetted server URL by the needs that use it and is used
// nowhere else; a string variable is the schedule's timezone only; enum and
// cron are the person's.
func checkConnectorVariables(r *Recipe) error {
	uses := variableUses(r)
	for _, name := range r.sortedVariableNames() {
		v, u := r.Variables[name], uses[name]
		if u == nil {
			u = &varUse{}
		}
		switch v.Type {
		case VarURL:
			if len(u.needs) == 0 || u.cron || u.timezone || u.scheduleInputs {
				return fmt.Errorf("connector: the url variable %s is not bound to a vetted server; a connector has no URL the person types", name)
			}
			first := r.Connector.Servers[u.needs[0]].URL
			for _, n := range u.needs[1:] {
				if r.Connector.Servers[n].URL != first {
					return fmt.Errorf("connector: the url variable %s is used by servers with different vetted URLs", name)
				}
			}
		case VarString:
			if !u.timezone || len(u.needs) > 0 || u.cron || u.scheduleInputs {
				return fmt.Errorf("connector: the string variable %s is not the schedule's timezone; a connector takes only enum, cron and the timezone", name)
			}
		}
	}
	return nil
}

// IsConnector reports whether the device may offer the recipe: it carries a
// connector block and its vetting is current.
func (r *Recipe) IsConnector() bool { return r != nil && r.Connector != nil && r.demoted == "" }

// ConnectorDemoted says why a recipe's connector block is not offered, ""
// when it is (or when it has none).
func (r *Recipe) ConnectorDemoted() string { return r.demoted }

// Connectors returns the recipes the device may offer, sorted by name.
func (c *Catalogue) Connectors() []*Recipe {
	if c == nil {
		return nil
	}
	var out []*Recipe
	for _, r := range c.Recipes {
		if r.IsConnector() {
			out = append(out, r)
		}
	}
	return out
}

// Demoted lists each demoted connector with why, for the load log.
func (c *Catalogue) Demoted() map[string]string {
	out := map[string]string{}
	if c == nil {
		return out
	}
	for _, r := range c.Recipes {
		if r.Connector != nil && r.demoted != "" {
			out[r.Name] = r.demoted
		}
	}
	return out
}

// DeviceVariables are the variables the device's form fills (§19.14.8 item
// 3), sorted by name: enum, cron and the timezone string.
func (r *Recipe) DeviceVariables() []RecipeVariableView {
	var out []RecipeVariableView
	for _, name := range r.sortedVariableNames() {
		v := r.Variables[name]
		if v.Type == VarURL {
			continue
		}
		out = append(out, RecipeVariableView{Name: name, Type: v.Type, Help: v.Help, Values: v.Values, Default: v.Default, Required: v.Default == nil})
	}
	return out
}

// ReturnsLines is the recipe's egress in plain words.
func (r *Recipe) ReturnsLines() []string { return append([]string(nil), r.egressLines...) }

// ConnectorInstallInput is install_recipe's input for a device install of
// the connector name (§19.14.8 item 5b): every URL from the vetted block,
// only the device-fillable variables from vars (anything else in vars is
// ignored, never echoed), each checked against its type, a timezone as an
// IANA zone. It refuses, saying why, a recipe the device may not offer.
func (r *Renderer) ConnectorInstallInput(name, project, account string, vars map[string]string) (json.RawMessage, string) {
	rec := r.cat.Get(name)
	if !rec.IsConnector() {
		return nil, fmt.Sprintf("%q is not a connector this device can install", name)
	}
	in := InstallRecipeInput{Recipe: name, Project: project, Account: account, Variables: map[string]json.RawMessage{}}
	uses := variableUses(rec)
	for _, vn := range rec.sortedVariableNames() {
		v := rec.Variables[vn]
		var val string
		switch {
		case v.Type == VarURL:
			val = rec.Connector.Servers[uses[vn].needs[0]].URL
		case vars[vn] != "":
			val = strings.TrimSpace(vars[vn])
			if why := checkVariableValue(vn, v, val); why != "" {
				return nil, why
			}
			if v.Type == VarString {
				if _, err := time.LoadLocation(val); err != nil || val == "" || val == "Local" {
					return nil, fmt.Sprintf("%s must be an IANA timezone, e.g. Europe/Prague", vn)
				}
			}
		default:
			continue // the recipe's default applies
		}
		raw, _ := json.Marshal(val)
		in.Variables[vn] = raw
	}
	if account != "" && account != agentns.DefaultAccount && !agentns.ValidAccountLabel(account) {
		return nil, "the account label must be a-z, 0-9 or dashes, start with a letter and be at most 32 characters"
	}
	raw, err := json.Marshal(in)
	if err != nil {
		return nil, err.Error()
	}
	return raw, ""
}
