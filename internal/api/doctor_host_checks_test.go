package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Process-spawn law, S2 (https://docs.vornik.io).
// Incident: POST /api/v1/doctor ran podman, skopeo and systemctl on the daemon
// host, and ?fix=true ran git — a REST request reaching a process spawn. The
// host checks now run in vornikctl doctor on the host; the daemon reports only
// what it holds, plus one entry saying where the host checks went.

var movedHostChecks = []string{"podman_config", "agent_images", "agent_image_uid", "image_freshness"}

func checkNames(r DoctorReport) map[string]DoctorCheck {
	out := map[string]DoctorCheck{}
	for _, c := range r.Checks {
		out[c.Name] = c
	}
	return out
}

func TestRunDoctor_HostChecksLeftTheDaemon(t *testing.T) {
	h := NewDoctorHandlers(closedDB(t))
	rec := httptest.NewRecorder()
	h.RunDoctor(rec, httptest.NewRequest(http.MethodPost, "/api/v1/doctor", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	var report DoctorReport
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &report))
	names := checkNames(report)
	for _, moved := range movedHostChecks {
		assert.NotContains(t, names, moved, "the daemon must not run %s", moved)
	}
	hc, ok := names["host_checks"]
	require.True(t, ok, "the report says where the host checks went")
	assert.Equal(t, "SKIPPED", hc.Status)
	assert.Contains(t, hc.Message, "vornikctl doctor")
}

// The support report runs the doctor read-only; it must not reach podman either.
func TestRunReportReadOnly_HostChecksLeftTheDaemon(t *testing.T) {
	report := NewDoctorHandlers(closedDB(t)).RunReportReadOnly(t.Context())
	names := checkNames(report)
	for _, moved := range movedHostChecks {
		assert.NotContains(t, names, moved)
	}
	assert.Contains(t, names, "host_checks")
}

// vornikctl compares the images on the host with the daemon's own build, so the
// report carries the daemon's revision (from build info; no spawn).
func TestRunDoctor_ReportsTheDaemonRevision(t *testing.T) {
	h := NewDoctorHandlers(closedDB(t))
	h.daemonRevisionFunc = func() string { return "0123456789ab" }
	rec := httptest.NewRecorder()
	h.RunDoctor(rec, httptest.NewRequest(http.MethodPost, "/api/v1/doctor", nil))
	var report DoctorReport
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &report))
	assert.Equal(t, "0123456789ab", report.DaemonRevision)
}
