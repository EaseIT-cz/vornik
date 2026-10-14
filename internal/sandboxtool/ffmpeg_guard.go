package sandboxtool

// FFmpegInputDemuxers is the allowlist of demuxers an ffmpeg or ffprobe run
// may use on its input (GitHub #348; process-spawn-law design, "Change
// 2026-10-12"). The input's format is not known in advance (an attachment
// arrives as any container, and its MIME type is the sender's claim), so the
// demuxer is pinned to a set rather than with -f.
//
// Each name is a container that opens nothing named inside it: mov (mp4, m4a,
// mov, 3gp; external data references are off by default), matroska (mkv,
// webm), ogg, mp3, wav, flac, aac (ADTS), avi, asf (wma, wmv), mpegts, mpeg
// (PS, vob), flv, aiff, amr, caf. The playlist and manifest demuxers (hls,
// dash, concat, imf, sdp, rtsp) and the text-to-video ones (tty, bin, xbin)
// are what this keeps out: a crafted DASH manifest read a local file into the
// video extractor's frames and probe output, and made requests where a network
// existed (measured 2026-10-12 against the agent image's ffmpeg 6.1.1).
//
// Adding a demuxer is a design change: check it opens no nested URL first.
const FFmpegInputDemuxers = "mov,matroska,ogg,mp3,wav,flac,aac,avi,asf,mpegts,mpeg,flv,aiff,amr,caf"

// FFmpegInputGuard returns the input options every ffmpeg and ffprobe run
// places immediately before its input: the file protocol only (the input is
// always a file under /in, so a nested http, tcp, concat, subfile or data open
// is refused), and the demuxer allowlist above. ffmpeg copies both whitelists
// into any nested demuxer it opens. The runs also have no network
// (--network=none, §7.2); the guard does not depend on that, nor that on it.
func FFmpegInputGuard() []string {
	return []string{"-protocol_whitelist", "file", "-format_whitelist", FFmpegInputDemuxers}
}
