package memory

// GitHub #508 F4: the forge API clients read the shapes GitHub (Enterprise),
// GitLab and Gitea/Forgejo answer, send the credential header, and map a
// repository the daemon cannot read to unavailable (unexaminable), never to
// an examined absence.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

const (
	head508 = "3333333333333333333333333333333333333333"
	blob508 = "4444444444444444444444444444444444444444"
)

func forgeStub508(t *testing.T, kind string, seen *[]string) *httptest.Server {
	t.Helper()
	return httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*seen = append(*seen, r.Header.Get("Authorization"))
		p := r.URL.EscapedPath()
		w.Header().Set("Content-Type", "application/json")
		switch kind {
		case ForgeKindGitHub:
			switch {
			case p == "/api/v3/repos/acme/repo":
				_, _ = w.Write([]byte(`{"default_branch":"main"}`))
			case p == "/api/v3/repos/acme/repo/branches/main":
				_, _ = w.Write([]byte(`{"commit":{"sha":"` + head508 + `"}}`))
			case p == "/api/v3/repos/acme/repo/contents/docs/a.md" && r.URL.Query().Get("ref") == head508:
				_, _ = w.Write([]byte(`{"type":"file","sha":"` + blob508 + `"}`))
			case p == "/api/v3/repos/acme/repo/contents/docs/link":
				_, _ = w.Write([]byte(`{"type":"symlink","sha":"` + blob508 + `"}`))
			case p == "/api/v3/repos/acme/repo/contents/docs":
				_, _ = w.Write([]byte(`[{"type":"file"}]`))
			default:
				http.NotFound(w, r)
			}
		case ForgeKindGitLab:
			switch {
			case p == "/api/v4/projects/group%2Fsub%2Frepo":
				_, _ = w.Write([]byte(`{"default_branch":"main"}`))
			case p == "/api/v4/projects/group%2Fsub%2Frepo/repository/branches/main":
				_, _ = w.Write([]byte(`{"commit":{"id":"` + head508 + `"}}`))
			case p == "/api/v4/projects/group%2Fsub%2Frepo/repository/files/docs%2Fa.md" && r.URL.Query().Get("ref") == head508:
				_, _ = w.Write([]byte(`{"blob_id":"` + blob508 + `"}`))
			default:
				http.NotFound(w, r)
			}
		case ForgeKindGitea:
			switch p {
			case "/git/api/v1/repos/acme/repo":
				_, _ = w.Write([]byte(`{"default_branch":"trunk"}`))
			case "/git/api/v1/repos/acme/repo/branches/trunk":
				_, _ = w.Write([]byte(`{"commit":{"id":"` + head508 + `"}}`))
			case "/git/api/v1/repos/acme/repo/contents/docs/a.md":
				_, _ = w.Write([]byte(`{"type":"file","sha":"` + blob508 + `"}`))
			default:
				http.NotFound(w, r)
			}
		}
	}))
}

func clientFor508(t *testing.T, srv *httptest.Server, kind, basePath string, repo []string, token string) *forgeHTTPAPI {
	t.Helper()
	u, _ := url.Parse(srv.URL + basePath)
	f := &ProvenanceForge{Name: "f", Kind: kind, Prefix: []string{"x"}, BaseURL: u, Token: token}
	c, ok := newForgeHTTPAPI(scopeTarget{prefix: f.Prefix, repo: repo, forge: f}, f.authHeader())
	if !ok {
		t.Fatalf("no client for %s", kind)
	}
	// The stub's TLS certificate, with the guarded client's redirect policy.
	guarded := c.client
	c.client = srv.Client()
	c.client.CheckRedirect = guarded.CheckRedirect
	return c
}

func TestForgeAPI508_Shapes(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		kind, base, branch, path string
		repo                     []string
	}{
		{ForgeKindGitHub, "", "main", "docs/a.md", []string{"acme", "repo"}},
		{ForgeKindGitLab, "", "main", "docs/a.md", []string{"group", "sub", "repo"}},
		{ForgeKindGitea, "/git", "trunk", "docs/a.md", []string{"acme", "repo"}},
	} {
		t.Run(tc.kind, func(t *testing.T) {
			var seen []string
			srv := forgeStub508(t, tc.kind, &seen)
			defer srv.Close()
			c := clientFor508(t, srv, tc.kind, tc.base, tc.repo, "tok-508")
			b, err := c.DefaultBranch(ctx)
			if err != nil || b != tc.branch {
				t.Fatalf("DefaultBranch = %q, %v", b, err)
			}
			h, err := c.BranchHead(ctx, b)
			if err != nil || h != head508 {
				t.Fatalf("BranchHead = %q, %v", h, err)
			}
			blob, kind, err := c.BlobAt(ctx, h, tc.path)
			if err != nil || blob != blob508 || kind != ForgeEntryFile {
				t.Fatalf("BlobAt = %q %q %v", blob, kind, err)
			}
			if _, _, err := c.BlobAt(ctx, h, "docs/missing.md"); !errors.Is(err, ErrForgeNotFound) {
				t.Errorf("missing path: %v", err)
			}
			if _, err := c.BranchHead(ctx, "nope"); !errors.Is(err, ErrForgeNotFound) {
				t.Errorf("missing branch: %v", err)
			}
			for _, a := range seen {
				if a != "Bearer tok-508" {
					t.Errorf("Authorization = %q", a)
				}
			}
		})
	}
}

func TestForgeAPI508_GitHubEntryKinds(t *testing.T) {
	var seen []string
	srv := forgeStub508(t, ForgeKindGitHub, &seen)
	defer srv.Close()
	c := clientFor508(t, srv, ForgeKindGitHub, "", []string{"acme", "repo"}, "")
	if _, kind, _ := c.BlobAt(context.Background(), head508, "docs/link"); kind != "symlink" {
		t.Errorf("symlink kind = %q", kind)
	}
	if _, kind, _ := c.BlobAt(context.Background(), head508, "docs"); kind != "dir" {
		t.Errorf("directory kind = %q", kind)
	}
	for _, a := range seen {
		if a != "" {
			t.Errorf("anonymous client sent %q", a)
		}
	}
}

// A repository the daemon cannot see (404 on the repository, 401, 5xx) is
// unavailable, so the version stays unexaminable rather than unverified.
func TestForgeAPI508_UnreadableRepositoryIsUnavailable(t *testing.T) {
	for _, code := range []int{http.StatusNotFound, http.StatusUnauthorized, http.StatusForbidden, http.StatusBadGateway} {
		srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(code) }))
		c := clientFor508(t, srv, ForgeKindGitea, "", []string{"acme", "repo"}, "t")
		if _, err := c.DefaultBranch(context.Background()); !errors.Is(err, ErrForgeUnavailable) {
			t.Errorf("HTTP %d: %v", code, err)
		}
		srv.Close()
	}
}

// The API client refuses a redirect (no hop to another host).
func TestForgeAPI508_NoRedirect(t *testing.T) {
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "https://169.254.169.254/latest", http.StatusFound)
	}))
	defer srv.Close()
	c := clientFor508(t, srv, ForgeKindGitea, "", []string{"acme", "repo"}, "t")
	if _, err := c.DefaultBranch(context.Background()); !errors.Is(err, ErrForgeUnavailable) || strings.Contains(err.Error(), "169.254") {
		t.Errorf("redirect: %v", err)
	}
}
