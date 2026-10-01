package agentpackage

import (
	"errors"
	"strings"
	"testing"
)

// Slice 2 (package design §8): `package upgrade`. Provenance keeps each row's
// hash at install, so the upgrade decides per file and never merges lines;
// any edited or deleted contribution refuses the whole upgrade.

const (
	oldTriage = "# triage v1\n"
	newTriage = "# triage v2\n"
	oldLead   = "# lead v1\n"
)

func installedRows() []Contribution {
	return []Contribution{
		{Package: "acme-incident-response", Kind: KindWorkflow, RowID: "incident-triage", Path: "workflows/incident-triage.md", ContentHashAtInstall: ContentHash([]byte(oldTriage))},
		{Package: "acme-incident-response", Kind: KindRole, RowID: "incident-lead", Path: "role-library/incident-lead.md", ContentHashAtInstall: ContentHash([]byte(oldLead))},
	}
}

func upgradeEnv(payload, deployed map[string]string, claims map[string]string) UpgradeEnvironment {
	return UpgradeEnvironment{
		Environment: Environment{
			ReadPayload: func(p string) ([]byte, error) {
				b, ok := payload[p]
				if !ok {
					return nil, errors.New("no such file in package: " + p)
				}
				return []byte(b), nil
			},
			DeployedExists: func(p string) bool { _, ok := deployed[p]; return ok },
			ClaimedBy: func(k Kind, id string) (string, bool) {
				pkg, ok := claims[string(k)+"/"+id]
				return pkg, ok
			},
		},
		ReadDeployed: func(p string) ([]byte, bool) {
			b, ok := deployed[p]
			return []byte(b), ok
		},
	}
}

func v2Manifest() Manifest {
	m := sample()
	m.Version = "1.3.0"
	return m
}

func ownClaims() map[string]string {
	return map[string]string{
		"workflow/incident-triage": "acme-incident-response",
		"role/incident-lead":       "acme-incident-response",
	}
}

// Row 1: unedited → replace (changed bytes) or keep (identical bytes).
func TestPlanUpgrade_ReplaceAndKeep(t *testing.T) {
	plan, err := PlanUpgrade(v2Manifest(), "1.2.0", installedRows(), upgradeEnv(
		map[string]string{"incident-triage.md": newTriage, "incident-lead.md": oldLead},
		map[string]string{"workflows/incident-triage.md": oldTriage, "role-library/incident-lead.md": oldLead},
		ownClaims()))
	if err != nil {
		t.Fatalf("PlanUpgrade() = %v", err)
	}
	if len(plan.Replace) != 1 || plan.Replace[0].RowID != "incident-triage" {
		t.Fatalf("replace = %+v, want incident-triage", plan.Replace)
	}
	if len(plan.Keep) != 1 || plan.Keep[0].RowID != "incident-lead" {
		t.Fatalf("keep = %+v, want incident-lead", plan.Keep)
	}
	if plan.IsNoop() {
		t.Fatal("a plan with a replacement is not a no-op")
	}
	rows := plan.Contributions()
	if len(rows) != 2 {
		t.Fatalf("new provenance has %d rows, want 2", len(rows))
	}
	for _, r := range rows {
		if r.RowID == "incident-triage" && r.ContentHashAtInstall != ContentHash([]byte(newTriage)) {
			t.Fatal("the replaced row must carry the NEW content's hash")
		}
	}
}

// Row 2: the operator already applied the new version → adopt.
func TestPlanUpgrade_Adopt(t *testing.T) {
	plan, err := PlanUpgrade(v2Manifest(), "1.2.0", installedRows(), upgradeEnv(
		map[string]string{"incident-triage.md": newTriage, "incident-lead.md": oldLead},
		map[string]string{"workflows/incident-triage.md": newTriage, "role-library/incident-lead.md": oldLead},
		ownClaims()))
	if err != nil {
		t.Fatalf("PlanUpgrade() = %v", err)
	}
	if len(plan.Adopt) != 1 || plan.Adopt[0].RowID != "incident-triage" {
		t.Fatalf("adopt = %+v", plan.Adopt)
	}
}

