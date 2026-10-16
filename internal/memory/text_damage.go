package memory

import (
	"errors"
	"fmt"
	"unicode/utf8"
)

// ErrDamagedText is returned (wrapped) when text handed to the Indexer is not
// valid UTF-8 or carries a C0 control byte other than TAB, LF, CR and FF.
// Nothing is stored. GitHub #389 / memory-ingest-paths.md §8.2.
var ErrDamagedText = errors.New("damaged text refused from memory")

// TextDamage is what ScanTextDamage found in one text.
type TextDamage struct {
	// Controls counts C0 control bytes other than TAB (0x09), LF (0x0A),
	// FF (0x0C) and CR (0x0D). NUL counts.
	Controls int
	// InvalidUTF8 counts byte positions where no valid UTF-8 sequence starts
	// (utf8.DecodeRuneInString returning RuneError with width 1).
	InvalidUTF8 int
	// FirstOffset is the byte offset of the first damaged byte, -1 when none.
	FirstOffset int
}

// Damaged reports whether any damage was found.
func (d TextDamage) Damaged() bool { return d.Controls > 0 || d.InvalidUTF8 > 0 }

// Err returns nil for undamaged text, else an error wrapping ErrDamagedText
// that names the source, the first offset and the counts.
func (d TextDamage) Err(source string) error {
	if !d.Damaged() {
		return nil
	}
	return fmt.Errorf("%w: %q has %d control byte(s) and %d invalid UTF-8 byte(s), first at offset %d (likely a damaged upload, e.g. base64 written by a model)",
		ErrDamagedText, source, d.Controls, d.InvalidUTF8, d.FirstOffset)
}

// ScanTextDamage reports the damage in s: invalid UTF-8, or a C0 control
// other than TAB, LF, CR and FF.
//
// FF is allowed because pdftotext emits it as the page break and plain-text
// files use it. U+FFFD is NOT damage: it is a legitimate character, and the
// Indexer runs this on the text BEFORE the chunker repairs invalid UTF-8 to
// U+FFFD, which is the only point where that evidence still exists.
// Measured on the production corpus 2026-10-16: 3 of 86,007 chunks carry such
// a control byte, all three from damaged uploads; none carries ESC or FF alone.
func ScanTextDamage(s string) TextDamage {
	d := TextDamage{FirstOffset: -1}
	for i := 0; i < len(s); {
		b := s[i]
		if b < utf8.RuneSelf {
			if b < 0x20 && b != '\t' && b != '\n' && b != '\r' && b != '\f' {
				d.Controls++
				if d.FirstOffset < 0 {
					d.FirstOffset = i
				}
			}
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(s[i:])
		if r == utf8.RuneError && size == 1 {
			d.InvalidUTF8++
			if d.FirstOffset < 0 {
				d.FirstOffset = i
			}
		}
		i += size
	}
	return d
}

// TextDamageGate quarantines a candidate whose content is damaged text, so a
// refusal on the gated pipeline path lands in project_memory_quarantine where
// operators look, instead of surfacing as an Indexer error.
func TextDamageGate(c *IngestCandidate) GateOutcome {
	d := ScanTextDamage(c.Content)
	if !d.Damaged() {
		return GateOutcome{Action: GateAllow, Gate: GateTextDamage}
	}
	return GateOutcome{
		Action: GateQuarantine,
		Gate:   GateTextDamage,
		Detail: fmt.Sprintf("%d control byte(s) and %d invalid UTF-8 byte(s), first at offset %d", d.Controls, d.InvalidUTF8, d.FirstOffset),
	}
}
