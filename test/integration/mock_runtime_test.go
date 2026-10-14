//go:build integration
// +build integration

package integration_test

import (
	"context"
	"fmt"
	"sync"
	"time"

	"vornik.io/vornik/internal/runtime"
)

// mu guards runs: since #142 the executor removes a container through a
// bounded call that runs RemoveContainer on its own goroutine, so the map is
// written concurrently with the test goroutine (race seen in
// TestExecutor_RecoverRunningExecution, 2026-10-14).
type mockRuntimeManager struct {
	mu        sync.Mutex
	runs      map[string]*mockRun
	exitCode  int
	exitDelay time.Duration
}

type mockRun struct {
	done    chan struct{}
	code    int
	taskID  string
	started time.Time
}

func (m *mockRuntimeManager) StartContainer(ctx context.Context, cfg *runtime.ContainerConfig) (string, error) {
	run := &mockRun{
		done:    make(chan struct{}),
		code:    m.exitCode,
		taskID:  cfg.TaskID,
		started: time.Now(),
	}
	containerID := fmt.Sprintf("mock-container-%d", time.Now().UnixNano())
	m.mu.Lock()
	m.runs[containerID] = run
	m.mu.Unlock()

	if m.exitDelay > 0 {
		go func() {
			time.Sleep(m.exitDelay)
			select {
			case <-run.done:
			default:
				close(run.done)
			}
		}()
	}

	return containerID, nil
}

func (m *mockRuntimeManager) StopContainer(ctx context.Context, containerID string, force bool) error {
	if run, ok := m.run(containerID); ok {
		select {
		case <-run.done:
		default:
			close(run.done)
		}
	}
	return nil
}

func (m *mockRuntimeManager) InspectContainer(ctx context.Context, containerID string) (*runtime.Container, error) {
	return nil, nil
}

func (m *mockRuntimeManager) WaitForExit(ctx context.Context, containerID string, timeout time.Duration) (int, error) {
	if run, ok := m.run(containerID); ok {
		if timeout <= 0 {
			select {
			case <-run.done:
				return run.code, nil
			case <-ctx.Done():
				return -1, ctx.Err()
			}
		}

		select {
		case <-run.done:
			return run.code, nil
		case <-ctx.Done():
			return -1, ctx.Err()
		case <-time.After(timeout):
			return -1, fmt.Errorf("timeout waiting for container exit")
		}
	}
	return 0, nil
}

func (m *mockRuntimeManager) GetContainerByTask(ctx context.Context, taskID string) (*runtime.Container, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, run := range m.runs {
		if run.taskID == taskID {
			return &runtime.Container{
				ID:     id,
				Status: "running",
			}, nil
		}
	}
	return nil, nil
}

func (m *mockRuntimeManager) RemoveContainer(ctx context.Context, containerID string, force bool) error {
	m.mu.Lock()
	delete(m.runs, containerID)
	m.mu.Unlock()
	return nil
}

// run looks one container up under the lock.
func (m *mockRuntimeManager) run(containerID string) (*mockRun, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.runs[containerID]
	return r, ok
}

func (m *mockRuntimeManager) Logs(ctx context.Context, containerID string, tail int) (string, error) {
	return "", nil
}
