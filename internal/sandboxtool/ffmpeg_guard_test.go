package sandboxtool

import (
	"strings"
	"testing"
)

// audit 2026-10-12 (GitHub #348): ffmpeg's playlist and manifest demuxers open
// files and URLs named inside the input; a crafted DASH manifest made the video
// extractor's runs read a local file into their output. The guard limits every
// ffmpeg/ffprobe input to the file protocol and an allowlist of demuxers.
func TestFFmpegInputGuard_FileProtocolAndDemuxerAllowlist(t *testing.T) {
	got := strings.Join(FFmpegInputGuard(), " ")
	if got != "-protocol_whitelist file -format_whitelist "+FFmpegInputDemuxers {
		t.Fatalf("guard = %q", got)
	}
	// The allowlist is exactly the measured set (process-spawn-law design,
	// "Change 2026-10-12"): adding a demuxer is a design change.
	want := "mov,matroska,ogg,mp3,wav,flac,aac,avi,asf,mpegts,mpeg,flv,aiff,amr,caf"
	if FFmpegInputDemuxers != want {
		t.Fatalf("FFmpegInputDemuxers = %q, want %q", FFmpegInputDemuxers, want)
	}
	// None of the demuxers that open nested files or URLs, nor the
	// text-to-video ones, may ever be on it.
	allowed := map[string]bool{}
	for _, d := range strings.Split(FFmpegInputDemuxers, ",") {
		allowed[d] = true
	}
	for _, d := range []string{"hls", "dash", "concat", "imf", "sdp", "rtsp", "rtp", "image2", "tty", "bin", "xbin", "ffmetadata", "data", "webvtt"} {
		if allowed[d] {
			t.Errorf("demuxer %q must not be allowed", d)
		}
	}
	// A fresh slice per call: a caller appending to it cannot change the next.
	a := FFmpegInputGuard()
	a[1] = "http"
	if FFmpegInputGuard()[1] != "file" {
		t.Fatal("FFmpegInputGuard shares its backing array")
	}
}
