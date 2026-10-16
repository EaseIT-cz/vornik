package subjectstore

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"sync"
	"unicode/utf8"

	"vornik.io/vornik/internal/secrets"
)

// Export-time withholding and secret redaction for the transcript JSON columns
// (design 2026-07-29-art17-redact-and-reembed §15.11.4a, security reviews
// 2026-10-16).
//
// WHY AT EXPORT AND NOT ONLY AT WRITE. Of the three JSON columns slice 5d-E
// exports, only chat_audit_log.tool_calls_json passes a write-time scan (the
// auditredact.ChatAudit decorator), and only when secrets are enabled.
// hallucination_signals_json is built from the reply and never scanned, and
// task_messages has no write-time scan at all — its REST writer stores
// caller-supplied metadata verbatim. An Art 15 export is a file handed to a
// person, so it is the last point at which a credential or another person's
// data can be kept out of it.
//
// FAIL CLOSED. The export never emits the stored bytes. A value is exported
// only as a re-encoding of what a strict parse produced, and only when its
// shape is one the column's writers produce:
//   - the text must be valid UTF-8 and exactly ONE JSON value, with no
//     trailing content and no duplicate object key at any depth (a second
//     parser may keep the other duplicate);
//   - tool_calls_json must be an array of objects whose keys are the
//     writer's {name, args, result, error}; hallucination_signals_json an
//     array of objects with hallucination.Signal's keys; metadata an object.
//
// Anything else is withheld whole with a marker, so near-JSON (a trailing
// comma, single quotes, NaN, a truncated array) cannot carry a secret-named
// value past the key-name rule.
//
// Within a recognised shape:
//   - Art 15(4): a tool call's result and error are withheld (what a tool
//     returned, not what the subject supplied); metadata keys outside the
//     reviewed allowlist, or holding a non-scalar, are dropped and only
//     counted — their names are NOT echoed, since a caller-chosen key can
//     itself be personal data;
//   - secrets, rule 1: every string value, number and key is run through
//     the default secrets detector (the DefaultPatterns the support bundle
//     uses), each finding replaced by its [REDACTED:<type>] marker;
//   - secrets, rule 2: the whole value under a secret-NAMED key (password,
//     token, secret, api_key, authorization, …) becomes
//     [REDACTED:secret_field], because the detector's key=value rule cannot
//     see a JSON "password": "…" pair. The rule runs on the DECODED key, so a
//     \u-escaped name is caught.
//
// A tool call's args is a STRING holding JSON cut at 1024 bytes by the
// writer. It is strictly parsed in turn; if it does not parse (the cut is the
// usual reason) it is withheld whole, because the key-name rule cannot run on
// it.
//
// Residual: a secret of no known shape under an innocuous key in an exported
// field survives. The design states it.

// Markers. Each says that something was there and why it is not shown.
const (
	redactedSecretField   = "[REDACTED:secret_field]"
	withheldToolResult    = "[WITHHELD:tool_result]"
	withheldUnparseable   = "[WITHHELD:unparseable_json]"
	withheldShape         = "[WITHHELD:unrecognised_shape]"
	withheldToolArgs      = "[WITHHELD:unparseable_tool_args]"
	withheldMetadataCount = "[WITHHELD:unreviewed_fields]"
	withheldNoScanner     = "[WITHHELD:secret_scanner_unavailable]"
)

// exportDetectorFn is the detector source; a test seam.
var exportDetectorFn = defaultExportDetector

// toolCallKeys are the keys dispatcher/chat_audit.go's chatAuditToolCall
// writes; api/chat_proxy_audit.go writes only "[]".
var toolCallKeys = map[string]bool{"name": true, "args": true, "result": true, "error": true}

// signalKeys are hallucination.Signal's JSON keys.
var signalKeys = map[string]bool{
	"detector": true, "severity": true, "claim_type": true, "claim_value": true,
	"sentence": true, "evidence_searched": true, "detail": true, "recorded_at": true,
}

// metadataExportKeys are the task_messages.metadata keys whose writers were
// reviewed (§15.11.4a, 25 writers) and found to hold a constant, an internal
// id, or the author's own choice or reason. Every other key is dropped and
// counted: the REST writer stores caller JSON of any shape, Telegram a group
// chat id, the taint park raw source URLs and queries, and the lead's
// checkpoint, wait and closure payloads are model output. An allowlist, so a
// key a future writer adds is withheld until someone reviews it.
var metadataExportKeys = map[string]bool{
	"kind": true, "source": true, "choice": true, "reason": true, "from": true,
	"closedBy": true, "reply_to_message_id": true, "thread_id": true,
	"phase": true, "status": true, "expected_by": true, "resolved": true,
	"summarized_message_count": true,
}

// secretKeyWords are the key segments whose value is treated as a credential
// whatever it looks like. A key is split into segments on non-alphanumerics
// and at lower→upper case changes ("accessToken" → access, token), so
// "author_id" or "max_tokens" do not match while "api_key", "apiKey" and
// "X-Auth-Token" do.
var secretKeyWords = map[string]bool{
	"password": true, "passwd": true, "passphrase": true, "pass": true, "pwd": true,
	"secret": true, "token": true, "apikey": true, "auth": true, "authorization": true,
	"bearer": true, "cookie": true, "credential": true, "credentials": true,
	"signature": true, "privatekey": true, "otp": true,
}

