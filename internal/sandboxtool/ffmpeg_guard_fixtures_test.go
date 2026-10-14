package sandboxtool_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"vornik.io/vornik/internal/extractor"
	"vornik.io/vornik/internal/extractor/audio"
	"vornik.io/vornik/internal/extractor/video"
	"vornik.io/vornik/internal/sandboxtool"
	"vornik.io/vornik/internal/sandboxtool/sandboxtest"
	"vornik.io/vornik/internal/voice"
)

// audit 2026-10-12 (GitHub #348): ffmpeg's playlist and manifest demuxers open
// files and URLs named inside the media. Measured against the agent image's
// ffmpeg 6.1.1, a DASH manifest uploaded as a video made the probe and frames
// runs read a local file into their output, and fetch from a server where a
// network existed (process-spawn-law design, "Change 2026-10-12").
//
// These tests play each untrusted-input run's OWN argv (audio extraction,
// video probe and frames, voice normalise) with the host's ffmpeg, /in and
// /out mapped to temp directories, against one fixture each: the run must
// fail, write no media output, and send no request. The host has a network,
// so the capture server is reachable: what holds is the argv, not the
// sandbox's --network=none. They skip without ffmpeg on PATH; the argv tests
// in each caller's package are the CI gate.
//
// Red before the guard on ffmpeg 8.1.3: dash-file and dash-subtitle on every
// run that reaches them (and dash-http on ffmpeg < 8, which follows http from
// a file manifest). HLS, concat and SDP were already held by ffmpeg's own
// checks and stay as regression pins.

type fixture struct {
	name string
	body string // %[1]s = canary dir, %[2]s = capture server URL
}

var guardFixtures = []fixture{
	{"hls-file", "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n" +
		"#EXTINF:2.0,\nfile://%[1]s/canary.ts\n#EXTINF:2.0,\n%[1]s/canary.ts\n#EXT-X-ENDLIST\n"},
	{"hls-http", "#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-TARGETDURATION:2\n#EXT-X-MEDIA-SEQUENCE:0\n" +
		"#EXTINF:2.0,\n%[2]s/hls-seg.ts\n#EXTINF:2.0,\nfile:///etc/passwd\n#EXT-X-ENDLIST\n"},
	{"concat", "ffconcat version 1.0\nfile %[1]s/canary.ts\nfile %[2]s/concat-seg.ts\n"},
	{"dash-http", dashManifest(`<AdaptationSet mimeType="video/mp2t"><Representation id="v" bandwidth="64000">` +
		`<BaseURL>%[2]s/dash-seg.ts</BaseURL></Representation></AdaptationSet>`)},
	{"dash-file", dashManifest(`<AdaptationSet mimeType="video/mp2t"><Representation id="v" bandwidth="64000">` +
		`<BaseURL>file://%[1]s/canary.ts</BaseURL></Representation></AdaptationSet>`)},
	// A container naming an external subtitle file.
	{"dash-subtitle", dashManifest(`<AdaptationSet mimeType="text/vtt" contentType="text" lang="en">` +
		`<Representation id="s" bandwidth="100"><BaseURL>file://%[1]s/sub.mp4</BaseURL></Representation></AdaptationSet>`)},
	{"sdp", "v=0\no=- 0 0 IN IP4 127.0.0.1\ns=x\nc=IN IP4 127.0.0.1\nt=0 0\nm=audio 9 RTP/AVP 0\n"},
}

