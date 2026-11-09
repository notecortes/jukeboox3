// =====================================================================
// AuraBox Service Worker - PWA Caching & Offline Capabilities
// =====================================================================

const CACHE_NAME = 'aurabox-v13';
const STATIC_ASSETS = [
  '/',
  '/index.html',
  '/manifest.json',
  '/css/app.css',
  '/css/player.css',
  '/css/views.css',
  '/js/app.js',
  '/js/state.js',
  '/js/api.js',
  '/js/gestures.js',
  '/js/player.js',
  '/js/queue.js',
  '/js/lyrics.js',
  '/js/views.js',
  '/assets/default-cover.svg'
];

// installListener almacena en caché los recursos estáticos esenciales.
self.addEventListener('install', (event) => {
  self.skipWaiting();
  event.waitUntil(
    caches.open(CACHE_NAME).then((cache) => {
      return cache.addAll(STATIC_ASSETS);
    })
  );
});

// activateListener purga cachés obsoletas tras actualizaciones del reproductor.
self.addEventListener('activate', (event) => {
  event.waitUntil(
    caches.keys().then((keys) => {
      return Promise.all(
        keys.filter((key) => key !== CACHE_NAME).map((key) => caches.delete(key))
      );
    }).then(() => self.clients.claim())
  );
});

// fetchListener utiliza estrategia Network-First para asegurar que el navegador siempre reciba el código más reciente.
self.addEventListener('fetch', (event) => {
  if (event.request.method !== 'GET') {
    return;
  }

  const url = new URL(event.request.url);
  if (url.origin !== self.location.origin) {
    return;
  }

  // No interceptar peticiones dinámicas de API ni flujo SSE
  if (url.pathname.startsWith('/api/')) {
    return;
  }

  // Network-First: intentar siempre red primero para obtener las últimas funciones
  event.respondWith(
    fetch(event.request)
      .then((networkResponse) => {
        if (networkResponse && networkResponse.status === 200) {
          const clone = networkResponse.clone();
          caches.open(CACHE_NAME).then((cache) => cache.put(event.request, clone));
        }
        return networkResponse;
      })
      .catch(() => {
        return caches.match(event.request);
      })
  );
});

