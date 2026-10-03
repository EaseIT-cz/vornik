package agentadmin

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"gopkg.in/yaml.v3"

	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/cronexpr"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
)

// Recipes (agent-administered Vornik design §19): a broker workflow Vornik
// ships, tested in CI, that an admin agent installs with install_recipe by
// filling a few typed variables. The catalogue lives in
// configs/agent-templates/recipes: envelope.json (the answer envelope every
// recipe returns, §19.3) and one <name>/recipe.yaml per recipe (§19.1 as
// amended by §19.7 to §19.11). Every rule a recipe must follow is checked
// when the catalogue loads, so a recipe that breaks one never reaches an
// agent: the renderer refuses to build.

// Recipe directory layout.
const (
	recipesDir   = "recipes"
	envelopeFile = "recipes/envelope.json"
	recipeFile   = "recipe.yaml"
)

// Variable types (§19.1).
const (
	VarString = "string"
	VarEnum   = "enum"
	VarURL    = "url"
	VarCron   = "cron"
)

// maxVarStringRunes bounds a string variable (§19.1: bounded, no newlines).
const maxVarStringRunes = 200

// maxRecipeNeeds bounds a recipe's integrations.
const maxRecipeNeeds = 4

// Reserved envelope names (§19.7 F8, §19.8 F3): a recipe may not declare
// them itself, at the top level or per item.
var (
	reservedTopLevel = []string{"status", "empty", "as_of", "items", "errors"}
	reservedItem     = []string{"source", "link", "id"}
)