// Rows 3 and 4, and row 6: edited or deleted contributions refuse the WHOLE
// upgrade, every one reported in one pass.
func TestPlanUpgrade_EditedAndDeletedRefuseTheWholeUpgrade(t *testing.T) {
	m := v2Manifest()
	m.Contributes.Roles = nil // row 6: the new version drops an edited role
	_, err := PlanUpgrade(m, "1.2.0", installedRows(), upgradeEnv(
		map[string]string{"incident-triage.md": newTriage},
		map[string]string{"role-library/incident-lead.md": "# lead, tuned\n"}, // triage deleted, lead edited
		ownClaims()))
	var ce *ConflictError
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want a ConflictError", err)
	}
	if len(ce.Conflicts) != 2 {
		t.Fatalf("conflicts = %v, want both reported in one pass", ce.Conflicts)
	}
	joined := ce.Error()
	for _, want := range []string{"incident-triage", "deleted", "incident-lead", "edited"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("refusal %q does not mention %q", joined, want)
		}
	}
}

// Row 5: an unedited contribution the new version dropped is removed.
// Row 7: a dropped contribution the operator already deleted just loses its row.
func TestPlanUpgrade_RemoveAndDrop(t *testing.T) {
	m := v2Manifest()
	m.Contributes.Roles = nil
	plan, err := PlanUpgrade(m, "1.2.0", installedRows(), upgradeEnv(
		map[string]string{"incident-triage.md": newTriage},
		map[string]string{"workflows/incident-triage.md": oldTriage, "role-library/incident-lead.md": oldLead},
		ownClaims()))
	if err != nil {
		t.Fatalf("PlanUpgrade() = %v", err)
	}
	if len(plan.Remove) != 1 || plan.Remove[0].RowID != "incident-lead" {
		t.Fatalf("remove = %+v", plan.Remove)
	}

	plan, err = PlanUpgrade(m, "1.2.0", installedRows(), upgradeEnv(
		map[string]string{"incident-triage.md": newTriage},
		map[string]string{"workflows/incident-triage.md": oldTriage},
		ownClaims()))
	if err != nil {
		t.Fatalf("PlanUpgrade() = %v", err)
	}
	if len(plan.Drop) != 1 || plan.Drop[0].RowID != "incident-lead" {
		t.Fatalf("drop = %+v", plan.Drop)
	}
	for _, r := range plan.Contributions() {
		if r.RowID == "incident-lead" {
			t.Fatal("a dropped contribution must not stay in the new provenance")
		}
	}
}

// Row 8: a new contribution goes through install's checks.
func TestPlanUpgrade_AddAndItsConflicts(t *testing.T) {
	m := v2Manifest()
	m.Contributes.Workflows = append(m.Contributes.Workflows, "postmortem.md")
	payload := map[string]string{"incident-triage.md": newTriage, "incident-lead.md": oldLead, "postmortem.md": "# pm\n"}
	deployed := map[string]string{"workflows/incident-triage.md": oldTriage, "role-library/incident-lead.md": oldLead}
	plan, err := PlanUpgrade(m, "1.2.0", installedRows(), upgradeEnv(payload, deployed, ownClaims()))
	if err != nil {
		t.Fatalf("PlanUpgrade() = %v", err)
	}
	if len(plan.Add) != 1 || plan.Add[0].RowID != "postmortem" {
		t.Fatalf("add = %+v", plan.Add)
	}

	claims := ownClaims()
	claims["workflow/postmortem"] = "other-package"
	_, err = PlanUpgrade(m, "1.2.0", installedRows(), upgradeEnv(payload, deployed, claims))
	var ce *ConflictError
	if !errors.As(err, &ce) || !strings.Contains(ce.Error(), "other-package") {
		t.Fatalf("err = %v, want a conflict naming the other package", err)
	}

	deployed["workflows/postmortem.md"] = "# mine\n"
	_, err = PlanUpgrade(m, "1.2.0", installedRows(), upgradeEnv(payload, deployed, ownClaims()))
	if !errors.As(err, &ce) {
		t.Fatalf("err = %v, want a conflict on an unclaimed deployed file", err)
	}
}

