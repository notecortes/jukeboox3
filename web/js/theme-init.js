/**
 * theme-init.js
 * Inicialización inmediata del tema visual (claro/oscuro) de AuraBox.
 * Se ejecuta síncronamente antes de la renderización del DOM para evitar destellos (FOUC).
 */

// initEarlyTheme configura el atributo data-theme en el documento HTML y el color de barra meta según preferencia guardada o del sistema.
(function initEarlyTheme() {
  try {
    var savedTheme = localStorage.getItem('aurabox_theme');
    var theme = savedTheme || (window.matchMedia && window.matchMedia('(prefers-color-scheme: light)').matches ? 'light' : 'dark');
    document.documentElement.setAttribute('data-theme', theme);
    var metaTheme = document.querySelector('meta[name="theme-color"]');
    if (metaTheme) {
      metaTheme.setAttribute('content', theme === 'light' ? '#f8fafc' : '#090a0f');
    }
  } catch (e) {
    // Modo seguro sin interrupción de ejecución
  }
})();
