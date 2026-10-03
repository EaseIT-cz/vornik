package registry

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"
)

// A document input (broker design §18, GREEN at review 7514): a top-level
// input_schema property declared
//
//	{"type": "string", "x-untrusted-document": {"max_bytes": N, "media_type": T}}
//
// carries one bounded text document. It is not an x-untrusted string: it is
// counted in its own budget, never inlined into the prompt, staged read-only
// at artifacts/in/<property>.<ext> before each step, and never returned.

// Document bounds (§18.1).
const (
	BrokerDocumentMaxBytes       = 256 * 1024
	BrokerDocumentsMax           = 2
	BrokerDocumentsTotalMaxBytes = 512 * 1024
	brokerDocumentKey            = "x-untrusted-document"
)

// brokerDocumentExt maps each allowed media type to the staged file's
// extension (§18.7 F6: the extension comes only from the media type). Text
// only: there is no parser for an attacker to aim at (§18.7 F7).
var brokerDocumentExt = map[string]string{
	"text/plain":    ".txt",
	"text/markdown": ".md",
	"text/x-diff":   ".diff",
}

var brokerDocumentNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)

// brokerDocumentSchemaKeys are the only keys a document property may carry:
// any other bound (maxLength, pattern, enum, format) would be a second limit
// beside max_bytes, so it is refused rather than reconciled.
var brokerDocumentSchemaKeys = map[string]bool{"type": true, "description": true, brokerDocumentKey: true}

// BrokerDocument is one declared document input. Its JSON is part of an agent
// workflow's reach signature (agent-administered design §7.6).
type BrokerDocument struct {
	Property  string `json:"property"`
	MaxBytes  int    `json:"max_bytes"`
	MediaType string `json:"media_type"`
}

// FileName is the staged file's name: the property and the media type's
// extension.
func (d BrokerDocument) FileName() string { return d.Property + brokerDocumentExt[d.MediaType] }

// Path is where a step finds the document, relative to its workspace.
func (d BrokerDocument) Path() string { return "artifacts/in/" + d.FileName() }

func brokerDocumentMediaTypes() []string {
	out := make([]string, 0, len(brokerDocumentExt))
	for t := range brokerDocumentExt {
		out = append(out, t)
	}
	sort.Strings(out)
	return out
}

// Documents lists the workflow's declared documents, sorted by property.
// Nil for a workflow that declares none, or whose input schema does not load.
func (b *WorkflowBroker) Documents() []BrokerDocument {
	if b == nil || b.InputSchema == nil {
		return nil
	}
	w := &brokerSchemaWalker{}
	if err := w.walk("input_schema", b.InputSchema, 1); err != nil {
		return nil
	}
	sort.Slice(w.documents, func(i, j int) bool { return w.documents[i].Property < w.documents[j].Property })
	return w.documents
}

// walkDocument checks one document declaration. Refusals name the property
// path and the rule, never a value of the declaration (§18.7 F9).
func (w *brokerSchemaWalker) walkDocument(at string, node map[string]any) error {
	name, topLevel := strings.CutPrefix(at, "input_schema.properties.")
	if w.args || !topLevel || strings.Contains(name, ".") {
		return documentError(at, "only a top-level input property may be a document")
	}
	if !brokerDocumentNameRE.MatchString(name) {
		return documentError(at, "the property name must match "+brokerDocumentNameRE.String())
	}
	for k := range node {
		if !brokerDocumentSchemaKeys[k] {
			return documentError(at, "a document takes only type, description and "+brokerDocumentKey+"; its bound is max_bytes")
		}
	}
	if t, _ := node["type"].(string); t != "string" {
		return documentError(at, "a document is type: string")
	}
	decl, ok := node[brokerDocumentKey].(map[string]any)
	if !ok {
		return documentError(at, brokerDocumentKey+" must be a mapping of max_bytes and media_type")
	}
	for k := range decl {
		if k != "max_bytes" && k != "media_type" {
			return documentError(at, brokerDocumentKey+" takes only max_bytes and media_type")
		}
	}
	n, ok := schemaInt(decl["max_bytes"])
	if !ok || n < 1 || n > BrokerDocumentMaxBytes {
		return documentError(at, fmt.Sprintf("max_bytes must be from 1 to %d", BrokerDocumentMaxBytes))
	}
	mt, _ := decl["media_type"].(string)
	if _, ok := brokerDocumentExt[mt]; !ok {
		return documentError(at, "media_type must be one of "+strings.Join(brokerDocumentMediaTypes(), ", "))
	}
	return w.addDocument(at, BrokerDocument{Property: name, MaxBytes: n, MediaType: mt})
}

