package ui

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"net/http"
	"sync"
)

// swVersionPlaceholder is the literal in static/sw.js that serviceWorkerHandler
// replaces with staticVersion(staticFS), so the worker's bytes change whenever
// a file it caches changes (web-ui-design.md "The console service worker, the
// CSP plan and the placeholder colour — 2026-10-17 (#416)" §B).
const swVersionPlaceholder = "__VORNIK_STATIC_VERSION__"

// swPath is the worker script inside staticFS. It is excluded from its own
// version: the version names the files the worker caches, not the worker.
const swPath = "static/sw.js"

// staticVersion is the first 12 hex of a SHA-256 over every file under
// static/ except the worker itself, as path, NUL, bytes, NUL, in WalkDir's
// lexical order. A renamed file changes it as well as an edited one.
func staticVersion(fsys fs.FS) string {
	h := sha256.New()
	_ = fs.WalkDir(fsys, "static", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || p == swPath {
			return nil
		}
		b, rerr := fs.ReadFile(fsys, p)
		if rerr != nil {
			// staticFS is embedded, so a read cannot fail at run time; a
			// file skipped here would only make the version less sensitive.
			return nil
		}
		h.Write([]byte(p))
		h.Write([]byte{0})
		h.Write(b)
		h.Write([]byte{0})
		return nil
	})
	return hex.EncodeToString(h.Sum(nil))[:12]
}

var (
	swOnce  sync.Once
	swBytes []byte
)

// serviceWorkerHandler serves /ui/static/sw.js (mounted as the exact pattern
// /static/sw.js, which wins over the /static/ file server).
//
// Service-Worker-Allowed: /ui/ lets pageHead register it with scope /ui/;
// without it a script's maximum scope is its own directory and registration
// was refused in every browser (GitHub #416). The script stays under the
// auth-exempt /ui/static/ prefix: moved under /ui/ it would sit behind the
// auth middleware, and a worker script fetch that is redirected to the login
// page fails. no-cache keeps an intermediary from pinning an old worker.
func serviceWorkerHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		swOnce.Do(func() {
			b, err := fs.ReadFile(staticFS, swPath)
			if err != nil {
				return // embedded at build time; absent only in a broken build
			}
			swBytes = bytes.ReplaceAll(b, []byte(swVersionPlaceholder), []byte(staticVersion(staticFS)))
		})
		if swBytes == nil {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "text/javascript; charset=utf-8")
		w.Header().Set("Service-Worker-Allowed", "/ui/")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(swBytes)
	})
}
