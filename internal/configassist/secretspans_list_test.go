package configassist

import (
	"errors"
	"strings"
	"testing"
)

// TestBuild_RedactsSecretNamedLists is the regression test raised by the
// architectural review of the 2026-10-01 external-scan fix (swarms.vornik.io
// F2): the span walker redacted a secret-bearing key only when its value was a
// SCALAR, so a secret-named key holding a LIST (`api_keys:`, `allowed_keys:`)
// was walked item by item — and list items have no key, so none was redacted
// and the raw keys reached the model. residualSecret mirrored the same blind
// spot, so the post-condition reported clean.
func TestBuild_RedactsSecretNamedLists(t *testing.T) {
	const doc = `projectId: p
admin:
  allowed_keys:
    - "sk-vornik-admin-operator-Zq81xW0pLmN4vB7cR2tY9uE3"
    - sk-vornik-admin-broker-Hj5kL2mN8pQ1rS4tU7vW0xY3
  label: keep-me
api:
  api_keys: ["sk-vornik-flow-A1b2C3d4E5f6G7h8I9j0K1l2M3"]
env_only:
  api_keys:
    - ${VORNIK_OPERATOR_KEY}
`
	root := writeTree(t, map[string]string{"projects/p.yaml": doc})
	s, err := Build(root, nil, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got := string(s.Files["projects/p.yaml"])
	for _, raw := range []string{"Zq81xW0pLmN4vB7cR2tY9uE3", "Hj5kL2mN8pQ1rS4tU7vW0xY3", "A1b2C3d4E5f6G7h8I9j0K1l2M3"} {
		if strings.Contains(got, raw) {
			t.Fatalf("a secret-named list item reached the snapshot (%s):\n%s", raw, got)
		}
	}
	if !strings.Contains(got, "label: keep-me") {
		t.Fatalf("a sibling of the redacted list must survive:\n%s", got)
	}
	if !strings.Contains(got, "- ${VORNIK_OPERATOR_KEY}") {
		t.Fatalf("a list of env references is NAMES and must stay editable:\n%s", got)
	}
	if s.Placeholders() != 2 {
		t.Fatalf("placeholders = %d, want 2 (one per literal secret list)", s.Placeholders())
	}
	// The list comes back byte-identical when the assistant writes the file.
	back, err := s.Rematerialize("projects/p.yaml", []byte(got))
	if err != nil {
		t.Fatal(err)
	}
	if back != doc {
		t.Fatalf("re-materialised list differs from the original:\n%s", back)
	}
	if leftover, bad := residualSecret([]byte(doc)); !bad || leftover != "allowed_keys" {
		t.Fatalf("residualSecret on the raw doc = (%q, %v); it must see a literal secret list", leftover, bad)
	}
}

// TestBuild_RedactsNestedSecretNamedLists closes the residual named in the
// 2026-10-01 round-2 review: isSecretList looked only at DIRECT scalar items,
// so a secret-named list of lists, or of maps whose own keys are not
// secret-shaped (the legacy `api_keys: [{key: ..., projects: [...]}]` form),
// still reached the model, and residualSecret agreed.
func TestBuild_RedactsNestedSecretNamedLists(t *testing.T) {
	const doc = `projectId: p
credentials:
  - - user-a
    - "Zp4mW8qL2nR6tV0xB3cF7hJ1"
api_keys:
  - key: sk-vornik-legacy-Kq9wE2rT5yU8iO1pA4sD7fG0
    projects: [p]
env_nested:
  api_keys:
    - - ${VORNIK_A}
`
	root := writeTree(t, map[string]string{"projects/p.yaml": doc})
	s, err := Build(root, nil, Limits{})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got := string(s.Files["projects/p.yaml"])
	for _, raw := range []string{"Zp4mW8qL2nR6tV0xB3cF7hJ1", "Kq9wE2rT5yU8iO1pA4sD7fG0"} {
		if strings.Contains(got, raw) {
			t.Fatalf("a nested secret-named list item reached the snapshot (%s):\n%s", raw, got)
		}
	}
	if !strings.Contains(got, "- - ${VORNIK_A}") {
		t.Fatalf("a nested list of env references must stay editable:\n%s", got)
	}
	back, err := s.Rematerialize("projects/p.yaml", []byte(got))
	if err != nil {
		t.Fatal(err)
	}
	if back != doc {
		t.Fatalf("re-materialised nested list differs from the original:\n%s", back)
	}
	if leftover, bad := residualSecret([]byte(doc)); !bad {
		t.Fatalf("residualSecret must see a nested literal secret list, got (%q, %v)", leftover, bad)
	}
}

// TestBuild_RefusesSecretReachedThroughAlias pins round-3 review finding 2 on
// the 2026-10-01 external-scan fix. The snapshot is TEXT: an alias under a
// secret-named key (`api_key: *tok`, `api_keys: [*tok]`) shows only `*tok`,
// while the secret itself sits as a literal at the anchor, under a key whose
// NAME is harmless, so neither rule redacts it. Following the alias in the
// walker would redact the `*tok` text and leave the anchor, so the only
// sound answer is to refuse the file: residualSecret reports it, and Build
// fails closed with ErrSecretRedactionIncomplete.
func TestBuild_RefusesSecretReachedThroughAlias(t *testing.T) {
	cases := map[string]string{
		"scalar alias":  "common: &tok Wq3eR5tY7uI9oP1aS2dF4gH6\nchat:\n  api_key: *tok\n",
		"alias in list": "common: &tok Wq3eR5tY7uI9oP1aS2dF4gH6\napi_keys:\n  - *tok\n",
		"nested alias":  "common: &tok Wq3eR5tY7uI9oP1aS2dF4gH6\napi_keys:\n  - - *tok\n",
	}
	for name, doc := range cases {
		t.Run(name, func(t *testing.T) {
			if _, bad := residualSecret([]byte(doc)); !bad {
				t.Fatal("residualSecret must refuse a secret-named value that aliases a literal")
			}
			root := writeTree(t, map[string]string{"projects/p.yaml": doc})
			s, err := Build(root, nil, Limits{})
			if err == nil {
				defer s.Close()
				t.Fatalf("Build must refuse; snapshot was:\n%s", s.Files["projects/p.yaml"])
			}
			if !errors.Is(err, ErrSecretRedactionIncomplete) {
				t.Fatalf("err = %v, want ErrSecretRedactionIncomplete", err)
			}
		})
	}
	// An alias to an ENV reference is a name, not a value: still editable.
	ok := "common: &tok ${VORNIK_X}\nchat:\n  api_key: *tok\n"
	if leftover, bad := residualSecret([]byte(ok)); bad {
		t.Fatalf("an alias to an env reference must pass, got %q", leftover)
	}
}

// TestResidualSecret_EachNestedShapeAlone pins round-3 review finding 1: the
// post-condition is asserted per shape, not only on a document mixing them,
// so a shape cannot pass because another one in the same file tripped it.
func TestResidualSecret_EachNestedShapeAlone(t *testing.T) {
	for name, doc := range map[string]string{
		"list of lists":      "credentials:\n  - - Zp4mW8qL2nR6tV0xB3cF7hJ1\n",
		"deep list":          "credentials:\n  - - - Zp4mW8qL2nR6tV0xB3cF7hJ1\n",
		"list of maps":       "api_keys:\n  - key: Kq9wE2rT5yU8iO1pA4sD7fG0\n",
		"map value two deep": "api_keys:\n  - entry:\n      key: Kq9wE2rT5yU8iO1pA4sD7fG0\n",
	} {
		if _, bad := residualSecret([]byte(doc)); !bad {
			t.Errorf("%s: residualSecret reported clean", name)
		}
	}
}

// TestBuild_AliasRefusalCoversEveryRoute pins round-4 review suggestions on the
// 2026-10-01 external-scan fix, plus one route the review did not name: when a
// secret-named list holds a literal AND an alias, the walker redacts the whole
// list, the `*tok` text disappears into the placeholder, and a check run on
// the SANITISED text no longer sees the alias while the anchor's literal is
// still there. The alias check therefore runs on the ORIGINAL document and
// follows alias chains to the end.
func TestBuild_AliasRefusalCoversEveryRoute(t *testing.T) {
	refuse := map[string]string{
		"two-hop chain":          "inner: &inner Wq3eR5tY7uI9oP1aS2dF4gH6\nouter: &outer [*inner]\napi_keys: *outer\n",
		"list hides the alias":   "common: &tok Wq3eR5tY7uI9oP1aS2dF4gH6\napi_keys:\n  - sk-vornik-literal-Kq9wE2rT5yU8iO1pA4sD7fG0\n  - *tok\n",
		"mapping anchor":         "common: &m {label: x, val: Wq3eR5tY7uI9oP1aS2dF4gH6}\ncredentials: *m\n",
		"named_secrets value":    "common: &tok Wq3eR5tY7uI9oP1aS2dF4gH6\nnamed_secrets:\n  - name: A\n    value: *tok\n",
		"merge key under secret": "base: &base {k: Wq3eR5tY7uI9oP1aS2dF4gH6}\ncredentials:\n  <<: *base\n",
	}
	for name, doc := range refuse {
		t.Run(name, func(t *testing.T) {
			root := writeTree(t, map[string]string{"projects/p.yaml": doc})
			s, err := Build(root, nil, Limits{})
			if err == nil {
				defer s.Close()
				t.Fatalf("Build must refuse; snapshot was:\n%s", s.Files["projects/p.yaml"])
			}
			if !errors.Is(err, ErrSecretRedactionIncomplete) {
				t.Fatalf("err = %v, want ErrSecretRedactionIncomplete", err)
			}
		})
	}
	// The positive path end to end: an alias to an env reference is a name.
	ok := "common: &tok ${VORNIK_X}\nchat:\n  api_key: *tok\n"
	root := writeTree(t, map[string]string{"projects/p.yaml": ok})
	s, err := Build(root, nil, Limits{})
	if err != nil {
		t.Fatalf("an alias to an env reference must build: %v", err)
	}
	defer s.Close()
	if got := string(s.Files["projects/p.yaml"]); got != ok {
		t.Fatalf("env-reference alias must pass through unchanged:\n%s", got)
	}
}

// TestBuild_RefusesShapesTheParserCannotScreen pins round-5 review findings on
// the 2026-10-01 external-scan fix. Both alias checks and the span walker read
// ONE parsed YAML document. (1) A document yaml.v3 cannot parse falls back to
// the line pass, which redacts only values under secret-named keys, so an
// anchor's literal under a harmless key shipped. (2) Only the first `---`
// document was walked, so secrets in later documents were never screened.
// The config tree holds single-document files, so both now refuse.
func TestBuild_RefusesShapesTheParserCannotScreen(t *testing.T) {
	cases := []struct{ name, doc string }{
		{"unparseable with anchor and alias", "common: &tok Wq3eR5tY7uI9oP1aS2dF4gH6\napi_key: *tok\nbroken: \"unterminated\n"},
		{"second document", "projectId: p\n---\nchat:\n  api_key: Wq3eR5tY7uI9oP1aS2dF4gH6\n"},
		// Round 6: a tag before the anchor hid it from a separator-anchored regex.
		{"unparseable with unicode anchor name", "common: &tök Wq3eR5tY7uI9oP1aS2dF4gH6\napi_key: *tök\nbroken: \"unterminated\n"},
		{"unparseable, anchor after a closing brace", "x: {a: 1}&tok Wq3eR5tY7uI9oP1aS2dF4gH6\napi_key: }*tok\nbroken: \"unterminated\n"},
		// Round 8: over-matching is intended. With no parse tree, `&` and `*`
		// inside string values are indistinguishable from an anchor and an
		// alias; the refusal costs a repair-by-hand, never a leak.
		{"unparseable with ampersand and asterisk only in values", "name: \"AT&T\"\nmatch: \"x*y\"\nbroken: \"open\n"},
		{"unparseable with tagged anchor", "common: !!str &tok Wq3eR5tY7uI9oP1aS2dF4gH6\napi_key: *tok\nbroken: \"unterminated\n"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			root := writeTree(t, map[string]string{"projects/p.yaml": c.doc})
			s, err := Build(root, nil, Limits{})
			if err == nil {
				defer s.Close()
				t.Fatalf("Build must refuse; snapshot was:\n%s", s.Files["projects/p.yaml"])
			}
			if !errors.Is(err, ErrSecretRedactionIncomplete) {
				t.Fatalf("err = %v, want ErrSecretRedactionIncomplete", err)
			}
		})
	}
	// Markdown frontmatter still takes the line pass and still builds.
	md := "---\nworkflowId: digest\n---\n# body\n"
	root := writeTree(t, map[string]string{"workflows/digest.md": md})
	s, err := Build(root, nil, Limits{})
	if err != nil {
		t.Fatalf("a markdown workflow must still build: %v", err)
	}
	s.Close()
}

