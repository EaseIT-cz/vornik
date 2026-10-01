package configassist

import (
	"bytes"
	"strings"

	"gopkg.in/yaml.v3"

	"vornik.io/vornik/internal/secrethygiene"
)

// Secret redaction is driven by the YAML PARSER, not by a line regex.
//
// Two audits in a row found secrets reaching the model because the sanitiser
// matched TEXT: the first missed block scalars (`value: |` replaced the marker
// and left the body), and the fix for it still missed `value: &pem |` (an
// anchor between the key and the header) and `"value": |` (a quoted key that
// the key regex does not match at all). Each fix taught one more shape to a
// matcher that can never enumerate YAML's representation space — anchors,
// tags, quoting styles, folded vs literal, explicit indentation indicators,
// and combinations of all of them.
//
// So identification moved to the thing that already understands every one of
// those: the parser. `yaml.Node` reports the key's NAME already unquoted and
// untagged, which is the question the sanitiser actually needs answered, and
// its Line/Column say where the value sits in the original bytes.
//
// Redaction still edits TEXT, not a re-serialised tree, because
// re-materialisation must be byte-exact: a round-trip through yaml.Marshal
// would silently reformat an operator's file.

// secretSpan is one value to redact: the lines it occupies and where its text
// starts on the first of them.
type secretSpan struct {
	key       string
	startLine int // 1-based, the line carrying the key
	endLine   int // 1-based, inclusive; == startLine for a single-line value
	valueCol  int // 0-based byte offset on startLine where the VALUE text begins
}

// secretSpans locates every secret-bearing value in a YAML document.
// A document that does not parse yields (nil, false) and the caller falls
// back to the line-oriented pass, which still serves markdown frontmatter and
// non-YAML files.
func secretSpans(data []byte) ([]secretSpan, bool) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil || len(root.Content) == 0 {
		return nil, false
	}
	lines := bytes.Split(data, []byte("\n"))
	var out []secretSpan
	var walk func(n *yaml.Node, inNamedSecrets bool)
	walk = func(n *yaml.Node, inNamedSecrets bool) {
		if n == nil {
			return
		}
		switch n.Kind {
		case yaml.DocumentNode:
			for _, c := range n.Content {
				walk(c, inNamedSecrets)
			}
		case yaml.SequenceNode:
			for _, item := range n.Content {
				walk(item, inNamedSecrets)
			}
		case yaml.MappingNode:
			for i := 0; i+1 < len(n.Content); i += 2 {
				k, v := n.Content[i], n.Content[i+1]
				// k.Value is the DECODED key: quotes and tags are already
				// gone, which is the whole reason for parsing.
				if (v.Kind == yaml.ScalarNode && isSecretBearing(inNamedSecrets, k.Value, v.Value)) || isSecretList(k, v) {
					if span, ok := spanFor(lines, k, v); ok {
						out = append(out, span)
						continue
					}
				}
				walk(v, inNamedSecrets || strings.EqualFold(k.Value, "named_secrets"))
			}
		case yaml.AliasNode:
			// An alias REFERS to a node redacted at its definition; expanding
			// it here would double-count. Its own text carries no secret.
		}
	}
	walk(&root, false)
	return out, true
}

// isSecretBearing is the single rule for "this field holds a secret",
// asked of a DECODED key and value.
func isSecretBearing(inNamedSecrets bool, key, value string) bool {
	v := strings.TrimSpace(value)
	if v == "" || strings.HasPrefix(v, "${") || strings.HasPrefix(v, "$") {
		return false // env reference: a NAME, editable, never a value
	}
	if inNamedSecrets && strings.EqualFold(key, "value") {
		return true // design §10a: unconditional
	}
	if secrethygiene.IsSecretBearingName(key) {
		return true
	}
	return secrethygiene.LooksLikeRawSecret(v) && secrethygiene.IsSecretBearingName(strings.TrimSuffix(key, "_file"))
}

// spanFor computes the text extent of a value, given its key and value nodes.
//
// The extent runs from just after the key's `:` separator to the last line
// more indented than the key. That one rule covers a plain scalar, a quoted
// scalar, an anchored or tagged value, and every block-scalar variant,
// because all of them are "the key's line, plus any lines that belong to it
// by indentation".
func spanFor(lines [][]byte, k, v *yaml.Node) (secretSpan, bool) {
	start := k.Line - 1 // to 0-based
	if start < 0 || start >= len(lines) {
		return secretSpan{}, false
	}
	header := string(lines[start])
	sep := separatorAfterKey(header, k.Column-1)
	if sep < 0 {
		return secretSpan{}, false
	}
	// valueCol points at the first NON-SPACE after the separator, so the
	// header's original spacing stays in the prefix we keep and the span text
	// we store carries no leading blank. Storing the blank instead produced
	// `value:  |` on re-materialisation.
	valueCol := sep + 1
	for valueCol < len(header) && (header[valueCol] == ' ' || header[valueCol] == '\t') {
		valueCol++
	}
	keyIndent := k.Column - 1
	end := start
	for j := start + 1; j < len(lines); j++ {
		text := string(lines[j])
		if strings.TrimSpace(text) == "" {
			// A blank line belongs to the value only if the value continues
			// after it.
			continues := false
			for m := j + 1; m < len(lines); m++ {
				if strings.TrimSpace(string(lines[m])) == "" {
					continue
				}
				continues = indentWidth(string(lines[m])) > keyIndent
				break
			}
			if !continues {
				break
			}
			continue
		}
		if indentWidth(text) <= keyIndent {
			break
		}
		end = j
	}
	_ = v
	return secretSpan{key: k.Value, startLine: start + 1, endLine: end + 1, valueCol: valueCol}, true
}

