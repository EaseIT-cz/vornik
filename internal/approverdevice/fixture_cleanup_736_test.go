package approverdevice

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"vornik.io/vornik/internal/persistence"
)

// slowPushRepo delays the push goroutine's subscription read, so a fixture
// that does not wait for it returns while it is still running.
type slowPushRepo struct {
	persistence.ApproverPushRepository
	done atomic.Bool
}

func (r *slowPushRepo) ListActivePushSubscriptions(ctx context.Context) ([]persistence.ApproverPushSubscriptionRow, error) {
	time.Sleep(100 * time.Millisecond)
	defer r.done.Store(true)
	return r.ApproverPushRepository.ListActivePushSubscriptions(ctx)
}

// GitHub #736: TestWebPush_SubscribeRefusesBadInput paired a device, which
// starts the enrollment push goroutine, and returned without waiting for it.
// The goroutine read the test's SQLite file while t.TempDir's cleanup removed
// the directory ("unlinkat ...: directory not empty"; publish-ce run
// 43035744742). The fixture now waits for every push goroutine in a cleanup
// that runs before the temp dir and database cleanups.
func TestFixture_WaitsForPushGoroutinesBeforeCleanup_736(t *testing.T) {
	var repo *slowPushRepo
	t.Run("pairs and returns at once", func(t *testing.T) {
		f, _, _ := newPushFixture(t)
		repo = &slowPushRepo{ApproverPushRepository: f.svc.pushRepo}
		f.svc.pushRepo = repo
		f.firstDevice(t) // the enrollment alert starts a push goroutine
	})
	if !repo.done.Load() {
		t.Fatal("the subtest's cleanups ran while its push goroutine was still running")
	}
}
