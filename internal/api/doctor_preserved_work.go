package api

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"vornik.io/vornik/internal/executor"
)

// checkPreservedWork lists the refs a failed merge-back pinned the task's work
// to, across every project workspace (executor design §9.5.2 P3, GitHub #354).
//
// WARNING while any exist: each is work that never reached the project branch
// and that nobody has recovered yet. Startup prune expires them after the
// retention bound. The listing reads ref files only (executor.ListPreservedWork):
// this runs inside a REST request and may not spawn git (process-spawn law S2).
// No --fix: deleting an operator's only copy of the work is not a repair.
func (h *DoctorHandlers) checkPreservedWork() DoctorCheck {
	name := "preserved_work"
	if h.workspacesRoot == "" {
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: "no workspaces root configured; skipping"}
	}
	entries, err := os.ReadDir(h.workspacesRoot)
	if err != nil {
		return DoctorCheck{Name: name, Status: "SKIPPED", Message: fmt.Sprintf("workspaces root not readable: %v; skipping", err)}
	}
	var items []string
	projects := 0
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		projectDir := filepath.Join(h.workspacesRoot, e.Name())
		if fi, err := os.Stat(filepath.Join(projectDir, ".git")); err != nil || !fi.IsDir() {
			continue
		}
		projects++
		refs, err := executor.ListPreservedWork(projectDir)
		if err != nil {
			items = append(items, fmt.Sprintf("%s: could not read refs: %v", e.Name(), err))
			continue
		}
		for _, p := range refs {
			age := "age unknown"
			if !p.PreservedAt.IsZero() {
				age = time.Since(p.PreservedAt).Round(time.Hour).String() + " old"
			}
			items = append(items, fmt.Sprintf("%s: %s %s (%s); recover with: git -C %s merge %s",
				e.Name(), p.Ref, p.SHA, age, projectDir, p.Ref))
		}
	}
	if len(items) == 0 {
		return DoctorCheck{Name: name, Status: "OK",
			Message: fmt.Sprintf("no preserved merge-failure work across %d project(s)", projects)}
	}
	return DoctorCheck{
		Name:   name,
		Status: "WARNING",
		Message: fmt.Sprintf("%d preserved merge-failure ref(s) across %d project(s): work that never reached the project branch; refs expire %d days after the failure",
			len(items), projects, int(executor.PreservedWorkRetention.Hours()/24)),
		Items: items,
	}
}
