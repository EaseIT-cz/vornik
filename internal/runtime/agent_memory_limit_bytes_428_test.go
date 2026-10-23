package runtime

import "testing"

// TestAgentMemoryLimitBytes pins the one rule that turns
// runtime.agent_memory_limit into bytes, shared by the daemon
// (service.resolveAgentMemoryLimit) and `vornikctl deps install` (GitHub
// #428, project dependency provisioning design §8.6): absent derives, a size
// parses, "none" is unbounded, garbage is refused.
func TestAgentMemoryLimitBytes(t *testing.T) {
	const gib = int64(1) << 30
	if got, err := AgentMemoryLimitBytes("", 4, 8*gib); err != nil || got != DeriveAgentMemoryLimit(8*gib, 4) {
		t.Errorf(`"" = %d, %v; want the derived %d`, got, err, DeriveAgentMemoryLimit(8*gib, 4))
	}
	if got, err := AgentMemoryLimitBytes("  ", 4, 0); err != nil || got != 0 {
		t.Errorf("blank with an unreadable host = %d, %v; want 0 (unbounded)", got, err)
	}
	if got, err := AgentMemoryLimitBytes("2GiB", 4, 8*gib); err != nil || got != 2*gib {
		t.Errorf(`"2GiB" = %d, %v`, got, err)
	}
	if got, err := AgentMemoryLimitBytes("none", 4, 8*gib); err != nil || got != 0 {
		t.Errorf(`"none" = %d, %v; want 0`, got, err)
	}
	if _, err := AgentMemoryLimitBytes("lots", 4, 8*gib); err == nil {
		t.Error(`"lots" must be refused`)
	}
}
