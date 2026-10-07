// Brand mark assets: the Harness mark decided 2026-10-07
// (https://docs.vornik.io §2.3).
//
// Before this test nothing asserted what the mark looks like, so a stale or
// half-replaced icon passed every test (design §1.3). These checks pin the
// shapes and colours that make the mark the mark, and the maskable contract
// both manifests declare.
package ui

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"encoding/xml"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io/fs"
	"os"
	"strings"
	"testing"
)

const (
	hexMeshPath = "M14 3L24 8.5"                   // retired hexagon mark
	bracketPath = "M42 24H26V96H42M78 24H94V96H78" // Harness brackets (120 grid)
	navyHex     = "#101826"
)

var (
	navy      = color.RGBA{0x10, 0x18, 0x26, 0xff}
	cadetTeal = color.RGBA{0x55, 0x8a, 0x98, 0xff} // retired ground colour
)

type svgDoc struct {
	ViewBox string      `xml:"viewBox,attr"`
	Rects   []svgRect   `xml:"rect"`
	Circles []svgCircle `xml:"circle"`
	Groups  []svgGroup  `xml:"g"`
}

type svgGroup struct {
	Circles []svgCircle `xml:"circle"`
	Groups  []svgGroup  `xml:"g"`
}

type svgRect struct {
	Width  string `xml:"width,attr"`
	Height string `xml:"height,attr"`
	Rx     string `xml:"rx,attr"`
	Fill   string `xml:"fill,attr"`
}

type svgCircle struct {
	Fill  string `xml:"fill,attr"`
	Class string `xml:"class,attr"`
}

func (g svgGroup) allCircles() []svgCircle {
	out := append([]svgCircle(nil), g.Circles...)
	for _, c := range g.Groups {
		out = append(out, c.allCircles()...)
	}
	return out
}

func parseSVG(t *testing.T, raw []byte) svgDoc {
	t.Helper()
	var d svgDoc
	if err := xml.Unmarshal(raw, &d); err != nil {
		t.Fatalf("parse svg: %v", err)
	}
	return d
}

func circlesOf(d svgDoc) []svgCircle {
	return svgGroup{Circles: d.Circles, Groups: d.Groups}.allCircles()
}

func readStatic(t *testing.T, name string) []byte {
	t.Helper()
	raw, err := fs.ReadFile(staticFS, "static/"+name)
	if err != nil {
		t.Fatalf("static/%s not embedded: %v", name, err)
	}
	return raw
}

// assertHarness checks the Vornik Harness mark: the bracket path and exactly
// three dots, two agents (blue) over one approved outcome (green).
func assertHarness(t *testing.T, name string, raw []byte, wantFills map[string]int) {
	t.Helper()
	s := string(raw)
	if strings.Contains(s, hexMeshPath) {
		t.Errorf("%s still draws the retired hexagon mesh", name)
	}
	if !strings.Contains(s, bracketPath) {
		t.Errorf("%s lacks the Harness bracket path", name)
	}
	got := map[string]int{}
	cs := circlesOf(parseSVG(t, raw))
	for _, c := range cs {
		got[strings.ToUpper(c.Fill)]++
	}
	if len(cs) != 3 {
		t.Errorf("%s: want 3 dots (2 agents + 1 outcome), got %d", name, len(cs))
	}
	for fill, n := range wantFills {
		if got[fill] != n {
			t.Errorf("%s: want %d dot(s) filled %s, got %d (all: %v)", name, n, fill, got[fill], got)
		}
	}
}

var onNavyFills = map[string]int{"#6FB4EE": 2, "#9BDB6B": 1}

func TestBrandMark_IconSVGIsHarnessOnNavy(t *testing.T) {
	raw := readStatic(t, "icon.svg")
	assertHarness(t, "icon.svg", raw, onNavyFills)
	d := parseSVG(t, raw)
	if len(d.Rects) == 0 || !strings.EqualFold(d.Rects[0].Fill, navyHex) {
		t.Errorf("icon.svg: want a navy %s tile as the first rect, got %+v", navyHex, d.Rects)
	}
}

func TestBrandMark_MaskableIconIsFullBleed(t *testing.T) {
	raw := readStatic(t, "icon-maskable.svg")
	assertHarness(t, "icon-maskable.svg", raw, onNavyFills)
	d := parseSVG(t, raw)
	vb := strings.Fields(d.ViewBox)
	if len(vb) != 4 {
		t.Fatalf("icon-maskable.svg: bad viewBox %q", d.ViewBox)
	}
	if len(d.Rects) == 0 {
		t.Fatal("icon-maskable.svg: no background rect")
	}
	r := d.Rects[0]
	if r.Rx != "" && r.Rx != "0" {
		t.Errorf("maskable ground must be full-bleed, got rx=%q", r.Rx)
	}
	if r.Width != vb[2] || r.Height != vb[3] || !strings.EqualFold(r.Fill, navyHex) {
		t.Errorf("maskable ground must cover the %sx%s viewBox in navy, got %+v", vb[2], vb[3], r)
	}
}

