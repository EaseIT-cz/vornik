package agentpackage

import (
	"errors"
	"fmt"
	"path/filepath"
	"sort"
)

// `package upgrade` (package design §8). Provenance keeps each row's content
// HASH at install, not the content, so an upgrade decides per file and never
// merges lines: the base text is gone. Any edited or deleted contribution
// refuses the whole upgrade, for the reason uninstall is whole-set.

// ErrNotInstalled is returned when the manifest names a package with no
// installed rows: that is an install, not an upgrade.
var ErrNotInstalled = errors.New("package is not installed; use install")

// UpgradeEnvironment is install's Environment plus a read of the deployed
// tree, which the per-file decision compares against.
type UpgradeEnvironment struct {
	Environment
	// ReadDeployed returns a deployed file's bytes, and whether it exists.
	ReadDeployed func(relPath string) ([]byte, bool)
}

// UpgradePlan is every per-file decision, computed before anything is written.
type UpgradePlan struct {
	Manifest    Manifest
	FromVersion string
	// Replace: unedited, and the new version ships different bytes.
	Replace []ReplaceItem
	// Keep: unedited, and the new version ships the same bytes.
	Keep []PlannedItem
	// Adopt: the deployed file already equals the new version.
	Adopt []PlannedItem
	// Reclaim: a newly contributed file is already deployed with exactly the
	// new bytes and claimed by nobody — the residue of a failed provenance
	// swap. Recorded, never written.
	Reclaim []PlannedItem
	// Add: a newly contributed file, written as install would.
	Add []PlannedItem
	// Remove: an unedited contribution the new version dropped.
	Remove []Contribution
	// Drop: a dropped contribution the operator already deleted; only its
	// row goes.
	Drop []Contribution
}

// IsNoop reports whether the upgrade would change nothing on disk or in
// provenance beyond the version string.
func (p *UpgradePlan) IsNoop() bool {
	return len(p.Replace)+len(p.Adopt)+len(p.Reclaim)+len(p.Add)+len(p.Remove)+len(p.Drop) == 0 &&
		p.FromVersion == p.Manifest.Version
}

