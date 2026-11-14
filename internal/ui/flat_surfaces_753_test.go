package ui

// Design system v2 part D (ui-design-refresh-design.md §12.2 D, #753): flat
// surfaces. Before #753 the page painted three brand radial gradients behind
// translucent glass (body::before). The rail's logo dot was the on-dark green
// #9BDB6B in both themes because the rail was dark glass in both.

import (
	"regexp"
	"strings"
	"testing"
)

func TestFlat_NoBrandGlowBackground_753(t *testing.T) {
	src := partials(t)
	if strings.Contains(src, "radial-gradient(") {
		t.Error("_partials.html still paints a radial-gradient page glow")
	}
	if regexp.MustCompile(`body::before\s*\{[^}]*radial`).MatchString(src) {
		t.Error("body::before still paints the brand glow")
	}
	// The fine-grain noise overlay (body::after, an feTurbulence SVG at 3.5%)
	// went with the glow: a flat page has no texture.
	if strings.Contains(src, "feTurbulence") {
		t.Error("_partials.html still paints the grain overlay")
	}
}

// The logo dot follows the theme like the logo files: Signal Green on a
// light rail, the on-dark green on a dark one.
func TestFlat_LogoDotFollowsTheTheme_753(t *testing.T) {
	src := partials(t)
	if !regexp.MustCompile(`(?i)\.nav-logo-dot\s*\{\s*fill:\s*#6EA83B`).MatchString(src) {
		t.Error(".nav-logo-dot should be Signal Green #6EA83B on the light rail")
	}
	if !regexp.MustCompile(`(?i)\[data-theme="dark"\]\s*\.nav-logo-dot\s*\{\s*fill:\s*#9BDB6B`).MatchString(src) {
		t.Error(".nav-logo-dot should be #9BDB6B on the dark rail")
	}
}
