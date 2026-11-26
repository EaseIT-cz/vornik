package cli

// GitHub #866 (enterprise packaging design, amendment 2026-11-21): four
// vornikctl checks tested os.ModeCharDevice, which /dev/null also sets, so a
// command run from automation with stdin or stdout at /dev/null took itself
// for interactive. Each now asks internal/termcheck.IsTerminal.
//
// Found at: review (while fixing #865); a user meets it as a CLI run from
// automation. Regression test at: unit. Lower because: each decision is the
// helper's answer for the file, which these tests pass directly.

import (
	"bytes"
	"os"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func devNull(t *testing.T, flag int) *os.File {
	t.Helper()
	f, err := os.OpenFile(os.DevNull, flag, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// withStdinFile866 points os.Stdin at f for the test.
func withStdinFile866(t *testing.T, f *os.File) {
	t.Helper()
	prev := os.Stdin
	os.Stdin = f
	t.Cleanup(func() { os.Stdin = prev })
}

func TestAuthLogin_NoHintWhenStdinIsDevNull_866(t *testing.T) {
	null := devNull(t, os.O_RDONLY)
	withStdinFile866(t, null)
	cmd := &cobra.Command{}
	var stderr bytes.Buffer
	cmd.SetErr(&stderr)
	cmd.SetIn(null)
	err := runAuthLogin(cmd, nil)
	if err == nil || !strings.Contains(err.Error(), "no API key provided") {
		t.Fatalf("want the empty-key error, got %v", err)
	}
	if strings.Contains(stderr.String(), "Enter API key") {
		t.Fatalf("/dev/null is not a terminal: no prompt hint, got %q", stderr.String())
	}
}

func TestAgentConnect_DevNullIsNotATerminal_866(t *testing.T) {
	_, err := readAPIKey(devNull(t, os.O_RDONLY))
	if err == nil {
		t.Fatal("an empty key must be refused")
	}
	if strings.Contains(err.Error(), "is a terminal") {
		t.Fatalf("/dev/null was refused as a terminal: %v", err)
	}
	if !strings.Contains(err.Error(), "is empty") {
		t.Fatalf("an empty /dev/null read must be refused as empty, got %v", err)
	}
}

func TestInitProjectPrompt_TakesTheDefaultSilentlyFromDevNull_866(t *testing.T) {
	withStdinFile866(t, devNull(t, os.O_RDONLY))
	cmd := &cobra.Command{}
	var out bytes.Buffer
	cmd.SetOut(&out)
	if got := promptDefault(cmd, "Display name", "fallback"); got != "fallback" {
		t.Fatalf("promptDefault = %q, want the default", got)
	}
	if out.Len() != 0 {
		t.Fatalf("/dev/null is not a terminal: no prompt, got %q", out.String())
	}
}

func TestOperationsCredentials_DevNullIsNotATerminal_866(t *testing.T) {
	if stdoutIsTerminal(devNull(t, os.O_WRONLY)) {
		t.Fatal("stdout at /dev/null was refused as a terminal")
	}
	if stdoutIsTerminal(&bytes.Buffer{}) {
		t.Fatal("a non-file writer is not a terminal")
	}
}
