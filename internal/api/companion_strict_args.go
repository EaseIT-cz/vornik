package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"unicode"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"

	"vornik.io/vornik/internal/persistence"
)

// decodeToolArgs decodes a companion tool's arguments strictly (LLD 21 §A,
// "list takes a project, and tool arguments are strict", GitHub #305).
//
// json.Unmarshal drops a field the struct does not declare, so a call such as
// list({"project": "prague-venues"}) answered for the key's home project with
// no error (Hermes, 2026-10-08). Here an unknown field is an error naming it
// and the fields the tool accepts. Empty or null arguments decode as {};
// trailing data is refused. The agent-admin verbs already decode this way
// (agentadmin.decodeStrict); this is the companion tools' twin.
func decodeToolArgs(tool string, raw json.RawMessage, v any) error {
	b := bytes.TrimSpace(raw)
	if len(b) == 0 || bytes.Equal(b, []byte("null")) {
		b = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if field, ok := unknownJSONField(err); ok {
			return unknownArgumentError(tool, field, v)
		}
		return fmt.Errorf("invalid arguments: %w", err)
	}
	if dec.More() {
		return errors.New("invalid arguments: trailing data after the arguments object")
	}
	return nil
}

// unknownJSONField extracts the field name from encoding/json's
// DisallowUnknownFields error (`json: unknown field "x"`).
func unknownJSONField(err error) (string, bool) {
	rest, ok := strings.CutPrefix(err.Error(), `json: unknown field "`)
	if !ok {
		return "", false
	}
	field, _, ok := strings.Cut(rest, `"`)
	return field, ok
}

func unknownArgumentError(tool, field string, v any) error {
	fields := jsonFieldNames(v)
	if len(fields) == 0 {
		return fmt.Errorf("%s: unknown argument %q; this tool takes no arguments", tool, field)
	}
	return fmt.Errorf("%s: unknown argument %q; this tool accepts: %s", tool, field, strings.Join(fields, ", "))
}

// jsonFieldNames lists the json names of v's top-level struct fields, in
// declaration order. v is a pointer to a struct.
func jsonFieldNames(v any) []string {
	t := reflect.TypeOf(v)
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil || t.Kind() != reflect.Struct {
		return nil
	}
	var out []string
	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out = append(out, name)
	}
	return out
}

// noArgs is the argument struct of a tool that takes none (catalog, doctor,
// list_scopes): any field it is sent is refused.
type noArgs struct{}

// companionToolArgs maps every companion tool to a constructor of its
// argument type. handleCompanionToolCall decodes each call strictly against
// it before the tool runs, so a tool cannot be offered without a strict
// decode (TestCompanionStrictArgs_EveryToolRefusesUnknownField fails on a
// tool missing here). The tools decode into the same types.
var companionToolArgs = map[string]func() any{
	"list_artifacts":   func() any { return &artifactListArgs{} },
	"read_artifact":    func() any { return &artifactReadArgs{} },
	"delegate":         func() any { return &delegateArgs{} },
	"status":           func() any { return &taskIDArg{} },
	"result":           func() any { return &resultArgs{} },
	"cancel":           func() any { return &cancelArgs{} },
	"list":             func() any { return &listArgs{} },
	"catalog":          func() any { return &noArgs{} },
	"whoami":           func() any { return &whoamiArgs{} },
	"report_problem":   func() any { return &reportProblemArgs{} },
	"recall":           func() any { return &recallArgs{} },
	"remember":         func() any { return &rememberArgs{} },
	"recent_memory":    func() any { return &recentMemoryArgs{} },
	"list_scopes":      func() any { return &noArgs{} },
	"memory_correct":   func() any { return &correctArgs{} },
	"memory_list":      func() any { return &memoryListArgs{} },
	"skill_propose":    func() any { return &skillProposeArgs{} },
	"skill_search":     func() any { return &skillSearchArgs{} },
	"skill_get":        func() any { return &skillGetArgs{} },
	"skill_list":       func() any { return &skillListArgs{} },
	"skill_approve":    func() any { return &skillModerateArgs{} },
	"skill_reject":     func() any { return &skillModerateArgs{} },
	"skill_set_global": func() any { return &skillSetGlobalArgs{} },
	"task_logs":        func() any { return &taskLogsArgs{} },
	"task_explain":     func() any { return &taskIDArg{} },
	"daemon_log":       func() any { return &daemonLogArgs{} },
	"doctor":           func() any { return &noArgs{} },
}