// A same-version upgrade with nothing to change is a no-op that says so.
func TestPlanUpgrade_Noop(t *testing.T) {
	m := sample()
	plan, err := PlanUpgrade(m, "1.2.0", installedRows(), upgradeEnv(
		map[string]string{"incident-triage.md": oldTriage, "incident-lead.md": oldLead},
		map[string]string{"workflows/incident-triage.md": oldTriage, "role-library/incident-lead.md": oldLead},
		ownClaims()))
	if err != nil {
		t.Fatalf("PlanUpgrade() = %v", err)
	}
	if !plan.IsNoop() {
		t.Fatalf("plan %+v, want a no-op", plan)
	}
}

// Upgrade of a package that is not installed is refused.
func TestPlanUpgrade_NotInstalled(t *testing.T) {
	_, err := PlanUpgrade(v2Manifest(), "", nil, upgradeEnv(nil, nil, nil))
	if !errors.Is(err, ErrNotInstalled) {
		t.Fatalf("err = %v, want ErrNotInstalled", err)
	}
}

// Round 2 (review-20260930-3da8 F1): a file the new version added that is
// already deployed with exactly the new bytes and claimed by nobody (the
// residue of a failed provenance swap) is reclaimed, not refused, so the
// re-run converges.
func TestPlanUpgrade_ReclaimTheResidueOfAFailedSwap(t *testing.T) {
	m := v2Manifest()
	m.Contributes.Workflows = append(m.Contributes.Workflows, "postmortem.md")
	payload := map[string]string{"incident-triage.md": newTriage, "incident-lead.md": oldLead, "postmortem.md": "# pm\n"}
	// After a failed swap: the tree is the new version, the provenance is old.
	deployed := map[string]string{
		"workflows/incident-triage.md":  newTriage,
		"role-library/incident-lead.md": oldLead,
		"workflows/postmortem.md":       "# pm\n",
	}
	plan, err := PlanUpgrade(m, "1.2.0", installedRows(), upgradeEnv(payload, deployed, ownClaims()))
	if err != nil {
		t.Fatalf("the re-run after a failed swap must converge, got %v", err)
	}
	if len(plan.Reclaim) != 1 || plan.Reclaim[0].RowID != "postmortem" {
		t.Fatalf("reclaim = %+v, want postmortem", plan.Reclaim)
	}
	if len(plan.Adopt) != 1 || plan.Adopt[0].RowID != "incident-triage" {
		t.Fatalf("adopt = %+v, want incident-triage", plan.Adopt)
	}
	if len(plan.Contributions()) != 3 {
		t.Fatalf("new provenance has %d rows, want 3", len(plan.Contributions()))
	}
}

// The table is version-agnostic: a downgrade plans like an upgrade.
func TestPlanUpgrade_DowngradeIsAllowed(t *testing.T) {
	m := sample()
	m.Version = "1.1.0"
	plan, err := PlanUpgrade(m, "1.2.0", installedRows(), upgradeEnv(
		map[string]string{"incident-triage.md": newTriage, "incident-lead.md": oldLead},
		map[string]string{"workflows/incident-triage.md": oldTriage, "role-library/incident-lead.md": oldLead},
		ownClaims()))
	if err != nil {
		t.Fatalf("PlanUpgrade() = %v", err)
	}
	if plan.FromVersion != "1.2.0" || plan.Manifest.Version != "1.1.0" {
		t.Fatalf("versions %q -> %q", plan.FromVersion, plan.Manifest.Version)
	}
}
