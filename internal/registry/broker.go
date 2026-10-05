package registry

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v5"

	"vornik.io/vornik/internal/agentns"
	"vornik.io/vornik/internal/cronexpr"
)

// Broker workflows — https://docs.vornik.io
// 2026-09-29-companion-privileged-work-broker-design.md.
//
// A broker workflow is one a front-end agent (Hermes, OpenClaw, …) may run
// through a companion key on a broker project. The front agent holds no
// credentials; the broker does the privileged work and returns ONLY the
// declared egress document. This file owns the static half of that boundary:
// what a `broker:` block must declare before the workflow is allowed to load.

const (
	// BrokerProvenanceThirdParty is the default: the egress was derived from
	// content the operator did not author (mail, web, tickets), so the
	// companion runs outputguard's injection rules on it and wraps it in the
	// untrusted markers.
	BrokerProvenanceThirdParty = "third_party"
	// BrokerProvenanceFirstParty is accepted only when every MCP server in
	// the broker project declares operator_authored (checked at delegate).
	BrokerProvenanceFirstParty = "first_party"

	// BrokerEgressMaxBytesCeiling matches the companion result inline cap
	// (companionResultInlineCapBytes). An egress document is never
	// truncated — oversize is an error — so the ceiling is also the largest
	// document a front agent can ever receive.
	BrokerEgressMaxBytesCeiling = 64 * 1024
	// BrokerEgressMaxBytesDefault applies when the block omits max_bytes.
	BrokerEgressMaxBytesDefault = 16 * 1024

	// BrokerUntrustedStringMax bounds a single x-untrusted string field.
	BrokerUntrustedStringMax = 512
	// BrokerUntrustedBudget bounds the SUM over all x-untrusted leaves of
	// maxLength × the maxItems of every array ancestor, so many short fields
	// cannot add up to one long instruction channel (design §4.2, review F1).
	BrokerUntrustedBudget = 1024
)

// brokerAllowedFormats are the string formats that constrain a value enough to
// stand in for an enum. Free-form formats (email, uri, hostname) carry
// attacker-choosable text and are deliberately absent.
var brokerAllowedFormats = map[string]bool{
	"date-time": true,
	"date":      true,
	"time":      true,
	"duration":  true,
	"uuid":      true,
}

// brokerRefusedKeywords would make the bound proof go through indirection the
// walker does not follow; v1 refuses them outright.
var brokerRefusedKeywords = []string{"$ref", "$defs", "definitions", "oneOf", "anyOf", "allOf", "not", "if", "patternProperties", "additionalItems", "prefixItems", "dependentSchemas"}

// WorkflowBroker is the `broker:` front-matter block.
type WorkflowBroker struct {
	// InputSchema is the JSON Schema (object) that delegate() validates the
	// caller's `inputs` against. `prompt` is refused for broker workflows.
	InputSchema map[string]any `yaml:"input_schema" json:"input_schema"`
	// Egress declares the one document that may leave.
	Egress BrokerEgress `yaml:"egress" json:"egress"`
	// Proposes declares the writes the workflow may propose. A proposal is
	// approved by a human in /inbox and then executed by the daemon itself,
	// never by an agent. See https://docs.vornik.io
	// 2026-09-29-broker-write-actions-and-push-design.md §4.
	Proposes []BrokerProposal `yaml:"proposes,omitempty" json:"proposes,omitempty"`
	// Schedule runs the workflow unattended, for agent workflows only
	// (agent-administered Vornik design §17). nil: on request only.
	Schedule *BrokerSchedule `yaml:"schedule,omitempty" json:"schedule,omitempty"`
}

// BrokerSchedule is when a broker workflow runs by itself, and with what.
type BrokerSchedule struct {
	// Cron is a 5-field POSIX expression, evaluated in Timezone.
	Cron string `yaml:"cron" json:"cron"`
	// Timezone is an IANA zone name; empty means UTC.
	Timezone string `yaml:"timezone,omitempty" json:"timezone,omitempty"`
	// Inputs are the fixed inputs of every scheduled run, valid against the
	// workflow's input_schema.
	Inputs map[string]any `yaml:"inputs" json:"inputs"`
}

// BrokerScheduleMinGap is the hourly floor of design §17.1: it bounds
// unattended spend independently of the budget.
const BrokerScheduleMinGap = time.Hour

