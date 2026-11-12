// =====================================================================
// AuraBox Service Worker - PWA Caching & Offline Recovery
// =====================================================================

const CACHE_NAME = 'aurabox-v19';
const STATIC_ASSETS = [
  '/',
  '/manifest.json',
  '/css/app.css?v=12',
  '/css/player.css?v=14',
  '/css/views.css?v=13',
  '/js/app.js?v=19',
  '/js/sanitize.js',
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

// escapeSWText escapa caracteres especiales HTML en la vista offline generada por el Service Worker.
function escapeSWText(str) {
  return String(str || '')
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#039;');
}

// buildOfflinePageResponse genera una página HTML autónoma de recuperación cuando no hay conexión con esta URL/puerto.
function buildOfflinePageResponse(requestUrl) {
  let host = self.location.hostname || '127.0.0.1';
  try {
    host = new URL(requestUrl).hostname || host;
  } catch (e) {}
  host = host.replace(/[^a-zA-Z0-9.\-:\[\]]/g, '') || '127.0.0.1';

  const httpDirect = `http://${host}:4080/`;
  const httpBare = `http://${host}/`;
  const httpsDirect = `https://${host}:4443/`;

  const html = `<!DOCTYPE html>
<html lang="es">
<head>
  <meta charset="UTF-8"/>
  <meta name="viewport" content="width=device-width, initial-scale=1.0"/>
  <title>AuraBox — Sin conexión</title>
  <style>
    body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;background:#08090d;color:#f3f4f6;font-family:system-ui,-apple-system,sans-serif;padding:20px;box-sizing:border-box;}
    .card{max-width:440px;width:100%;background:#121521;border:1px solid rgba(255,255,255,0.1);border-radius:18px;padding:24px;box-shadow:0 16px 40px rgba(0,0,0,0.45);}
    h1{margin:0 0 8px;font-size:1.2rem;}
    p{margin:0 0 14px;color:#9ca3af;font-size:0.9rem;line-height:1.45;}
    code{background:rgba(255,255,255,0.08);padding:2px 6px;border-radius:6px;font-size:0.82rem;color:#e5e7eb;}
    .actions{display:flex;flex-direction:column;gap:10px;margin-top:16px;}
    a,button{display:flex;align-items:center;justify-content:center;gap:8px;min-height:44px;border-radius:12px;border:1px solid rgba(255,255,255,0.12);background:rgba(255,255,255,0.06);color:#f9fafb;font-weight:600;font-size:0.92rem;text-decoration:none;cursor:pointer;padding:8px 14px;}
    .primary{background:#6366f1;border-color:#6366f1;}
    .danger{background:rgba(239,68,68,0.16);border-color:rgba(239,68,68,0.4);color:#fca5a5;}
  </style>
</head>
<body>
  <div class="card">
    <h1>No se puede conectar con AuraBox</h1>
    <p>El servidor no responde en <code>${escapeSWText(self.location.origin)}</code>. Si acabas de desactivar HTTPS o cambiar los puertos en la Raspberry Pi, entra por la dirección activa:</p>
    <div class="actions">
      <a class="primary" href="${escapeSWText(httpDirect)}">Abrir por HTTP (${escapeSWText(httpDirect)})</a>
      <a href="${escapeSWText(httpBare)}">Abrir por HTTP puerto 80 (${escapeSWText(httpBare)})</a>
      <a href="${escapeSWText(httpsDirect)}">Abrir por HTTPS directo (${escapeSWText(httpsDirect)})</a>
      <button type="button" id="btnRetryConnection">Reintentar aquí</button>
      <button type="button" class="danger" id="btnResetSW">Limpiar caché PWA de este navegador</button>
    </div>
  </div>
  <script>
    document.getElementById('btnRetryConnection').addEventListener('click', () => {
      location.reload();
    });
    document.getElementById('btnResetSW').addEventListener('click', async () => {
      try {
        if ('serviceWorker' in navigator) {
          const regs = await navigator.serviceWorker.getRegistrations();
          await Promise.all(regs.map((r) => r.unregister()));
        }
        if ('caches' in window) {
          const keys = await caches.keys();
          await Promise.all(keys.map((k) => caches.delete(k)));
        }
      } catch (e) {}
      location.href = ${JSON.stringify(httpDirect)};
    });
  </script>
</body>
</html>`;

  return new Response(html, {
    status: 503,
    statusText: 'Service Unavailable',
    headers: {
      'Content-Type': 'text/html; charset=utf-8',
      'Cache-Control': 'no-store'
    }
  });
}

// handleNetworkFirst resuelve primero contra red y garantiza siempre una respuesta HTTP válida sin pasar Error Responses.
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
    if (request.mode === 'navigate') {
      return buildOfflinePageResponse(request.url);
    }

    const exactMatch = await caches.match(request);
    if (exactMatch) {
      return exactMatch;
    }

    const looseMatch = await caches.match(request, { ignoreSearch: true });
    if (looseMatch) {
      return looseMatch;
    }

    return new Response('', {
      status: 503,
      statusText: 'Service Unavailable',
      headers: {
        'Content-Type': 'text/plain; charset=utf-8',
        'Cache-Control': 'no-store'
      }
    });
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
