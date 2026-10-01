package ui

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"
)

// TestStaticHandler_NoDirectoryListing is the regression test for the
// 2026-10-01 external scan of swarms.vornik.io (finding F3): GET /ui/static/
// is auth-exempt by design, and http.FileServer answered it with an HTML
// index of every vendored asset. Files stay public; directory indexes do not.
func TestStaticHandler_NoDirectoryListing(t *testing.T) {
	h := staticHandler()
	for _, path := range []string{"/static/", "/static", "/static/./", "/static//"} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code == http.StatusOK {
			t.Errorf("GET %s = 200 (body %.60q); a directory must not be listed", path, rec.Body.String())
		}
	}
	// The assets themselves must still be served — the login page needs them.
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/static/htmx.min.js", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /static/htmx.min.js = %d, want 200", rec.Code)
	}
}

// TestNoDirFS_ServesDirectoryIndex pins the review point on the F3 fix: a
// directory that carries an index.html is a page, not a listing, and must
// still be served if a vendored component ever ships one.
func TestNoDirFS_ServesDirectoryIndex(t *testing.T) {
	fsys := fstest.MapFS{
		"site/index.html": {Data: []byte("<p>page</p>")},
		"bare/a.js":       {Data: []byte("x")},
	}
	h := http.FileServer(http.FS(noDirFS{fsys}))
	for path, want := range map[string]int{"/site/": http.StatusOK, "/bare/": http.StatusNotFound, "/bare/a.js": http.StatusOK} {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != want {
			t.Errorf("GET %s = %d, want %d", path, rec.Code, want)
		}
	}
}