// Contributions is the package's provenance after the upgrade: every item the
// new version contributes, each with the hash of the bytes it ships.
func (p *UpgradePlan) Contributions() []Contribution {
	var out []Contribution
	replaced := make([]PlannedItem, 0, len(p.Replace))
	for _, r := range p.Replace {
		replaced = append(replaced, r.PlannedItem)
	}
	for _, group := range [][]PlannedItem{replaced, p.Keep, p.Adopt, p.Reclaim, p.Add} {
		for _, it := range group {
			out = append(out, Contribution{
				Package: p.Manifest.Package, Kind: it.Kind, RowID: it.RowID,
				Path: it.TargetPath, ContentHashAtInstall: it.Hash,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out
}

// ReplaceItem is a planned replacement plus the hash the deployed file must
// still have when it is written — the write-time check of design §8.
type ReplaceItem struct {
	PlannedItem
	CurrentHash string
}

type rowKey struct {
	kind  Kind
	rowID string
}

// PlanUpgrade decides every row in the union of the installed rows and the new
// manifest (design §8's table), or refuses the whole upgrade with every
// reason in one pass.
func PlanUpgrade(m Manifest, fromVersion string, installed []Contribution, env UpgradeEnvironment) (*UpgradePlan, error) {
	if err := m.Validate(); err != nil {
		return nil, err
	}
	if len(installed) == 0 {
		return nil, fmt.Errorf("%w: %q", ErrNotInstalled, m.Package)
	}
	old := make(map[rowKey]Contribution, len(installed))
	for _, c := range installed {
		old[rowKey{c.Kind, c.RowID}] = c
	}

	plan := &UpgradePlan{Manifest: m, FromVersion: fromVersion}
	var conflicts []Conflict
	seen := map[rowKey]bool{}

	for _, src := range manifestSources(m) {
		item, err := plannedItem(src, env)
		if err != nil {
			return nil, err
		}
		k := rowKey{item.Kind, item.RowID}
		seen[k] = true
		if c, ok := old[k]; ok {
			if conflict := decideInstalled(plan, c, item, env); conflict != nil {
				conflicts = append(conflicts, *conflict)
			}
			continue
		}
		if conflict := decideNew(plan, item, env); conflict != nil {
			conflicts = append(conflicts, *conflict)
		}
	}
	for _, c := range installed {
		if seen[rowKey{c.Kind, c.RowID}] {
			continue
		}
		if conflict := decideDropped(plan, c, env); conflict != nil {
			conflicts = append(conflicts, *conflict)
		}
	}

	if len(conflicts) > 0 {
		sort.Slice(conflicts, func(i, j int) bool {
			if conflicts[i].Kind != conflicts[j].Kind {
				return conflicts[i].Kind < conflicts[j].Kind
			}
			return conflicts[i].RowID < conflicts[j].RowID
		})
		return nil, &ConflictError{Conflicts: conflicts}
	}
	return plan, nil
}

type manifestSource struct {
	kind Kind
	dir  string
	path string
}

func manifestSources(m Manifest) []manifestSource {
	var out []manifestSource
	for _, w := range m.Contributes.Workflows {
		out = append(out, manifestSource{KindWorkflow, WorkflowsDir, w})
	}
	for _, r := range m.Contributes.Roles {
		out = append(out, manifestSource{KindRole, RoleLibraryDir, r})
	}
	return out
}

func plannedItem(src manifestSource, env UpgradeEnvironment) (PlannedItem, error) {
	body, err := env.ReadPayload(src.path)
	if err != nil {
		return PlannedItem{}, fmt.Errorf("read %s %q from the package: %w", src.kind, src.path, err)
	}
	rowID := contributionRowID(src.path)
	return PlannedItem{
		Kind: src.kind, RowID: rowID, SourcePath: src.path,
		TargetPath: filepath.Join(src.dir, rowID+".md"),
		Bytes:      body, Hash: ContentHash(body),
	}, nil
}

// decideInstalled: a row the package already owns, still in the manifest.
func decideInstalled(plan *UpgradePlan, c Contribution, item PlannedItem, env UpgradeEnvironment) *Conflict {
	current, exists := env.ReadDeployed(c.Path)
	switch {
	case !exists:
		return &Conflict{Kind: item.Kind, RowID: item.RowID, Target: c.Path,
			Reason: "the operator deleted this contribution; restore it (or uninstall the package) and upgrade again — an upgrade does not re-create a file the operator may have meant to remove"}
	case ContentHash(current) == c.ContentHashAtInstall:
		if item.Hash == c.ContentHashAtInstall {
			plan.Keep = append(plan.Keep, item)
		} else {
			plan.Replace = append(plan.Replace, ReplaceItem{PlannedItem: item, CurrentHash: c.ContentHashAtInstall})
		}
	case ContentHash(current) == item.Hash:
		plan.Adopt = append(plan.Adopt, item)
	default:
		return &Conflict{Kind: item.Kind, RowID: item.RowID, Target: c.Path,
			Reason: "edited since install; restore it, or copy your tuning aside, then upgrade and reapply it — an upgrade never overwrites an operator's edit"}
	}
	return nil
}

// decideNew: a contribution the installed version did not have.
func decideNew(plan *UpgradePlan, item PlannedItem, env UpgradeEnvironment) *Conflict {
	owner, claimed := env.ClaimedBy(item.Kind, item.RowID)
	if claimed && owner != plan.Manifest.Package {
		return &Conflict{Kind: item.Kind, RowID: item.RowID, Target: item.TargetPath,
			Reason: fmt.Sprintf("already contributed by package %q — two packages contributing one id is an operator decision, not something an installer resolves by ordering", owner)}
	}
	if current, exists := env.ReadDeployed(item.TargetPath); exists {
		if !claimed && ContentHash(current) == item.Hash {
			plan.Reclaim = append(plan.Reclaim, item)
			return nil
		}
		return &Conflict{Kind: item.Kind, RowID: item.RowID, Target: item.TargetPath,
			Reason: "a file is already deployed there and no package claims it; remove or rename it first rather than have an upgrade overwrite an operator's own config"}
	}
	plan.Add = append(plan.Add, item)
	return nil
}

// decideDropped: a row the package owns that the new version no longer ships.
func decideDropped(plan *UpgradePlan, c Contribution, env UpgradeEnvironment) *Conflict {
	current, exists := env.ReadDeployed(c.Path)
	switch {
	case !exists:
		plan.Drop = append(plan.Drop, c)
	case ContentHash(current) == c.ContentHashAtInstall:
		plan.Remove = append(plan.Remove, c)
	default:
		return &Conflict{Kind: c.Kind, RowID: c.RowID, Target: c.Path,
			Reason: "the new version drops this contribution, but it was edited since install; keep your copy by moving it, or restore it, then upgrade again"}
	}
	return nil
}
