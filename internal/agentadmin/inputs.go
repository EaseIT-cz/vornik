package agentadmin

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strings"
	"unicode"

	"vornik.io/vornik/internal/agentns"
)

// The typed inputs of the mutating verbs (§6). Decoded strictly: an unknown
// field is a refusal, never ignored.

// CreateProjectInput is create_project.
type CreateProjectInput struct {
	Slug     string `json:"slug"`
	Purpose  string `json:"purpose"`
	Template string `json:"template,omitempty"`
}

// RoleInput is one role of define_swarm.
type RoleInput struct {
	Name         string   `json:"name"`
	Instructions string   `json:"instructions"`
	Tools        []string `json:"tools"`
	// Model is optional: a model id from the installation's catalogue
	// (design §18.6 item 2).
	Model string `json:"model,omitempty"`
}

// DefineSwarmInput is define_swarm. Every agent project owns exactly one
// swarm with the project's slug, so Slug names the project whose roles these
// are (plan as-built: a swarm meets exactly one project).
type DefineSwarmInput struct {
	Slug  string      `json:"slug"`
	Roles []RoleInput `json:"roles"`
}

// StepInput is one step of define_workflow. Steps run in the given order.
type StepInput struct {
	Name         string `json:"name"`
	Role         string `json:"role"`
	Instructions string `json:"instructions"`
}

// DefineWorkflowInput is define_workflow.
type DefineWorkflowInput struct {
	Project  string          `json:"project"`
	Slug     string          `json:"slug"`
	Purpose  string          `json:"purpose,omitempty"`
	Steps    []StepInput     `json:"steps"`
	Inputs   json.RawMessage `json:"inputs"`
	Egress   json.RawMessage `json:"egress"`
	Proposes json.RawMessage `json:"proposes,omitempty"`
	// Schedule runs the workflow by itself (design §17); nil: on request
	// only.
	Schedule *ScheduleInput `json:"schedule,omitempty"`
}

// ScheduleInput is define_workflow's schedule: a 5-field cron evaluated in
// an IANA timezone (default UTC), and the fixed inputs of every run.
type ScheduleInput struct {
	Cron     string          `json:"cron"`
	Timezone string          `json:"timezone,omitempty"`
	Inputs   json.RawMessage `json:"inputs"`
}

// MCPAuthInput is add_mcp_server's auth.
type MCPAuthInput struct {
	Mode       string   `json:"mode"` // none | oauth | static
	Scopes     []string `json:"scopes,omitempty"`
	Credential string   `json:"credential,omitempty"`
}

// AddMCPServerInput is add_mcp_server.
type AddMCPServerInput struct {
	Project    string       `json:"project"`
	Name       string       `json:"name"`
	URL        string       `json:"url"`
	Auth       MCPAuthInput `json:"auth"`
	WriteTools []string     `json:"write_tools,omitempty"`
}

// SetBudgetInput is set_budget.
type SetBudgetInput struct {
	Project    string  `json:"project"`
	MonthlyUSD float64 `json:"monthly_usd"`
}

// RemoveInput is remove.
type RemoveInput struct {
	Kind string `json:"kind"` // project | workflow | swarm | integration | credential
	ID   string `json:"id"`
	// Project names the project for kind integration and credential.
	Project string `json:"project,omitempty"`
}

// decodeStrict decodes raw into v, refusing unknown fields and trailing data.
func decodeStrict(raw json.RawMessage, v any) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return fmt.Errorf("the input does not match the verb's fields: %v", err)
	}
	if dec.More() {
		return fmt.Errorf("the input has trailing data")
	}
	return nil
}

var (
	slugRe        = regexp.MustCompile(`^[a-z][a-z0-9-]{1,31}$`)
	nameRe        = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,31}$`)
	credentialRe  = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
	toolNameRe    = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,128}$`)
	scopeRe       = regexp.MustCompile(`^[A-Za-z0-9:._/-]{1,128}$`)
	outPathRe     = regexp.MustCompile(`artifacts/out/[A-Za-z0-9_/-]([A-Za-z0-9._/-]*[A-Za-z0-9_/-])?`)
	outIntentRe   = regexp.MustCompile(`(?i)\b(write|save|put|place|emit|output|produce|store|hand-?off)\b[^.!?\n]{0,60}\b(to|into|as|at)\s*$`)
	maxTextRunes  = 8000
	maxShortRunes = 300
)

