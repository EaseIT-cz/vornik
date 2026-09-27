package probe

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"math"
	"strings"
)

// Fixtures are generated, never committed: a one-page PDF, a PNG of a
// rendered word, and a sine WAV are each a few hundred bytes to a few tens of
// KiB, and generating them keeps binaries out of the repository.

// PDFText is the text the PDF fixture carries; OCRWord is the word the PNG
// fixture renders.
const (
	PDFText = "Vornik doctor probe"
	OCRWord = "VORNIK"
)

// PDF returns a valid one-page PDF 1.4 whose only content is text, in
// Helvetica, with a correct cross-reference table.
func PDF(text string) []byte {
	escaped := strings.NewReplacer(`\`, `\\`, "(", `\(`, ")", `\)`).Replace(text)
	content := fmt.Sprintf("BT /F1 24 Tf 72 700 Td (%s) Tj ET", escaped)
	objects := []string{
		"<< /Type /Catalog /Pages 2 0 R >>",
		"<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
		"<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
		fmt.Sprintf("<< /Length %d >>\nstream\n%s\nendstream", len(content), content),
		"<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
	}
	var b bytes.Buffer
	b.WriteString("%PDF-1.4\n")
	offsets := make([]int, len(objects))
	for i, obj := range objects {
		offsets[i] = b.Len()
		fmt.Fprintf(&b, "%d 0 obj\n%s\nendobj\n", i+1, obj)
	}
	xref := b.Len()
	fmt.Fprintf(&b, "xref\n0 %d\n0000000000 65535 f \n", len(objects)+1)
	for _, off := range offsets {
		fmt.Fprintf(&b, "%010d 00000 n \n", off)
	}
	fmt.Fprintf(&b, "trailer\n<< /Size %d /Root 1 0 R >>\nstartxref\n%d\n%%%%EOF\n", len(objects)+1, xref)
	return b.Bytes()
}

// glyphs draws the letters OCRWord needs as strokes on a 10x14 grid: a
// stroked outline reads to tesseract as type, where a blocky bitmap font
// does not.
var glyphs = map[rune][][2]point{
	'V': {{{0, 0}, {5, 14}}, {{5, 14}, {10, 0}}},
	'O': ellipse(5, 7, 5, 7, 20),
	'R': {{{0, 0}, {0, 14}}, {{0, 0}, {7, 0}}, {{7, 0}, {9.5, 2}}, {{9.5, 2}, {9.5, 5}}, {{9.5, 5}, {7, 7}}, {{7, 7}, {0, 7}}, {{4, 7}, {10, 14}}},
	'N': {{{0, 14}, {0, 0}}, {{0, 0}, {10, 14}}, {{10, 14}, {10, 0}}},
	'I': {{{5, 0}, {5, 14}}, {{2, 0}, {8, 0}}, {{2, 14}, {8, 14}}},
	'K': {{{0, 0}, {0, 14}}, {{10, 0}, {0, 9}}, {{3, 6.5}, {10, 14}}},
}

type point struct{ x, y float64 }

func ellipse(cx, cy, rx, ry float64, n int) [][2]point {
	segs := make([][2]point, 0, n)
	for i := 0; i < n; i++ {
		a0 := 2 * math.Pi * float64(i) / float64(n)
		a1 := 2 * math.Pi * float64(i+1) / float64(n)
		segs = append(segs, [2]point{
			{cx + rx*math.Cos(a0), cy + ry*math.Sin(a0)},
			{cx + rx*math.Cos(a1), cy + ry*math.Sin(a1)},
		})
	}
	return segs
}

// TextPNG renders word (letters of OCRWord only) black on white: each glyph
// stroked on a 10x14 grid at scale pixels per unit, with the letter spacing
// and margin tesseract needs to find the line.
func TextPNG(word string, scale int) []byte {
	const advance, margin, stroke = 15.0, 6.0, 1.0 // grid units
	letters := []rune(word)
	w := int((float64(len(letters))*advance + 2*margin) * float64(scale))
	h := int((14 + 2*margin) * float64(scale))
	img := image.NewGray(image.Rect(0, 0, w, h))
	for py := 0; py < h; py++ {
		for px := 0; px < w; px++ {
			x, y := float64(px)/float64(scale)-margin, float64(py)/float64(scale)-margin
			ink := false
			if i := int(math.Floor(x / advance)); i >= 0 && i < len(letters) {
				gx := x - float64(i)*advance
				for _, seg := range glyphs[letters[i]] {
					if segmentDistance(point{gx, y}, seg[0], seg[1]) <= stroke {
						ink = true
						break
					}
				}
			}
			if ink {
				img.SetGray(px, py, color.Gray{})
			} else {
				img.SetGray(px, py, color.Gray{Y: 0xff})
			}
		}
	}
	var b bytes.Buffer
	_ = png.Encode(&b, img)
	return b.Bytes()
}

// segmentDistance is the distance from p to the segment ab.
func segmentDistance(p, a, b point) float64 {
	dx, dy := b.x-a.x, b.y-a.y
	t := 0.0
	if l := dx*dx + dy*dy; l > 0 {
		t = math.Max(0, math.Min(1, ((p.x-a.x)*dx+(p.y-a.y)*dy)/l))
	}
	return math.Hypot(p.x-(a.x+t*dx), p.y-(a.y+t*dy))
}

// SineWAV returns a mono 16-bit PCM WAV of a 440 Hz tone.
func SineWAV(seconds float64, sampleRate int) []byte {
	n := int(seconds * float64(sampleRate))
	data := make([]byte, 2*n)
	for i := 0; i < n; i++ {
		v := int16(0.3 * math.MaxInt16 * math.Sin(2*math.Pi*440*float64(i)/float64(sampleRate)))
		binary.LittleEndian.PutUint16(data[2*i:], uint16(v))
	}
	var b bytes.Buffer
	b.WriteString("RIFF")
	_ = binary.Write(&b, binary.LittleEndian, uint32(36+len(data)))
	b.WriteString("WAVEfmt ")
	_ = binary.Write(&b, binary.LittleEndian, uint32(16))
	_ = binary.Write(&b, binary.LittleEndian, uint16(1)) // PCM
	_ = binary.Write(&b, binary.LittleEndian, uint16(1)) // mono
	_ = binary.Write(&b, binary.LittleEndian, uint32(sampleRate))
	_ = binary.Write(&b, binary.LittleEndian, uint32(2*sampleRate))
	_ = binary.Write(&b, binary.LittleEndian, uint16(2))
	_ = binary.Write(&b, binary.LittleEndian, uint16(16))
	b.WriteString("data")
	_ = binary.Write(&b, binary.LittleEndian, uint32(len(data)))
	b.Write(data)
	return b.Bytes()
}
