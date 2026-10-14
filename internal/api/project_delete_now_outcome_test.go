package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"

	"vornik.io/vornik/internal/projectarchive"
	"vornik.io/vornik/internal/registry"
)

// outcomeSweeper answers a kick with a canned outcome (or none), the way
// projectarchive.Sweeper reports one after a sweep.
type outcomeSweeper struct {
	mu     sync.Mutex
	seq    uint64
	out    *projectarchive.DeleteOutcome
	result error
	report bool
	kicked chan struct{}
}

func (o *outcomeSweeper) SweepNow(context.Context) {
	o.mu.Lock()
	if o.report {
		o.seq++
		o.out = &projectarchive.DeleteOutcome{Seq: o.seq, At: time.Now(), Err: o.result}
	}
	o.mu.Unlock()
	close(o.kicked)
}

func (o *outcomeSweeper) OutcomeSeq() uint64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.seq
}

func (o *outcomeSweeper) WaitOutcome(ctx context.Context, _ string, after uint64) (projectarchive.DeleteOutcome, bool) {
	select {
	case <-o.kicked:
	case <-ctx.Done():
		return projectarchive.DeleteOutcome{}, false
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.out == nil || o.out.Seq <= after {
		<-ctx.Done()
		return projectarchive.DeleteOutcome{}, false
	}
	return *o.out, true
}

func deleteNowServer(t *testing.T, sw *outcomeSweeper) *Server {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "projects"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "projects", "proj.yaml"), []byte("projectId: proj\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := registry.New()
	registry.SeedForTest(reg, map[string]*registry.Project{"proj": {ID: "proj", Lifecycle: registry.ProjectLifecycle{
		Status: "archived", ScheduledDeleteAt: time.Now().Add(time.Hour).UTC().Format(time.RFC3339)}}})
	return &Server{
		logger:          zerolog.Nop(),
		projectRegistry: reg,
		archiveService: &projectarchive.LifecycleService{
			ConfigDir: dir,
			Patcher:   func(c []byte, _ []projectarchive.PatchOp) ([]byte, error) { return c, nil },
			Sweeper:   sw,
		},
		deleteNowWait: 2 * time.Second,
	}
}

func postDeleteNow(s *Server) *httptest.ResponseRecorder {
	rec := httptest.NewRecorder()
	s.ProjectDeleteNow(rec, httptest.NewRequest(http.MethodPost, "/api/v1/projects/proj/delete-now", nil), "proj")
	return rec
}

// GitHub #365: a wipe that failed was answered 200 "archived"; the caller (and
// vornikctl, which exits on the status) must see the failure.
func TestProjectDeleteNow_FailedWipeIs500(t *testing.T) {
	sw := &outcomeSweeper{report: true, result: errors.New("db wipe: no such table: memory_embed_queue"), kicked: make(chan struct{})}
	rec := postDeleteNow(deleteNowServer(t, sw))
	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want 500; body %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "DELETE_FAILED") || !strings.Contains(rec.Body.String(), "memory_embed_queue") {
		t.Errorf("body %s: want DELETE_FAILED and the wipe's error", rec.Body)
	}
}

func TestProjectDeleteNow_SucceededWipeIsDeleted(t *testing.T) {
	sw := &outcomeSweeper{report: true, kicked: make(chan struct{})}
	rec := postDeleteNow(deleteNowServer(t, sw))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body %s", rec.Code, rec.Body)
	}
	var resp ArchiveResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != "deleted" {
		t.Errorf("status field = %q, want deleted", resp.Status)
	}
}

func TestProjectDeleteNow_NoOutcomeIsPending202(t *testing.T) {
	sw := &outcomeSweeper{report: false, kicked: make(chan struct{})}
	s := deleteNowServer(t, sw)
	s.deleteNowWait = 50 * time.Millisecond
	rec := postDeleteNow(s)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want 202; body %s", rec.Code, rec.Body)
	}
	var resp ArchiveResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Status != "deletion_pending" {
		t.Errorf("status field = %q, want deletion_pending", resp.Status)
	}
}