func TestBrandMark_ManifestsSplitAnyAndMaskable(t *testing.T) {
	for _, name := range []string{"manifest.webmanifest", "approve.webmanifest"} {
		var m struct {
			Icons []struct {
				Src     string `json:"src"`
				Purpose string `json:"purpose"`
			} `json:"icons"`
		}
		if err := json.Unmarshal(readStatic(t, name), &m); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		got := map[string]string{}
		for _, ic := range m.Icons {
			got[ic.Src] = ic.Purpose
		}
		if got["/ui/static/icon.svg"] != "any" {
			t.Errorf("%s: icon.svg must be purpose \"any\" (it has rounded corners), got %q", name, got["/ui/static/icon.svg"])
		}
		if got["/ui/static/icon-maskable.svg"] != "maskable" {
			t.Errorf("%s: want icon-maskable.svg with purpose \"maskable\", got %v", name, got)
		}
	}
}

func near(c color.Color, want color.RGBA, tol int) bool {
	r, g, b, a := c.RGBA()
	d := func(x uint32, y uint8) int {
		v := int(x>>8) - int(y)
		if v < 0 {
			return -v
		}
		return v
	}
	return d(r, want.R)+d(g, want.G)+d(b, want.B)+d(a, want.A) <= tol
}

func noTeal(t *testing.T, name string, img image.Image) {
	t.Helper()
	b := img.Bounds()
	for y := b.Min.Y; y < b.Max.Y; y++ {
		for x := b.Min.X; x < b.Max.X; x++ {
			if near(img.At(x, y), cadetTeal, 24) {
				t.Errorf("%s: retired cadet-teal pixel at %d,%d", name, x, y)
				return
			}
		}
	}
}

func TestBrandMark_AppleTouchIconIsFullBleedNavy(t *testing.T) {
	img, err := png.Decode(bytes.NewReader(readStatic(t, "apple-touch-icon.png")))
	if err != nil {
		t.Fatal(err)
	}
	if b := img.Bounds(); b.Dx() != 180 || b.Dy() != 180 {
		t.Fatalf("apple-touch-icon.png: want 180x180, got %v", b)
	}
	// iOS rounds the corners itself; a pre-rounded tile shows double corners.
	if c := img.At(0, 0); !near(c, navy, 12) {
		t.Errorf("apple-touch-icon.png: corner pixel %v is not navy, so the icon is not full-bleed", c)
	}
	noTeal(t, "apple-touch-icon.png", img)
}

// assertSmallTile checks a favicon-sized rendering of the navy tile.
func assertSmallTile(t *testing.T, name string, img image.Image) {
	t.Helper()
	b := img.Bounds()
	if c := img.At(b.Min.X+b.Dx()/2, b.Min.Y+1); !near(c, navy, 40) {
		t.Errorf("%s (%dpx): top-centre pixel %v is not navy", name, b.Dx(), c)
	}
	noTeal(t, name, img)
}

func TestBrandMark_Favicon32PNG(t *testing.T) {
	img, err := png.Decode(bytes.NewReader(readStatic(t, "favicon-32.png")))
	if err != nil {
		t.Fatal(err)
	}
	assertSmallTile(t, "favicon-32.png", img)
}

// decodeICO returns every image in an .ico; entries are PNG or 32-bit BMP DIBs.
func decodeICO(t *testing.T, raw []byte) []image.Image {
	t.Helper()
	if len(raw) < 6 || binary.LittleEndian.Uint16(raw[2:]) != 1 {
		t.Fatal("favicon.ico: not an icon file")
	}
	n := int(binary.LittleEndian.Uint16(raw[4:]))
	var out []image.Image
	for i := 0; i < n; i++ {
		e := raw[6+16*i:]
		size := int(binary.LittleEndian.Uint32(e[8:]))
		off := int(binary.LittleEndian.Uint32(e[12:]))
		data := raw[off : off+size]
		if bytes.HasPrefix(data, []byte("\x89PNG")) {
			img, err := png.Decode(bytes.NewReader(data))
			if err != nil {
				t.Fatalf("favicon.ico entry %d: %v", i, err)
			}
			out = append(out, img)
			continue
		}
		out = append(out, decodeDIB32(t, data))
	}
	return out
}