// secretKeyPairs are two-segment names that form a credential name.
var secretKeyPairs = map[[2]string]bool{
	{"api", "key"}: true, {"private", "key"}: true, {"access", "key"}: true,
	{"session", "id"}: true, {"session", "key"}: true, {"client", "secret"}: true,
}

var keySegmentSplit = regexp.MustCompile(`[^A-Za-z0-9]+|([a-z0-9])([A-Z])`)

// tokenCountQualifiers precede "token(s)" in a key that counts model tokens
// (max_tokens, prompt_tokens), which is not a credential.
var tokenCountQualifiers = map[string]bool{
	"max": true, "prompt": true, "completion": true, "total": true, "input": true,
	"output": true, "num": true, "cached": true, "reasoning": true,
}

// singular drops one plural "s" so "tokens", "apiKeys" and "passwords" match
// (review c2d9 F1). "pass" and "credentials" are listed as they are.
func singular(s string) string {
	if len(s) > 3 && strings.HasSuffix(s, "s") && !strings.HasSuffix(s, "ss") {
		return strings.TrimSuffix(s, "s")
	}
	return s
}

// isSecretKeyName reports whether a (decoded) JSON object key names a
// credential.
func isSecretKeyName(k string) bool {
	segs := strings.Fields(strings.ToLower(keySegmentSplit.ReplaceAllString(k, "${1} ${2}")))
	for i, raw := range segs {
		s := singular(raw)
		prev := ""
		if i > 0 {
			prev = segs[i-1]
		}
		if s == "token" && tokenCountQualifiers[prev] {
			continue
		}
		if secretKeyWords[raw] || secretKeyWords[s] {
			return true
		}
		if i > 0 && secretKeyPairs[[2]string{prev, s}] {
			return true
		}
	}
	return false
}

var (
	exportDetectorOnce sync.Once
	exportDetector     secrets.Detector
)

// defaultExportDetector builds the default-pattern detector once. The patterns
// are constants, so an error is a programming error; the detector is then nil
// and only the structural rules apply.
func defaultExportDetector() secrets.Detector {
	exportDetectorOnce.Do(func() {
		if d, err := secrets.NewMultiDetector(secrets.Config{}); err == nil {
			exportDetector = d
		}
	})
	return exportDetector
}

// redactExportText replaces every detector finding in s with its marker.
func redactExportText(d secrets.Detector, s string) string {
	if d == nil || s == "" {
		return s
	}
	findings := d.Scan([]byte(s))
	if len(findings) == 0 {
		return s
	}
	return string(secrets.Redact([]byte(s), findings))
}

var errNotStrictJSON = errors.New("not exactly one well-formed JSON value")

// parseStrictJSON parses s as exactly one JSON value: valid UTF-8, nothing
// after it but whitespace, and no duplicate key in any object. It decodes
// twice on purpose — a token walk for duplicates, then the value — trading
// throughput for strictness; an export reads one value per linked row.
func parseStrictJSON(s string) (any, error) {
	if !utf8.ValidString(s) {
		return nil, errNotStrictJSON
	}
	dup := json.NewDecoder(strings.NewReader(s))
	if err := checkNoDuplicateKeys(dup); err != nil {
		return nil, err
	}
	if _, err := dup.Token(); !errors.Is(err, io.EOF) {
		return nil, errNotStrictJSON // trailing content
	}
	dec := json.NewDecoder(strings.NewReader(s))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errNotStrictJSON
	}
	return v, nil
}

// checkNoDuplicateKeys walks one JSON value token by token and fails on a
// duplicate object key at any depth, or on any syntax error.
func checkNoDuplicateKeys(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil // scalar
	}
	switch delim {
	case '{':
		seen := map[string]bool{}
		for dec.More() {
			kt, err := dec.Token()
			if err != nil {
				return err
			}
			k, ok := kt.(string)
			if !ok || seen[k] {
				return errNotStrictJSON
			}
			seen[k] = true
			if err := checkNoDuplicateKeys(dec); err != nil {
				return err
			}
		}
	case '[':
		for dec.More() {
			if err := checkNoDuplicateKeys(dec); err != nil {
				return err
			}
		}
	default:
		return errNotStrictJSON
	}
	_, err = dec.Token() // the closing delimiter
	return err
}

// encodeJSON re-encodes v compactly without HTML escaping.
func encodeJSON(v any) (string, bool) {
	var out bytes.Buffer
	enc := json.NewEncoder(&out)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return "", false
	}
	return string(bytes.TrimRight(out.Bytes(), "\n")), true
}

