package auditredact

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"

	"vornik.io/vornik/internal/secrets"
)

// The write-time scan shared by the transcript decorators (chat-audit
// retention and redaction design §9.2, GitHub #395).

const (
	// markerSecretField replaces the whole value under a secret-named key —
	// the same marker the Art 15 export uses.
	markerSecretField = "[REDACTED:secret_field]"
	// findingSecretField / findingCutToken are the finding types counted for
	// the two rules that are not detector patterns.
	findingSecretField = FindingSecretField
	findingCutToken    = FindingCutToken
	// knownMatchMin is the shortest finding remembered as a known match, and
	// the shortest fragment of one the fragment rule replaces.
	knownMatchMin = 8
	// edgeRunMin is the shortest secret-alphabet run replaced at a cut edge of
	// a signal string. Ordinary words are shorter; every strong pattern's
	// token body is longer.
	edgeRunMin = 16
)

// FindingSecretField and FindingCutToken are the finding types of the two rules
// that are not detector patterns: the key-name rule and the edge-run rule.
// They are exported because `secrets scan-history` names them in --rules
// (design §10.2.1).
const (
	FindingSecretField = "secret_field"
	FindingCutToken    = "cut_token"
)

type knownMatch struct{ match, typ string }

// transcriptScan carries one row's scan: the detector, whether the entropy
// class is dropped (machine-read metadata and identities), the per-type counts
// of what was redacted, the per-type counts of everything found, and the
// matches redacted so far (the fragment rule's known matches).
//
// sel decides which finding types are written back. The live seam leaves it
// nil, so everything found is redacted; `secrets scan-history` passes its
// --rules selection (design §10.2.1). A finding that is not selected is
// counted in found and left in place, and it never becomes a known match: a
// value left in place is left in place everywhere in the row.
type transcriptScan struct {
	det         secrets.Detector
	dropEntropy bool
	sel         func(findingType string) bool
	counts      map[string]int
	found       map[string]int
	known       []knownMatch
}

func newTranscriptScan(det secrets.Detector, dropEntropy bool) *transcriptScan {
	return &transcriptScan{det: det, dropEntropy: dropEntropy, counts: map[string]int{}, found: map[string]int{}}
}

// selects reports whether findings of type t are written back.
func (s *transcriptScan) selects(t string) bool { return s.sel == nil || s.sel(t) }

// hit records one finding of type t, and reports whether to redact it.
func (s *transcriptScan) hit(t string) bool {
	s.found[t]++
	if !s.selects(t) {
		return false
	}
	s.counts[t]++
	return true
}

func (s *transcriptScan) total() int {
	n := 0
	for _, v := range s.counts {
		n += v
	}
	return n
}

// text redacts every detector finding in in and remembers each match.
func (s *transcriptScan) text(in string) string {
	if in == "" {
		return in
	}
	findings := s.det.Scan([]byte(in))
	if s.dropEntropy {
		kept := findings[:0:0]
		for _, f := range findings {
			if f.Type != secrets.FindingTypeEntropy {
				kept = append(kept, f)
			}
		}
		findings = kept
	}
	if len(findings) == 0 {
		return in
	}
	selected := findings[:0:0]
	for _, f := range findings {
		if !s.hit(f.Type) {
			continue
		}
		selected = append(selected, f)
		if len(f.Match) >= knownMatchMin {
			s.known = append(s.known, knownMatch{match: f.Match, typ: f.Type})
		}
	}
	if len(selected) == 0 {
		return in
	}
	return string(secrets.Redact([]byte(in), selected))
}

func marker(typ string) string { return "[REDACTED:" + typ + "]" }

// fragments applies the fragment rule (§9.2.2): every known match is replaced
// wherever it appears, a prefix of one that ends str (before a trailing "…")
// and a suffix of one that begins str are replaced too.
func (s *transcriptScan) fragments(str string) string {
	for _, k := range s.known {
		if strings.Contains(str, k.match) {
			n := strings.Count(str, k.match)
			s.counts[k.typ] += n
			s.found[k.typ] += n
			str = strings.ReplaceAll(str, k.match, marker(k.typ))
		}
	}
	core, ell := strings.CutSuffix(str, "…")
	tail := ""
	if ell {
		tail = "…"
	}
	for _, k := range s.known {
		for l := min(len(k.match)-1, len(core)); l >= knownMatchMin; l-- {
			if strings.HasSuffix(core, k.match[:l]) {
				core = core[:len(core)-l] + marker(k.typ)
				s.counts[k.typ]++
				s.found[k.typ]++
				break
			}
		}
		for l := min(len(k.match)-1, len(core)); l >= knownMatchMin; l-- {
			if strings.HasPrefix(core, k.match[len(k.match)-l:]) {
				core = marker(k.typ) + core[l:]
				s.counts[k.typ]++
				s.found[k.typ]++
				break
			}
		}
	}
	return core + tail
}

// edgeRuns applies the edge-run rule (§9.2.2) to a string that a cut may have
// split: at the end when it carries the "…" the cut appends, and at the start
// when atStart (a sentence begins after a '.', which a JWT contains).
func (s *transcriptScan) edgeRuns(str string, atStart bool) string {
	if core, ok := strings.CutSuffix(str, "…"); ok {
		i := len(core)
		for i > 0 && isSecretAlphabet(core[i-1]) {
			i--
		}
		if len(core)-i >= edgeRunMin && s.hit(findingCutToken) {
			str = core[:i] + marker(findingCutToken) + "…"
		}
	}
	if atStart {
		j := 0
		for j < len(str) && isSecretAlphabet(str[j]) {
			j++
		}
		if j >= edgeRunMin && s.hit(findingCutToken) {
			str = marker(findingCutToken) + str[j:]
		}
	}
	return str
}

