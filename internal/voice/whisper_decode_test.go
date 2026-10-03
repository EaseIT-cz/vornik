package voice

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"vornik.io/vornik/internal/sandboxtool"
	"vornik.io/vornik/internal/sandboxtool/sandboxtest"
)

// Incident 2026-10-03 12:52 (voice-messages-design.md §8): an audio clip
// sent to the Telegram bot got "I couldn't make out the voice message".
// ffmpeg exited cleanly with a non-empty WAV; whisper-cli printed
// "failed to read audio file '/in/audio.wav'" and EXITED 0, so the runner
// reported success, whisper's words were dropped, and the daemon logged
// only "read whisper JSON: … no such file or directory".

// whisperMiniaudioFailure is what whisper-cli printed in production.
const whisperMiniaudioFailure = "read_audio_data: trying to decode with miniaudio\n" +
	"read_audio_data: failed to read audio data\n" +
	"error: failed to read audio file '/in/audio.wav'\n"

// printingTools plays ffmpeg and whisper-cli exiting 0, writing what is set
// and printing what is set.
type printingTools struct {
	wav        []byte // nil: ffmpeg writes no audio.wav
	ffmpegOut  string
	ffmpegErr  error
	transcript string // "": whisper-cli writes no transcript.json
	whisperOut string
}

func (p printingTools) fake(t *testing.T) *sandboxtest.Fake {
	return sandboxtest.NewWithOutput(t, func(spec sandboxtool.Spec, _ map[string][]byte, out string) ([]byte, error) {
		switch spec.Entrypoint {
		case "ffmpeg":
			if p.ffmpegErr != nil {
				return nil, p.ffmpegErr
			}
			if p.wav != nil {
				if err := os.WriteFile(filepath.Join(out, "audio.wav"), p.wav, 0o600); err != nil {
					return nil, err
				}
			}
			return []byte(p.ffmpegOut), nil
		case "whisper-cli":
			if p.transcript != "" {
				if err := os.WriteFile(filepath.Join(out, "transcript.json"), []byte(p.transcript), 0o600); err != nil {
					return nil, err
				}
			}
			return []byte(p.whisperOut), nil
		}
		return nil, errors.New("unexpected tool " + spec.Entrypoint)
	})
}

func ranWhisper(sb *sandboxtest.Fake) bool {
	for _, s := range sb.Specs() {
		if s.Entrypoint == "whisper-cli" {
			return true
		}
	}
	return false
}

func transcribeWith(t *testing.T, sb *sandboxtest.Fake) error {
	t.Helper()
	w := newWhisper(t, sb, WhisperConfig{})
	_, err := w.Transcribe(context.Background(), bytes.NewReader([]byte("ID3-clip")), Hint{MimeType: "audio/mpeg"})
	return err
}

// The production failure: whisper-cli exits 0 with no JSON. The error must
// carry what whisper said.
func TestWhisperLocalSTT_CleanExitWithoutJSONCarriesWhisperOutput(t *testing.T) {
	err := transcribeWith(t, printingTools{wav: makeWAV(16000, 1600), whisperOut: whisperMiniaudioFailure}.fake(t))
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "failed to read audio file") {
		t.Fatalf("the error must carry whisper's own message: %v", err)
	}
}

// The tail is bounded: a chatty tool cannot flood the log line.
func TestWhisperLocalSTT_ToolOutputTailIsBounded(t *testing.T) {
	noisy := strings.Repeat("whisper_init_state: noise\n", 400) + "error: the last word"
	err := transcribeWith(t, printingTools{wav: makeWAV(16000, 1600), whisperOut: noisy}.fake(t))
	if err == nil || !strings.Contains(err.Error(), "the last word") {
		t.Fatalf("the tail must keep the end of the output: %v", err)
	}
	if len(err.Error()) > toolOutputTail+300 {
		t.Fatalf("error is %d bytes; the tail must be bounded to ~%d", len(err.Error()), toolOutputTail)
	}
}

