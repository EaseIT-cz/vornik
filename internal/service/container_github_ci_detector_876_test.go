package service

// GitHub #876, CI outcomes design §19.2: the CI ingest masks artifact content
// with the daemon's secret detector, so the container must hand it the one it
// built; nil (secrets disabled) reaches the ingest as nil, which withholds
// content.

import (
	"context"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/registry"
	"vornik.io/vornik/internal/secrets"
	"vornik.io/vornik/internal/storage"
)

type nopCIOutcomes struct{}

func (nopCIOutcomes) Upsert(context.Context, *persistence.ForgeCIOutcome) error { return nil }
func (nopCIOutcomes) ListByHeadSHA(context.Context, string, string, string) ([]*persistence.ForgeCIOutcome, error) {
	return nil, nil
}
func (nopCIOutcomes) ClaimComment(context.Context, string, string, int64, time.Time) (bool, error) {
	return true, nil
}
func (nopCIOutcomes) ReleaseComment(context.Context, string, string, int64) error { return nil }
func (nopCIOutcomes) PruneBefore(context.Context, time.Time) (int64, error)       { return 0, nil }

type markerDetector struct{}

func (markerDetector) Scan([]byte) []secrets.Finding { return nil }

func TestCIIngest_876_GetsTheDaemonsDetector(t *testing.T) {
	p := &registry.Project{ID: "p"}
	p.Forge.CI.Enabled = true
	for name, d := range map[string]secrets.Detector{"enabled": markerDetector{}, "disabled": nil} {
		c := &Container{Logger: zerolog.Nop(), repos: &storage.Repositories{ForgeCIOutcomes: nopCIOutcomes{}}, secretsDetector: d}
		ing := c.ciIngestForProject(p)
		if ing == nil {
			t.Fatalf("%s: no ingest", name)
		}
		if got := ing.Cfg().Detector; got != d {
			t.Errorf("%s: the ingest's detector is %v, want %v", name, got, d)
		}
	}
}

// GitHub #873: the scorer reads the project's canary floors; an unknown
// project, or a container with no registry, measures only.
func TestCanaryFloors_873_ReadTheProject(t *testing.T) {
	c := &Container{}
	if f := c.canaryFloors("p"); f.FloorDetections != nil || f.CeilingDecoys != nil {
		t.Fatalf("no registry: %+v", f)
	}
}
