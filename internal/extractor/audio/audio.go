// Package audio implements the audio extractor for the document
// pipeline. See https://docs.vornik.io
// §11 (Phase 4).
//
// Approach: whisper.cpp's whisper-cli, run in the pinned agent image
// through the sandbox runner (process-spawn law S5b,
// https://docs.vornik.io §7) — the
// engine voice speech-to-text already uses. An uploaded recording is
// untrusted input, so it is decoded in a network-less, memory-bounded
// one-shot, never on the daemon host. Two runs, both feature "audio":
// ffmpeg normalises any container/codec to 16 kHz mono PCM WAV, then
// whisper-cli transcribes it with its --output-json envelope.
//
// The model is a ggml .bin file the operator configures
// (extractors.audio.model_path, falling back to voice.stt.model); its
// DIRECTORY is mounted read-only at /models. With neither set, audio
// extraction reports "not available" (design §7.1 decision 3).
//
// Behaviour change (design §7.1 decision 2, release 2026.9.7): this
// extractor used to run OpenAI's Python whisper CLI on the host with its
// "turbo" PyTorch model, auto-downloaded. That path is removed. The model
// family and format change with it — ggml models, not .pt — and quality
// and language coverage follow the model the operator picks.
//
// One section per whisper segment, timestamped from the segment's
// millisecond offsets so retrieval can cite "from the recording at
// 00:43:21".
package audio

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"vornik.io/vornik/internal/extractor"
	"vornik.io/vornik/internal/sandboxtool"
)

const (
	Name    = "vornik-extract-audio"
	Version = "0.2.0"

	// maxAudioSegments caps the per-document segment count. A
	// 24-hour audiobook at 30s per segment would produce ~2880
	// segments — manageable but the chunker downstream would
	// also have to ingest that many. 10000 is a generous cap;
	// real-world podcasts / meetings sit well below.
	maxAudioSegments = 10000

	// minSegmentChars filters out whisper's empty/near-empty
	// segments (silence detection noise). 4 chars covers single
	// utterances like "um" / "yes" while dropping pure-whitespace
	// segments.
	minSegmentChars = 4

	// noSpeechRejectThreshold is whisper's recommended cutoff for
	// "this segment is probably not speech". Above this, the
	// transcript is unreliable enough that including it pollutes
	// memory_search hits. whisper.cpp does not report it today; a
	// segment that carries it is still filtered.
	noSpeechRejectThreshold = 0.6
)

// New returns an audio extractor that runs ffmpeg and whisper-cli through
// sb with the ggml model at modelPath. A nil sb, an empty modelPath, or a
// model file that does not exist makes every extraction "not available".
func New(sb sandboxtool.Sandbox, modelPath string) *Extractor {
	return &Extractor{sandbox: sb, modelPath: strings.TrimSpace(modelPath)}
}

// Extractor implements extractor.Extractor for audio files via
// whisper.cpp in the sandbox. Stateless across calls.
type Extractor struct {
	sandbox   sandboxtool.Sandbox
	modelPath string
}

func (*Extractor) Name() string    { return Name }
func (*Extractor) Version() string { return Version }

