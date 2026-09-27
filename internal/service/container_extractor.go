// Document-extraction pipeline wiring. See
// https://docs.vornik.io
//
// Builds:
//   - extractor.Registry with the bundled-default extractors registered
//   - extractor.Runner with the artifact-store base path + DB repo
//
// Both surfaces are nil-safe at the consumer (api.Server). Construction
// happens lazily on first access so the extractor list stays accurate
// after future config-reload work — for the slice today, lazy = init
// once on first lookup, no reload story.
package service

import (
	"sync"

	"vornik.io/vornik/internal/extractor"
	"vornik.io/vornik/internal/extractor/audio"
	"vornik.io/vornik/internal/extractor/epub"
	htmlx "vornik.io/vornik/internal/extractor/html"
	imagex "vornik.io/vornik/internal/extractor/image"
	"vornik.io/vornik/internal/extractor/pdf"
	"vornik.io/vornik/internal/extractor/textfile"
	videox "vornik.io/vornik/internal/extractor/video"
)

// extractorPipeline is a thin lazy-init holder. The fields are
// populated on first call to ExtractorRegistry / ExtractorRunner;
// initOnce ensures the construction runs exactly once even under
// concurrent api.Server boot + UI boot races.
type extractorPipeline struct {
	initOnce sync.Once
	registry *extractor.Registry
	runner   *extractor.Runner
}

// ExtractorRegistry returns the daemon's MIME-keyed extractor
// registry, constructing it on first call. Returns nil only when
// the artifact-store base path isn't configured — extraction
// requires somewhere to write extracted sections.
func (c *Container) ExtractorRegistry() *extractor.Registry {
	c.extractorPipeline.initOnce.Do(c.initExtractorPipeline)
	return c.extractorPipeline.registry
}

// ExtractorRunner returns the Runner shared by every extraction
// trigger (HTTP endpoint today; future workflow steps + email
// channel auto-trigger). Nil when ExtractorRegistry is nil.
func (c *Container) ExtractorRunner() *extractor.Runner {
	c.extractorPipeline.initOnce.Do(c.initExtractorPipeline)
	return c.extractorPipeline.runner
}

func (c *Container) initExtractorPipeline() {
	if c == nil || c.Config == nil {
		return
	}
	basePath := c.Config.Storage.ArtifactsPath
	if basePath == "" {
		c.Logger.Warn().Msg("extractor: ArtifactsPath unset — extraction disabled")
		return
	}
	if c.repos == nil || c.repos.ExtractedDocuments == nil {
		c.Logger.Warn().Msg("extractor: ExtractedDocuments repo unwired — extraction disabled")
		return
	}

	reg := extractor.NewRegistry()
	// IANA registers application/epub+zip; some senders (Gmail
	// included) strip the +zip and ship application/epub. Register
	// both so the email channel's verbatim Content-Type
	// pass-through lands on the right extractor regardless.
	if err := reg.Register(epub.New(), "application/epub+zip", "application/epub"); err != nil {
		c.Logger.Error().Err(err).Msg("extractor: failed to register EPUB extractor")
		return
	}
	// PDF, audio, image OCR and video run their tools (pdftotext,
	// ffmpeg + whisper-cli, tesseract, ffprobe + ffmpeg) in the pinned
	// agent image through the sandbox runner (process-spawn law S5b,
	// https://docs.vornik.io §7),
	// never on the daemon host. They register unconditionally: a missing
	// runner, image, tool or model reports "not available" at Extract
	// time, and initSandboxTools logs at boot which tools the image
	// declares.
	sb := c.sandbox()
	if err := reg.Register(pdf.New(sb), "application/pdf"); err != nil {
		c.Logger.Error().Err(err).Msg("extractor: failed to register PDF extractor")
		return
	}
	// HTML — pure Go. Some senders advertise application/xhtml+xml
	// for the same content shape; cover both.
	if err := reg.Register(htmlx.New(), "text/html", "application/xhtml+xml"); err != nil {
		c.Logger.Error().Err(err).Msg("extractor: failed to register HTML extractor")
		return
	}
	// Plain text + markdown — pure Go. Markdown variants in the
	// wild use either text/markdown or text/x-markdown; register
	// both bare-name + text/* style. text/plain is the most common
	// inbound emailed-notes shape.
	if err := reg.Register(textfile.New(),
		"text/plain", "text/markdown", "text/x-markdown"); err != nil {
		c.Logger.Error().Err(err).Msg("extractor: failed to register text extractor")
		return
	}
	// Audio — ffmpeg normalises, whisper-cli (whisper.cpp) transcribes
	// with the ggml model from extractors.audio.model_path, falling back
	// to voice.stt.model (design §7.1 decisions 2 and 3). audio/* so any
	// inbound audio MIME flows to the same extractor.
	if err := reg.Register(audio.New(sb, c.Config.AudioExtractionModel()), "audio/*"); err != nil {
		c.Logger.Error().Err(err).Msg("extractor: failed to register audio extractor")
		return
	}
	// Images — pure-Go header decode for dimensions + tesseract OCR in
	// the sandbox (graceful degradation when not available). image/* covers jpeg/png/gif/webp at the
	// dispatch layer; the stdlib decoder handles png/jpeg/gif
	// natively. Other image variants degrade to a metadata-only
	// section via the same error path.
	if err := reg.Register(imagex.New(sb), "image/*"); err != nil {
		c.Logger.Error().Err(err).Msg("extractor: failed to register image extractor")
		return
	}
	// Video — ffprobe metadata + uniform-interval keyframe sampling via
	// ffmpeg, both in the sandbox. Registered unconditionally like PDF
	// and audio: an image without them reports "not available" at
	// Extract time rather than leaving a dispatch-time gap the operator
	// cannot see.
	//
	// see LLD § https://docs.vornik.io §4.6
	videoOpts := videox.NewWithOptions(
		sb,
		c.Config.Media.Video.MaxFrames,
		c.Config.Media.Video.MinIntervalSeconds,
	)
	if err := reg.Register(videoOpts, "video/*"); err != nil {
		c.Logger.Error().Err(err).Msg("extractor: failed to register video extractor")
		return
	}
	c.Logger.Info().
		Strs("mime_types", reg.SupportedMimeTypes()).
		Msg("extractor: registry constructed")

	c.extractorPipeline.registry = reg
	c.extractorPipeline.runner = &extractor.Runner{
		Repo:     c.repos.ExtractedDocuments,
		BasePath: basePath,
		// nil until wireComponentMetrics runs at boot; the Runner is
		// nil-safe so an early lazy-init simply doesn't emit.
		Metrics: c.extractorMetrics,
	}
}