func dashManifest(set string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>` + "\n" +
		`<MPD xmlns="urn:mpeg:dash:schema:mpd:2011" type="static" mediaPresentationDuration="PT2S" minBufferTime="PT1S" ` +
		`profiles="urn:mpeg:dash:profile:isoff-on-demand:2011"><Period>` + set + `</Period></MPD>` + "\n"
}

// guardHarness holds the canary directory and the capture server.
type guardHarness struct {
	canaryDir string
	server    *httptest.Server
	requests  atomic.Int64
}

func newGuardHarness(t *testing.T) *guardHarness {
	t.Helper()
	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(bin); err != nil {
			t.Skipf("%s not on PATH: the #348 fixture tests need the host's ffmpeg", bin)
		}
	}
	h := &guardHarness{canaryDir: t.TempDir()}
	ts := filepath.Join(h.canaryDir, "canary.ts")
	// A 2 s red video with a tone: any local media file a run could see.
	out, err := exec.Command("ffmpeg", "-nostdin", "-hide_banner", "-loglevel", "error", "-y",
		"-f", "lavfi", "-i", "color=c=red:s=64x64:d=2:r=5", "-f", "lavfi", "-i", "sine=f=440:d=2",
		"-c:v", "mpeg2video", "-c:a", "mp2", "-shortest", ts).CombinedOutput()
	if err != nil {
		t.Skipf("host ffmpeg cannot make the canary: %v: %s", err, out)
	}
	if err := os.WriteFile(filepath.Join(h.canaryDir, "sub.mp4"),
		[]byte("WEBVTT\n\n00:00.000 --> 00:01.000\nCANARY-SUBTITLE\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	canary, err := os.ReadFile(ts)
	if err != nil {
		t.Fatal(err)
	}
	h.server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		h.requests.Add(1)
		w.Header().Set("Content-Type", "video/mp2t")
		_, _ = w.Write(canary)
	}))
	t.Cleanup(h.server.Close)
	return h
}

func (h *guardHarness) write(t *testing.T, fx fixture) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "upload")
	if err := os.WriteFile(p, []byte(fmt.Sprintf(fx.body, h.canaryDir, h.server.URL)), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// hostRun records what one host-played run did.
type hostRun struct {
	entrypoint string
	err        error
	outFiles   []string // non-empty media files the run wrote
	probe      string   // ffprobe's format_name and stream count, if any
}

// hostTools plays every ffmpeg/ffprobe Spec with the host's binary, the Spec's
// argv unchanged except /in and /out mapped to the run's temp directories.
// Any other tool (whisper-cli) is refused: these tests stop at the media run.
func hostTools(t *testing.T) (*sandboxtest.Fake, func() []hostRun) {
	t.Helper()
	var mu sync.Mutex
	var runs []hostRun
	fake := sandboxtest.New(t, func(spec sandboxtool.Spec, in map[string][]byte, out string) error {
		if spec.Entrypoint != "ffmpeg" && spec.Entrypoint != "ffprobe" {
			return errors.New("not played here: " + spec.Entrypoint)
		}
		inDir := t.TempDir()
		for name, b := range in {
			if err := os.WriteFile(filepath.Join(inDir, name), b, 0o600); err != nil {
				return err
			}
		}
		args := make([]string, len(spec.Args))
		for i, a := range spec.Args {
			a = strings.Replace(a, "/in/", inDir+"/", 1)
			args[i] = strings.Replace(a, "/out/", out+"/", 1)
		}
		cmd := exec.Command(spec.Entrypoint, args...) //nolint:gosec // test: the Spec's own argv
		cmd.Dir = out
		printed, err := cmd.CombinedOutput()
		if err != nil {
			err = fmt.Errorf("%s: %w: %s", spec.Entrypoint, err, strings.TrimSpace(string(printed)))
		}
		run := hostRun{entrypoint: spec.Entrypoint, err: err}
		entries, _ := os.ReadDir(out)
		for _, e := range entries {
			info, ierr := e.Info()
			if ierr != nil || info.Size() == 0 {
				continue
			}
			if e.Name() == "probe.json" {
				run.probe = probeSummary(filepath.Join(out, e.Name()))
				continue
			}
			run.outFiles = append(run.outFiles, fmt.Sprintf("%s (%d B)", e.Name(), info.Size()))
		}
		mu.Lock()
		runs = append(runs, run)
		mu.Unlock()
		return err
	})
	return fake, func() []hostRun {
		mu.Lock()
		defer mu.Unlock()
		return append([]hostRun(nil), runs...)
	}
}

func probeSummary(path string) string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var p struct {
		Format struct {
			Name string `json:"format_name"`
		} `json:"format"`
		Streams []json.RawMessage `json:"streams"`
	}
	if json.Unmarshal(raw, &p) != nil || (p.Format.Name == "" && len(p.Streams) == 0) {
		return ""
	}
	return fmt.Sprintf("%s, %d streams", p.Format.Name, len(p.Streams))
}

func (h *guardHarness) assertHeld(t *testing.T, fx fixture, runs []hostRun) {
	t.Helper()
	if len(runs) == 0 {
		t.Fatalf("%s: no ffmpeg/ffprobe run happened", fx.name)
	}
	for _, r := range runs {
		if r.err == nil {
			t.Errorf("%s: %s accepted the fixture", fx.name, r.entrypoint)
		}
		if len(r.outFiles) > 0 {
			t.Errorf("%s: %s wrote media from what the fixture named: %v", fx.name, r.entrypoint, r.outFiles)
		}
		if r.probe != "" {
			t.Errorf("%s: ffprobe read the fixture as %s", fx.name, r.probe)
		}
	}
	if n := h.requests.Load(); n != 0 {
		t.Errorf("%s: the capture server got %d request(s)", fx.name, n)
	}
}

func TestFFmpegInputGuard_Fixtures_VideoProbeAndFrames(t *testing.T) {
	h := newGuardHarness(t)
	for _, fx := range guardFixtures {
		t.Run(fx.name, func(t *testing.T) {
			h.requests.Store(0)
			sb, runs := hostTools(t)
			path := h.write(t, fx)
			// Extract runs the probe and, when it succeeds, the frames run.
			_, _ = video.NewWithOptions(sb, 1, 5).Extract(context.Background(),
				extractor.Source{FilePath: path, OriginalName: "clip.mp4"})
			h.assertHeld(t, fx, runs())
		})
	}
}

func TestFFmpegInputGuard_Fixtures_VideoFramesRun(t *testing.T) {
	// The frames run on its own: Extract skips it when the probe fails, and
	// it must hold even if a probe were to pass.
	h := newGuardHarness(t)
	probeOK := `{"format":{"duration":"2","format_name":"mov,mp4,m4a"},"streams":[{"codec_type":"video"}]}`
	for _, fx := range guardFixtures {
		t.Run(fx.name, func(t *testing.T) {
			h.requests.Store(0)
			host, runs := hostTools(t)
			sb := sandboxtest.New(t, func(spec sandboxtool.Spec, _ map[string][]byte, out string) error {
				if spec.Entrypoint == "ffprobe" {
					return os.WriteFile(filepath.Join(out, "probe.json"), []byte(probeOK), 0o600)
				}
				res, err := host.Run(context.Background(), spec)
				if err != nil {
					return err
				}
				entries, _ := os.ReadDir(res.OutDir)
				for _, e := range entries {
					b, rerr := os.ReadFile(filepath.Join(res.OutDir, e.Name()))
					if rerr == nil {
						_ = os.WriteFile(filepath.Join(out, e.Name()), b, 0o600)
					}
				}
				return nil
			})
			_, _ = video.NewWithOptions(sb, 1, 5).Extract(context.Background(),
				extractor.Source{FilePath: h.write(t, fx), OriginalName: "clip.mp4"})
			h.assertHeld(t, fx, runs())
		})
	}
}

func TestFFmpegInputGuard_Fixtures_AudioExtraction(t *testing.T) {
	h := newGuardHarness(t)
	model := filepath.Join(t.TempDir(), "ggml-tiny.bin")
	if err := os.WriteFile(model, []byte("ggml"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, fx := range guardFixtures {
		t.Run(fx.name, func(t *testing.T) {
			h.requests.Store(0)
			sb, runs := hostTools(t)
			_, _ = audio.New(sb, model).Extract(context.Background(),
				extractor.Source{FilePath: h.write(t, fx), OriginalName: "note.m4a"})
			h.assertHeld(t, fx, runs())
		})
	}
}

func TestFFmpegInputGuard_Fixtures_VoiceNormalise(t *testing.T) {
	h := newGuardHarness(t)
	for _, fx := range guardFixtures {
		t.Run(fx.name, func(t *testing.T) {
			h.requests.Store(0)
			sb, runs := hostTools(t)
			body, err := os.ReadFile(h.write(t, fx))
			if err != nil {
				t.Fatal(err)
			}
			if res, err := sb.Run(context.Background(), voice.NormaliseSpec(body)); err == nil {
				res.Close()
			}
			h.assertHeld(t, fx, runs())
		})
	}
}
