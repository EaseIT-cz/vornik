package ui

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// GitHub #750 (2026-11-13): the settings form saved autonomy.pollInterval
// "20" (meant "20m"); the autonomy manager could not parse it and polled
// every 5 minutes, silently. The form now refuses, before the write, what
// the manager would fall back on (web-ui-design.md, "The settings page
// refuses a duration with no unit").
func TestProjectConfigFormSave_RefusesADurationTheManagerCannotUse_750(t *testing.T) {
	for _, bad := range []string{"20", "0s", "-5m", "soon"} {
		root := writeFormFixture(t)
		server, reloader := formServer(t, root)
		path := filepath.Join(root, "projects", "form-demo.yaml")
		before, err := os.ReadFile(path)
		require.NoError(t, err)

		form := baselineFormValues()
		form.Set("autonomy_pollInterval", bad)
		rec := httptest.NewRecorder()
		server.ProjectConfigFormSave(rec, postForm(form), "form-demo")

		assert.Equal(t, http.StatusBadRequest, rec.Code, "pollInterval %q", bad)
		assert.Contains(t, rec.Body.String(), "autonomy_pollInterval must be a positive duration with a unit, such as 20m or 1h")
		assert.Equal(t, 0, reloader.calls)
		after, err := os.ReadFile(path)
		require.NoError(t, err)
		assert.Equal(t, string(before), string(after), "pollInterval %q: nothing is written", bad)
	}
}

func TestProjectConfigFormSave_SavesAValidOrEmptyDuration_750(t *testing.T) {
	for _, good := range []string{"20m", ""} {
		root := writeFormFixture(t)
		server, _ := formServer(t, root)
		form := baselineFormValues()
		form.Set("autonomy_pollInterval", good)
		rec := httptest.NewRecorder()
		server.ProjectConfigFormSave(rec, postForm(form), "form-demo")
		assert.NotContains(t, rec.Body.String(), "must be a duration", "pollInterval %q", good)
		if good != "" {
			b, err := os.ReadFile(filepath.Join(root, "projects", "form-demo.yaml"))
			require.NoError(t, err)
			assert.True(t, strings.Contains(string(b), "pollInterval: 20m") || strings.Contains(string(b), `pollInterval: "20m"`), "a valid duration is written:\n%s", b)
		}
	}
}
