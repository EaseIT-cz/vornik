package service

import (
	"fmt"
	"path/filepath"
	"sort"

	"vornik.io/vornik/internal/api"
	"vornik.io/vornik/internal/projectdeps"
	"vornik.io/vornik/internal/registry"
)

// dependencyInventory is the project_dependencies doctor check's input: for
// every project that declares dependencies, its plans and, for each installed
// tree, the project's role images it was not installed for. It reads only: the
// daemon never installs (dependency provisioning design §8.2).
//
// The ROOT is the project's main checkout, the one `vornikctl deps install`
// installs from; the mount path plans over the task's worktree when there is
// one. The planner and the content-addressed key are shared, so an unchanged
// lockfile gives the SAME key in both; a worktree whose lockfile differs is the
// named branch-lockfile limitation (§8.3), and its step refuses with the
// install command rather than mounting a tree for other bytes.
func dependencyInventory(reg *registry.Registry, workspaceRoot, cacheDir string) []api.ProjectDependencyStatus {
	store := projectdeps.NewStore(cacheDir)
	resolver := projectdeps.NewResolver(store, "")
	var out []api.ProjectDependencyStatus
	for _, p := range reg.ListProjects() {
		if len(p.Dependencies) == 0 {
			continue
		}
		st := api.ProjectDependencyStatus{
			ProjectID: p.ID,
			Plans:     resolver.Plan(filepath.Join(workspaceRoot, p.ID), p.Dependencies),
		}
		images := roleImages(reg, p.ID)
		for i, plan := range st.Plans {
			if !plan.Materialised {
				continue
			}
			meta, err := store.ReadMarker(plan.Key)
			if err != nil {
				// The mount path refuses on an unreadable marker, so the doctor
				// must not count it installed (review-20260925-f900 F2).
				st.Plans[i].Problem = fmt.Errorf("installed tree's completion marker is unreadable: %w", err)
				continue
			}
			for _, img := range images {
				if !meta.ListsImage(img) {
					if st.StaleImages == nil {
						st.StaleImages = map[string][]string{}
					}
					st.StaleImages[plan.Key] = append(st.StaleImages[plan.Key], img)
				}
			}
		}
		out = append(out, st)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ProjectID < out[j].ProjectID })
	return out
}

func roleImages(reg *registry.Registry, projectID string) []string {
	_, swarm, err := reg.GetProjectWithSwarm(projectID)
	if err != nil || swarm == nil {
		return nil
	}
	seen := map[string]bool{}
	var out []string
	for _, r := range swarm.Roles {
		if r.Runtime.Image != "" && !seen[r.Runtime.Image] {
			seen[r.Runtime.Image] = true
			out = append(out, r.Runtime.Image)
		}
	}
	sort.Strings(out)
	return out
}
