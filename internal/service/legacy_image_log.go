package service

import "vornik.io/vornik/internal/imageref"

// installLegacyImageLogger makes the legacy agent image rewrite visible: the
// first time a config names ghcr.io/grinco/vornik-agent, the daemon logs the
// file and the fix once (EaseIT-cz migration design §5.2). The doctor's
// legacy_image_names check reports the same files on demand.
func (c *Container) installLegacyImageLogger() {
	logger := c.Logger
	imageref.SetLegacyImageLogger(func(old, canonical, source string) {
		logger.Warn().
			Str("legacy_image", old).
			Str("canonical_image", canonical).
			Str("source", source).
			Msg("config names the agent image's legacy registry; using the canonical name. " +
				"Update the file: sed -i 's#ghcr.io/grinco/vornik-agent#ghcr.io/easeit-cz/vornik-agent#g' <file>")
	})
}