// separatorAfterKey returns the byte index of the `:` that ends the key,
// starting the scan at the key's column and skipping any quoted key text.
func separatorAfterKey(line string, keyCol int) int {
	if keyCol < 0 || keyCol > len(line) {
		return -1
	}
	i := keyCol
	if i < len(line) && (line[i] == '"' || line[i] == '\'') {
		quote := line[i]
		i++
		for i < len(line) {
			if line[i] == '\\' && quote == '"' {
				i += 2
				continue
			}
			if line[i] == quote {
				i++
				break
			}
			i++
		}
	}
	for ; i < len(line); i++ {
		if line[i] == ':' {
			return i
		}
	}
	return -1
}

// residualSecret re-parses SANITISED bytes and reports the first
// secret-bearing value that is not a placeholder — the post-condition that
// makes this safe against representations nobody has thought of yet.
//
// A representation the span walker cannot locate is a representation whose
// secret would ship to a model. Refusing is the only honest outcome: a
// sanitiser that silently passes what it did not understand reports "clean"
// and means "not examined".
func residualSecret(sanitized []byte) (string, bool) {
	var root yaml.Node
	if err := yaml.Unmarshal(sanitized, &root); err != nil || len(root.Content) == 0 {
		return "", false // unparseable output is handled by the caller's own gate
	}
	var found string
	var walk func(n *yaml.Node, inNamedSecrets bool)
	walk = func(n *yaml.Node, inNamedSecrets bool) {
		if n == nil || found != "" {
			return
		}
		switch n.Kind {
		case yaml.DocumentNode, yaml.SequenceNode:
			for _, c := range n.Content {
				walk(c, inNamedSecrets)
			}
		case yaml.MappingNode:
			for i := 0; i+1 < len(n.Content); i += 2 {
				k, v := n.Content[i], n.Content[i+1]
				if v.Kind == yaml.ScalarNode && isSecretBearing(inNamedSecrets, k.Value, v.Value) {
					if !strings.Contains(v.Value, placeholderPrefix) {
						found = k.Value
						return
					}
					continue
				}
				if isSecretList(k, v) {
					found = k.Value // a placeholder is a scalar; a literal list survived
					return
				}
				// A secret-named value reached through an alias: the text
				// shows only `*name`, the literal sits at the anchor under a
				// harmless key, and neither rule redacts it. Refuse the file.
				if secretNamedValue(inNamedSecrets, k.Value) && aliasesLiteral(v) {
					found = k.Value
					return
				}
				walk(v, inNamedSecrets || strings.EqualFold(k.Value, "named_secrets"))
			}
		}
	}
	walk(&root, false)
	return found, found != ""
}

// isSecretList reports whether a secret-named key holds a LIST carrying a
// literal value anywhere inside it (`api_keys:`, `allowed_keys:`, a list of
// lists, or a list of maps such as the legacy `api_keys: [{key: ...}]` form,
// whose inner keys are not secret-shaped on their own). List items have no
// key of their own, so the per-scalar rule never sees them; the list is
// redacted whole, as one value. A list holding only env references is names,
// not values, and stays editable. (Review of the 2026-10-01 external-scan fix:
// the scalar-only rule let such lists through, residualSecret mirrored the
// blind spot, and the first cut of this rule looked only at direct items.)
func isSecretList(k, v *yaml.Node) bool {
	if v.Kind != yaml.SequenceNode || !secrethygiene.IsSecretBearingName(k.Value) {
		return false
	}
	// named_secrets is the one secret-named list with its own per-field rule
	// (§10a: every item's `value` is replaced unconditionally, its `name`
	// stays editable). Blanking it whole would hide the names and ${VAR}
	// references the operator edits through the assistant.
	if strings.EqualFold(k.Value, "named_secrets") {
		return false
	}
	return hasLiteralScalar(v)
}

