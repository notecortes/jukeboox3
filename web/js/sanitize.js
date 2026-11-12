// =====================================================================
// AuraBox - Frontend Sanitization & Safe Asset Helpers
// Vanilla ES Module (HTML/CSS/JS puro sin dependencias)
// =====================================================================

const DEFAULT_COVER_URL = '/assets/default-cover.svg';

// escapeHtml escapa caracteres especiales HTML para prevenir inyecciones XSS en plantillas DOM.
export function escapeHtml(str) {
  if (str === null || str === undefined) return '';
  return String(str)
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;')
    .replace(/'/g, '&#039;');
}

// sanitizeCoverURL valida que una URL de carátula o logotipo sea relativa segura (/assets/, /api/covers/) o http(s) pública sin caracteres peligrosos.
export function sanitizeCoverURL(rawURL, fallback = DEFAULT_COVER_URL) {
  if (!rawURL || typeof rawURL !== 'string') {
    return fallback;
  }
  const clean = rawURL.trim();
  if (!clean || /[\r\n\x00"'<>`()\\]/.test(clean)) {
    return fallback;
  }
  if (clean.startsWith('/assets/') || clean.startsWith('/api/covers/')) {
    return clean.includes('..') ? fallback : clean;
  }
  try {
    const parsed = new URL(clean, window.location.origin);
    if (parsed.protocol === 'http:' || parsed.protocol === 'https:') {
      return parsed.href;
    }
  } catch (e) {
    return fallback;
  }
  return fallback;
}

// sanitizeHTTPSRedirectURL valida que una URL de acceso HTTPS devuelta por la API use exclusivamente esquema https:.
export function sanitizeHTTPSRedirectURL(rawURL) {
  if (!rawURL || typeof rawURL !== 'string') {
    return '';
  }
  try {
    const parsed = new URL(rawURL.trim(), window.location.origin);
    if (parsed.protocol === 'https:') {
      return parsed.href;
    }
  } catch (e) {
    return '';
  }
  return '';
}

// installImageFallbackListener sustituye manejadores onerror inline aplicando carátula por defecto en imágenes fallidas.
export function installImageFallbackListener() {
  if (window.__auraboxImgFallbackInstalled) return;
  window.__auraboxImgFallbackInstalled = true;

  document.addEventListener('error', (event) => {
    const target = event.target;
    if (!target || target.tagName !== 'IMG') return;
    if (target.dataset.fallbackApplied === '1') return;

    const fallback = target.dataset.fallback || DEFAULT_COVER_URL;
    target.dataset.fallbackApplied = '1';
    target.src = fallback;
  }, true);
}