var errNotOneValue = errors.New("not exactly one JSON value")

// decodeOne decodes exactly one JSON value from data into v.
func decodeOne(data []byte, v any, useNumber, strictFields bool) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	if useNumber {
		dec.UseNumber()
	}
	if strictFields {
		dec.DisallowUnknownFields()
	}
	if err := dec.Decode(v); err != nil {
		return err
	}
	var extra json.RawMessage
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errNotOneValue
	}
	return nil
}

// toolCallShape is the dispatcher writer's chat_audit_log.tool_calls_json
// element ({name, args, result, error}).
type toolCallShape struct {
	Name   string `json:"name"`
	Args   string `json:"args,omitempty"`
	Result string `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

// toolCalls scans (when s is non-nil) and then cuts every element of a
// tool_calls_json in the dispatcher's shape. Any other shape keeps raw-text
// redaction and is not cut; the chat proxy writes "[]".
func toolCalls(s *transcriptScan, in string) string {
	if in == "" {
		return in
	}
	var calls []toolCallShape
	if err := decodeOne([]byte(in), &calls, false, true); err != nil {
		if s == nil {
			return in
		}
		return s.text(in)
	}
	if calls == nil {
		return in
	}
	for i := range calls {
		c := &calls[i]
		if s != nil {
			c.Name = s.text(c.Name)
			c.Args = s.text(c.Args)
			c.Result = s.text(c.Result)
			c.Error = s.text(c.Error)
		}
		c.Args = Cut(c.Args, ToolFieldLimit)
		c.Result = Cut(c.Result, ToolFieldLimit)
		c.Error = Cut(c.Error, ToolErrorLimit)
	}
	out, err := json.Marshal(calls)
	if err != nil {
		return in
	}
	return string(out)
}

// cutSignalKeys are the hallucination.Signal fields the detector package cuts
// (claim_value at 256 bytes, sentence at 240) — the only ones with cut edges.
var cutSignalKeys = map[string]bool{"claim_value": true, "sentence": true}

// signals scans a hallucination.Signal list as JSON (chat_audit_log's
// hallucination_signals_json, execution_step_outcomes.hallucination_signals):
// every string value through the detector, then the fragment rule, then the
// edge-run rule on the two cut fields. Re-encoded only when something changed.
//
// Two passes (chat-audit retention and redaction design §11.2.1, GitHub #425):
// the detector runs over every value of every signal BEFORE the fragment rule
// runs on any. In one pass, a sentence quoting the head of a key that the same
// signal's claim_value holds whole kept that head whenever Go's random map
// order walked the sentence first.
func signals(s *transcriptScan, in string) string {
	if in == "" {
		return in
	}
	var list []map[string]any
	if err := decodeOne([]byte(in), &list, true, false); err != nil {
		// No writer produces this; no edges are known, so the detector and the
		// fragment rule are all that apply. The export fails closed on it.
		return s.fragments(s.text(in))
	}
	changed := false
	// Pass one keeps each string value beside its detector output, so pass two
	// never re-reads the decoded map.
	type value struct{ orig, out string }
	scanned := make([]map[string]value, len(list))
	for i, obj := range list {
		scanned[i] = map[string]value{}
		for k, v := range obj {
			if str, ok := v.(string); ok {
				scanned[i][k] = value{orig: str, out: s.text(str)}
			}
		}
	}
	for i, obj := range list {
		for k, v := range scanned[i] {
			out := s.fragments(v.out)
			if cutSignalKeys[k] {
				out = s.edgeRuns(out, k == "sentence")
			}
			if out != v.orig {
				obj[k] = out
				changed = true
			}
		}
	}
	if !changed {
		return in
	}
	out, err := json.Marshal(list)
	if err != nil {
		return in
	}
	return string(out)
}

// metadata scans task_messages.metadata (§9.2.3). JSON is walked: every key and
// string value through the detector (the caller built s without entropy), and
// the whole value under a secret-named key replaced. Re-encoded only when
// something changed, so clean metadata is stored byte-identical. Metadata that
// does not decode is redacted as text.
func metadata(s *transcriptScan, raw []byte) []byte {
	if len(raw) == 0 {
		return raw
	}
	var v any
	if err := decodeOne(raw, &v, true, false); err != nil {
		out := s.text(string(raw))
		if out == string(raw) {
			return raw
		}
		return []byte(out)
	}
	walked, changed := s.walk(v)
	if !changed {
		return raw
	}
	out, err := json.Marshal(walked)
	if err != nil {
		return raw
	}
	return out
}

func (s *transcriptScan) walk(v any) (any, bool) {
	switch x := v.(type) {
	case string:
		out := s.text(x)
		return out, out != x
	case []any:
		changed := false
		for i := range x {
			var c bool
			x[i], c = s.walk(x[i])
			changed = changed || c
		}
		return x, changed
	case map[string]any:
		changed := false
		out := make(map[string]any, len(x))
		for k, val := range x {
			key := s.text(k)
			if key != k {
				changed = true
			}
			// A value that is already the marker holds nothing: counting it
			// again would make every rescan of a redacted row report a match
			// forever (GitHub #414, design §10.2.2).
			if secrets.IsSecretKeyName(k) && val != nil && val != "" && val != markerSecretField &&
				s.hit(findingSecretField) {
				out[key] = markerSecretField
				changed = true
				continue
			}
			w, c := s.walk(val)
			out[key] = w
			changed = changed || c
		}
		return out, changed
	}
	return v, false
}