// hasLiteralScalar reports whether n holds, at any depth, a scalar that is
// neither empty, an env reference, nor an issued placeholder. Mapping KEYS
// are names and are skipped. Aliases are NOT followed here: the walker edits
// text, so redacting `*name` would leave the anchor's literal in place;
// residualSecret refuses that case instead (aliasesLiteral).
func hasLiteralScalar(n *yaml.Node) bool {
	switch n.Kind {
	case yaml.ScalarNode:
		iv := strings.TrimSpace(n.Value)
		return iv != "" && !strings.HasPrefix(iv, "$") && !strings.Contains(iv, placeholderPrefix)
	case yaml.SequenceNode:
		for _, c := range n.Content {
			if hasLiteralScalar(c) {
				return true
			}
		}
	case yaml.MappingNode:
		for i := 1; i < len(n.Content); i += 2 {
			if hasLiteralScalar(n.Content[i]) {
				return true
			}
		}
	}
	return false
}

// secretNamedValue reports whether a key's value is secret by NAME alone:
// the shared token rule, or `value` inside named_secrets (§10a).
func secretNamedValue(inNamedSecrets bool, key string) bool {
	return (inNamedSecrets && strings.EqualFold(key, "value")) || secrethygiene.IsSecretBearingName(key)
}

// aliasesLiteral reports whether n reaches a literal scalar THROUGH an alias:
// an alias anywhere in n (sequence items, mapping values; keys are names)
// whose target holds, after following any further alias chain, a literal.
// The walker never follows aliases (it edits text, and the alias text is
// only `*name`), so this check must: the secret lives at the anchor, where
// nothing redacts it. Round-3/4 reviews of the 2026-10-01 external-scan fix;
// this predated the list rule (`api_key: *tok`).
func aliasesLiteral(n *yaml.Node) bool {
	return aliasReach(n, 0)
}

// maxAliasDepth bounds alias chasing; yaml.v3 refuses cyclic aliases, so this
// is a backstop, and hitting it counts as reaching a literal (fail closed).
const maxAliasDepth = 64

func aliasReach(n *yaml.Node, depth int) bool {
	if n == nil {
		return false
	}
	if depth > maxAliasDepth {
		return true
	}
	switch n.Kind {
	case yaml.AliasNode:
		return literalThroughAliases(n.Alias, depth+1)
	case yaml.SequenceNode:
		for _, c := range n.Content {
			if aliasReach(c, depth) {
				return true
			}
		}
	case yaml.MappingNode:
		for i := 1; i < len(n.Content); i += 2 { // values only: keys are names
			if aliasReach(n.Content[i], depth) {
				return true
			}
		}
	}
	return false
}

// literalThroughAliases is hasLiteralScalar that also follows aliases, for
// the alias check only. hasLiteralScalar itself must not follow them: the
// walker would then redact the `*name` text and report the file clean while
// the anchor's literal stayed in place.
func literalThroughAliases(n *yaml.Node, depth int) bool {
	if n == nil {
		return false
	}
	if depth > maxAliasDepth {
		return true
	}
	switch n.Kind {
	case yaml.AliasNode:
		return literalThroughAliases(n.Alias, depth+1)
	case yaml.SequenceNode:
		for _, c := range n.Content {
			if literalThroughAliases(c, depth) {
				return true
			}
		}
	case yaml.MappingNode:
		for i := 1; i < len(n.Content); i += 2 {
			if literalThroughAliases(n.Content[i], depth) {
				return true
			}
		}
	default:
		return hasLiteralScalar(n)
	}
	return false
}

// aliasedSecret reports the first secret-named key whose value reaches a
// literal through an alias, judged on the ORIGINAL document. residualSecret
// runs on the sanitised text and cannot see an alias the walker swallowed:
// in `api_keys: [sk-lit, *tok]` the whole list becomes one placeholder, the
// `*tok` text with it, and the anchor's literal is left in plain sight.
func aliasedSecret(data []byte) (string, bool) {
	var root yaml.Node
	if err := yaml.Unmarshal(data, &root); err != nil || len(root.Content) == 0 {
		return "", false
	}
	var found string
	var walk func(n *yaml.Node, inNamedSecrets bool)
	walk = func(n *yaml.Node, inNamedSecrets bool) {
		if n == nil || found != "" {
			return
		}
		switch n.Kind {
		case yaml.DocumentNode, yaml.SequenceNode:
			for _, c := range n.Content {
				walk(c, inNamedSecrets)
			}
		case yaml.MappingNode:
			for i := 0; i+1 < len(n.Content); i += 2 {
				k, v := n.Content[i], n.Content[i+1]
				if secretNamedValue(inNamedSecrets, k.Value) && aliasesLiteral(v) {
					found = k.Value
					return
				}
				walk(v, inNamedSecrets || strings.EqualFold(k.Value, "named_secrets"))
			}
		}
	}
	walk(&root, false)
	return found, found != ""
}
