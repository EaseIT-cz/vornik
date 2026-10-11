package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
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