var (
	recipeNameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)
	varNameRe    = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	hostnameRe   = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?(\.[a-z0-9]([a-z0-9-]*[a-z0-9])?)+$`)
	varRefRe     = regexp.MustCompile(`^\{\{([a-z][a-z0-9_]{0,31})\}\}$`)
)

// Envelope is recipes/envelope.json: a versioned JSON Schema fragment merged
// into every recipe's egress (§19.3, §19.7 F8).
type Envelope struct {
	Version int            `json:"version"`
	Schema  map[string]any `json:"schema"`
}

// RecipeVariable is one variable the agent fills.
type RecipeVariable struct {
	Type    string   `yaml:"type" json:"type"`
	Help    string   `yaml:"help" json:"help,omitempty"`
	Values  []string `yaml:"values" json:"values,omitempty"`
	Default *string  `yaml:"default" json:"default,omitempty"`
}

// RecipeCredential is a needs entry's credential (§19.10): by name, with one
// kind.
type RecipeCredential struct {
	Name string `yaml:"name" json:"name"`
	Kind string `yaml:"kind" json:"kind"` // secret | oauth
}

// RecipeNeed is one integration a recipe needs: always an MCP server in this
// release (§19.10 F1).
type RecipeNeed struct {
	Name string `yaml:"name" json:"name"`
	// Reads says in plain words what the server reads ("your mail"); the
	// approval sentence uses it, so it is catalogue text, never a variable.
	Reads      string            `yaml:"reads" json:"reads"`
	URL        string            `yaml:"url" json:"-"`
	ReadTools  []string          `yaml:"read_tools" json:"read_tools"`
	Credential *RecipeCredential `yaml:"credential" json:"credential,omitempty"`
}

// RecipeRole is one role of a recipe's team.
type RecipeRole struct {
	Name         string   `yaml:"name"`
	Instructions string   `yaml:"instructions"`
	Tools        []string `yaml:"tools"`
}

// RecipeStep is one step of a recipe's workflow.
type RecipeStep struct {
	Name         string `yaml:"name"`
	Role         string `yaml:"role"`
	Instructions string `yaml:"instructions"`
}

// RecipeEgress is the recipe's own part of the answer (§19.7 F8): fields per
// item and fields beside the envelope's.
type RecipeEgress struct {
	ItemProperties map[string]any `yaml:"item_properties"`
	ItemRequired   []string       `yaml:"item_required"`
	Properties     map[string]any `yaml:"properties"`
	Required       []string       `yaml:"required"`
}

// RecipeWorkflow is the workflow a recipe installs.
type RecipeWorkflow struct {
	Slug    string         `yaml:"slug"`
	Purpose string         `yaml:"purpose"`
	Inputs  map[string]any `yaml:"inputs"`
	Steps   []RecipeStep   `yaml:"steps"`
	Egress  RecipeEgress   `yaml:"egress"`
}

// RecipeSchedule is schedule_default; its values may name variables.
type RecipeSchedule struct {
	Cron     string         `yaml:"cron"`
	Timezone string         `yaml:"timezone"`
	Inputs   map[string]any `yaml:"inputs"`
}

// Recipe is one recipe.yaml, as loaded and checked.
type Recipe struct {
	Name            string                    `yaml:"name"`
	Version         int                       `yaml:"version"`
	EnvelopeVersion int                       `yaml:"envelope_version"`
	Title           string                    `yaml:"title"`
	Summary         string                    `yaml:"summary"`
	Returns         string                    `yaml:"returns"`
	Variables       map[string]RecipeVariable `yaml:"variables"`
	Needs           []RecipeNeed              `yaml:"needs"`
	LinkHosts       []string                  `yaml:"link_hosts"`
	Team            []RecipeRole              `yaml:"team"`
	Workflow        RecipeWorkflow            `yaml:"workflow"`
	ScheduleDefault *RecipeSchedule           `yaml:"schedule_default"`

	// Computed at load.
	inputSchema string         // canonical JSON
	egress      map[string]any // the envelope merged in
	egressJSON  string         // its canonical JSON
	egressLines []string       // the plain-words field list
}

// Catalogue is the loaded recipes, sorted by name.
type Catalogue struct {
	Envelope Envelope
	Recipes  []*Recipe
}

// Get returns the named recipe, or nil.
func (c *Catalogue) Get(name string) *Recipe {
	if c == nil {
		return nil
	}
	for _, r := range c.Recipes {
		if r.Name == name {
			return r
		}
	}
	return nil
}

// LoadCatalogue reads the recipes under fsys (the agent-templates tree). A
// tree without a recipes directory has an empty catalogue; a recipe that
// breaks a rule is an error naming it (§19.8 F2: refused when the catalogue
// loads).
func LoadCatalogue(fsys fs.FS) (*Catalogue, error) {
	cat := &Catalogue{}
	if _, err := fs.Stat(fsys, recipesDir); errors.Is(err, fs.ErrNotExist) {
		return cat, nil
	}
	raw, err := fs.ReadFile(fsys, envelopeFile)
	if err != nil {
		return nil, fmt.Errorf("recipes: %w", err)
	}
	if err := json.Unmarshal(raw, &cat.Envelope); err != nil || cat.Envelope.Version < 1 || cat.Envelope.Schema == nil {
		return nil, fmt.Errorf("recipes: %s must carry a version and a schema", envelopeFile)
	}
	if err := checkEnvelope(cat.Envelope.Schema); err != nil {
		return nil, fmt.Errorf("recipes: %s: %w", envelopeFile, err)
	}
	entries, err := fs.ReadDir(fsys, recipesDir)
	if err != nil {
		return nil, fmt.Errorf("recipes: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		file := path.Join(recipesDir, e.Name(), recipeFile)
		body, err := fs.ReadFile(fsys, file)
		if err != nil {
			return nil, fmt.Errorf("recipes: %w", err)
		}
		r, err := parseRecipe(body, cat.Envelope)
		if err != nil {
			return nil, fmt.Errorf("recipe %s: %w", e.Name(), err)
		}
		if r.Name != e.Name() {
			return nil, fmt.Errorf("recipe %s: its name is %q; the directory and the name must be the same", e.Name(), r.Name)
		}
		cat.Recipes = append(cat.Recipes, r)
	}
	sort.Slice(cat.Recipes, func(i, j int) bool { return cat.Recipes[i].Name < cat.Recipes[j].Name })
	return cat, nil
}

// checkEnvelope requires the reserved fields the merge writes into.
func checkEnvelope(s map[string]any) error {
	props, _ := s["properties"].(map[string]any)
	for _, k := range reservedTopLevel {
		if _, ok := props[k]; !ok {
			return fmt.Errorf("the envelope does not declare %q", k)
		}
	}
	if itemProps(s) == nil {
		return errors.New("the envelope's items have no properties")
	}
	return nil
}

// itemProps returns schema.properties.items.items.properties, or nil.
func itemProps(schema map[string]any) map[string]any {
	props, _ := schema["properties"].(map[string]any)
	items, _ := props["items"].(map[string]any)
	item, _ := items["items"].(map[string]any)
	ip, _ := item["properties"].(map[string]any)
	return ip
}

// parseRecipe decodes one recipe strictly and checks every load rule.
func parseRecipe(body []byte, env Envelope) (*Recipe, error) {
	var generic any
	if err := yaml.Unmarshal(body, &generic); err != nil {
		return nil, fmt.Errorf("does not parse: %v", err)
	}
	var r Recipe
	dec := yaml.NewDecoder(bytes.NewReader(body))
	dec.KnownFields(true)
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("does not match the recipe fields: %v", err)
	}
	// YAML numbers decode as int; JSON Schema walkers and the canonical
	// form expect JSON numbers, so the schema parts are re-read as JSON.
	for _, m := range []*map[string]any{&r.Workflow.Inputs, &r.Workflow.Egress.ItemProperties, &r.Workflow.Egress.Properties} {
		if err := asJSON(m); err != nil {
			return nil, err
		}
	}
	if r.ScheduleDefault != nil {
		if err := asJSON(&r.ScheduleDefault.Inputs); err != nil {
			return nil, err
		}
	}
	if err := checkRecipeHead(&r, env); err != nil {
		return nil, err
	}
	if err := checkRecipeVariables(&r); err != nil {
		return nil, err
	}
	if err := checkReferences(&r, generic); err != nil {
		return nil, err
	}
	if err := checkRecipeNeeds(&r); err != nil {
		return nil, err
	}
	if err := checkRecipeTeam(&r); err != nil {
		return nil, err
	}
	if err := checkRecipeWorkflow(&r); err != nil {
		return nil, err
	}
	egress, err := mergeEnvelope(env, &r)
	if err != nil {
		return nil, err
	}
	r.egress = egress
	canon, err := canonicalOf(egress)
	if err != nil {
		return nil, err
	}
	if len(canon) > maxEgressBytes {
		return nil, fmt.Errorf("the merged egress schema is over %d bytes", maxEgressBytes)
	}
	r.egressJSON = string(canon)
	lines, err := egressLinesOf(egress)
	if err != nil {
		return nil, fmt.Errorf("egress: %v", err)
	}
	r.egressLines = lines
	if _, err := registry.CompileBrokerSchema(r.Name+"-egress", egress); err != nil {
		return nil, fmt.Errorf("the merged egress schema does not compile: %v", err)
	}
	return &r, nil
}

// asJSON re-reads a YAML-decoded map as JSON.
func asJSON(m *map[string]any) error {
	if *m == nil {
		return nil
	}
	raw, err := json.Marshal(*m)
	if err != nil {
		return fmt.Errorf("a schema is not JSON: %v", err)
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil {
		return err
	}
	*m = out
	return nil
}

func checkRecipeHead(r *Recipe, env Envelope) error {
	if !recipeNameRe.MatchString(r.Name) || strings.Contains(r.Name, "--") {
		return fmt.Errorf("name %q must be 2 to 32 characters of a-z, 0-9 and single dashes", r.Name)
	}
	if r.Version < 1 {
		return errors.New("version must be an integer from 1")
	}
	if r.EnvelopeVersion != env.Version {
		return fmt.Errorf("envelope_version %d is not the catalogue's envelope (version %d)", r.EnvelopeVersion, env.Version)
	}
	for what, s := range map[string]string{"title": r.Title, "summary": r.Summary, "returns": r.Returns} {
		if err := checkOneLine(what, s, true); err != nil {
			return err
		}
	}
	return nil
}

func checkRecipeVariables(r *Recipe) error {
	for name, v := range r.Variables {
		if !varNameRe.MatchString(name) {
			return fmt.Errorf("variable %q must be a-z, 0-9 or _", name)
		}
		if err := checkOneLine("variable "+name+" help", v.Help, false); err != nil {
			return err
		}
		switch v.Type {
		case VarString, VarURL, VarCron:
			if len(v.Values) > 0 {
				return fmt.Errorf("variable %s: values are for an enum", name)
			}
		case VarEnum:
			if len(v.Values) == 0 {
				return fmt.Errorf("variable %s: an enum needs values", name)
			}
		default:
			return fmt.Errorf("variable %s: type %q must be string, enum, url or cron", name, v.Type)
		}
		if v.Default != nil {
			if why := checkVariableValue(name, v, *v.Default); why != "" {
				return fmt.Errorf("variable %s: its default: %s", name, why)
			}
		}
	}
	return nil
}

// checkVariableValue validates one value against its variable's type, or
// says why it is refused. The value is never echoed.
func checkVariableValue(name string, v RecipeVariable, val string) string {
	switch v.Type {
	case VarString:
		if n := len([]rune(val)); n == 0 || n > maxVarStringRunes {
			return fmt.Sprintf("%s must be 1 to %d characters", name, maxVarStringRunes)
		}
		for _, c := range val {
			if unicode.IsControl(c) {
				return fmt.Sprintf("%s must be one line without control characters", name)
			}
		}
	case VarEnum:
		if !contains(v.Values, val) {
			return fmt.Sprintf("%s must be one of %s", name, strings.Join(v.Values, ", "))
		}
	case VarURL:
		if err := checkURL(val); err != nil {
			return fmt.Sprintf("%s is not https (plain http is allowed only to this machine), or not a plain URL", name)
		}
	case VarCron:
		if len(strings.Fields(val)) != 5 {
			return fmt.Sprintf("%s must be a 5-field cron expression", name)
		}
		if _, err := cronexpr.Compile(val, nil); err != nil {
			return fmt.Sprintf("%s is not a valid cron expression", name)
		}
	}
	return ""
}

// checkReferences walks the raw document: a "{{" or "}}" may appear only as
// a whole value "{{name}}" of a needs entry's url or of schedule_default,
// naming a declared variable; every variable must be referenced (§19.1,
// §19.7 F7, §19.9 minor). Map keys may never hold one.
func checkReferences(r *Recipe, generic any) error {
	w := &refWalker{r: r, used: map[string]bool{}}
	if err := w.walk(generic, nil); err != nil {
		return err
	}
	for name := range r.Variables {
		if !w.used[name] {
			return fmt.Errorf("the variable %q is not used by any needs entry or schedule_default", name)
		}
	}
	return nil
}

// refWalker records the variables a recipe document references.
type refWalker struct {
	r    *Recipe
	used map[string]bool
}

func (w *refWalker) walk(v any, at []string) error {
	switch t := v.(type) {
	case map[string]any:
		for k, child := range t {
			if strings.Contains(k, "{{") || strings.Contains(k, "}}") {
				return fmt.Errorf("a key may not name a variable (%s)", strings.Join(at, "."))
			}
			if err := w.walk(child, append(append([]string(nil), at...), k)); err != nil {
				return err
			}
		}
	case []any:
		for i, child := range t {
			if err := w.walk(child, append(append([]string(nil), at...), fmt.Sprint(i))); err != nil {
				return err
			}
		}
	case string:
		return w.value(t, at)
	}
	return nil
}

// value checks one scalar: a reference is a whole value, at a position
// that may hold one, naming a declared variable.
func (w *refWalker) value(s string, at []string) error {
	if !strings.Contains(s, "{{") && !strings.Contains(s, "}}") {
		return nil
	}
	where := strings.Join(at, ".")
	m := varRefRe.FindStringSubmatch(s)
	switch {
	case m == nil:
		return fmt.Errorf("%s: a variable must be a whole value, \"{{name}}\"", where)
	case !substitutable(at):
		return fmt.Errorf("%s: variables are substituted only into a needs entry's url or schedule_default, never into instructions, schemas or tools", where)
	}
	if _, ok := w.r.Variables[m[1]]; !ok {
		return fmt.Errorf("%s names the undeclared variable %q", where, m[1])
	}
	w.used[m[1]] = true
	return nil
}

// substitutable reports whether a document path may hold a variable.
func substitutable(at []string) bool {
	switch {
	case len(at) == 3 && at[0] == "needs" && at[2] == "url":
		return true
	case len(at) == 2 && at[0] == "schedule_default" && (at[1] == "cron" || at[1] == "timezone"):
		return true
	case len(at) == 3 && at[0] == "schedule_default" && at[1] == "inputs":
		return true
	}
	return false
}

func checkRecipeNeeds(r *Recipe) error {
	if len(r.Needs) == 0 || len(r.Needs) > maxRecipeNeeds {
		return fmt.Errorf("needs between 1 and %d entries", maxRecipeNeeds)
	}
	seen := map[string]bool{}
	kinds := map[string]string{}
	for _, n := range r.Needs {
		if seen[n.Name] {
			return fmt.Errorf("needs entry %q appears twice", n.Name)
		}
		seen[n.Name] = true
		if err := checkNeed(n); err != nil {
			return err
		}
		if n.Credential == nil {
			continue
		}
		if err := checkNeedCredential(n); err != nil {
			return err
		}
		c := n.Credential
		if k, ok := kinds[c.Name]; ok && k != c.Kind {
			return fmt.Errorf("the credential %s is named with two kinds (%s and %s); a credential is identified by its name", c.Name, k, c.Kind)
		}
		kinds[c.Name] = c.Kind
	}
	for _, h := range r.LinkHosts {
		if !hostnameRe.MatchString(h) {
			return fmt.Errorf("link_hosts: %q is not a host name", h)
		}
	}
	return nil
}

// checkNeed checks one entry's name, words, URL and read tools.
func checkNeed(n RecipeNeed) error {
	if !nameRe.MatchString(n.Name) || strings.Contains(n.Name, "__") || strings.HasSuffix(n.Name, agentns.WriteSuffix) {
		return fmt.Errorf("needs entry %q: the name must be a-z, 0-9, _ or - (no double underscore, not ending in %s)", n.Name, agentns.WriteSuffix)
	}
	if err := checkOneLine("needs "+n.Name+" reads", n.Reads, true); err != nil {
		return err
	}
	if !varRefRe.MatchString(n.URL) {
		if err := checkURL(n.URL); err != nil {
			return fmt.Errorf("needs %s: %v", n.Name, err)
		}
	}
	if len(n.ReadTools) == 0 {
		return fmt.Errorf("needs %s: read_tools is required", n.Name)
	}
	for _, t := range n.ReadTools {
		if !toolNameRe.MatchString(t) {
			return fmt.Errorf("needs %s: %q is not a tool name", n.Name, t)
		}
	}
	return nil
}

// checkNeedCredential checks an entry's credential: a name, one kind, and
// for oauth the token slot name of its server.
func checkNeedCredential(n RecipeNeed) error {
	c := n.Credential
	if !credentialRe.MatchString(c.Name) || !agentns.ValidSecretName(c.Name) {
		return fmt.Errorf("needs %s: credential %q must be A-Z, 0-9 or _ and start with a letter", n.Name, c.Name)
	}
	switch c.Kind {
	case CredentialSecret:
	case CredentialOAuth:
		if c.Name != OAuthCredentialName(n.Name) {
			return fmt.Errorf("needs %s: an oauth credential is named %s", n.Name, OAuthCredentialName(n.Name))
		}
	default:
		return fmt.Errorf("needs %s: credential kind %q must be secret or oauth", n.Name, c.Kind)
	}
	return nil
}

// checkRecipeTeam applies §7.3 to the catalogue's roles: a role tool is a
// no-egress built-in or mcp__<entry>__<tool> of a needs entry's read_tools
// (§19.11 minor), and every read tool is held by some role (§19.8 F2).
func checkRecipeTeam(r *Recipe) error {
	if len(r.Team) == 0 || len(r.Team) > maxRoles {
		return fmt.Errorf("team: give between 1 and %d roles", maxRoles)
	}
	needs := map[string]*RecipeNeed{}
	for i := range r.Needs {
		needs[r.Needs[i].Name] = &r.Needs[i]
	}
	held := map[string]bool{}
	seen := map[string]bool{}
	for _, role := range r.Team {
		if !nameRe.MatchString(role.Name) || seen[role.Name] {
			return fmt.Errorf("team: role name %q must be unique and a-z, 0-9, _ or -", role.Name)
		}
		seen[role.Name] = true
		if err := checkRecipeRole(r, role, needs, held); err != nil {
			return err
		}
	}
	for _, n := range r.Needs {
		for _, t := range n.ReadTools {
			if !held[n.Name+"/"+t] {
				return fmt.Errorf("needs %s: the read tool %q is held by no role", n.Name, t)
			}
		}
	}
	return nil
}

// checkRecipeRole checks one role and records the read tools it holds.
func checkRecipeRole(r *Recipe, role RecipeRole, needs map[string]*RecipeNeed, held map[string]bool) error {
	if !nameRe.MatchString(r.RoleName(role.Name)) {
		return fmt.Errorf("team: the installed role name %q is too long", r.RoleName(role.Name))
	}
	if err := checkText("role "+role.Name+" instructions", role.Instructions, maxTextRunes, true); err != nil {
		return err
	}
	if len(role.Tools) == 0 || len(role.Tools) > maxRoleTools {
		return fmt.Errorf("role %s needs between 1 and %d tools", role.Name, maxRoleTools)
	}
	for _, t := range role.Tools {
		if registry.IsBrokerSafeBuiltin(t) {
			continue
		}
		srv, tool, ok := splitMCPTool(t)
		if !ok {
			return fmt.Errorf("role %s: %q is not an allowed tool (a no-egress built-in, or mcp__<needs entry>__<read tool>)", role.Name, t)
		}
		n := needs[srv]
		switch {
		case n == nil:
			return fmt.Errorf("role %s: %q names %q, which is not a needs entry", role.Name, t, srv)
		case !contains(n.ReadTools, tool):
			return fmt.Errorf("role %s: %q is not one of the read_tools of %s", role.Name, tool, srv)
		}
		held[srv+"/"+tool] = true
	}
	return nil
}

func checkRecipeWorkflow(r *Recipe) error {
	w := &r.Workflow
	if err := checkSlug("workflow slug", w.Slug); err != nil {
		return err
	}
	if err := checkOneLine("workflow purpose", w.Purpose, true); err != nil {
		return err
	}
	if len(w.Steps) == 0 || len(w.Steps) > maxSteps {
		return fmt.Errorf("workflow: give between 1 and %d steps", maxSteps)
	}
	roles := map[string]bool{}
	for _, role := range r.Team {
		roles[role.Name] = true
	}
	seen := map[string]bool{"done": true}
	for _, s := range w.Steps {
		if !nameRe.MatchString(s.Name) || seen[s.Name] {
			return fmt.Errorf("step name %q must be unique, not \"done\", and a-z, 0-9, _ or -", s.Name)
		}
		seen[s.Name] = true
		if !roles[s.Role] {
			return fmt.Errorf("step %s uses the role %q, which the team does not have", s.Name, s.Role)
		}
		if err := checkText("step "+s.Name+" instructions", s.Instructions, maxTextRunes, true); err != nil {
			return err
		}
	}
	raw, err := json.Marshal(w.Inputs)
	if err != nil {
		return err
	}
	if w.Inputs == nil {
		raw = nil
	}
	in, err := checkInputSchema(raw)
	if err != nil {
		return fmt.Errorf("workflow inputs: %v", err)
	}
	r.inputSchema = in
	for k := range w.Egress.Properties {
		if contains(reservedTopLevel, k) {
			return fmt.Errorf("egress: %q is the envelope's; a recipe may not declare it", k)
		}
	}
	for k := range w.Egress.ItemProperties {
		if contains(reservedItem, k) {
			return fmt.Errorf("egress: the item field %q is the envelope's; a recipe may not declare it", k)
		}
	}
	for _, k := range w.Egress.ItemRequired {
		if _, ok := w.Egress.ItemProperties[k]; !ok {
			return fmt.Errorf("egress: item_required names %q, which item_properties does not declare", k)
		}
	}
	for _, k := range w.Egress.Required {
		if _, ok := w.Egress.Properties[k]; !ok {
			return fmt.Errorf("egress: required names %q, which properties does not declare", k)
		}
	}
	return nil
}

// mergeEnvelope builds a recipe's egress: the envelope, its sources narrowed
// to the recipe's needs entries, items[].link given the host pattern of
// link_hosts (or dropped when there are none, §19.8 F2), and the recipe's
// own fields beside the envelope's (§19.7 F5, F8).
func mergeEnvelope(env Envelope, r *Recipe) (map[string]any, error) {
	schema, err := deepCopy(env.Schema)
	if err != nil {
		return nil, err
	}
	props := schema["properties"].(map[string]any)
	var sources []any
	for _, n := range r.Needs {
		sources = append(sources, n.Name)
	}
	ip := itemProps(schema)
	ip["source"] = map[string]any{"enum": sources}
	if errs, ok := props["errors"].(map[string]any); ok {
		if item, ok := errs["items"].(map[string]any); ok {
			if ep, ok := item["properties"].(map[string]any); ok {
				ep["source"] = map[string]any{"enum": sources}
			}
		}
	}
	if len(r.LinkHosts) == 0 {
		delete(ip, "link")
	} else {
		quoted := make([]string, len(r.LinkHosts))
		for i, h := range r.LinkHosts {
			quoted[i] = regexp.QuoteMeta(h)
		}
		link, _ := ip["link"].(map[string]any)
		if link == nil {
			link = map[string]any{"type": "string", "maxLength": float64(2000)}
		}
		link["pattern"] = "^https://(" + strings.Join(quoted, "|") + ")/"
		ip["link"] = link
	}
	for k, v := range r.Workflow.Egress.ItemProperties {
		ip[k] = v
	}
	item := schema["properties"].(map[string]any)["items"].(map[string]any)["items"].(map[string]any)
	item["required"] = appendRequired(item["required"], r.Workflow.Egress.ItemRequired)
	for k, v := range r.Workflow.Egress.Properties {
		props[k] = v
	}
	schema["required"] = appendRequired(schema["required"], r.Workflow.Egress.Required)
	return schema, nil
}

func appendRequired(have any, add []string) []any {
	var out []any
	seen := map[string]bool{}
	if list, ok := have.([]any); ok {
		for _, v := range list {
			if s, ok := v.(string); ok && !seen[s] {
				seen[s] = true
				out = append(out, s)
			}
		}
	}
	for _, s := range add {
		if !seen[s] {
			seen[s] = true
			out = append(out, s)
		}
	}
	return out
}

// egressLinesOf is the plain-words field list of a merged egress, from the
// same walker define_workflow uses. The link's host pattern is the one
// keyword agents may not write themselves; the walk reads a copy without it.
func egressLinesOf(egress map[string]any) ([]string, error) {
	cp, err := deepCopy(egress)
	if err != nil {
		return nil, err
	}
	if ip := itemProps(cp); ip != nil {
		if link, ok := ip["link"].(map[string]any); ok {
			delete(link, "pattern")
		}
	}
	raw, err := json.Marshal(cp)
	if err != nil {
		return nil, err
	}
	_, lines, err := checkEgressSchema(raw)
	return lines, err
}

func deepCopy(m map[string]any) (map[string]any, error) {
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	var out map[string]any
	return out, json.Unmarshal(raw, &out)
}

// sortedVariableNames returns a recipe's variable names, sorted.
func (r *Recipe) sortedVariableNames() []string {
	out := make([]string, 0, len(r.Variables))
	for k := range r.Variables {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// RoleName is a recipe role's installed name (§19.7 F4).
func (r *Recipe) RoleName(role string) string { return r.Name + "-" + role }

// EgressJSON is the merged egress schema's canonical JSON.
func (r *Recipe) EgressJSON() string { return r.egressJSON }

// SetupIncompleteError refuses an agent workflow whose integration's
// credential is not set (design §19.8 F4): at task creation, at plan
// resolve and before each retry, beside the reach check.
type SetupIncompleteError struct {
	Workflow   string
	Credential string
}

// FailureClass is the task failure class the executor records.
func (e *SetupIncompleteError) FailureClass() string {
	return persistence.TaskFailureClassSetupIncomplete
}

func (e *SetupIncompleteError) Error() string {
	return "SETUP_INCOMPLETE: enter " + e.Credential + " on your phone; the workflow " + e.Workflow + " cannot run until it is set"
}

// recipeVersionTag marks a workflow a recipe installed, in its version's
// build metadata ("1.0.0+recipe.inbox-digest.1"), which the workflow
// validator accepts and define_workflow never writes.
func recipeVersionTag(name string, version int) string {
	return fmt.Sprintf("1.0.0+recipe.%s.%d", name, version)
}

var recipeVersionRe = regexp.MustCompile(`^1\.0\.0\+recipe\.([a-z][a-z0-9-]{1,31})\.([0-9]+)$`)

// InstalledRecipe reads the recipe and version a workflow was installed
// from, or "", 0.
func InstalledRecipe(wf *registry.Workflow) (string, int) {
	if wf == nil {
		return "", 0
	}
	m := recipeVersionRe.FindStringSubmatch(wf.Version)
	if m == nil {
		return "", 0
	}
	var v int
	_, _ = fmt.Sscanf(m[2], "%d", &v)
	return m[1], v
}