// addDocument counts a document against the per-workflow limits.
func (w *brokerSchemaWalker) addDocument(at string, d BrokerDocument) error {
	if len(w.documents) >= BrokerDocumentsMax {
		return documentError(at, fmt.Sprintf("at most %d documents per workflow", BrokerDocumentsMax))
	}
	if w.docBytes+d.MaxBytes > BrokerDocumentsTotalMaxBytes {
		return documentError(at, fmt.Sprintf("the documents together may declare at most %d bytes", BrokerDocumentsTotalMaxBytes))
	}
	w.docBytes += d.MaxBytes
	w.documents = append(w.documents, d)
	return nil
}

func documentError(at, detail string) error {
	return ruleError(at, ruleDocument, "rule document: "+detail)
}

// CheckDocumentJSON checks the documents in a delegate's raw inputs object
// (§18.2). Go's JSON decoder silently replaces invalid UTF-8 bytes and lone
// surrogate escapes with U+FFFD, so validity is judged on the raw bytes of
// each document's string literal before the decoded values are checked.
func (b *WorkflowBroker) CheckDocumentJSON(raw []byte) error {
	docs := b.Documents()
	if len(docs) == 0 {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return fmt.Errorf("inputs must be a JSON object")
	}
	values := map[string]any{}
	for _, d := range docs {
		lit, present := fields[d.Property]
		if !present {
			continue
		}
		if !utf8.Valid(lit) || !surrogatesPaired(lit) {
			return fmt.Errorf("inputs/%s fails document: it is not valid UTF-8 text", d.Property)
		}
		var s string
		if err := json.Unmarshal(lit, &s); err != nil {
			return fmt.Errorf("inputs/%s fails document: it must be a string of text", d.Property)
		}
		values[d.Property] = s
	}
	return b.CheckDocumentValues(values)
}

// surrogatesPaired reports whether every \uD800-\uDFFF escape in a JSON
// string literal is a high half immediately followed by a low half.
func surrogatesPaired(lit []byte) bool {
	hex4 := func(i int) (int, bool) {
		if i+6 > len(lit) || lit[i] != '\\' || lit[i+1] != 'u' {
			return 0, false
		}
		v, err := strconv.ParseUint(string(lit[i+2:i+6]), 16, 16)
		return int(v), err == nil
	}
	for i := 0; i < len(lit); i++ {
		if lit[i] != '\\' || i+1 >= len(lit) {
			continue
		}
		if lit[i+1] != 'u' {
			i++ // an escaped character, \\ included
			continue
		}
		v, ok := hex4(i)
		if !ok {
			return false
		}
		switch {
		case v >= 0xDC00 && v <= 0xDFFF:
			return false
		case v >= 0xD800 && v <= 0xDBFF:
			low, ok := hex4(i + 6)
			if !ok || low < 0xDC00 || low > 0xDFFF {
				return false
			}
			i += 11
		default:
			i += 5
		}
	}
	return true
}

// CheckDocumentValues checks the documents among inputs against their
// declarations (§18.2): text that is valid UTF-8, holds no NUL, and is at
// most max_bytes long. An absent document passes (required is the schema's
// rule). The refusal names the property and the rule, never the value.
func (b *WorkflowBroker) CheckDocumentValues(inputs map[string]any) error {
	for _, d := range b.Documents() {
		v, present := inputs[d.Property]
		if !present {
			continue
		}
		s, ok := v.(string)
		if !ok {
			return fmt.Errorf("inputs/%s fails document: it must be a string of text", d.Property)
		}
		if !utf8.ValidString(s) {
			return fmt.Errorf("inputs/%s fails document: it is not valid UTF-8 text", d.Property)
		}
		if strings.ContainsRune(s, 0) {
			return fmt.Errorf("inputs/%s fails document: it contains a NUL character", d.Property)
		}
		if len(s) > d.MaxBytes {
			return fmt.Errorf("inputs/%s fails document: it is %d bytes, above the %d-byte limit", d.Property, len(s), d.MaxBytes)
		}
	}
	return nil
}
