package voice

import (
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"
	"unicode/utf8"
)

// CheckWAV reports whether path is a WAV whisper.cpp can read: a RIFF/WAVE
// header, a "fmt " chunk with a non-zero block align, and after it a
// "data" chunk holding at least one whole frame. The data size is the
// declared size clipped to the bytes present; a streaming size
// (0xFFFFFFFF) counts as what is present.
//
// It is the one definition of a usable WAV: the STT pipeline runs it
// before whisper-cli, and `vornikctl doctor`'s decode probe runs it on
// ffmpeg's output (voice-messages-design.md §8). A refusal names the
// first 16 header bytes in hex and the file size; it reads only chunk
// headers, never samples.
func CheckWAV(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("voice: WAV unreadable: %w", err)
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return fmt.Errorf("voice: WAV unreadable: %w", err)
	}
	size := st.Size()
	head := make([]byte, 16)
	n, _ := io.ReadFull(f, head)
	head = head[:n]
	refuse := func(why string) error {
		return fmt.Errorf("voice: WAV refused before whisper: %s (header %s, %d bytes)", why, hex.EncodeToString(head), size)
	}
	if n < 12 || string(head[0:4]) != "RIFF" || string(head[8:12]) != "WAVE" {
		return refuse("not a RIFF/WAVE file")
	}
	var blockAlign uint16
	sawFmt := false
	hdr := make([]byte, 8)
	for off := int64(12); off+8 <= size; {
		if _, err := f.ReadAt(hdr, off); err != nil {
			return refuse("unreadable chunk header")
		}
		id := string(hdr[0:4])
		declared := int64(binary.LittleEndian.Uint32(hdr[4:8]))
		body := off + 8
		switch id {
		case "fmt ":
			if declared < 16 || body+16 > size {
				return refuse("short fmt chunk")
			}
			fmtBody := make([]byte, 16)
			if _, err := f.ReadAt(fmtBody, body); err != nil {
				return refuse("unreadable fmt chunk")
			}
			blockAlign = binary.LittleEndian.Uint16(fmtBody[12:14])
			sawFmt = true
		case "data":
			if !sawFmt {
				return refuse("no fmt chunk before the data chunk")
			}
			if blockAlign == 0 {
				return refuse("the fmt chunk has a zero block align")
			}
			present := size - body
			if declared != 0xFFFFFFFF && declared < present {
				present = declared
			}
			if present < int64(blockAlign) {
				return refuse(fmt.Sprintf("no audio frames (data chunk %d bytes)", present))
			}
			return nil
		}
		off = body + declared + declared%2 // chunks are padded to even sizes
	}
	return refuse("no data chunk")
}

// ErrNoAudioStream reports an inbound file with no audio stream ffmpeg can
// map (`-map 0:a:0`). Channels answer it with "that file has no audio",
// not "try again" (voice-messages-design.md §8).
var ErrNoAudioStream = errors.New("voice: the file has no audio stream")

// noAudioStreamMarker is ffmpeg's message when `-map 0:a:0` matches nothing.
// It is ffmpeg's wording (7.x), not an API: re-check it when the agent
// image's ffmpeg is upgraded, or the no-audio reply degrades to the generic
// one (review e6a7 F2).
const noAudioStreamMarker = "matches no streams"

// toolOutputTail bounds how much of a tool's output an error carries.
const toolOutputTail = 500

// transcriptLine is a whisper.cpp result line, "[00:00:00.000 --> 00:00:02.000]  text".
// whisper prints these to stdout even with -np; they carry the user's speech.
// This is whisper.cpp's current format: a whisper upgrade that prints speech
// any other way would reach the error text, so re-check this when the image's
// whisper-cli changes (review e6a7 F4).
var transcriptLine = regexp.MustCompile(`^\s*\[\d+:\d\d:\d\d\.\d{3} --> \d+:\d\d:\d\d\.\d{3}\]`)

// toolTail is the last toolOutputTail bytes of a tool's output, with
// whisper's transcript lines dropped first, quoted so control bytes cannot
// forge log lines. Empty output reads as "(none)".
func toolTail(out []byte) string {
	lines := strings.Split(string(out), "\n")
	kept := lines[:0]
	for _, l := range lines {
		if !transcriptLine.MatchString(l) {
			kept = append(kept, l)
		}
	}
	s := strings.TrimSpace(strings.Join(kept, "\n"))
	if s == "" {
		return "(none)"
	}
	if len(s) > toolOutputTail {
		s = s[len(s)-toolOutputTail:]
		for len(s) > 0 && !utf8.RuneStart(s[0]) {
			s = s[1:]
		}
		s = "…" + s
	}
	return fmt.Sprintf("%q", s)
}