// TestBuild_FrontmatterAliasRefused pins round-6 review finding 2: markdown
// always takes the line pass, so an anchor in frontmatter reached by an alias
// under a secret-named key shipped the anchor's literal. The alias checks now
// run on the frontmatter block, parsed or not.
func TestBuild_FrontmatterAliasRefused(t *testing.T) {
	for name, md := range map[string]string{
		"parseable frontmatter":        "---\nworkflowId: w\ncommon: &tok Wq3eR5tY7uI9oP1aS2dF4gH6\napi_key: *tok\n---\n# body\n",
		"crlf unparseable frontmatter": "---\r\nworkflowId: w\r\ncommon: &tok Wq3eR5tY7uI9oP1aS2dF4gH6\r\napi_key: *tok\r\nbad: \"open\r\n---\r\n# body\r\n",
		// Round 8: the alias check runs inside the parsed branch, so a body
		// that is invalid as a second YAML document must not divert a
		// parseable frontmatter to the line pass.
		"parseable frontmatter, invalid body": "---\nworkflowId: w\ncommon: &tok Wq3eR5tY7uI9oP1aS2dF4gH6\napi_key: *tok\n---\n# body\na: b: c\n",
		"unclosed fence":                      "---\nworkflowId: w\ncommon: &tok Wq3eR5tY7uI9oP1aS2dF4gH6\napi_key: *tok\nbad: \"open\n",
		"unparseable frontmatter":             "---\nworkflowId: w\ncommon: &tok Wq3eR5tY7uI9oP1aS2dF4gH6\napi_key: *tok\nbad: \"open\n---\n# body\n",
	} {
		t.Run(name, func(t *testing.T) {
			root := writeTree(t, map[string]string{"workflows/w.md": md})
			s, err := Build(root, nil, Limits{})
			if err == nil {
				defer s.Close()
				t.Fatalf("Build must refuse; snapshot was:\n%s", s.Files["workflows/w.md"])
			}
			if !errors.Is(err, ErrSecretRedactionIncomplete) {
				t.Fatalf("err = %v, want ErrSecretRedactionIncomplete", err)
			}
		})
	}
	// A markdown BODY using `*emphasis*` and `&amp;` is prose, not YAML.
	prose := "---\nworkflowId: w\n---\n# body\nUse *care* with &amp; entities.\n"
	root := writeTree(t, map[string]string{"workflows/p.md": prose})
	s, err := Build(root, nil, Limits{})
	if err != nil {
		t.Fatalf("markdown prose must still build: %v", err)
	}
	s.Close()
}