// Extract normalises src.FilePath with ffmpeg and transcribes it with
// whisper-cli, both in the sandbox, then parses the JSON transcript into
// sections. Each whisper segment becomes one extractor.Section; the
// OutlineEntry carries the segment's timestamp.
func (e *Extractor) Extract(ctx context.Context, src extractor.Source) (extractor.Result, error) {
	if src.FilePath == "" {
		return extractor.Result{}, fmt.Errorf("audio: source file path is empty")
	}
	if e.modelPath == "" {
		return extractor.Result{}, fmt.Errorf("audio: %w: no whisper model configured "+
			"(set extractors.audio.model_path, or voice.stt.model, to a ggml .bin file)", sandboxtool.ErrNotAvailable)
	}
	if st, err := os.Stat(e.modelPath); err != nil || st.IsDir() {
		return extractor.Result{}, fmt.Errorf("audio: %w: whisper model %s is not a readable file "+
			"(check extractors.audio.model_path or voice.stt.model)", sandboxtool.ErrNotAvailable, e.modelPath)
	}
	if e.sandbox == nil {
		return extractor.Result{}, fmt.Errorf("audio: whisper-cli: %w (no sandbox runner)", sandboxtool.ErrNotAvailable)
	}

	// Run 1: any container/codec → 16 kHz mono 16-bit PCM WAV, the input
	// whisper.cpp is built for.
	norm, err := e.sandbox.Run(ctx, sandboxtool.Spec{
		Feature:    sandboxtool.FeatureAudio,
		Entrypoint: "ffmpeg",
		Args: []string{"-nostdin", "-loglevel", "error", "-threads", sandboxtool.FFmpegThreads, "-filter_threads", sandboxtool.FFmpegThreads,
			"-i", "/in/audio", "-threads", sandboxtool.FFmpegThreads, "-ac", "1", "-ar", "16000", "-acodec", "pcm_s16le", "-f", "wav", "/out/audio.wav"},
		Inputs: []sandboxtool.Input{{Name: "audio", Path: src.FilePath}},
	})
	if err != nil {
		return extractor.Result{}, fmt.Errorf("audio: ffmpeg normalise: %w", err)
	}
	defer norm.Close()

	// Run 2: whisper-cli. -l auto detects the language (the Python CLI's
	// behaviour; an English-only model ignores it). -oj -of writes
	// /out/transcript.json; -np keeps the combined output a diagnostic.
	res, err := e.sandbox.Run(ctx, sandboxtool.Spec{
		Feature:    sandboxtool.FeatureAudio,
		Entrypoint: "whisper-cli",
		Args: []string{"-m", "/models/" + filepath.Base(e.modelPath), "-f", "/in/audio.wav",
			"-l", "auto", "-oj", "-of", "/out/transcript", "-np"},
		Inputs:   []sandboxtool.Input{{Name: "audio.wav", Path: filepath.Join(norm.OutDir, "audio.wav")}},
		ModelDir: filepath.Dir(e.modelPath),
	})
	if err != nil {
		return extractor.Result{}, fmt.Errorf("audio: whisper-cli: %w", err)
	}
	defer res.Close()
	raw, err := os.ReadFile(filepath.Join(res.OutDir, "transcript.json"))
	if err != nil {
		return extractor.Result{}, fmt.Errorf("audio: whisper-cli wrote no transcript: %w", err)
	}

	segments, language, err := parseWhisperCppJSON(raw)
	if err != nil {
		return extractor.Result{}, err
	}
	if len(segments) > maxAudioSegments {
		return extractor.Result{}, fmt.Errorf("audio: %d segments exceeds cap %d", len(segments), maxAudioSegments)
	}

	sections, outline := buildSections(segments)
	if len(sections) == 0 {
		return extractor.Result{}, ErrNoSpeech
	}

	metadata := extractor.Metadata{
		Title:    titleFromSource(src),
		Language: language,
	}
	if d := durationFromSegments(segments); d > 0 {
		metadata.DurationSeconds = int(d)
	}

	return extractor.Result{
		Metadata: metadata,
		Outline:  outline,
		Sections: sections,
	}, nil
}

// ErrNoSpeech is returned when whisper produced zero usable
// segments — typically a silent file or one where every segment
// is a non-speech marker. Callers may route to re-extraction with
// a different model or surface "no speech detected" to the operator.
var ErrNoSpeech = errors.New("audio: no speech segments above the quality threshold")

// whisperCppOutput is the subset of whisper-cli's --output-json envelope
// we read.
type whisperCppOutput struct {
	Result struct {
		Language string `json:"language"`
	} `json:"result"`
	Transcription []struct {
		Offsets struct {
			From int64 `json:"from"`
			To   int64 `json:"to"`
		} `json:"offsets"`
		Text         string   `json:"text"`
		NoSpeechProb *float64 `json:"no_speech_prob,omitempty"`
	} `json:"transcription"`
}

