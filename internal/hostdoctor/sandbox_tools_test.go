package hostdoctor

import (
	"context"
	"errors"
	"strings"
	"testing"

	"vornik.io/vornik/internal/sandboxtool/probe"
)

// Process-spawn law S5b, design §7.1 decision 4: the doctor runs every
// sandbox tool on a fixture, with the daemon's flags, and reports what each
// did. A tool that ran and did the wrong thing, or a configured model that is
// unusable, is an ERROR; a tool the image does not carry is a WARNING (the
// feature reports "not available", visibly); an unlabelled image is a
// WARNING naming the cause.
func TestCheckSandboxTools(t *testing.T) {
	ok := []probe.Result{{Name: "pdftotext", Status: probe.StatusOK, Detail: "extracted"}, {Name: "piper", Status: probe.StatusSkipped, Detail: "voice.tts.voice is not set"}}
	all := map[string]bool{"pdftotext": true, "piper": true}
	for _, tc := range []struct {
		name    string
		tools   map[string]bool
		loadErr error
		results []probe.Result
		status  string
		want    string
	}{
		{"all ok", all, nil, ok, "OK", "OK pdftotext: extracted"},
		{"a failure", all, nil, append(ok, probe.Result{Name: "tesseract", Status: probe.StatusFailed, Detail: "did not read"}), "ERROR", "FAILED tesseract: did not read"},
		{"not available", all, nil, append(ok, probe.Result{Name: "ffprobe", Status: probe.StatusNotAvailable, Detail: "absent"}), "WARNING", "NOT AVAILABLE ffprobe: absent"},
		{"unlabelled image", map[string]bool{}, nil, nil, "WARNING", "declares no sandbox tools"},
		{"image missing", nil, errors.New("inspect img: image not known"), nil, "WARNING", "image not known"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ran := false
			h := &Checker{sandboxEnv: &SandboxEnv{Models: probe.Models{VoiceSTT: "/m.bin"}},
				sandboxProbeFunc: func(_ context.Context, env SandboxEnv) sandboxOutcome {
					ran = true
					if env.Models.VoiceSTT != "/m.bin" {
						t.Errorf("the models must reach the probe: %+v", env.Models)
					}
					return sandboxOutcome{tools: tc.tools, loadErr: tc.loadErr, results: tc.results}
				}}
			c := h.checkSandboxTools(context.Background())
			if !ran || c.Name != "sandbox_tools" || c.Status != tc.status {
				t.Fatalf("check = %+v", c)
			}
			if !strings.Contains(c.Message+" "+strings.Join(c.Items, " | "), tc.want) {
				t.Fatalf("want %q in %+v", tc.want, c)
			}
		})
	}
	if c := (&Checker{}).checkSandboxTools(context.Background()); c.Status != "SKIPPED" {
		t.Fatalf("no daemon config: %+v", c)
	}
}

func TestWithSandbox_SetsTheEnvironment(t *testing.T) {
	h := New("", "", "").WithSandbox(SandboxEnv{Models: probe.Models{VoiceTTS: "/v.onnx"}})
	if h.sandboxEnv == nil || h.sandboxEnv.Models.VoiceTTS != "/v.onnx" {
		t.Fatalf("env = %+v", h.sandboxEnv)
	}
	if !strings.HasPrefix(doctorScratchRoot(), "/") || !strings.Contains(doctorScratchRoot(), "vornik-doctor-sandbox-") {
		t.Fatalf("scratch root = %s", doctorScratchRoot())
	}
}
