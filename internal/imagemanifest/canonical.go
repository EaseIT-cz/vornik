package imagemanifest

import "vornik.io/vornik/internal/imageref"

// The legacy agent image name and its canonical form live in
// internal/imageref, the one implementation (migration design §5.2); these
// wrappers keep imagemanifest's callers on it.

// CanonicalImageRef maps the legacy agent repository onto the canonical one,
// keeping the tag or digest; any other reference is returned unchanged.
func CanonicalImageRef(ref string) string { return imageref.Canonical(ref) }

// CanonicalImageRefFrom is CanonicalImageRef for a reference read from a
// named source, logging the rewrite once per reference per process.
func CanonicalImageRefFrom(ref, source string) string { return imageref.CanonicalFrom(ref, source) }

// SetLegacyImageLogger installs the sink for legacy-name rewrites.
func SetLegacyImageLogger(fn func(old, canonical, source string)) (restore func()) {
	return imageref.SetLegacyImageLogger(fn)
}

func resetLegacyImageNotes() { imageref.ResetLegacyImageNotes() }
