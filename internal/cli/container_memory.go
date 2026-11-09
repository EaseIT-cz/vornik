package cli

import (
	"vornik.io/vornik/internal/runtime"
)

// oneSlotMemoryLimit is the memory bound, in bytes, for a container vornikctl
// starts outside the daemon (the bench acceptance grader, the scraper login
// sidecar): the agents' derivation for one slot, half the host's memory
// (runtime.AgentMemoryLimitBytes with no configured value and a concurrency
// of 1). These commands load no daemon config, so runtime.agent_memory_limit
// is not available to them; each runs one container at a time. 0 (unbounded)
// when the host total is unreadable, as for the daemon. GitHub #460;
// 08-runtime-manager-podman.md S6.
func oneSlotMemoryLimit() int64 {
	total, err := runtime.HostTotalMemory()
	if err != nil {
		return 0
	}
	limit, err := runtime.AgentMemoryLimitBytes("", 1, total)
	if err != nil {
		return 0
	}
	return limit
}
