package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// GitHub #625 item 2: vornikctl credential set reads the value from standard
// input only, refuses a terminal, and strips one trailing newline. Found at:
// review.
func TestReadCredentialValue_625(t *testing.T) {
	v, err := readCredentialValue(strings.NewReader("tok-123\n"))
	if err != nil || string(v) != "tok-123" {
		t.Fatalf("got %q, %v", v, err)
	}
	if v, _ := readCredentialValue(strings.NewReader("a\n\n")); string(v) != "a\n" {
		t.Fatalf("only one trailing newline is stripped: %q", v)
	}
	if _, err := readCredentialValue(strings.NewReader("")); err == nil {
		t.Fatal("an empty value was accepted")
	}
	prev := stdinIsTerminal
	stdinIsTerminal = func(io.Reader) bool { return true }
	t.Cleanup(func() { stdinIsTerminal = prev })
	if _, err := readCredentialValue(strings.NewReader("x")); err == nil || !strings.Contains(err.Error(), "terminal") {
		t.Fatalf("a terminal was accepted: %v", err)
	}
}

func TestRenderCredentialList_625(t *testing.T) {
	var b bytes.Buffer
	_ = renderCredentialList(&b, true, func(w io.Writer) { _, _ = io.WriteString(w, "CONTEXT7_API_KEY\tstored\t-\tContext7\n") })
	out := b.String()
	for _, want := range []string{"key is missing or wrong", "NAME", "CONTEXT7_API_KEY", "stored", "Context7"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}
