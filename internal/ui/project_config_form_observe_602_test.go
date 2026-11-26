package ui

// GitHub #602 (design 11, amendment 2026-11-20, B6): the project config form
// offers "Observe only", pre-set for a project whose autonomy is off, so
// turning autonomy on from the form starts it in observe mode unless the
// operator chooses otherwise. Load, save and the patch round-trip.

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	"vornik.io/vornik/internal/registry"
)

func TestProjectConfigForm_ObserveRoundTrip_602(t *testing.T) {
	var off ProjectConfigFormData
	populateFormFromProject(&off, &registry.Project{ID: "p"})
	if !off.AutonomyObserve {
		t.Fatal("a project with autonomy off must load with Observe only pre-set")
	}

	var acting ProjectConfigFormData
	populateFormFromProject(&acting, &registry.Project{ID: "p", Autonomy: registry.ProjectAutonomy{Enabled: true}})
	if acting.AutonomyObserve {
		t.Fatal("a project already acting must load as it is: observe false")
	}

	var observing ProjectConfigFormData
	populateFormFromProject(&observing, &registry.Project{ID: "p", Autonomy: registry.ProjectAutonomy{Enabled: true, Observe: true}})
	if !observing.AutonomyObserve {
		t.Fatal("an observing project must load with observe true")
	}

	for _, v := range []string{"true", "false"} {
		form := url.Values{"autonomy_enabled": {"true"}, "autonomy_observe": {v}}
		r, _ := http.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		if err := r.ParseForm(); err != nil {
			t.Fatal(err)
		}
		var data ProjectConfigFormData
		overlayFormValuesOntoData(&data, r)
		if data.AutonomyObserve != (v == "true") {
			t.Fatalf("posted observe=%s, parsed %v", v, data.AutonomyObserve)
		}
		found := false
		for _, p := range buildFormPatches(&data) {
			if strings.Join(p.Path, ".") == "autonomy.observe" {
				found = true
				if p.Value != data.AutonomyObserve {
					t.Fatalf("patch value %v, want %v", p.Value, data.AutonomyObserve)
				}
			}
		}
		if !found {
			t.Fatal("the save must patch autonomy.observe")
		}
	}
}

// With autonomy off, the pre-set value is not written: saving an untouched
// form must leave the file as it was (#748), whatever observe it holds.
func TestProjectConfigForm_ObserveNotWrittenWhileAutonomyIsOff_602(t *testing.T) {
	var data ProjectConfigFormData
	populateFormFromProject(&data, &registry.Project{ID: "p"})
	for _, p := range buildFormPatches(&data) {
		if strings.Join(p.Path, ".") == "autonomy.observe" {
			t.Fatal("a save with autonomy off must not patch autonomy.observe")
		}
	}
}

func TestProjectConfigForm_RendersObserveControl_602(t *testing.T) {
	raw, err := templatesFS.ReadFile("templates/project_config_form.html")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `name="autonomy_observe"`) {
		t.Fatal("the form must render the autonomy_observe control")
	}
}