func decodeDIB32(t *testing.T, data []byte) image.Image {
	t.Helper()
	hdr := int(binary.LittleEndian.Uint32(data))
	w := int(int32(binary.LittleEndian.Uint32(data[4:])))
	h := int(int32(binary.LittleEndian.Uint32(data[8:]))) / 2 // XOR + AND masks
	if bpp := binary.LittleEndian.Uint16(data[14:]); bpp != 32 {
		t.Fatalf("favicon.ico: unsupported %d-bit DIB entry", bpp)
	}
	img := image.NewNRGBA(image.Rect(0, 0, w, h))
	px := data[hdr:]
	for y := 0; y < h; y++ {
		row := px[(h-1-y)*w*4:] // bottom-up
		for x := 0; x < w; x++ {
			p := row[x*4:]
			img.SetNRGBA(x, y, color.NRGBA{p[2], p[1], p[0], p[3]})
		}
	}
	return img
}

func TestBrandMark_FaviconICO(t *testing.T) {
	imgs := decodeICO(t, readStatic(t, "favicon.ico"))
	sizes := map[int]bool{}
	for _, img := range imgs {
		sizes[img.Bounds().Dx()] = true
		assertSmallTile(t, "favicon.ico", img)
	}
	for _, want := range []int{16, 32, 48} {
		if !sizes[want] {
			t.Errorf("favicon.ico: missing %dpx image (have %v)", want, sizes)
		}
	}
}

func TestBrandMark_NavLogoTemplate(t *testing.T) {
	raw, err := os.ReadFile("templates/_partials.html")
	if err != nil {
		t.Fatal(err)
	}
	s := string(raw)
	i := strings.Index(s, `{{define "navLogo"}}`)
	if i < 0 {
		t.Fatal(`navLogo template not found`)
	}
	j := strings.Index(s[i:], "</a>")
	if j < 0 {
		t.Fatal("navLogo: no closing </a>")
	}
	block := s[i : i+j]
	if strings.Contains(block, hexMeshPath) {
		t.Error("navLogo still draws the retired hexagon mesh")
	}
	if !strings.Contains(block, bracketPath) {
		t.Error("navLogo lacks the Harness bracket path")
	}
	if n := strings.Count(block, "<circle"); n != 3 {
		t.Errorf("navLogo: want 3 dots, got %d", n)
	}
	if !strings.Contains(block, `class="nav-logo-dot"`) {
		t.Error("navLogo: the outcome dot must keep the theme-aware nav-logo-dot class")
	}
}

func TestBrandMark_PublicDocsMark(t *testing.T) {
	raw, err := os.ReadFile("../../docs/public/assets/vornik-mark.svg")
	if err != nil {
		t.Fatal(err)
	}
	assertHarness(t, "docs/public/assets/vornik-mark.svg", raw, onNavyFills)
}

// TestBrandMark_DocsSiteTheme pins docs.vornik.io to the brand manual
// (design §3.2): custom palette, the Harness logo and favicon, and no Google
// Fonts request (theme.font: false with self-hosted fonts via extra_css).
func TestBrandMark_DocsSiteTheme(t *testing.T) {
	// mkdocs.yml exists only in the Enterprise tree: the CE export prunes it
	// (scripts/export-public-ce.sh), and docs.vornik.io is built from EE. On
	// the exported tree the config checks cannot run, but the shipped assets
	// below still must. Reading it unconditionally failed the 2026.10.5 CE
	// export (publish-ce run 37685664597, 2026-10-07).
	raw, err := os.ReadFile("../../mkdocs.yml")
	switch {
	case errors.Is(err, fs.ErrNotExist):
		t.Log("mkdocs.yml absent (CE export tree): config checks skipped, asset checks still run")
	case err != nil:
		t.Fatal(err)
	}
	cfg := string(raw)
	for _, want := range []string{
		"  font: false",
		"  logo: assets/vornik-mark-header.svg",
		"  favicon: assets/vornik-mark.svg",
		"primary: custom",
		"accent: custom",
		"assets/brand/brand-tokens.css",
		"stylesheets/brand.css",
	} {
		if raw != nil && !strings.Contains(cfg, want) {
			t.Errorf("mkdocs.yml: missing %q", want)
		}
	}
	for _, f := range []string{
		"assets/vornik-mark-header.svg",
		"assets/vornik-mark.svg",
		"assets/brand/brand-tokens.css",
		"assets/brand/fonts/SpaceGrotesk-Variable.woff2",
		"assets/brand/fonts/Manrope-Variable.woff2",
		"assets/brand/fonts/SpaceGrotesk-OFL.txt",
		"assets/brand/fonts/Manrope-OFL.txt",
		"stylesheets/brand.css",
	} {
		if _, err := os.Stat("../../docs/public/" + f); err != nil {
			t.Errorf("docs/public/%s: %v", f, err)
		}
	}
	header, err := os.ReadFile("../../docs/public/assets/vornik-mark-header.svg")
	if err == nil {
		assertHarness(t, "vornik-mark-header.svg", header, onNavyFills)
	}
}
