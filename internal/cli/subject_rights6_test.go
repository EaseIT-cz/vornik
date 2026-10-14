package cli

import (
	"strings"
	"testing"

	"vornik.io/vornik/internal/datasubject"
)

// GDPR increment 6 (GitHub #343): the operator surface for Art 16 and Art 18.
// audit 2026-10-12: both kinds could be requested and verified, and no command
// could execute either.

func TestSubjectCommands_Increment6VerbsAreRegistered(t *testing.T) {
	want := map[string]bool{"restrict": false, "lift-restriction": false, "rectify": false}
	for _, c := range subjectCmd.Commands() {
		name := strings.Fields(c.Use)[0]
		if _, ok := want[name]; ok {
			want[name] = true
		}
	}
	for name, ok := range want {
		if !ok {
			t.Errorf("vornikctl subject %s is not registered", name)
		}
	}
}

func TestRestrictionGroundHelp_ListsEveryGround(t *testing.T) {
	help := restrictionGroundHelp()
	for _, g := range datasubject.RestrictionGrounds() {
		if !strings.Contains(help, string(g)) || !strings.Contains(help, g.Article()) {
			t.Errorf("help omits %q / %s", g, g.Article())
		}
	}
}

// Art 18(3) is checked BEFORE anything opens the database: an operator who forgot
// the affirmation gets the reason, not a connection error that hides it.
func TestLiftRestriction_RefusesWithoutTheArt18_3Affirmation(t *testing.T) {
	err := validateLiftFlags("operator", "accuracy verified", false)
	if err == nil || !strings.Contains(err.Error(), "18(3)") {
		t.Fatalf("got %v, want an Art 18(3) refusal", err)
	}
	if err := validateLiftFlags("", "x", true); err == nil {
		t.Error("--by is required")
	}
	if err := validateLiftFlags("op", " ", true); err == nil {
		t.Error("--reason is required")
	}
	if err := validateLiftFlags("op", "verified", true); err != nil {
		t.Errorf("valid flags refused: %v", err)
	}
}

// The operator affirms; the software does not certify (change review F6).
func TestLiftConfirmationWordingIsAnAffirmation(t *testing.T) {
	msg := liftAffirmationLine("operator-1")
	if !strings.Contains(msg, "affirmed") || strings.Contains(strings.ToLower(msg), "subject informed:") {
		t.Fatalf("wording must state the operator's affirmation, got %q", msg)
	}
}