// redactExportJSON returns what the export may show of one JSON column value:
// a re-encoding of a strictly parsed, shape-checked, withheld-and-redacted
// value, or a single marker. It never returns the input bytes.
func redactExportJSON(col, s string) string {
	v, err := parseStrictJSON(s)
	if err != nil {
		return withheldUnparseable
	}
	d := exportDetectorFn()
	if d == nil {
		// Rule 1 cannot run; exporting with rule 2 alone would be a
		// half-scan with no signal (review c2d9 F5).
		return withheldNoScanner
	}
	var out any
	switch col {
	case "tool_calls_json":
		out, err = exportToolCalls(d, v)
	case "hallucination_signals_json":
		out, err = exportSignals(d, v)
	case "metadata":
		out, err = exportMetadata(d, v)
	default:
		err = errNotStrictJSON
	}
	if err != nil {
		return withheldShape
	}
	enc, ok := encodeJSON(out)
	if !ok {
		return withheldShape
	}
	return enc
}

// objectsWithKeys returns v as a list of objects whose keys are all in allowed.
func objectsWithKeys(v any, allowed map[string]bool) ([]map[string]any, error) {
	list, ok := v.([]any)
	if !ok {
		return nil, errNotStrictJSON
	}
	out := make([]map[string]any, 0, len(list))
	for _, e := range list {
		obj, ok := e.(map[string]any)
		if !ok {
			return nil, errNotStrictJSON
		}
		for k := range obj {
			if !allowed[k] {
				return nil, errNotStrictJSON
			}
		}
		out = append(out, obj)
	}
	return out, nil
}

func exportToolCalls(d secrets.Detector, v any) (any, error) {
	calls, err := objectsWithKeys(v, toolCallKeys)
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(calls))
	for _, c := range calls {
		call := map[string]any{}
		for k, val := range c {
			switch k {
			case "name":
				name, ok := val.(string)
				if !ok {
					return nil, errNotStrictJSON
				}
				call[k] = redactExportText(d, name)
			case "args":
				call[k] = exportToolArgs(d, val)
			default: // result, error: strings in the writer's shape
				str, ok := val.(string)
				if val != nil && !ok {
					return nil, errNotStrictJSON // review f0fa F1
				}
				if str != "" {
					call[k] = withheldToolResult
				}
			}
		}
		out = append(out, call)
	}
	return out, nil
}

// exportToolArgs handles args, which the writer stores as a JSON string. A
// string that strictly parses is walked like any other JSON and re-encoded; one
// that does not is withheld, since rule 2 cannot run on unparsed text.
func exportToolArgs(d secrets.Detector, val any) any {
	str, isString := val.(string)
	if !isString {
		// Not the writer's shape (chatAuditToolCall.Args is a string): withheld,
		// not walked (review 38b6 F3).
		return withheldToolArgs
	}
	if str == "" {
		return ""
	}
	inner, err := parseStrictJSON(str)
	if err != nil {
		return withheldToolArgs
	}
	enc, ok := encodeJSON(redactJSONValue(d, inner))
	if !ok {
		return withheldToolArgs
	}
	return enc
}

func exportSignals(d secrets.Detector, v any) (any, error) {
	signals, err := objectsWithKeys(v, signalKeys)
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(signals))
	for _, s := range signals {
		out = append(out, redactJSONValue(d, s))
	}
	return out, nil
}

// isJSONScalar reports whether a parsed JSON value is a string, number,
// boolean or null.
func isJSONScalar(v any) bool {
	switch v.(type) {
	case string, json.Number, bool, nil:
		return true
	}
	return false
}

func exportMetadata(d secrets.Detector, v any) (any, error) {
	obj, ok := v.(map[string]any)
	if !ok {
		return nil, errNotStrictJSON
	}
	out := map[string]any{}
	dropped := 0
	for k, val := range obj {
		// The allowlist vouches for a key's scalar text, not for a structure a
		// future writer nests under it (review c2d9 F3): a non-scalar value
		// under an allowed key is dropped and counted like an unreviewed key.
		if !metadataExportKeys[k] || !isJSONScalar(val) {
			dropped++
			continue
		}
		// Wrapped in a one-key map so the map branch runs rule 2 on the key
		// as well as rule 1 on the value; do not "simplify" to a string scan.
		out[k] = redactJSONValue(d, map[string]any{k: val}).(map[string]any)[k]
	}
	if dropped > 0 {
		out[withheldMetadataCount] = dropped
	}
	return out, nil
}

// redactJSONValue applies both secret rules to a parsed JSON value.
func redactJSONValue(d secrets.Detector, v any) any {
	switch x := v.(type) {
	case string:
		return redactExportText(d, x)
	case json.Number:
		if r := redactExportText(d, string(x)); r != string(x) {
			return r
		}
		return x
	case []any:
		out := make([]any, len(x))
		for i := range x {
			out[i] = redactJSONValue(d, x[i])
		}
		return out
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, val := range x {
			key := redactExportText(d, k)
			if isSecretKeyName(k) && val != nil {
				// The whole value goes, container or not: a credential nested
				// one level down under an innocuous key is still a credential.
				out[key] = redactedSecretField
				continue
			}
			out[key] = redactJSONValue(d, val)
		}
		return out
	}
	return v
}
