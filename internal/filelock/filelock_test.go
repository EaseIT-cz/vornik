package filelock

import (
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Two holders never overlap: each open of the lock file is its own open file
// description, so flock serialises them even inside one process.
func TestWithExclusive_Serialises(t *testing.T) {
	lock := filepath.Join(t.TempDir(), "x.lock")
	var inside, maxInside int32
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = WithExclusive(lock, func() error {
				n := atomic.AddInt32(&inside, 1)
				for {
					m := atomic.LoadInt32(&maxInside)
					if n <= m || atomic.CompareAndSwapInt32(&maxInside, m, n) {
						break
					}
				}
				time.Sleep(5 * time.Millisecond)
				atomic.AddInt32(&inside, -1)
				return nil
			})
		}()
	}
	wg.Wait()
	if maxInside != 1 {
		t.Fatalf("%d holders overlapped", maxInside)
	}
}

func TestWithExclusive_UnlockableStillRuns(t *testing.T) {
	ran := false
	_ = WithExclusive(filepath.Join(t.TempDir(), "missing-dir", "x.lock"), func() error { ran = true; return nil })
	if !ran {
		t.Fatal("fn did not run when the lock file could not be opened")
	}
}

// Exclusive refuses rather than running unlocked.
func TestExclusive_RefusesWhenItCannotLock(t *testing.T) {
	ran := false
	err := Exclusive(filepath.Join(t.TempDir(), "missing-dir", "x.lock"), func() error { ran = true; return nil })
	if err == nil || ran {
		t.Fatalf("err %v ran %v: must refuse, and not run fn", err, ran)
	}
}
