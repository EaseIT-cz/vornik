package registry

// WithProjectSnapshot holds the registry read lock while fn performs an action
// authorized by this project's current policy. Reload activation waits until
// the action finishes. fn must not call Registry methods or mutate the project.
func (r *Registry) WithProjectSnapshot(id string, fn func(*Project) error) error {
	r.mu.RLock()
	defer r.mu.RUnlock()
	return fn(r.projects[id])
}