// parseWhisperCppJSON folds whisper-cli's JSON into segments, with the
// millisecond offsets turned into seconds.
func parseWhisperCppJSON(raw []byte) ([]whisperSegment, string, error) {
	var out whisperCppOutput
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, "", fmt.Errorf("audio: parse whisper JSON: %w", err)
	}
	segments := make([]whisperSegment, 0, len(out.Transcription))
	for i, t := range out.Transcription {
		seg := whisperSegment{
			ID:    i,
			Start: float64(t.Offsets.From) / 1000,
			End:   float64(t.Offsets.To) / 1000,
			Text:  t.Text,
		}
		if t.NoSpeechProb != nil {
			seg.NoSpeechProb = *t.NoSpeechProb
		}
		segments = append(segments, seg)
	}
	return segments, strings.TrimSpace(out.Result.Language), nil
}

// whisperSegment carries the per-segment metadata the section builder
// reads.
type whisperSegment struct {
	ID           int
	Start        float64
	End          float64
	Text         string
	NoSpeechProb float64
}

// nonSpeechMarker reports whisper.cpp's bracketed non-speech tokens
// ("[BLANK_AUDIO]", "(music)"), which are not transcript text.
func nonSpeechMarker(text string) bool {
	return (strings.HasPrefix(text, "[") && strings.HasSuffix(text, "]")) ||
		(strings.HasPrefix(text, "(") && strings.HasSuffix(text, ")"))
}

// buildSections turns whisper segments into the extractor's
// section + outline shape. Filters out segments that fail the
// quality bar (empty text, non-speech markers, dominated by
// non-speech) so the indexed text stays clean.
func buildSections(segments []whisperSegment) ([]extractor.Section, []extractor.OutlineEntry) {
	sections := make([]extractor.Section, 0, len(segments))
	outline := make([]extractor.OutlineEntry, 0, len(segments))
	for i, seg := range segments {
		text := strings.TrimSpace(seg.Text)
		if len(text) < minSegmentChars || nonSpeechMarker(text) {
			continue
		}
		if seg.NoSpeechProb > noSpeechRejectThreshold {
			continue
		}
		sectionID := fmt.Sprintf("segment-%04d", i+1)
		title := formatTimestampRange(seg.Start, seg.End)
		sections = append(sections, extractor.Section{
			SectionID: sectionID,
			Title:     title,
			Content:   text,
		})
		outline = append(outline, extractor.OutlineEntry{
			SectionID:         sectionID,
			Title:             title,
			Depth:             0,
			TimestampStartSec: int(seg.Start),
			TextBytes:         len(text),
		})
	}
	return sections, outline
}

// durationFromSegments returns the end-timestamp of the last
// segment as the document's total duration. Whisper's JSON
// doesn't carry an explicit "duration" field at the top level,
// but segment.End on the last segment is reliably the total.
func durationFromSegments(segments []whisperSegment) float64 {
	if len(segments) == 0 {
		return 0
	}
	return segments[len(segments)-1].End
}

// formatTimestampRange renders a section title like "00:00:12 —
// 00:00:42". Operator-readable + memory-search-friendly: a query
// for "what was said around 12 minutes" can land on a section
// whose title carries the exact second.
func formatTimestampRange(startSec, endSec float64) string {
	return fmt.Sprintf("%s — %s", formatTimestamp(startSec), formatTimestamp(endSec))
}

func formatTimestamp(sec float64) string {
	if sec < 0 {
		sec = 0
	}
	total := int(sec)
	h := total / 3600
	m := (total % 3600) / 60
	s := total % 60
	return fmt.Sprintf("%02d:%02d:%02d", h, m, s)
}

// titleFromSource derives the document title from the original
// filename, stripping the extension. Whisper doesn't expose
// embedded ID3 / metadata; pulling those would require a
// separate ffprobe pass we don't want in the v1 path.
func titleFromSource(src extractor.Source) string {
	name := src.OriginalName
	if name == "" && src.FilePath != "" {
		name = filepath.Base(src.FilePath)
	}
	if name == "" {
		return ""
	}
	if i := strings.LastIndex(name, "."); i > 0 {
		name = name[:i]
	}
	return strings.TrimSpace(name)
}
