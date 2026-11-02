// Anchor's service worker, served at /sw.js by serviceWorker in pwa.go,
// which fills in the asset version and the files to cache.
//
// It caches only static files and the offline page. Pages go to the network
// every time: they're per user and carry CSRF tokens. When the network is
// gone, a page request gets the offline page instead of the browser's error.
"use strict";

var CACHE = "anchor-" + "__VERSION__";
var PRECACHE = __PRECACHE__;
var OFFLINE = "/offline";

self.addEventListener("install", function (e) {
  e.waitUntil(
    caches.open(CACHE)
      // Past the HTTP cache: the unversioned files may be cached for a day.
      .then(function (c) {
        return c.addAll(PRECACHE.map(function (u) { return new Request(u, { cache: "reload" }); }));
      })
      .then(function () { return self.skipWaiting(); })
  );
});

self.addEventListener("activate", function (e) {
  e.waitUntil(
    caches.keys()
      .then(function (keys) {
        return Promise.all(keys.filter(function (k) { return k !== CACHE; })
          .map(function (k) { return caches.delete(k); }));
      })
      .then(function () { return self.clients.claim(); })
  );
});

self.addEventListener("fetch", function (e) {
  var req = e.request;
  if (req.method !== "GET") return;
  if (req.mode === "navigate") {
    e.respondWith(fetch(req).catch(function () { return caches.match(OFFLINE); }));
    return;
  }
  var url = new URL(req.url);
  if (url.origin !== location.origin) return;
  if (url.pathname.indexOf("/static/") === 0 || url.pathname.indexOf("/icon/") === 0) {
    e.respondWith(caches.match(req).then(function (hit) { return hit || fetch(req); }));
  }
});
