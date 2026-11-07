package memory

import (
	"context"
	"testing"

	"github.com/rs/zerolog"
)

// #639 M3 (design 22, "Amendment 2026-11-04 (#639)"): a ProvenanceOnly
// ingest is treated as delegated whatever its task, so its producer role
// never takes role-of-record authority and verifyProvenance runs.
func TestProvenanceDelegated_ProvenanceOnly_639(t *testing.T) {
	asked := false
	p := NewPipeline(nil, PipelineConfig{Logger: zerolog.Nop(),
		CompanionDelegated: func(context.Context, string) (bool, error) { asked = true; return false, nil }})
	ctx := context.Background()
	if !p.provenanceDelegated(ctx, "p", "", MirrorProducerRole, true, IngestArtifactOptions{ProvenanceOnly: true}) {
		t.Fatal("a ProvenanceOnly ingest was not treated as delegated")
	}
	// Not even a class that is never role-of-record escapes it: the mirror's
	// authority is provenance whatever the class.
	if !p.provenanceDelegated(ctx, "p", "", MirrorProducerRole, false, IngestArtifactOptions{ProvenanceOnly: true}) {
		t.Fatal("a ProvenanceOnly ingest of an ineligible class skipped provenance")
	}
	if asked {
		t.Fatal("a ProvenanceOnly ingest asked the task-origin hook")
	}
	// Without the option, the #482 rule is unchanged.
	if p.provenanceDelegated(ctx, "p", "task", "reviewer", true, IngestArtifactOptions{}) || !asked {
		t.Fatal("without ProvenanceOnly, a non-delegated task's role must keep role-of-record and be asked")
	}
}

// The policy branch itself, pinned so a later change to the class policy
// cannot reopen role-of-record for the mirror (review 11b5 F8).
func TestMirrorRoleNeverRoleOfRecord_639(t *testing.T) {
	if EffectiveClassPolicyFor(ClassSpec, MirrorProducerRole, true).RoleOfRecordEligible {
		t.Fatal("EffectiveClassPolicyFor(spec, repo-mirror, delegated) is role-of-record")
	}
}
