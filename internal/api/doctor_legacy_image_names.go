package api

// Doctor check: legacy_image_names (EaseIT-cz migration design §5.2).
//
// The agent image moved from ghcr.io/grinco to ghcr.io/easeit-cz with every
// tag copied at the same digest. Config deploys never overwrite a deployed
// file, so an install keeps naming the legacy registry; the daemon maps the
// name (internal/imageref) and keeps working. This check names the files and
// the one-line fix. It reads files and runs nothing (process-spawn law), and
// it always says how many files it examined, so "none" is a count, not a
// silence.

import (
	"bytes"
	"fmt"
	"os"
	"sort"

	"vornik.io/vornik/internal/imageref"
)

// The legacy repository's one definition is imageref's (review 3e87).
var legacyAgentImageRepo = imageref.LegacyAgentRepo

var legacyImageScanExtensions = map[string]bool{".yaml": true, ".yml": true, ".md": true, ".json": true, ".tmpl": true}

func (h *DoctorHandlers) checkLegacyImageNames() DoctorCheck {
	name := "legacy_image_names"
	// The deployed config tree and config.yaml, baselines excluded, as the
	// CRLF check walks it, plus templates (.tmpl): project and agent
	// templates name the agent image too.
	files := h.collectConfigFiles(legacyImageScanExtensions)
	if len(files) == 0 {
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "no config files to examine (config dir/path not wired)"}
	}
	var legacy []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil || !bytes.Contains(b, []byte(legacyAgentImageRepo)) {
			continue
		}
		legacy = append(legacy, relForDisplay(h.configDir, f))
	}
	sort.Strings(legacy)
	if len(legacy) == 0 {
		return DoctorCheck{Name: name, Status: "OK", Message: fmt.Sprintf(
			"examined %d config file(s), none name the agent image's legacy registry", len(files))}
	}
	return DoctorCheck{
		Name:   name,
		Status: "WARNING",
		Message: fmt.Sprintf("examined %d config file(s), %d name the agent image's legacy registry %s. "+
			"It still works (the daemon maps it to ghcr.io/easeit-cz/vornik-agent); to update a file: "+
			"sed -i 's#ghcr.io/grinco/vornik-agent#ghcr.io/easeit-cz/vornik-agent#g' <file>",
			len(files), len(legacy), legacyAgentImageRepo),
		Items: legacy,
	}
}