// whisper prints its transcript to stdout even with -np. If it ever printed
// one and still wrote no JSON, the user's speech must not reach the log.
func TestWhisperLocalSTT_ToolOutputTailDropsTranscriptLines(t *testing.T) {
	out := "[00:00:00.000 --> 00:00:02.000]   my bank PIN is 1234\n" +
		"error: failed to write output file '/out/transcript.json'\n"
	err := transcribeWith(t, printingTools{wav: makeWAV(16000, 1600), whisperOut: out}.fake(t))
	if err == nil {
		t.Fatal("want an error")
	}
	if strings.Contains(err.Error(), "1234") || strings.Contains(err.Error(), "bank") {
		t.Fatalf("a transcript line reached the error: %v", err)
	}
	if !strings.Contains(err.Error(), "failed to write output file") {
		t.Fatalf("the diagnostic line must survive: %v", err)
	}
}

// Run 1 the same way: a clean exit without audio.wav carries ffmpeg's words.
func TestWhisperLocalSTT_FFmpegCleanExitWithoutWAVCarriesItsOutput(t *testing.T) {
	sb := printingTools{ffmpegOut: "Output file is empty, nothing was encoded"}.fake(t)
	err := transcribeWith(t, sb)
	if err == nil || !strings.Contains(err.Error(), "nothing was encoded") {
		t.Fatalf("the error must carry ffmpeg's output: %v", err)
	}
	if ranWhisper(sb) {
		t.Fatal("whisper-cli must not run without a WAV")
	}
}

// A WAV whisper cannot read is refused before run 2, with its header in hex
// and its size, and whisper-cli is never started.
func TestWhisperLocalSTT_NonRIFFWAVRefusedBeforeWhisper(t *testing.T) {
	junk := []byte("ID3\x04\x00\x00\x00\x00\x00\x00not a wav at all, just bytes")
	sb := printingTools{wav: junk, transcript: helloJSON}.fake(t)
	err := transcribeWith(t, sb)
	if err == nil {
		t.Fatal("want an error")
	}
	if !strings.Contains(err.Error(), "494433040000000000006e6f74206120") { // first 16 bytes, hex
		t.Fatalf("the error must name the header bytes in hex: %v", err)
	}
	if !strings.Contains(err.Error(), "38 bytes") {
		t.Fatalf("the error must name the file size: %v", err)
	}
	if ranWhisper(sb) {
		t.Fatal("whisper-cli must not run on a file that is not a WAV")
	}
}

func TestWhisperLocalSTT_ZeroFrameWAVRefusedBeforeWhisper(t *testing.T) {
	sb := printingTools{wav: makeWAV(16000, 0), transcript: helloJSON}.fake(t)
	err := transcribeWith(t, sb)
	if err == nil || !strings.Contains(err.Error(), "no audio frames") {
		t.Fatalf("a zero-frame WAV must be refused: %v", err)
	}
	if ranWhisper(sb) {
		t.Fatal("whisper-cli must not run on a zero-frame WAV")
	}
}

// A file with no audio stream fails run 1 on the explicit map; that is
// ErrNoAudioStream, so the channel can say so plainly.
func TestWhisperLocalSTT_NoAudioStreamIsErrNoAudioStream(t *testing.T) {
	sb := printingTools{ffmpegErr: sandboxtest.Failed(sandboxtool.FeatureVoiceSTT,
		"Stream map '' matches no streams.\nTo ignore this, add a trailing '?' to the map.")}.fake(t)
	err := transcribeWith(t, sb)
	if !errors.Is(err, ErrNoAudioStream) {
		t.Fatalf("err = %v, want ErrNoAudioStream", err)
	}
	var re *sandboxtool.RunError
	if !errors.As(err, &re) {
		t.Fatalf("the run error must stay wrapped: %v", err)
	}
	// Any other decode failure is not "no audio stream".
	sb = printingTools{ffmpegErr: sandboxtest.Failed(sandboxtool.FeatureVoiceSTT, "Invalid data found when processing input")}.fake(t)
	if err := transcribeWith(t, sb); err == nil || errors.Is(err, ErrNoAudioStream) {
		t.Fatalf("a generic decode failure: %v", err)
	}
}

