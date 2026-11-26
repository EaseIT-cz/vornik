package registry

// GitHub #873: forge.canary holds the review canary's floors; unset means
// measure only.

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestForgeCanary_873_Parses(t *testing.T) {
	var unset ProjectForge
	if err := yaml.Unmarshal([]byte("provider: github\n"), &unset); err != nil {
		t.Fatal(err)
	}
	if unset.Canary.FloorDetections != nil || unset.Canary.CeilingDecoys != nil {
		t.Fatalf("unset must be nil: %+v", unset.Canary)
	}
	var set ProjectForge
	if err := yaml.Unmarshal([]byte("canary:\n  floor_detections: 5\n  ceiling_decoys: 0\n"), &set); err != nil {
		t.Fatal(err)
	}
	if set.Canary.FloorDetections == nil || *set.Canary.FloorDetections != 5 || set.Canary.CeilingDecoys == nil || *set.Canary.CeilingDecoys != 0 {
		t.Fatalf("set: %+v", set.Canary)
	}
}
