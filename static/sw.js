// CAS Service Worker
// Provides offline shell and asset caching for PWA install.
// Does NOT cache API responses or WebSocket — those are always live.

const CACHE_NAME = 'cas-v1.2.8';

// Static assets to pre-cache on install
const PRECACHE_ASSETS = [
  '/',
  '/manifest.json',
  '/icons/icon-192.png',
  '/icons/icon-512.png',
  // External CDN assets cached on first use (not pre-cached)
];

// Install: pre-cache the app shell
self.addEventListener('install', (event) => {
  event.waitUntil(
    caches.open(CACHE_NAME).then((cache) => cache.addAll(PRECACHE_ASSETS))
  );
  self.skipWaiting();
});

// Activate: clean up old caches
self.addEventListener('activate', (event) => {
  event.waitUntil(
    caches.keys().then((keys) =>
      Promise.all(
        keys
          .filter((key) => key !== CACHE_NAME)
          .map((key) => caches.delete(key))
      )
    )
  );
  self.clients.claim();
});

// Fetch strategy:
//   - API calls (/api/*, /ws) → always network, never cache
//   - Static assets → cache-first with network fallback
//   - Navigation (HTML) → network-first, fall back to cached shell
self.addEventListener('fetch', (event) => {
  const url = new URL(event.request.url);

  // Never intercept API, WebSocket upgrade, or cross-origin requests
  if (
    url.pathname.startsWith('/api/') ||
    url.pathname.startsWith('/ws') ||
    url.pathname.startsWith('/oauth') ||
    event.request.url !== url.origin && !url.hostname.includes(self.location.hostname)
  ) {
    return; // let browser handle it normally
  }

  // Navigation requests: network-first so the app always gets latest HTML
  if (event.request.mode === 'navigate') {
    event.respondWith(
      fetch(event.request)
        .then((response) => {
          const clone = response.clone();
          caches.open(CACHE_NAME).then((cache) => cache.put(event.request, clone));
          return response;
        })
        .catch(() => caches.match('/'))
    );
    return;
  }

  // Static assets: cache-first
  event.respondWith(
    caches.match(event.request).then(
      (cached) => cached || fetch(event.request).then((response) => {
        if (response.ok) {
          const clone = response.clone();
          caches.open(CACHE_NAME).then((cache) => cache.put(event.request, clone));
        }
        return response;
      })
    )
  );
});