// Run 1 maps the first audio stream, drops metadata and writes bitexact
// output; input options stay before -i, output options after it, and the
// output path is last.
func TestNormaliseSpec_DeterministicArgsInValidOrder(t *testing.T) {
	args := NormaliseSpec([]byte("x")).Args
	in := slices.Index(args, "-i")
	if in < 0 || args[in+1] != "/in/voice" {
		t.Fatalf("no -i /in/voice: %v", args)
	}
	before, after := args[:in], args[in+2:]
	for _, pair := range [][]string{{"-nostdin"}, {"-loglevel", "error"},
		{"-threads", sandboxtool.FFmpegThreads}, {"-filter_threads", sandboxtool.FFmpegThreads}} {
		if !sliceContains(before, pair...) {
			t.Errorf("input option %v must precede -i: %v", pair, args)
		}
	}
	for _, pair := range [][]string{{"-map", "0:a:0"}, {"-map_metadata", "-1"},
		{"-fflags", "+bitexact"}, {"-flags:a", "+bitexact"},
		{"-ac", "1"}, {"-ar", "16000"}, {"-acodec", "pcm_s16le"}, {"-f", "wav"}} {
		if !sliceContains(after, pair...) {
			t.Errorf("output option %v must follow -i: %v", pair, args)
		}
		if sliceContains(before, pair...) {
			t.Errorf("output option %v must not precede -i: %v", pair, args)
		}
	}
	if args[len(args)-1] != "/out/audio.wav" {
		t.Errorf("the output path must be last: %v", args)
	}
}

// CheckWAV is the one definition of a WAV run 2 (and the doctor) accepts.
func TestCheckWAV(t *testing.T) {
	canonical := makeWAV(16000, 10)
	withList := insertChunk(canonical, "LIST", []byte("INFOISFT\x0e\x00\x00\x00Lavf61.7.100\x00\x00"))
	oddChunk := insertChunk(canonical, "junk", []byte{1, 2, 3}) // padded to even
	streaming := append([]byte(nil), canonical...)
	copy(streaming[40:44], []byte{0xff, 0xff, 0xff, 0xff})
	truncated := canonical[:44+1] // header says 20 bytes, one byte present
	zeroAlign := append([]byte(nil), canonical...)
	copy(zeroAlign[32:34], []byte{0, 0}) // fmt present, block align 0 (review e6a7 F3)
	dataFirst := append(append([]byte("RIFF\x00\x00\x00\x00WAVE"), []byte("data\x02\x00\x00\x00")...), 0, 0)
	cases := map[string]struct {
		wav  []byte
		want string // "" = accepted
	}{
		"canonical":              {canonical, ""},
		"LIST before data":       {withList, ""},
		"odd chunk padded":       {oddChunk, ""},
		"streaming data size":    {streaming, ""},
		"zero frames":            {makeWAV(16000, 0), "no audio frames"},
		"truncated to 1 byte":    {truncated, "no audio frames"},
		"not RIFF":               {[]byte("OggS\x00\x02\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00junk"), "not a RIFF/WAVE"},
		"RIFF but not WAVE":      {[]byte("RIFF\x10\x00\x00\x00AVI LIST"), "not a RIFF/WAVE"},
		"empty":                  {[]byte{}, "not a RIFF/WAVE"},
		"no data chunk":          {canonical[:36], "no data chunk"},
		"data before fmt":        {dataFirst, "no fmt chunk"},
		"fmt with block align 0": {zeroAlign, "fmt chunk has a zero block align"},
		"chunk size overflows":   {append([]byte("RIFF\x00\x00\x00\x00WAVELIST\xff\xff\xff\x7f"), make([]byte, 8)...), "no data chunk"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "audio.wav")
			if err := os.WriteFile(p, c.wav, 0o600); err != nil {
				t.Fatal(err)
			}
			err := CheckWAV(p)
			if c.want == "" {
				if err != nil {
					t.Fatalf("want accepted: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
		})
	}
	if err := CheckWAV(filepath.Join(t.TempDir(), "absent.wav")); err == nil {
		t.Fatal("a missing file must be refused")
	}
}

// insertChunk puts a chunk between the fmt and data chunks of a canonical
// 44-byte-header WAV.
func insertChunk(wav []byte, id string, body []byte) []byte {
	out := append([]byte(nil), wav[:36]...)
	out = append(out, id...)
	out = appendU32LE(out, uint32(len(body)))
	out = append(out, body...)
	if len(body)%2 == 1 {
		out = append(out, 0)
	}
	return append(out, wav[36:]...)
}
