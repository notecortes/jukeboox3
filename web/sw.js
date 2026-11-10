// =====================================================================
// AuraBox Service Worker - PWA Caching & Offline Capabilities
// =====================================================================

const CACHE_NAME = 'aurabox-v14';
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

// shouldBypassServiceWorker determina si una solicitud debe ir directa a red sin caché ni intercepción.
function shouldBypassServiceWorker(request, url) {
  if (request.method !== 'GET') {
    return true;
  }
  if (url.origin !== self.location.origin) {
    return true;
  }
  if (url.pathname.startsWith('/api/')) {
    return true;
  }
  if (request.headers.has('range')) {
    return true;
  }
  if (request.destination === 'audio' || request.destination === 'video') {
    return true;
  }
  if (request.cache === 'only-if-cached' && request.mode !== 'same-origin') {
    return true;
  }
  return false;
}

// handleNetworkFirst resuelve primero contra red y garantiza siempre un Response válido en respaldo offline.
async function handleNetworkFirst(request) {
  try {
    const networkResponse = await fetch(request);
    if (networkResponse && networkResponse.status === 200 && networkResponse.type === 'basic') {
      const clone = networkResponse.clone();
      caches.open(CACHE_NAME)
        .then((cache) => cache.put(request, clone))
        .catch(() => {});
    }
    return networkResponse;
  } catch (err) {
    const cachedResponse = await caches.match(request);
    if (cachedResponse) {
      return cachedResponse;
    }
    if (request.mode === 'navigate') {
      const indexFallback = await caches.match('/index.html');
      if (indexFallback) {
        return indexFallback;
      }
    }
    return Response.error();
  }
}

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

// fetchListener utiliza estrategia Network-First evitando interceptar flujos multimedia ni APIs.
self.addEventListener('fetch', (event) => {
  const url = new URL(event.request.url);
  if (shouldBypassServiceWorker(event.request, url)) {
    return;
  }

  event.respondWith(handleNetworkFirst(event.request));
});