// Location is the schedule's zone; an empty Timezone is UTC.
func (s *BrokerSchedule) Location() (*time.Location, error) {
	if strings.TrimSpace(s.Timezone) == "" {
		return time.UTC, nil
	}
	return time.LoadLocation(s.Timezone)
}

// BrokerProposal is one kind of write a broker workflow may propose.
type BrokerProposal struct {
	// Action is the kind's name, unique within the workflow.
	Action string `yaml:"action" json:"action"`
	// Tool is the MCP tool the daemon calls after approval, as
	// mcp__<server>__<tool>. The server must be declared broker_write, and
	// no role may hold the tool.
	Tool string `yaml:"tool" json:"tool"`
	// Output is the bare .json file name the agent writes the proposal to.
	Output string `yaml:"output" json:"output"`
	// ArgsSchema bounds the arguments the agent drafts.
	ArgsSchema map[string]any `yaml:"args_schema" json:"args_schema"`
	// MaxArgsBytes caps the proposal file; 0 = BrokerArgsMaxBytesDefault.
	MaxArgsBytes int `yaml:"max_args_bytes,omitempty" json:"max_args_bytes,omitempty"`
	// ApprovalTTL is how long a proposal waits for approval; "" = 24h.
	ApprovalTTL string `yaml:"approval_ttl,omitempty" json:"approval_ttl,omitempty"`
	// Standing declares the action eligible for standing grants (broker
	// write-actions design, tier 2). Absent: never covered, and the
	// proposal's JSON (the reach signature's input) is unchanged.
	Standing *BrokerStanding `yaml:"standing,omitempty" json:"standing,omitempty"`
}

// Proposal limits (design §4.1).
const (
	BrokerArgsMaxBytesDefault = 8 * 1024
	BrokerArgsMaxBytesCeiling = 32 * 1024
	brokerApprovalTTLDefault  = 24 * time.Hour
	brokerApprovalTTLMin      = 5 * time.Minute
	brokerApprovalTTLMax      = 7 * 24 * time.Hour
)

var brokerActionNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,40}$`)

// EffectiveMaxArgsBytes resolves the default.
func (p BrokerProposal) EffectiveMaxArgsBytes() int {
	if p.MaxArgsBytes <= 0 {
		return BrokerArgsMaxBytesDefault
	}
	return p.MaxArgsBytes
}

// EffectiveApprovalTTL resolves and bounds approval_ttl.
func (p BrokerProposal) EffectiveApprovalTTL() (time.Duration, error) {
	if strings.TrimSpace(p.ApprovalTTL) == "" {
		return brokerApprovalTTLDefault, nil
	}
	d, err := time.ParseDuration(p.ApprovalTTL)
	if err != nil {
		return 0, err
	}
	if d < brokerApprovalTTLMin || d > brokerApprovalTTLMax {
		return 0, fmt.Errorf("must be between %s and %s", brokerApprovalTTLMin, brokerApprovalTTLMax)
	}
	return d, nil
}

// ServerTool splits Tool into its MCP server and tool names.
func (p BrokerProposal) ServerTool() (server, tool string, ok bool) {
	rest, found := strings.CutPrefix(p.Tool, "mcp__")
	if !found {
		return "", "", false
	}
	server, tool, ok = strings.Cut(rest, "__")
	if !ok || server == "" || tool == "" || strings.ContainsAny(tool, "*?") {
		return "", "", false
	}
	return server, tool, true
}

// APITool splits an API write proposal "api:<name>:<METHOD>:<path>"
// (agent-administered Vornik plan P4.8): one write method of one project API
// at one fixed path. The path is part of what the person approves with the
// workflow; an action's arguments are the request body only.
func (p BrokerProposal) APITool() (api, method, apiPath string, ok bool) {
	rest, found := strings.CutPrefix(p.Tool, "api:")
	if !found {
		return "", "", "", false
	}
	parts := strings.SplitN(rest, ":", 3)
	if len(parts) != 3 {
		return "", "", "", false
	}
	api, method, apiPath = parts[0], parts[1], parts[2]
	if !apiNameRe.MatchString(api) || !apiMethods[method] || APIReadMethods[method] || !validAPIPath(apiPath) {
		return "", "", "", false
	}
	return api, method, apiPath, true
}

var apiPathRe = regexp.MustCompile(`^/[A-Za-z0-9._~/-]{0,199}$`)

// validAPIPath accepts a plain absolute path with no dot segments.
func validAPIPath(p string) bool {
	if !apiPathRe.MatchString(p) || strings.Contains(p, "//") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "." || seg == ".." {
			return false
		}
	}
	return true
}

func isBareJSONName(name string) bool {
	return name == path.Base(name) && !strings.ContainsAny(name, `/\`) && strings.HasSuffix(name, ".json") && len(name) > len(".json")
}

// BrokerEgress declares what a broker workflow may return.
type BrokerEgress struct {
	// Output is the ORIGINAL file name (before the executor's
	// disambiguation suffix) of the OUTPUT artifact that is the egress.
	Output string `yaml:"output" json:"output"`
	// Schema is the JSON Schema the output document is validated against.
	Schema map[string]any `yaml:"schema" json:"schema"`
	// MaxBytes caps the document; 0 = BrokerEgressMaxBytesDefault.
	MaxBytes int `yaml:"max_bytes,omitempty" json:"max_bytes,omitempty"`
	// Provenance is third_party (default) or first_party.
	Provenance string `yaml:"provenance,omitempty" json:"provenance,omitempty"`
}

// EffectiveProvenance resolves the default.
func (e BrokerEgress) EffectiveProvenance() string {
	if strings.TrimSpace(e.Provenance) == "" {
		return BrokerProvenanceThirdParty
	}
	return strings.TrimSpace(e.Provenance)
}

// EffectiveMaxBytes resolves the default.
func (e BrokerEgress) EffectiveMaxBytes() int {
	if e.MaxBytes <= 0 {
		return BrokerEgressMaxBytesDefault
	}
	return e.MaxBytes
}

// Validate enforces the §4.2 load rules. The error names the offending schema
// path; it never needs to echo a value because schemas carry none.
func (b *WorkflowBroker) Validate() error {
	if b == nil {
		return nil
	}
	if err := b.validateEgress(); err != nil {
		return err
	}
	if err := b.validateProposes(); err != nil {
		return err
	}
	if b.InputSchema == nil {
		return fmt.Errorf("broker.input_schema is required")
	}
	if t, _ := b.InputSchema["type"].(string); t != "object" {
		return fmt.Errorf("broker.input_schema must be type: object")
	}
	w := &brokerSchemaWalker{}
	if err := w.walk("input_schema", b.InputSchema, 1); err != nil {
		return err
	}
	if w.budget > BrokerUntrustedBudget {
		return ruleError("input_schema", ruleBudget, fmt.Sprintf("this schema's budget is %d", w.budget))
	}
	compiled, err := CompileBrokerSchema("input_schema", b.InputSchema)
	if err != nil {
		return fmt.Errorf("broker.input_schema: %w", err)
	}
	return b.validateSchedule(compiled)
}

// validateSchedule checks design §17.1: the cron, the zone, the hourly floor,
// and the fixed inputs against the input schema. A refusal names the field
// and the rule, never a value: the inputs may come from an agent.
func (b *WorkflowBroker) validateSchedule(inputSchema *jsonschema.Schema) error {
	sc := b.Schedule
	if sc == nil {
		return nil
	}
	expr := strings.TrimSpace(sc.Cron)
	if expr == "" {
		return fmt.Errorf("broker.schedule.cron is required")
	}
	if _, err := cronexpr.Parser.Parse(expr); err != nil || strings.HasPrefix(expr, "@") {
		return fmt.Errorf("broker.schedule.cron must be a 5-field cron expression (minute hour day-of-month month day-of-week)")
	}
	loc, err := sc.Location()
	if err != nil {
		return fmt.Errorf("broker.schedule.timezone is not a known IANA timezone")
	}
	gap, err := cronexpr.MinGap(expr, loc, 50)
	if err != nil {
		return fmt.Errorf("broker.schedule.cron must be a 5-field cron expression (minute hour day-of-month month day-of-week)")
	}
	if gap < BrokerScheduleMinGap {
		return fmt.Errorf("broker.schedule.cron: a schedule may run at most hourly")
	}
	doc, err := jsonDocument(sc.Inputs)
	if err != nil {
		return fmt.Errorf("broker.schedule.inputs must be an object")
	}
	if err := inputSchema.Validate(doc); err != nil {
		return fmt.Errorf("broker.schedule.inputs: %s", DescribeSchemaFailure(err))
	}
	if err := b.CheckDocumentValues(sc.Inputs); err != nil {
		return fmt.Errorf("broker.schedule.inputs: %w", err)
	}
	return nil
}

// jsonDocument turns a YAML-decoded value into what the schema validator
// expects: JSON types, numbers as json.Number.
func jsonDocument(v map[string]any) (any, error) {
	if v == nil {
		v = map[string]any{}
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var out any
	return out, dec.Decode(&out)
}

func (b *WorkflowBroker) validateEgress() error {
	out := strings.TrimSpace(b.Egress.Output)
	if out == "" {
		return fmt.Errorf("broker.egress.output is required: name the one output file that may leave")
	}
	if out != path.Base(out) || strings.ContainsAny(out, `/\`) || out == "." || out == ".." {
		return fmt.Errorf("broker.egress.output must be a bare file name, got a path")
	}
	if !strings.HasSuffix(out, ".json") {
		// Egress and proposal outputs are JSON, which also keeps them out of
		// the .md-only memory ingest (write-actions design §5.1).
		return fmt.Errorf("broker.egress.output must be a .json file name")
	}
	if b.Egress.Schema == nil {
		return fmt.Errorf("broker.egress.schema is required: the egress document is validated against it")
	}
	if b.Egress.MaxBytes > BrokerEgressMaxBytesCeiling || b.Egress.MaxBytes < 0 {
		return fmt.Errorf("broker.egress.max_bytes must be between 1 and %d", BrokerEgressMaxBytesCeiling)
	}
	switch b.Egress.EffectiveProvenance() {
	case BrokerProvenanceThirdParty, BrokerProvenanceFirstParty:
	default:
		return fmt.Errorf("broker.egress.provenance must be %s or %s", BrokerProvenanceThirdParty, BrokerProvenanceFirstParty)
	}
	if _, err := CompileBrokerSchema("egress_schema", b.Egress.Schema); err != nil {
		return fmt.Errorf("broker.egress.schema: %w", err)
	}
	return nil
}

// UntrustedInputPaths lists the dotted paths (relative to the input object) of
// every x-untrusted leaf, sorted. Delegate wraps those values in the
// untrusted markers before they reach an agent.
func (b *WorkflowBroker) UntrustedInputPaths() []string {
	if b == nil || b.InputSchema == nil {
		return nil
	}
	// Rooted at input_schema, as Validate walks it: a document declaration
	// is judged by its path (broker design §18.1), and a walk rooted
	// elsewhere would stop at the first document.
	w := &brokerSchemaWalker{}
	_ = w.walk("input_schema", b.InputSchema, 1)
	sort.Strings(w.untrusted)
	return w.untrusted
}

func (b *WorkflowBroker) validateProposes() error {
	seenAction := map[string]bool{}
	seenOutput := map[string]bool{strings.TrimSpace(b.Egress.Output): true}
	for i, p := range b.Proposes {
		at := fmt.Sprintf("broker.proposes[%d]", i)
		if !brokerActionNameRE.MatchString(p.Action) {
			return fmt.Errorf("%s.action must match %s", at, brokerActionNameRE)
		}
		if seenAction[p.Action] {
			return fmt.Errorf("%s: duplicate action %q", at, p.Action)
		}
		seenAction[p.Action] = true
		_, _, isMCP := p.ServerTool()
		_, _, _, isAPI := p.APITool()
		if !isMCP && !isAPI {
			return fmt.Errorf("%s.tool must name one MCP tool exactly, as mcp__<server>__<tool>, or one API write, as api:<name>:<METHOD>", at)
		}
		out := strings.TrimSpace(p.Output)
		if out == strings.TrimSpace(b.Egress.Output) {
			return fmt.Errorf("%s.output must differ from broker.egress.output", at)
		}
		if !isBareJSONName(out) {
			return fmt.Errorf("%s.output must be a bare file name ending in .json", at)
		}
		if seenOutput[out] {
			return fmt.Errorf("%s.output %q is already used", at, out)
		}
		seenOutput[out] = true
		if p.MaxArgsBytes < 0 || p.MaxArgsBytes > BrokerArgsMaxBytesCeiling {
			return fmt.Errorf("%s.max_args_bytes must be between 0 (the %d default) and %d", at, BrokerArgsMaxBytesDefault, BrokerArgsMaxBytesCeiling)
		}
		if _, err := p.EffectiveApprovalTTL(); err != nil {
			return fmt.Errorf("%s.approval_ttl: %w", at, err)
		}
		if p.ArgsSchema == nil {
			return fmt.Errorf("%s.args_schema is required", at)
		}
		if t, _ := p.ArgsSchema["type"].(string); t != "object" {
			return fmt.Errorf("%s.args_schema must be type: object", at)
		}
		w := &brokerSchemaWalker{args: true}
		if err := w.walk(fmt.Sprintf("proposes[%d].args_schema", i), p.ArgsSchema, 1); err != nil {
			return err
		}
		if _, err := CompileBrokerSchema(p.Action+"-args", p.ArgsSchema); err != nil {
			return fmt.Errorf("%s.args_schema: %w", at, err)
		}
		if err := p.validateStanding(); err != nil {
			return fmt.Errorf("%s.standing: %w", at, err)
		}
	}
	return nil
}

// brokerSchemaWalker proves every string leaf is bounded.
type brokerSchemaWalker struct {
	budget    int
	untrusted []string
	// documents and docBytes count the x-untrusted-document declarations
	// (broker design §18), in their own budget.
	documents []BrokerDocument
	docBytes  int
	// args switches to args_schema rules: the strings are model-drafted
	// prose meant for human review, so an x-untrusted field may be long (the
	// whole file is capped by max_args_bytes instead of a character budget),
	// and format: email is allowed because a recipient must be checkable.
	args bool
}

func (w *brokerSchemaWalker) walk(at string, node map[string]any, multiplier int) error {
	for _, kw := range brokerRefusedKeywords {
		if _, ok := node[kw]; ok {
			return ruleError(at, ruleKeywords, kw+" found")
		}
	}
	if _, ok := node[brokerDocumentKey]; ok {
		return w.walkDocument(at, node)
	}
	if _, ok := node["enum"]; ok {
		return nil
	}
	if _, ok := node["const"]; ok {
		return nil
	}
	switch t, _ := node["type"].(string); t {
	case "object":
		return w.walkObject(at, node, multiplier)
	case "array":
		return w.walkArray(at, node, multiplier)
	case "string":
		return w.walkString(at, node, multiplier)
	case "integer", "number", "boolean", "null":
		return nil
	case "":
		return ruleError(at, ruleType, "no type")
	default:
		return ruleError(at, ruleType, fmt.Sprintf("type %q", t))
	}
}

func (w *brokerSchemaWalker) walkObject(at string, node map[string]any, multiplier int) error {
	if ap, ok := node["additionalProperties"].(bool); !ok || ap {
		return ruleError(at, ruleObject, "")
	}
	props, _ := node["properties"].(map[string]any)
	names := make([]string, 0, len(props))
	for name := range props {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		// Dots and brackets encode traversal in UntrustedInputPaths and
		// UntrustedArgPaths; accepting them in literal names loses wrapping.
		if strings.ContainsAny(name, ".[]") {
			return ruleError(at, ruleObject, "property names must not contain dots or brackets")
		}
		child, ok := props[name].(map[string]any)
		if !ok {
			return fmt.Errorf("broker.%s: property schema must be a mapping", joinSchemaPath(at, "properties."+name))
		}
		if err := w.walk(joinSchemaPath(at, "properties."+name), child, multiplier); err != nil {
			return err
		}
	}
	return nil
}

func (w *brokerSchemaWalker) walkArray(at string, node map[string]any, multiplier int) error {
	maxItems, ok := schemaInt(node["maxItems"])
	if !ok || maxItems < 1 {
		return ruleError(at, ruleArray, "no maxItems")
	}
	items, ok := node["items"].(map[string]any)
	if !ok {
		return ruleError(at, ruleArray, "no single items schema")
	}
	// Only the untrusted-text budget uses this multiplier. Saturating one
	// above its ceiling preserves the proof without overflowing nested arrays.
	const capMultiplier = BrokerUntrustedBudget + 1
	if multiplier > capMultiplier/maxItems {
		multiplier = capMultiplier
	} else {
		multiplier *= maxItems
	}
	return w.walk(joinSchemaPath(at, "items"), items, multiplier)
}

func (w *brokerSchemaWalker) walkString(at string, node map[string]any, multiplier int) error {
	maxLen, hasMax := schemaInt(node["maxLength"])
	if untrusted, _ := node["x-untrusted"].(bool); untrusted {
		if !hasMax {
			return ruleError(at, ruleUntrusted, "no maxLength")
		}
		limit := BrokerUntrustedStringMax
		if w.args {
			limit = BrokerArgsMaxBytesCeiling
		}
		if maxLen < 1 || maxLen > limit {
			// The args_schema limit differs (w.args), so the detail states the
			// limit that applies here.
			return ruleError(at, ruleUntrusted, fmt.Sprintf("maxLength must be 1..%d here", limit))
		}
		// maxLen and multiplier are bounded here; saturate the sum too so
		// repeated leaves cannot wrap it back below the input budget.
		w.budget = min(BrokerUntrustedBudget+1, w.budget+maxLen*multiplier)
		w.untrusted = append(w.untrusted, untrustedPropertyPath(at))
		return nil
	}
	if f, ok := node["format"].(string); ok {
		if !brokerAllowedFormats[f] && (!w.args || f != "email") {
			return ruleError(at, ruleString, fmt.Sprintf("format %q does not bound the value", f))
		}
		return nil
	}
	if _, ok := node["pattern"].(string); ok {
		if !hasMax || maxLen < 1 || maxLen > BrokerUntrustedStringMax {
			return ruleError(at, rulePattern, "")
		}
		return nil
	}
	return ruleError(at, ruleString, "unconstrained string")
}

func joinSchemaPath(at, next string) string {
	if at == "" {
		return next
	}
	return at + "." + next
}

// untrustedPropertyPath maps a walker path ("input_schema.properties.a.items")
// to the input-document path ("a[]"), dropping schema keywords.
func untrustedPropertyPath(at string) string {
	parts := strings.Split(at, ".")
	out := make([]string, 0, len(parts))
	for i := 0; i < len(parts); i++ {
		switch parts[i] {
		case "input_schema":
		case "properties":
			if i+1 < len(parts) {
				out = append(out, parts[i+1])
				i++
			}
		case "items":
			if len(out) > 0 {
				out[len(out)-1] += "[]"
			}
		}
	}
	return strings.Join(out, ".")
}

func schemaInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		if n == float64(int(n)) {
			return int(n), true
		}
	}
	return 0, false
}

// CompileBrokerSchema compiles a schema taken from front matter. Format
// assertion is ON: a broker input declared `format: date-time` must actually
// be one, or the format constraint would be decorative.
func CompileBrokerSchema(id string, schema map[string]any) (*jsonschema.Schema, error) {
	body, err := json.Marshal(stripBrokerExtensions(schema))
	if err != nil {
		return nil, fmt.Errorf("encode: %w", err)
	}
	c := jsonschema.NewCompiler()
	c.AssertFormat = true
	url := "broker://" + id + ".json"
	if err := c.AddResource(url, bytes.NewReader(body)); err != nil {
		return nil, fmt.Errorf("add resource: %w", err)
	}
	return c.Compile(url)
}

// stripBrokerExtensions removes x-untrusted markers (vocabulary the validator
// does not know) from a deep copy of the schema.
func stripBrokerExtensions(v any) any {
	switch n := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(n))
		for k, val := range n {
			if k == "x-untrusted" || k == "x-destination" || k == "x-carries-content" || k == brokerDocumentKey {
				continue
			}
			out[k] = stripBrokerExtensions(val)
		}
		return out
	case []any:
		out := make([]any, len(n))
		for i, val := range n {
			out[i] = stripBrokerExtensions(val)
		}
		return out
	}
	return v
}

// brokerSafeBuiltins are the built-in tools a broker workflow's roles may
// hold. Deliberately narrower than ToolIsInert: that list answers "can this
// reach the network?", and set_reminder / update_operator_profile cannot — but
// a task-kind reminder spawns a task, so a hostile mail could use it to
// schedule arbitrary work inside the broker project. Everything here touches
// only the task's own workspace or the clock.
var brokerSafeBuiltins = map[string]bool{
	"current_time":     true,
	"file_read":        true,
	"file_write":       true,
	"file_edit":        true,
	"read_many_files":  true,
	"grep":             true,
	"glob":             true,
	"tool_result_read": true,
}

// BrokerSafeBuiltins lists brokerSafeBuiltins, sorted. It is the §7.3
// no-egress built-in allowlist of the agent admin verbs (agent-administered
// Vornik design), so the broker rule and the agent rule are one list.
func BrokerSafeBuiltins() []string {
	out := make([]string, 0, len(brokerSafeBuiltins))
	for k := range brokerSafeBuiltins {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// IsBrokerSafeBuiltin reports whether name is on the broker-safe list.
func IsBrokerSafeBuiltin(name string) bool { return brokerSafeBuiltins[name] }

// CheckBrokerRunnable reports why wf may not run as a broker workflow in
// project p with swarm s, or nil. Every "cannot prove it" case refuses: an
// unknown role, a role with no allowedTools (unrestricted in this codebase), a
// step type that can reach code outside its declared role, a wildcard, a
// daemon-level MCP server the project does not itself declare.
func CheckBrokerRunnable(p *Project, wf *Workflow, s *Swarm) error {
	if wf == nil || wf.Broker == nil {
		return fmt.Errorf("workflow is not a broker workflow")
	}
	if p == nil || !p.Broker {
		return fmt.Errorf("workflow %q is a broker workflow and runs only in a broker project (broker: true)", wf.ID)
	}
	if s == nil {
		return fmt.Errorf("workflow %q: the project's swarm is unknown", wf.ID)
	}
	servers := make(map[string]*MCPServerConfig, len(p.MCP.Servers))
	for i := range p.MCP.Servers {
		srv := &p.MCP.Servers[i]
		if srv.BrokerReadOnly && srv.BrokerWrite {
			return fmt.Errorf("MCP server %q is declared both broker_read_only and broker_write; a server is one or the other", srv.Name)
		}
		servers[srv.Name] = srv
	}
	for _, stepID := range sortedStepIDs(wf.Steps) {
		step := wf.Steps[stepID]
		if _, ok := stepTypesGrantingOnlyDeclaredRoles[strings.TrimSpace(step.Type)]; !ok {
			return fmt.Errorf("workflow %q step %q: step type %q can reach code outside its declared role; broker workflows allow agent, gate and approval steps only", wf.ID, stepID, step.Type)
		}
		roleName := strings.TrimSpace(step.Role)
		if roleName == "" {
			continue
		}
		role := findSwarmRole(s, roleName)
		if role == nil {
			return fmt.Errorf("workflow %q step %q: role %q is not defined by the project's swarm", wf.ID, stepID, roleName)
		}
		if len(role.Permissions.AllowedTools) == 0 {
			return fmt.Errorf("workflow %q role %q has no allowedTools, which means unrestricted; a broker role must list its tools", wf.ID, roleName)
		}
		for _, tool := range role.Permissions.AllowedTools {
			if err := checkBrokerTool(p, roleName, tool, servers); err != nil {
				return fmt.Errorf("workflow %q: %w", wf.ID, err)
			}
		}
	}
	if wf.Broker.Egress.EffectiveProvenance() == BrokerProvenanceFirstParty {
		for _, srv := range p.MCP.Servers {
			if !srv.OperatorAuthored {
				return fmt.Errorf("workflow %q declares first_party egress but MCP server %q is not operator_authored; unknown authorship is third party", wf.ID, srv.Name)
			}
		}
	}
	return nil
}

func checkBrokerTool(p *Project, role, tool string, servers map[string]*MCPServerConfig) error {
	tool = strings.TrimSpace(tool)
	if brokerSafeBuiltins[tool] {
		return nil
	}
	if tool == agentQueryAPITool {
		// A reader of the project's own APIs (agent-administered Vornik plan
		// P4.5): the agent API client admits GET and HEAD only, and writes
		// are proposals. An operator broker project would reach Kong's
		// unrestricted query_api, so it stays refused.
		if _, agent := agentns.FromID(p.ID); agent && len(p.APIs) > 0 {
			return nil
		}
		return fmt.Errorf("role %q holds %q, which a broker role may hold only in an agent project that declares apis", role, tool)
	}
	rest, ok := strings.CutPrefix(tool, "mcp__")
	if !ok {
		return fmt.Errorf("role %q holds %q, which is not a broker-safe built-in", role, tool)
	}
	serverName, toolName, ok := strings.Cut(rest, "__")
	if !ok || serverName == "" || toolName == "" || strings.ContainsAny(toolName, "*?") {
		return fmt.Errorf("role %q holds %q; broker roles must name each MCP tool exactly (mcp__<server>__<tool>)", role, tool)
	}
	srv := servers[serverName]
	if srv == nil {
		return fmt.Errorf("role %q holds %q but MCP server %q is not declared by the broker project", role, tool, serverName)
	}
	if !srv.BrokerReadOnly {
		return fmt.Errorf("role %q holds %q but MCP server %q is not declared broker_read_only", role, tool, serverName)
	}
	if len(srv.AllowedTools) == 0 {
		return fmt.Errorf("MCP server %q is broker_read_only but has no allowed_tools list; name the read-only tools explicitly", serverName)
	}
	for _, allowed := range srv.AllowedTools {
		if allowed == toolName {
			return nil
		}
	}
	return fmt.Errorf("role %q holds %q but %q is not in MCP server %q allowed_tools", role, tool, toolName, serverName)
}

// ErrBrokerWritesDisabled is returned by CheckBrokerProposals when the
// daemon's broker.writes is off. Its text is the error code front agents see.
var ErrBrokerWritesDisabled = errors.New("BROKER_WRITES_DISABLED")

// CheckBrokerProposals reports why wf's declared writes may not be proposed
// in project p, or nil. writesOn is the daemon's broker.writes. Run at
// delegate time next to CheckBrokerRunnable, which already refuses a role
// holding any tool from a server that is not broker_read_only, and so any
// write tool (write-actions design §4.2).
func CheckBrokerProposals(p *Project, wf *Workflow, writesOn bool) error {
	if wf == nil || wf.Broker == nil || len(wf.Broker.Proposes) == 0 {
		return nil
	}
	if !writesOn {
		return fmt.Errorf("%w: workflow %q proposes writes and broker.writes is off on this daemon", ErrBrokerWritesDisabled, wf.ID)
	}
	if p == nil {
		return fmt.Errorf("workflow %q: project unknown", wf.ID)
	}
	for _, prop := range wf.Broker.Proposes {
		if err := BrokerWriteToolDeclared(p, prop.Tool); err != nil {
			return fmt.Errorf("workflow %q action %q: %w", wf.ID, prop.Action, err)
		}
	}
	return nil
}

// BrokerWriteToolDeclared reports why project p does not declare tool as a
// broker write, or nil: the tool must name one tool of an MCP server the
// project declares broker_write, listed in its allowed_tools. Used at
// delegate time (CheckBrokerProposals) and by the action worker immediately
// before the call (write-actions design §5.4 step 2), so the two agree.
func BrokerWriteToolDeclared(p *Project, tool string) error {
	if p == nil {
		return errors.New("project unknown")
	}
	if api, method, _, isAPI := (BrokerProposal{Tool: tool}).APITool(); isAPI {
		return apiWriteDeclared(p, api, method)
	}
	serverName, toolName, ok := BrokerProposal{Tool: tool}.ServerTool()
	if !ok {
		return fmt.Errorf("tool %q is not mcp__<server>__<tool>", tool)
	}
	var srv *MCPServerConfig
	for i := range p.MCP.Servers {
		if p.MCP.Servers[i].Name == serverName {
			srv = &p.MCP.Servers[i]
		}
	}
	if srv == nil {
		return fmt.Errorf("MCP server %q is not declared by the broker project", serverName)
	}
	if !srv.BrokerWrite {
		return fmt.Errorf("MCP server %q is not declared broker_write", serverName)
	}
	for _, a := range srv.AllowedTools {
		if a == toolName {
			return nil
		}
	}
	return fmt.Errorf("%q is not in MCP server %q allowed_tools", toolName, serverName)
}

// UntrustedArgPaths lists the dotted paths of every x-untrusted leaf in the
// proposal's args_schema, sorted: the text a model drafted after reading
// third-party content, which the /inbox card flags (design §5.3).
func (p BrokerProposal) UntrustedArgPaths() []string {
	if p.ArgsSchema == nil {
		return nil
	}
	w := &brokerSchemaWalker{args: true}
	_ = w.walk("", p.ArgsSchema, 1)
	sort.Strings(w.untrusted)
	return w.untrusted
}

// agentQueryAPITool is the task agent's REST tool.
const agentQueryAPITool = "query_api"

// apiWriteDeclared reports why project p does not declare method as a write
// of its API api, or nil.
func apiWriteDeclared(p *Project, api, method string) error {
	for _, a := range p.APIs {
		if a.Name != api {
			continue
		}
		if !a.Writes {
			return fmt.Errorf("API %q declares no writes", api)
		}
		for _, m := range a.Methods {
			if m == method {
				return nil
			}
		}
		return fmt.Errorf("API %q does not list the method %s", api, method)
	}
	return fmt.Errorf("the project declares no API %q", api)
}
