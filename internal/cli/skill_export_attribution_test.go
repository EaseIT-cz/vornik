package cli

import (
	"bytes"
	"strings"
	"testing"
)

// workflow-md design, "Publication recommendations belong to publishing"
// (2026-10-03): the doctor no longer warns about a workflow's missing author
// or license, because almost no workflow is published; export, where the
// file is generated, says it instead. stderr only, so a redirected stdout is
// still exactly the file.
func exportWithStderr(t *testing.T, opts func()) (stdout, stderr string) {
	t.Helper()
	var errBuf bytes.Buffer
	skillExportCmd.SetErr(&errBuf)
	defer skillExportCmd.SetErr(nil)
	out := captureExportOutput(t, "demo/research", opts)
	return out, errBuf.String()
}

func TestSkillExport_WarnsOnMissingAttributionOnStderr(t *testing.T) {
	srv := startFakeDaemon(t, "demo", "research", "assistant")
	defer srv.Close()
	setSwarmctlAPIEnv(t, srv)

	out, errOut := exportWithStderr(t, func() {})
	for _, want := range []string{"--author", "--license"} {
		if !strings.Contains(errOut, want) {
			t.Errorf("stderr %q does not name %s", errOut, want)
		}
	}
	if strings.Contains(out, "--author") || strings.Contains(out, "warning") {
		t.Errorf("the warning leaked into stdout:\n%s", out)
	}
	if !strings.HasPrefix(out, "---\n") {
		t.Errorf("stdout is not the file:\n%s", out)
	}
}

func TestSkillExport_NoWarningWithAttribution(t *testing.T) {
	srv := startFakeDaemon(t, "demo", "research", "assistant")
	defer srv.Close()
	setSwarmctlAPIEnv(t, srv)

	_, errOut := exportWithStderr(t, func() {
		skillExportAuthor = "Vornik"
		skillExportLicense = "Apache-2.0"
	})
	if strings.Contains(errOut, "warning") {
		t.Errorf("warned although both fields are set: %q", errOut)
	}
}

// The warning names exactly the flag still needed.
func TestSkillExport_WarningNamesOnlyTheMissingFlag(t *testing.T) {
	srv := startFakeDaemon(t, "demo", "research", "assistant")
	defer srv.Close()
	setSwarmctlAPIEnv(t, srv)

	_, onlyAuthor := exportWithStderr(t, func() { skillExportAuthor = "Vornik" })
	if !strings.Contains(onlyAuthor, "--license") || strings.Contains(onlyAuthor, "--author") {
		t.Errorf("with --author only, stderr = %q; want --license named, not --author", onlyAuthor)
	}
	_, onlyLicense := exportWithStderr(t, func() { skillExportLicense = "Apache-2.0" })
	if !strings.Contains(onlyLicense, "--author") || strings.Contains(onlyLicense, "--license") {
		t.Errorf("with --license only, stderr = %q; want --author named, not --license", onlyLicense)
	}
}

// --standard drops only metadata.vornik.*: it still warns without author, and
// keeps both fields when they are set.
func TestSkillExport_StandardModeAndAttribution(t *testing.T) {
	srv := startFakeDaemon(t, "demo", "research", "assistant")
	defer srv.Close()
	setSwarmctlAPIEnv(t, srv)

	_, errOut := exportWithStderr(t, func() { skillExportStandard = true })
	if !strings.Contains(errOut, "--author") {
		t.Errorf("--standard without author did not warn: %q", errOut)
	}
	out, errOut := exportWithStderr(t, func() {
		skillExportStandard = true
		skillExportAuthor = "Vornik"
		skillExportLicense = "Apache-2.0"
	})
	if errOut != "" {
		t.Errorf("warned with both fields set: %q", errOut)
	}
	if !strings.Contains(out, "author: Vornik") || !strings.Contains(out, "license: Apache-2.0") {
		t.Errorf("--standard output lost a field:\n%s", out)
	}
}
