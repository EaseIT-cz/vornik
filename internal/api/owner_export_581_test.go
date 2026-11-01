package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/rs/zerolog"
	"github.com/stretchr/testify/require"

	"vornik.io/vornik/internal/artifacts"
	"vornik.io/vornik/internal/auth"
	"vornik.io/vornik/internal/config"
	"vornik.io/vornik/internal/listener"
	"vornik.io/vornik/internal/persistence"
)

// GitHub #581 D5 (enterprise-packaging-design.md, amendment 2026-10-29, D5
// and its correction, review aaef): the owner export is registered by the
// control plane's key and downloaded by the owner's session only, from the
// public listener, from a key the daemon builds itself, for 7 days by its own
// record.

const exportID581 = "x-abcdefghijklmnop"

type memExports581 struct {
	mu sync.Mutex
	at map[string]time.Time
}

func (m *memExports581) RegisterExport(_ context.Context, id string, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.at[id]; ok {
		return persistence.ErrExportExists
	}
	m.at[id] = at
	return nil
}

func (m *memExports581) ExportCreatedAt(_ context.Context, id string) (time.Time, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	at, ok := m.at[id]
	return at, ok, nil
}

type memStore581 struct {
	mu      sync.Mutex
	objects map[string]string
	keys    []string
	block   chan struct{} // non-nil: Get waits on it
	err     error
}

func (m *memStore581) Get(ctx context.Context, key string) (io.ReadCloser, error) {
	m.mu.Lock()
	m.keys = append(m.keys, key)
	block, err := m.block, m.err
	body, ok := m.objects[key]
	m.mu.Unlock()
	if block != nil {
		select {
		case <-block:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, artifacts.ErrNotFound
	}
	return io.NopCloser(strings.NewReader(body)), nil
}

func exportServer581(t *testing.T) (*Server, *memExports581, *memStore581, *time.Time) {
	t.Helper()
	cfg, err := config.LoadFromPath(writeConfig582(t, managedYAML582()))
	require.NoError(t, err)
	repo := &memExports581{at: map[string]time.Time{}}
	store := &memStore581{objects: map[string]string{}}
	now := time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC)
	s := NewServer(WithLogger(zerolog.Nop()), WithConfig(cfg), WithAdminClassKeys(cfg.Admin), WithOwnerExports(repo, store))
	s.exports.now = func() time.Time { return now }
	return s, repo, store, &now
}

func ownerGet581(s *Server, id, role string, l listener.Name) *httptest.ResponseRecorder {
	ctx := stampSessionIdentity(role)
	if role == "" {
		ctx = context.Background()
	}
	req := httptest.NewRequest(http.MethodGet, ownerExportPathPrefix+id, nil).WithContext(listener.With(ctx, l))
	rec := httptest.NewRecorder()
	s.ServeOwnerExport(rec, req)
	return rec
}

func TestOwnerExport581_TheOwnerDownloadsFromTheDaemonsOwnKey(t *testing.T) {
	s, repo, store, now := exportServer581(t)
	require.NoError(t, repo.RegisterExport(context.Background(), exportID581, *now))
	store.objects["instances/i582/exports/"+exportID581+".tar.zst"] = "the bytes"

	rec := ownerGet581(s, exportID581, auth.RoleTenantAdmin, listener.Public)
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	require.Equal(t, "the bytes", rec.Body.String())
	require.Equal(t, "application/zstd", rec.Header().Get("Content-Type"))
	require.Contains(t, rec.Header().Get("Content-Disposition"), exportID581+".tar.zst")
	require.Equal(t, []string{"instances/i582/exports/" + exportID581 + ".tar.zst"}, store.keys)
}

func TestOwnerExport581_OnlyTheOwnersSessionOnThePublicListener(t *testing.T) {
	s, repo, store, now := exportServer581(t)
	require.NoError(t, repo.RegisterExport(context.Background(), exportID581, *now))
	store.objects["instances/i582/exports/"+exportID581+".tar.zst"] = "x"

	for name, c := range map[string]struct {
		role string
		l    listener.Name
		want int
	}{
		"an admin session":                     {"admin", listener.Public, http.StatusForbidden},
		"a user session":                       {"user", listener.Public, http.StatusForbidden},
		"no session (an admin key or none)":    {"", listener.Public, http.StatusForbidden},
		"the owner on the operations listener": {auth.RoleTenantAdmin, listener.Operations, http.StatusNotFound},
		"the owner with no listener stamp":     {auth.RoleTenantAdmin, listener.Unknown, http.StatusNotFound},
	} {
		t.Run(name, func(t *testing.T) {
			if rec := ownerGet581(s, exportID581, c.role, c.l); rec.Code != c.want {
				t.Errorf("status %d, want %d: %s", rec.Code, c.want, rec.Body.String())
			}
		})
	}
	require.Empty(t, store.keys, "no refused request may read the object")
}

