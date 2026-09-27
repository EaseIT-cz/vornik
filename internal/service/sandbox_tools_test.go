package service

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/sandboxtool"
)

const (
	sbMiB = int64(1024 * 1024)
	sbGiB = 1024 * sbMiB
)

func TestResolveSandboxTools(t *testing.T) {
	t.Run("defaults resolve to an empty override set", func(t *testing.T) {
		cfg, err := resolveSandboxTools(config.SandboxToolsConfig{}, "img", "/scratch")
		if err != nil || cfg.Image != "img" || cfg.ScratchRoot != "/scratch" || cfg.Limits != nil || cfg.Timeouts != nil {
			t.Fatalf("got %+v, %v", cfg, err)
		}
	})
	t.Run("limits, cpus and timeouts are parsed per feature", func(t *testing.T) {
		cfg, err := resolveSandboxTools(config.SandboxToolsConfig{
			MaxConcurrent: 3,
			Limits:        map[string]string{"render": "768MiB"},
			CPUs:          map[string]string{"video": "1.5"},
			Timeouts:      map[string]int{"render": 30},
		}, "img", t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Limits[sandboxtool.FeatureRender] != 768*sbMiB || cfg.Timeouts[sandboxtool.FeatureRender] != 30*time.Second || cfg.MaxConcurrent != 3 {
			t.Fatalf("got %+v", cfg)
		}
		if cfg.CPUs[sandboxtool.FeatureVideo] != "1.5" {
			t.Fatalf("cpus: %+v", cfg.CPUs)
		}
	})
	for name, sc := range map[string]config.SandboxToolsConfig{
		"unknown feature":     {Limits: map[string]string{"randr": "1GiB"}},
		"unknown timeout key": {Timeouts: map[string]int{"randr": 5}},
		"bad size":            {Limits: map[string]string{"pdf": "lots"}},
		"unbounded limit":     {Limits: map[string]string{"pdf": "none"}},
		"zero timeout":        {Timeouts: map[string]int{"pdf": 0}},
		"zero cpus":           {CPUs: map[string]string{"pdf": "0"}},
		"unknown cpus key":    {CPUs: map[string]string{"randr": "1"}},
		"negative pool":       {MaxConcurrent: -1},
		"negative input cap":  {MaxInputBytes: -1},
	} {
		t.Run("refuses "+name, func(t *testing.T) {
			if _, err := resolveSandboxTools(sc, "img", t.TempDir()); err == nil {
				t.Fatal("want a startup error")
			}
		})
	}
}

func TestSandboxToolsConfig_IsSet(t *testing.T) {
	if (config.SandboxToolsConfig{Timeouts: map[string]int{"pdf": 5}, MaxInputBytes: 1}).IsSet() {
		t.Fatal("timeouts and the input cap do not size the memory commitment")
	}
	if !(config.SandboxToolsConfig{MaxConcurrent: 1}).IsSet() || !(config.SandboxToolsConfig{Limits: map[string]string{"pdf": "1GiB"}}).IsSet() {
		t.Fatal("max_concurrent and limits are what the operator sets")
	}
}

func TestInitSandboxTools_SweepsAndWires(t *testing.T) {
	data := t.TempDir()
	t.Setenv("VORNIK_DATA_DIR", data)
	left := filepath.Join(data, "sandbox-tools", "run-leftover")
	if err := os.MkdirAll(left, 0o700); err != nil {
		t.Fatal(err)
	}
	fake := &fakeSandboxHost{leftovers: "vornik-sbx-dead\n", label: "pdftotext,tesseract"}
	withSandboxCommand(t, fake.run)
	var logs bytes.Buffer
	c := &Container{Logger: zerolog.New(&logs), Config: &config.Config{SandboxTools: config.SandboxToolsConfig{MaxConcurrent: 1}}}
	if err := c.initSandboxTools(); err != nil {
		t.Fatal(err)
	}
	if c.sandboxRunner == nil || c.sandboxRunner.MaxConcurrent() != 1 {
		t.Fatalf("runner = %+v", c.sandboxRunner)
	}
	if _, err := os.Stat(left); !os.IsNotExist(err) {
		t.Fatalf("an earlier process's scratch must be swept at startup: %v", err)
	}
	// S5a review F1: the leftover container is swept too.
	if !fake.removed("vornik-sbx-dead") {
		t.Fatalf("an earlier process's container must be swept at startup: %v", fake.calls)
	}
	// §7.1 decision 4: the pinned image's declared tools are read at startup.
	if !strings.Contains(logs.String(), "pdftotext,tesseract") {
		t.Fatalf("startup must log the image's declared tools: %s", logs.String())
	}

	bad := &Container{Logger: zerolog.Nop(), Config: &config.Config{SandboxTools: config.SandboxToolsConfig{Limits: map[string]string{"nope": "1GiB"}}}}
	if err := bad.initSandboxTools(); err == nil {
		t.Fatal("an unknown feature must fail startup")
	}
}

// An image without the label (older than this release) boots, and says which
// features will report not available.
func TestInitSandboxTools_AnUnlabelledImageWarns(t *testing.T) {
	t.Setenv("VORNIK_DATA_DIR", t.TempDir())
	withSandboxCommand(t, (&fakeSandboxHost{label: "<no value>"}).run)
	var logs bytes.Buffer
	c := &Container{Logger: zerolog.New(&logs), Config: &config.Config{}}
	if err := c.initSandboxTools(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logs.String(), "declares no sandbox tools") {
		t.Fatalf("want a warning naming the missing label: %s", logs.String())
	}
}

// fakeSandboxHost answers the podman calls initSandboxTools makes.
type fakeSandboxHost struct {
	calls     [][]string
	leftovers string
	label     string
}

func (f *fakeSandboxHost) run(_ context.Context, _ io.Reader, _ string, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	switch {
	case len(args) > 0 && args[0] == "ps":
		return []byte(f.leftovers), nil
	case len(args) > 1 && args[0] == "image":
		return []byte(f.label), nil
	}
	return nil, nil
}

func (f *fakeSandboxHost) removed(name string) bool {
	for _, c := range f.calls {
		if len(c) > 0 && c[0] == "rm" && c[len(c)-1] == name {
			return true
		}
	}
	return false
}

func withSandboxCommand(t *testing.T, run sandboxtool.CommandRunner) {
	t.Helper()
	prev := sandboxCommand
	sandboxCommand = run
	t.Cleanup(func() { sandboxCommand = prev })
}

func TestSandboxScratchRoot(t *testing.T) {
	t.Setenv("VORNIK_DATA_DIR", "/data")
	if got := sandboxScratchRoot(); got != "/data/sandbox-tools" {
		t.Fatalf("got %q", got)
	}
	t.Setenv("VORNIK_DATA_DIR", "")
	if got := sandboxScratchRoot(); !strings.HasPrefix(filepath.Base(got), "vornik-sandbox-tools-") {
		t.Fatalf("got %q", got)
	}
}

// Design §7.3 (S5-R3A, S5-N3): the sandbox pool's commitment joins the startup
// memory check, but only numbers an operator SET can refuse a boot.
func TestCheckCombinedMemory(t *testing.T) {
	defaults := sandboxCommitment{maxConcurrent: 2, largest: 2 * sbGiB}

	t.Run("a pure-default config on a 4 GB host boots", func(t *testing.T) {
		t.Setenv("VORNIK_DATA_DIR", t.TempDir())
		c := &Container{Logger: zerolog.Nop(), Config: &config.Config{}}
		if err := c.initSandboxTools(); err != nil {
			t.Fatal(err)
		}
		reader := func(v int64) hostMemoryReader { return func() (int64, error) { return v, nil } }
		if _, err := c.applyAgentMemoryLimit(nil, reader(4*sbGiB), reader(3*sbGiB)); err != nil {
			t.Fatalf("defaults must never refuse a boot: %v", err)
		}
		// And the arithmetic does overcommit, so it is a WARN, not silence.
		warn, err := checkCombinedMemory("", 1*sbGiB, 4, defaults, 4*sbGiB, 3*sbGiB)
		if err != nil || !strings.Contains(warn, "Booting") {
			t.Fatalf("want a warning and a boot, got %q, %v", warn, err)
		}
	})

	t.Run("a set sandbox over total refuses and names all five exits", func(t *testing.T) {
		set := sandboxCommitment{set: true, maxConcurrent: 4, largest: 2 * sbGiB}
		_, err := checkCombinedMemory("2GiB", 2*sbGiB, 2, set, 8*sbGiB, 8*sbGiB)
		if err == nil {
			t.Fatal("want a refusal")
		}
		for _, exit := range []string{
			"runtime.agent_memory_limit", "scheduler.max_concurrent_tasks",
			"sandbox_tools.max_concurrent", "sandbox_tools.limits.<feature>", `"none"`,
		} {
			if !strings.Contains(err.Error(), exit) {
				t.Errorf("the refusal must name %s: %v", exit, err)
			}
		}
	})

	t.Run("the product uses the configured limits", func(t *testing.T) {
		// Lowering the largest limit brings the same pool under the host.
		lowered := sandboxCommitment{set: true, maxConcurrent: 4, largest: 1 * sbGiB}
		if _, err := checkCombinedMemory("2GiB", 2*sbGiB, 2, lowered, 8*sbGiB, 8*sbGiB); err != nil {
			t.Fatalf("4 x 1GiB + 4GiB fits 8GiB: %v", err)
		}
	})

	t.Run("a derived agent limit never counts toward the refusal", func(t *testing.T) {
		set := sandboxCommitment{set: true, maxConcurrent: 2, largest: 2 * sbGiB}
		warn, err := checkCombinedMemory("", 3*sbGiB, 2, set, 8*sbGiB, 8*sbGiB)
		if err != nil || warn == "" {
			t.Fatalf("derived 6GiB + set 4GiB over 8GiB warns, got %q, %v", warn, err)
		}
	})

	t.Run("the agent escape hatch drops the agent term", func(t *testing.T) {
		set := sandboxCommitment{set: true, maxConcurrent: 2, largest: 2 * sbGiB}
		if _, err := checkCombinedMemory("none", 0, 8, set, 8*sbGiB, 8*sbGiB); err != nil {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("under available is silent, over available warns", func(t *testing.T) {
		if warn, err := checkCombinedMemory("1GiB", 1*sbGiB, 2, defaults, 32*sbGiB, 16*sbGiB); warn != "" || err != nil {
			t.Fatalf("got %q, %v", warn, err)
		}
		if warn, _ := checkCombinedMemory("1GiB", 1*sbGiB, 2, defaults, 32*sbGiB, 5*sbGiB); !strings.Contains(warn, "available") {
			t.Fatalf("got %q", warn)
		}
	})

	t.Run("an unreadable host total has no basis", func(t *testing.T) {
		set := sandboxCommitment{set: true, maxConcurrent: 64, largest: 2 * sbGiB}
		if warn, err := checkCombinedMemory("2GiB", 2*sbGiB, 1, set, 0, 0); warn != "" || err != nil {
			t.Fatalf("got %q, %v", warn, err)
		}
	})

	t.Run("an overflowing pool is refused", func(t *testing.T) {
		huge := sandboxCommitment{set: true, maxConcurrent: 1 << 40, largest: 1 << 40}
		if _, err := checkCombinedMemory("", 0, 1, huge, 8*sbGiB, 8*sbGiB); err == nil {
			t.Fatal("want an overflow refusal")
		}
	})

	t.Run("wired: a set sandbox over total fails init", func(t *testing.T) {
		c := &Container{Logger: zerolog.Nop(), Config: &config.Config{
			SandboxTools: config.SandboxToolsConfig{MaxConcurrent: 8, Limits: map[string]string{"audio": "2GiB"}},
		}}
		t.Setenv("VORNIK_DATA_DIR", t.TempDir())
		if err := c.initSandboxTools(); err != nil {
			t.Fatal(err)
		}
		reader := func(v int64) hostMemoryReader { return func() (int64, error) { return v, nil } }
		if _, err := c.applyAgentMemoryLimit(nil, reader(8*sbGiB), reader(8*sbGiB)); err == nil {
			t.Fatal("8 x 2GiB of sandbox on an 8GiB host must refuse")
		}
	})
}