// ProjectRefRule and NewSlugRule are the agent-facing statement of what a
// project reference may be; the tool schemas render them (design section 5,
// amended 2026-10-07) so the text and the code cannot drift.
const (
	ProjectRefRule = "The project's slug, or its full id <namespace>--<slug> as list_my_setup shows it."
	NewSlugRule    = "A bare slug only (a full id is refused here): 2-32 chars: a-z, 0-9, single dashes."
)

// ProjectSlug is projectSlug for callers outside the package that read a
// verb's raw input (the service's pre-approval tool listing).
func ProjectSlug(ns, s string) string { return projectSlug(ns, s) }

// projectSlug accepts a slug or the full id "<ns>--<slug>" of a project in
// namespace ns and returns the slug. It strips the prefix once (a project
// whose slug equals the namespace has the full id "ns--ns"). A full id in
// another namespace is returned unchanged, so it is refused: "--" is not a
// slug character (checkSlug) and "ns--other--x" names no project. BACKLOG
// 2026-10-03.
func projectSlug(ns, s string) string {
	return strings.TrimPrefix(s, ns+agentns.Separator)
}

// checkSlug refuses a slug that is malformed or contains the reserved "--".
func checkSlug(what, s string) error {
	if !slugRe.MatchString(s) || strings.Contains(s, "--") {
		return fmt.Errorf("%s %q must be 2 to 32 characters of a-z, 0-9 and single dashes, starting with a letter", what, s)
	}
	return nil
}

// checkText bounds agent-written prose that lands in a config file's
// Markdown body. A line that is a heading or a front-matter fence could
// forge another step's prompt or the document's structure, so it is refused
// rather than escaped.
func checkText(what, s string, maxRunes int, required bool) error {
	if required && strings.TrimSpace(s) == "" {
		return fmt.Errorf("%s is required", what)
	}
	if n := len([]rune(s)); n > maxRunes {
		return fmt.Errorf("%s is %d characters; the limit is %d", what, n, maxRunes)
	}
	for _, line := range strings.Split(s, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "#") || strings.HasPrefix(t, "---") || strings.HasPrefix(t, "```") {
			return fmt.Errorf("%s may not contain a line starting with #, --- or ``` (it would change the document's structure)", what)
		}
	}
	for _, r := range s {
		if unicode.IsControl(r) && r != '\n' && r != '\t' {
			return fmt.Errorf("%s contains a control character", what)
		}
	}
	return nil
}

// checkOneLine bounds a short single-line field (a purpose, a description).
func checkOneLine(what, s string, required bool) error {
	if strings.ContainsAny(s, "\r\n") {
		return fmt.Errorf("%s must be one line", what)
	}
	return checkText(what, s, maxShortRunes, required)
}

// checkURL accepts https anywhere and http only to a loopback host (§6).
func checkURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Fragment != "" {
		return fmt.Errorf("%q is not a plain URL", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		if host == "localhost" {
			return nil
		}
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return nil
		}
		return fmt.Errorf("%s is not https (plain http is allowed only to this machine)", raw)
	default:
		return fmt.Errorf("%s is not https", raw)
	}
}

// Egress type bounds (§9.1): numbers, booleans, enums, strings with
// maxLength ≤ 2000, arrays with maxItems ≤ 50, and objects of those.
const (
	egressMaxString = 2000
	egressMaxItems  = 50
	egressMaxDepth  = 6
)

// checkEgressSchema refuses a schema outside the bounds. It returns the
// plain-words description the approval sentence uses.
func checkEgressSchema(raw json.RawMessage) (map[string]any, []string, error) {
	var schema map[string]any
	if err := json.Unmarshal(raw, &schema); err != nil || schema == nil {
		return nil, nil, fmt.Errorf("egress must be a JSON Schema object")
	}
	if schema["type"] != "object" {
		return nil, nil, fmt.Errorf("egress must describe an object (\"type\": \"object\")")
	}
	var lines []string
	if err := walkEgress(schema, "", 0, &lines); err != nil {
		return nil, nil, err
	}
	if len(lines) == 0 {
		return nil, nil, fmt.Errorf("egress declares no fields")
	}
	return schema, lines, nil
}