func TestOwnerExport581_UnknownExpiredAndBadIdsAreTheSame404(t *testing.T) {
	s, repo, store, now := exportServer581(t)
	old := "x-qrstuvwxyz234567"
	require.NoError(t, repo.RegisterExport(context.Background(), old, now.Add(-ownerExportTTL)))
	store.objects["instances/i582/exports/"+old+".tar.zst"] = "x"
	store.objects["instances/i582/exports/"+exportID581+".tar.zst"] = "x" // in S3, never registered

	var bodies []string
	for _, id := range []string{exportID581, old, "x-../../etc/passwd", "X-ABCDEFGHIJKLMNOP", "x-abcdefghijklmno1"} {
		rec := ownerGet581(s, id, auth.RoleTenantAdmin, listener.Public)
		require.Equal(t, http.StatusNotFound, rec.Code, id)
		bodies = append(bodies, rec.Body.String())
	}
	for _, b := range bodies[1:] {
		require.Equal(t, bodies[0], b, "an unknown, an expired and a malformed id must answer alike")
	}
	require.Empty(t, store.keys, "no unregistered or expired id may reach S3")
}

func TestOwnerExport581_AnObjectMissingFromS3IsThe404(t *testing.T) {
	s, repo, _, now := exportServer581(t)
	require.NoError(t, repo.RegisterExport(context.Background(), exportID581, *now))
	require.Equal(t, http.StatusNotFound, ownerGet581(s, exportID581, auth.RoleTenantAdmin, listener.Public).Code)
}

func TestOwnerExport581_AnS3FailureIs502WithoutTheKey(t *testing.T) {
	s, repo, store, now := exportServer581(t)
	var logs bytes.Buffer
	s.logger = zerolog.New(&logs)
	require.NoError(t, repo.RegisterExport(context.Background(), exportID581, *now))
	store.err = errors.New("get s3://backups/instances/i582/exports/" + exportID581 + ".tar.zst: access denied")
	rec := ownerGet581(s, exportID581, auth.RoleTenantAdmin, listener.Public)
	require.Equal(t, http.StatusBadGateway, rec.Code)
	require.NotContains(t, rec.Body.String(), "instances/")
	require.NotContains(t, logs.String(), "instances/", "the log must not carry the key")
	require.NotContains(t, logs.String(), "backups", "the log must not carry the bucket")
}

func TestOwnerExport581_AThirdDownloadIsBusy(t *testing.T) {
	s, repo, store, now := exportServer581(t)
	require.NoError(t, repo.RegisterExport(context.Background(), exportID581, *now))
	store.objects["instances/i582/exports/"+exportID581+".tar.zst"] = "x"
	store.block = make(chan struct{})

	var wg sync.WaitGroup
	for i := 0; i < ownerExportMaxStreams; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); ownerGet581(s, exportID581, auth.RoleTenantAdmin, listener.Public) }()
	}
	require.Eventually(t, func() bool { return len(s.exports.slots) == ownerExportMaxStreams }, 5*time.Second, 5*time.Millisecond)
	rec := ownerGet581(s, exportID581, auth.RoleTenantAdmin, listener.Public)
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	require.Equal(t, "60", rec.Header().Get("Retry-After"))
	close(store.block)
	wg.Wait()
	require.Empty(t, s.exports.slots, "every download gives its slot back")
}

func TestOwnerExport581_RegistrationTakesTheAdminKeyAndAValidId(t *testing.T) {
	cfg, err := config.LoadFromPath(writeConfig582(t, managedYAML582()))
	require.NoError(t, err)
	repo := &memExports581{at: map[string]time.Time{}}
	srv := NewServer(WithLogger(zerolog.Nop()), WithConfig(cfg), WithAdminClassKeys(cfg.Admin), WithOwnerExports(repo, &memStore581{}))
	hs := httptest.NewServer(NewRouter(srv, cfg).Handler())
	t.Cleanup(hs.Close)
	post := func(key, body string) int {
		req, _ := http.NewRequest(http.MethodPost, hs.URL+"/api/v1/operations/exports", strings.NewReader(body))
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		return resp.StatusCode
	}
	good, _ := json.Marshal(map[string]string{"export_id": exportID581})
	require.NotEqual(t, http.StatusCreated, post("", string(good)), "no key")
	require.Equal(t, http.StatusCreated, post(adminKey582, string(good)))
	require.Equal(t, http.StatusConflict, post(adminKey582, string(good)), "a second registration")
	for _, bad := range []string{`{"export_id":"x-../../a"}`, `{"export_id":"x-ABCDEFGHIJKLMNOP"}`, `{"export_id":"x-abc"}`, `{"export_id":"x-abcdefghijklmnop","extra":1}`} {
		require.Equal(t, http.StatusBadRequest, post(adminKey582, bad), bad)
	}
	_, ok, _ := repo.ExportCreatedAt(context.Background(), exportID581)
	require.True(t, ok)
}

func TestOwnerExport581_NotConfiguredIs503(t *testing.T) {
	cfg, err := config.LoadFromPath(writeConfig582(t, managedYAML582()))
	require.NoError(t, err)
	s := NewServer(WithLogger(zerolog.Nop()), WithConfig(cfg))
	require.Equal(t, http.StatusServiceUnavailable, ownerGet581(s, exportID581, auth.RoleTenantAdmin, listener.Public).Code)
}