// checkCompanionToolArgs refuses a call whose arguments the tool does not
// declare. A tool with no entry is left to the dispatcher's unknown-tool
// answer.
func checkCompanionToolArgs(tool string, raw json.RawMessage) error {
	newArgs, ok := companionToolArgs[tool]
	if !ok {
		return nil
	}
	return decodeToolArgs(tool, raw, newArgs())
}

// duplicateArgKey reports the first object key that appears more than once
// within one JSON object, at any depth, compared case-insensitively as the
// struct decode compares them, in a tool call's arguments. The
// handler decodes arguments into Go maps and structs, where a repeated key
// keeps its LAST value, while the companion tool audit stores the arguments
// as sent; a repeated key would let what was decided and what was recorded
// disagree (security review of 6035e952d, 2026-10-14). A repeated key is never
// legitimate in a tool call, so the dispatcher refuses it before any other
// argument check. Text it cannot tokenise is left to the strict decode.
func duplicateArgKey(raw json.RawMessage) (string, bool) {
	if len(bytes.TrimSpace(raw)) == 0 {
		return "", false
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	dup, err := firstRepeatedKey(dec)
	if err != nil {
		return "", false
	}
	return dup, dup != ""
}

// foldArgKey maps a key to a canonical case-folded form: two keys fold to
// the same string exactly when bytes.EqualFold treats them as equal, which is
// how encoding/json matches an object key to a struct field. Each rune becomes
// the smallest rune of its unicode.SimpleFold orbit, so "Workflow", "WORKFLOW"
// and "workflow" are one key, and the Kelvin sign (U+212A) is "k".
func foldArgKey(key string) string {
	return strings.Map(func(r rune) rune {
		lowest := r
		for f := unicode.SimpleFold(r); f != r; f = unicode.SimpleFold(f) {
			if f < lowest {
				lowest = f
			}
		}
		return lowest
	}, key)
}

// firstRepeatedKey consumes one JSON value from dec and returns the first key
// repeated within any object in it, or "" when there is none.
func firstRepeatedKey(dec *json.Decoder) (string, error) {
	tok, err := dec.Token()
	if err != nil {
		return "", err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return "", nil // a scalar holds no keys
	}
	seen := map[string]bool{}
	for dec.More() {
		if delim == '{' {
			keyTok, err := dec.Token()
			if err != nil {
				return "", err
			}
			key, _ := keyTok.(string)
			folded := foldArgKey(key)
			if seen[folded] {
				return key, nil
			}
			seen[folded] = true
		}
		dup, err := firstRepeatedKey(dec)
		if err != nil || dup != "" {
			return dup, err
		}
	}
	_, err = dec.Token() // the closing } or ]
	return "", err
}

// legacyArgRule decides one legacy argument: nil keeps the call (the name is
// then removed), an error refuses it.
type legacyArgRule func(key *persistence.APIKey, value json.RawMessage) error

// companionLegacyToolArgs names the arguments a SHIPPED client sends that its
// tool never declared (LLD 21 §A, "Strict arguments: the clients the census
// missed", 2026-10-14). Before GitHub #305 the daemon dropped them silently;
// after it, a 2026.10.5 client breaks on upgrade. Each entry is tied to the
// client that sends it and is removed once that client is unsupported; it is
// never a general escape (TestCompanionStrictArgs_LegacyArgsAreNotDeclared).
//
// POLICY: an entry is only for a shipped, versioned BINARY a user cannot
// upgrade in step with the daemon, and names the last version that sends it.
// Never for an argument a model composes from skill prose: a model reads the
// refusal and retries, and tolerating it would bring back the silent drop
// GitHub #305 removed.
var companionLegacyToolArgs = map[string]map[string]legacyArgRule{
	// vornikctl bench memory through 2026.10.5 (last sent by the 2026.10.6
	// development tree before 2026-10-14) sends the token budget beside the
	// converted limit. recall never read it; the result is bounded by limit,
	// as it always was.
	"recall": {"max_tokens": legacyRecallMaxTokens},
	// vornikctl bench agent through 2026.10.5 (same boundary) sends its
	// --project. delegate never read it: a key's delegation runs in the
	// key's project.
	"delegate": {"project": legacyDelegateProject},
}

func legacyRecallMaxTokens(_ *persistence.APIKey, value json.RawMessage) error {
	var n json.Number
	dec := json.NewDecoder(bytes.NewReader(value))
	dec.UseNumber()
	if err := dec.Decode(&n); err != nil || n == "" { // null decodes to ""
		return errors.New(`recall: "max_tokens" is not a recall argument (recall takes limit, a result count); it is accepted only as a number, from an older vornikctl bench memory, and ignored`)
	}
	return nil
}

func legacyDelegateProject(key *persistence.APIKey, value json.RawMessage) error {
	var p string
	if err := json.Unmarshal(value, &p); err == nil && !key.AgentAdmin && p != "" && p == key.ProjectID {
		return nil
	}
	if key.AgentAdmin {
		return errors.New(`delegate: "project" is not a delegate argument; an agent-admin key's delegation runs in the project that owns the workflow`)
	}
	return fmt.Errorf(`delegate: "project" is not a delegate argument; this key's delegations run in its own project %q, and project is accepted (from an older vornikctl bench agent) only when it names that project`, key.ProjectID)
}

var (
	companionLegacyArgOnce  sync.Once
	companionLegacyArgTotal *prometheus.CounterVec
)

// companionLegacyArgCounter is vornik_companion_legacy_argument_total{tool,
// argument, outcome}: outcome "ignored" (accepted and removed) or "refused".
func companionLegacyArgCounter() *prometheus.CounterVec {
	companionLegacyArgOnce.Do(func() {
		companionLegacyArgTotal = promauto.NewCounterVec(prometheus.CounterOpts{
			Namespace: "vornik", Subsystem: "companion", Name: "legacy_argument_total",
			Help: "Companion tool calls carrying a legacy argument a shipped client sends but the tool never declared, by outcome (ignored, refused).",
		}, []string{"tool", "argument", "outcome"})
	})
	return companionLegacyArgTotal
}

// admitLegacyToolArgs applies companionLegacyToolArgs to one call: it
// returns the arguments with every accepted legacy name removed, or an error
// refusing the call. Arguments it cannot read as an object are returned
// unchanged for the strict decode to refuse.
func (s *Server) admitLegacyToolArgs(key *persistence.APIKey, tool string, raw json.RawMessage) (json.RawMessage, error) {
	rules, ok := companionLegacyToolArgs[tool]
	if !ok || len(bytes.TrimSpace(raw)) == 0 {
		return raw, nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		return raw, nil
	}
	changed := false
	for name, rule := range rules {
		value, present := obj[name]
		if !present {
			continue
		}
		if err := rule(key, value); err != nil {
			companionLegacyArgCounter().WithLabelValues(tool, name, "refused").Inc()
			return nil, err
		}
		companionLegacyArgCounter().WithLabelValues(tool, name, "ignored").Inc()
		s.logger.Warn().Str("tool", tool).Str("argument", name).Str("key_id", key.ID).
			Str("client_kind", key.ClientKind).
			Msg("companion: ignored a legacy argument an older client sends (LLD 21 §A, 2026-10-14); upgrade the client")
		delete(obj, name)
		changed = true
	}
	if !changed {
		return raw, nil
	}
	out, err := json.Marshal(obj)
	if err != nil {
		return nil, fmt.Errorf("invalid arguments: %w", err)
	}
	return out, nil
}

// CheckCompanionToolArgs is the daemon's own argument check, exported for
// in-repo CLIENT tests (GitHub #305 follow-up, LLD 21 §A amendment
// 2026-10-14). A client's fake server calls it so the fake refuses exactly
// what the daemon refuses: a fake that accepted any argument is how the
// memory benchmark's recall "max_tokens" reached a release run unnoticed. It
// reads the same registry the dispatcher uses, never a hand list. An unknown
// tool is an error here (the dispatcher answers it separately).
func CheckCompanionToolArgs(tool string, raw json.RawMessage) error {
	if _, ok := companionToolArgs[tool]; !ok {
		return fmt.Errorf("unknown tool: %s", tool)
	}
	return checkCompanionToolArgs(tool, raw)
}