var egressKeywords = map[string]bool{
	"type": true, "properties": true, "required": true, "additionalProperties": true, "items": true,
	"maxItems": true, "minItems": true, "maxLength": true, "minLength": true, "enum": true,
	"minimum": true, "maximum": true, "format": true, "description": true,
}

func walkEgress(node map[string]any, path string, depth int, lines *[]string) error {
	if depth > egressMaxDepth {
		return fmt.Errorf("egress nests deeper than %d levels", egressMaxDepth)
	}
	for k := range node {
		if !egressKeywords[k] {
			return fmt.Errorf("egress field %q uses %q, which is not allowed", label(path), k)
		}
	}
	if enum, ok := node["enum"].([]any); ok {
		return walkEnum(enum, path, lines)
	}
	switch node["type"] {
	case "object":
		return walkObject(node, path, depth, lines)
	case "array":
		return walkArray(node, path, depth, lines)
	case "string":
		n, ok := intOf(node["maxLength"])
		if !ok || n < 1 || n > egressMaxString {
			return fmt.Errorf("egress text %q needs \"maxLength\" between 1 and %d", label(path), egressMaxString)
		}
		*lines = append(*lines, fmt.Sprintf("%s (a text of up to %d characters)", label(path), n))
	case "integer", "number":
		*lines = append(*lines, fmt.Sprintf("%s (a number)", label(path)))
	case "boolean":
		*lines = append(*lines, fmt.Sprintf("%s (yes or no)", label(path)))
	default:
		return fmt.Errorf("egress field %q has type %v; allowed are object, array, string, integer, number, boolean and enum", label(path), node["type"])
	}
	return nil
}

func walkEnum(enum []any, path string, lines *[]string) error {
	for _, v := range enum {
		if s, isStr := v.(string); isStr && len([]rune(s)) > 100 {
			return fmt.Errorf("egress field %q has an enum value over 100 characters", label(path))
		}
	}
	*lines = append(*lines, fmt.Sprintf("%s (one of %d fixed values)", label(path), len(enum)))
	return nil
}

func walkObject(node map[string]any, path string, depth int, lines *[]string) error {
	if ap, ok := node["additionalProperties"]; !ok || ap != false {
		return fmt.Errorf("egress object %q must set \"additionalProperties\": false", label(path))
	}
	props, _ := node["properties"].(map[string]any)
	if len(props) == 0 {
		return fmt.Errorf("egress object %q declares no properties", label(path))
	}
	for _, name := range sortedAnyKeys(props) {
		if !nameRe.MatchString(name) {
			return fmt.Errorf("egress field name %q must be a-z, 0-9, _ or -", name)
		}
		child, ok := props[name].(map[string]any)
		if !ok {
			return fmt.Errorf("egress field %q is not a schema", name)
		}
		if err := walkEgress(child, path+"."+name, depth+1, lines); err != nil {
			return err
		}
	}
	return nil
}

func walkArray(node map[string]any, path string, depth int, lines *[]string) error {
	n, ok := intOf(node["maxItems"])
	if !ok || n < 1 || n > egressMaxItems {
		return fmt.Errorf("egress list %q needs \"maxItems\" between 1 and %d", label(path), egressMaxItems)
	}
	items, ok := node["items"].(map[string]any)
	if !ok {
		return fmt.Errorf("egress list %q needs an \"items\" schema", label(path))
	}
	var inner []string
	if err := walkEgress(items, path+"[]", depth+1, &inner); err != nil {
		return err
	}
	*lines = append(*lines, fmt.Sprintf("%s (a list of up to %d)", label(path), n))
	*lines = append(*lines, inner...)
	return nil
}

func label(path string) string {
	p := strings.TrimPrefix(path, ".")
	if p == "" {
		return "the result"
	}
	return p
}

func intOf(v any) (int, bool) {
	f, ok := v.(float64)
	if !ok || f != float64(int(f)) {
		return 0, false
	}
	return int(f), true
}

func sortedAnyKeys(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sortStrings(out)
	return out
}
