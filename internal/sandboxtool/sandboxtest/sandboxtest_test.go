package sandboxtest

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"vornik.io/vornik/internal/sandboxtool"
)

func TestFake_RecordsSpecsAndPlaysTheTool(t *testing.T) {
	src := filepath.Join(t.TempDir(), "doc")
	if err := os.WriteFile(src, []byte("from path"), 0o600); err != nil {
		t.Fatal(err)
	}
	f := New(t, func(spec sandboxtool.Spec, in map[string][]byte, out string) error {
		if string(in["a"]) != "inline" || string(in["b"]) != "from path" {
			t.Errorf("inputs = %q", in)
		}
		return os.WriteFile(filepath.Join(out, "x"), []byte(spec.Entrypoint), 0o600)
	})
	res, err := f.Run(context.Background(), sandboxtool.Spec{Feature: sandboxtool.FeaturePDF, Entrypoint: "pdftotext",
		Inputs: []sandboxtool.Input{{Name: "a", Data: []byte("inline")}, {Name: "b", Path: src}}})
	if err != nil {
		t.Fatal(err)
	}
	defer res.Close()
	if b, _ := os.ReadFile(filepath.Join(res.OutDir, "x")); string(b) != "pdftotext" {
		t.Fatalf("out = %q", b)
	}
	if len(f.Specs()) != 1 {
		t.Fatal("the run must be recorded")
	}
	if _, err := f.Run(context.Background(), sandboxtool.Spec{Inputs: []sandboxtool.Input{{Name: "c", Path: filepath.Join(t.TempDir(), "absent")}}}); err == nil {
		t.Fatal("an unreadable path input is an error")
	}
	if !errors.Is(NotAvailable(sandboxtool.FeaturePDF), sandboxtool.ErrNotAvailable) {
		t.Fatal("NotAvailable must be not available")
	}
	if errors.Is(Failed(sandboxtool.FeaturePDF, "x"), sandboxtool.ErrNotAvailable) {
		t.Fatal("Failed is not not-available")
	}
	boom := New(t, func(sandboxtool.Spec, map[string][]byte, string) error { return errors.New("boom") })
	if _, err := boom.Run(context.Background(), sandboxtool.Spec{}); err == nil {
		t.Fatal("the handler's error is the run's")
	}
}

// A tool that exits 0 still prints: in the voice incident of 2026-10-03,
// whisper-cli printed why it could not read its input and exited 0, so
// the fake has to be able to play that output.
func TestFake_NewWithOutputCarriesWhatTheToolPrinted(t *testing.T) {
	f := NewWithOutput(t, func(sandboxtool.Spec, map[string][]byte, string) ([]byte, error) {
		return []byte("error: failed to read audio file"), nil
	})
	res, err := f.Run(context.Background(), sandboxtool.Spec{})
	if err != nil {
		t.Fatal(err)
	}
	defer res.Close()
	if string(res.Output) != "error: failed to read audio file" {
		t.Fatalf("Output = %q", res.Output)
	}
}
