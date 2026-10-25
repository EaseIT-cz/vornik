//go:build integration

package memory_test

// GitHub #508 F5 (design 22, "Amendment 2026-10-24 (#508)"): a chunk the
// forge API verified (provenance:forge-api) is protected by the C5 guard
// exactly as a clone-verified one: an unavailable upload never retires it,
// and the two provenance roles retire each other as one class. Red with the
// exact-string guard, which knew only provenance:origin/main.

import (
	"testing"

	"vornik.io/vornik/internal/memory"
)

func TestIntegration508_ForgeAPIVerifiedIsProtectedByC5(t *testing.T) {
	f := newDocFixture(t)
	proven := f.artifact("proven", "INPUT", "upload", 0)
	f.chunkWith(proven, docPath, "verified", memory.ValidatorRoleProvenanceForgeAPI)
	forged := f.artifact("forged", "INPUT", "upload", 10)
	f.chunkWith(forged, docPath, "verified", memory.ValidatorRoleProvenanceUnavailable)

	f.supersede(docPath, "")
	sameIDs(t, f.live(docPath), proven, forged)
}

func TestIntegration508_ProvenanceRolesRetireEachOther(t *testing.T) {
	for _, pair := range [][2]string{
		{memory.ValidatorRoleProvenance, memory.ValidatorRoleProvenanceForgeAPI},
		{memory.ValidatorRoleProvenanceForgeAPI, memory.ValidatorRoleProvenance},
	} {
		t.Run(pair[0]+"->"+pair[1], func(t *testing.T) {
			f := newDocFixture(t)
			old := f.artifact("old", "INPUT", "upload", 0)
			f.chunkWith(old, docPath, "verified", pair[0])
			newer := f.artifact("new", "INPUT", "upload", 10)
			f.chunkWith(newer, docPath, "verified", pair[1])

			f.supersede(docPath, "")
			sameIDs(t, f.live(docPath), newer)
		})
	}
}
