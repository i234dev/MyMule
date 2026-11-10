"use strict";
// MyMule's service worker: it lets the web interface be installed as an
// app, and opens it without a network (showing the last page). The page
// and its files come from the network first, the copy only when it fails;
// the API is never kept.
const CACHE = "mymule-v1";
const SHELL = ["/", "/assets/app.css", "/assets/app.js", "/assets/terms.js", "/assets/icon.svg"];

self.addEventListener("install", e => {
  e.waitUntil(caches.open(CACHE).then(c => c.addAll(SHELL)).then(() => self.skipWaiting()));
});

self.addEventListener("activate", e => {
  e.waitUntil(caches.keys()
    .then(keys => Promise.all(keys.filter(k => k !== CACHE).map(k => caches.delete(k))))
    .then(() => self.clients.claim()));
});

self.addEventListener("fetch", e => {
  const url = new URL(e.request.url);
  if (e.request.method !== "GET" || url.origin !== location.origin) return;
  if (url.pathname !== "/" && !url.pathname.startsWith("/assets/")) return; // the API: never kept
  e.respondWith(fetch(e.request).then(res => {
    if (res.ok) {
      const copy = res.clone();
      caches.open(CACHE).then(c => c.put(e.request, copy));
    }
    return res;
  }).catch(() => caches.match(e.request, { ignoreSearch: true })));
});
