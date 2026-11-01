package docrender

// GitHub #117, Art 50(2) step 2 commit 5 (Art 50 design §14.8): the fields
// that carry the mark into a rendered document.

import (
	"reflect"
	"strings"
	"testing"
)

func TestMarkMeta_SignedAndUnsigned(t *testing.T) {
	signed := map[string]any{"title": "t"}
	MarkMeta(signed, "AAA.BBB")
	if d, _ := signed["description"].(string); !strings.HasPrefix(d, AIGeneratedDescription) || !strings.HasSuffix(d, "vornik-ai-mark: AAA.BBB") {
		t.Errorf("description %q", d)
	}
	if !reflect.DeepEqual(signed["keywords"], []string{"trainedAlgorithmicMedia", "vornik-ai-mark"}) || signed["vornik-ai-mark"] != "AAA.BBB" || signed["title"] != "t" {
		t.Errorf("meta %v", signed)
	}
	unsigned := map[string]any{}
	MarkMeta(unsigned, "")
	if unsigned["description"] != AIGeneratedDescription {
		t.Errorf("unsigned description %q", unsigned["description"])
	}
	if _, ok := unsigned["vornik-ai-mark"]; ok {
		t.Error("an unsigned document must not carry a mark field")
	}
}

func TestUnsignedMarkArgs(t *testing.T) {
	got := strings.Join(UnsignedMarkArgs(), " ")
	for _, want := range []string{"--metadata=description:" + AIGeneratedDescription, "--metadata=keywords:trainedAlgorithmicMedia", "--metadata=keywords:vornik-ai-mark"} {
		if !strings.Contains(got, want) {
			t.Errorf("args %q lack %q", got, want)
		}
	}
}
