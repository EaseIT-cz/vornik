// Console service worker (Phase 42; rewritten for GitHub #416).
//
// Goal: install-to-home-screen on iOS Safari + Android Chrome, and a cache of
// the static assets under /ui/static/.
//
// It caches NO page and no per-user response. Cache Storage is per origin and
// per browser profile, not per session: a cached page outlives logout and is
// readable by the next person who uses the profile, and the Cache API ignores
// Cache-Control: no-store. Everything except /ui/static/* (navigations, HTMX
// swaps, /api/v1/, SSE) is left to the network untouched.
//
// /ui/static/* is auth-exempt and holds nothing per user. It is served
// cache-first; only ok, same-origin responses are stored, so a 404 or 500 is
// never pinned. The daemon serves this script with __VORNIK_STATIC_VERSION__
// replaced by a hash of the embedded static tree, so a release that changes
// any static file changes these bytes: the browser installs the new worker,
// which drops every older cache on activate.
//
// web-ui-design.md "The console service worker, the CSP plan and the
// placeholder colour — 2026-10-17 (#416)".

const VERSION = '__VORNIK_STATIC_VERSION__';
const STATIC_CACHE = `vornik-static-${VERSION}`;

const STATIC_PRECACHE = [
  '/ui/static/htmx.min.js',
  '/ui/static/htmx-ext-sse.js',
  '/ui/static/manifest.webmanifest',
  '/ui/static/icon.svg',
];

self.addEventListener('install', (event) => {
  event.waitUntil(
    caches.open(STATIC_CACHE).then((cache) =>
      // Pre-cache best-effort; missing assets don't block install.
      Promise.allSettled(STATIC_PRECACHE.map((url) => cache.add(url)))
    )
  );
  self.skipWaiting();
});

self.addEventListener('activate', (event) => {
  event.waitUntil(
    caches.keys().then((keys) =>
      Promise.all(keys.filter((k) => k !== STATIC_CACHE).map((k) => caches.delete(k)))
    )
  );
  self.clients.claim();
});

self.addEventListener('fetch', (event) => {
  const req = event.request;
  if (req.method !== 'GET') return;
  const url = new URL(req.url);
  if (url.origin !== self.location.origin) return;
  if (!url.pathname.startsWith('/ui/static/')) return;

  event.respondWith(
    caches.match(req).then((hit) => hit || fetch(req).then((res) => {
      if (res.ok && res.type === 'basic') {
        const copy = res.clone();
        event.waitUntil(caches.open(STATIC_CACHE).then((c) => c.put(req, copy)));
      }
      return res;
    }))
  );
});
