package api

// GitHub #617 slice 2 (federation design, "Slice 2: the corpus-walk
// finding"). An agent's memory_search reaches this handler with its task
// key, but the retrieval audit row carried only actor_kind rest_api and
// the key id: all 6,149 such rows of the 30 days measured had no task, and
// the task key is deleted when its task ends, so the chunks an A2A task
// retrieved could not be tied to it afterwards. The handler now stamps the
// task (task key) or the role (warm-pool key) on the retrieval context.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/memory"
	"vornik.io/vornik/internal/persistence"
	"vornik.io/vornik/internal/taskkeys"
)

// ctxStub records the retrieval context each routing search ran under.
type ctxStub struct{ got []memory.RetrievalContext }

func (c *ctxStub) Search(ctx context.Context, _, _ string, _ int) ([]MemorySearchResult, error) {
	c.got = append(c.got, memory.RetrievalContextFromContext(ctx))
	return nil, nil
}

func (c *ctxStub) SearchRouting(ctx context.Context, _, _ string, _ MemorySearchRequest) ([]MemorySearchResult, *RoutingVerdictWire, error) {
	c.got = append(c.got, memory.RetrievalContextFromContext(ctx))
	return nil, nil, nil
}

func keyRowReq(row *persistence.APIKey) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/api/v1/projects/p1/memory/search?q=x", nil)
	if row == nil {
		return r
	}
	ctx := context.WithValue(r.Context(), identityKey, &auth.Identity{Extra: map[string]any{auth.ExtraDBKeyRow: row}})
	return r.WithContext(ContextWithAPIKeyIDForTesting(ctx, row.ID))
}

func searchRetrievalContext(t *testing.T, row *persistence.APIKey) memory.RetrievalContext {
	t.Helper()
	stub := &ctxStub{}
	srv := NewServer(WithMemorySearcher(stub))
	rec := httptest.NewRecorder()
	srv.MemorySearch(rec, keyRowReq(row))
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d, body %s", rec.Code, rec.Body.String())
	}
	if len(stub.got) != 1 {
		t.Fatalf("want one search, got %d", len(stub.got))
	}
	return stub.got[0]
}

func TestMemorySearch617_TaskKeySearchNamesItsTask(t *testing.T) {
	rc := searchRetrievalContext(t, &persistence.APIKey{ID: "k-task", Name: persistence.TaskKeyNamePrefix + "task_a2a_1",
		ProjectID: "p1", CreatedBy: executorKeyCreator})
	if rc.TaskID != "task_a2a_1" {
		t.Fatalf("audit task = %q, want task_a2a_1", rc.TaskID)
	}
	if rc.ActorKind != "rest_api" || rc.ActorID != "k-task" {
		t.Fatalf("actor = %q/%q, want rest_api/k-task (dashboards and the firewall read these)", rc.ActorKind, rc.ActorID)
	}
}

func TestMemorySearch617_WarmKeySearchNamesItsRole(t *testing.T) {
	rc := searchRetrievalContext(t, &persistence.APIKey{ID: "k-warm", Name: taskkeys.WarmAgentKeyNamePrefix + "p1:researcher", ProjectID: "p1"})
	if rc.Role != "researcher" || rc.TaskID != "" {
		t.Fatalf("got role %q task %q, want role researcher and no task", rc.Role, rc.TaskID)
	}
	if rc.ActorKind != "rest_api" {
		t.Fatalf("actor kind = %q, want rest_api", rc.ActorKind)
	}
}

func TestMemorySearch617_NonAgentKeyIsUnchanged(t *testing.T) {
	rc := searchRetrievalContext(t, &persistence.APIKey{ID: "k-op", Name: "operator laptop", ProjectID: "p1"})
	want := memory.RetrievalContext{ActorKind: "rest_api", ActorID: "k-op"}
	if rc != want {
		t.Fatalf("got %+v, want %+v", rc, want)
	}
}
