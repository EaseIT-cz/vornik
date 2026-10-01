package ui

import (
	"io/fs"
	"net/http"
	"path"
)

// staticHandler serves the vendored assets under /static/. The /ui/static/
// prefix is auth-exempt (api isPublicEndpoint) so the login page's chrome
// loads, which makes this handler reachable by anyone: it serves files and
// answers 404 for directories, so the tree is not enumerable by index.
func staticHandler() http.Handler {
	return http.StripPrefix("/", http.FileServer(http.FS(noDirFS{staticFS})))
}

// noDirFS hides index-less directories: Open on one reports fs.ErrNotExist,
// which http.FileServer renders as 404 instead of an HTML listing.
type noDirFS struct{ fs.FS }

func (n noDirFS) Open(name string) (fs.File, error) {
	f, err := n.FS.Open(name)
	if err != nil {
		return nil, err
	}
	st, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fs.ErrNotExist
	}
	if st.IsDir() {
		// A directory with an index.html is a page, and FileServer serves the
		// index rather than listing; only an index-less directory is hidden.
		if _, err := fs.Stat(n.FS, path.Join(name, "index.html")); err == nil {
			return f, nil
		}
		_ = f.Close()
		return nil, fs.ErrNotExist
	}
	return f, nil
}
