package registry

// SeedForTest replaces the registry's active project map with the
// provided fixtures. Used by sibling-package tests that exercise
// code paths walking the registry (project-archive sweeper, etc.)
// without standing up a full YAML loader fixture.
//
// Intentionally exported under a Test-prefixed name so production
// callers don't reach for it; production wiring must always go
// through Load / LoadFromPaths.
func SeedForTest(r *Registry, projects map[string]*Project) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active == nil {
		r.active = &ConfigSet{}
	}
	r.active.projects = projects
	r.projects = projects
}

// SeedWorkflowsForTest replaces the registry's active workflow map, the
// workflow counterpart of SeedForTest.
func SeedWorkflowsForTest(r *Registry, workflows map[string]*Workflow) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active == nil {
		r.active = &ConfigSet{}
	}
	r.active.workflows = workflows
	r.workflows = workflows
}

// SeedSwarmsForTest replaces the registry's active swarm map, the swarm
// counterpart of SeedForTest.
func SeedSwarmsForTest(r *Registry, swarms map[string]*Swarm) {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.active == nil {
		r.active = &ConfigSet{}
	}
	r.active.swarms = swarms
	r.swarms = swarms
}
