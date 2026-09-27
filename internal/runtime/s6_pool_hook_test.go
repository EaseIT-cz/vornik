package runtime

// newS6WarmPool builds the pool the daemon wires. Before S6 it was given the
// project workspace path (WithPoolProjectWorkspacePath, now removed), which is
// what made every warm container mount the project directory read-write; the
// pre-S6 run of TestS6_WarmContainerMountsNoProjectDirectory used that option
// and failed on the rw project mount it produced.
func newS6WarmPool(mgr *Manager, ws string) *WarmPool {
	_ = ws
	return NewWarmPool(mgr, PoolConfig{MaxPerRole: 2})
}
