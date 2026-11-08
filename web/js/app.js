// =====================================================================
// AuraBox - Main Application Entry Point & SSE Event Listener
// Multi-Device Real-Time Synchronization (<50ms) & PWA Bootstrap
// =====================================================================

import * as api from './api.js';
import { state, updatePlayerState, updateQueueState, notifyListeners } from './state.js';
import { initPlayer } from './player.js';
import { initGestures } from './gestures.js';
import { initQueue } from './queue.js';
import { initLyrics } from './lyrics.js';
import { initViews } from './views.js';

// bootstrap inicializa todos los módulos, suscripción SSE y Service Worker.
document.addEventListener('DOMContentLoaded', () => {
  console.log('[AuraBox] Iniciando sistema de audio audiófilo...');

  initDockNavigation();
  initPlayer();
  initGestures();
  initQueue();
  initLyrics();
  initViews();
  initWebStreamPlayer();
  initKeyboardShortcuts();
  initHistoryHandling();

  // Establecer conexión bidireccional en tiempo real con Server-Sent Events
  connectSSE();

  // Registrar PWA Service Worker si es soportado
  registerServiceWorker();

  // Cargar estado inicial
  api.getStatus().then(updatePlayerState).catch(() => {});
  api.getQueue().then((res) => {
    if (res && res.queue) updateQueueState(res.queue);
  }).catch(() => {});
});

// connectSSE mantiene el canal de eventos en tiempo real con reintento automático.
function connectSSE() {
  const evtSource = new EventSource('/api/events');

  evtSource.addEventListener('connected', () => {
    console.log('[SSE] Conexión en tiempo real activa (<50ms latencia)');
    document.getElementById('headerStatusDot')?.style.setProperty('background-color', 'var(--color-success)');
  });

  evtSource.addEventListener('player_state', (e) => {
    try {
      const data = JSON.parse(e.data);
      updatePlayerState(data);
    } catch (err) {
      console.error('[SSE] Error al procesar player_state:', err);
    }
  });

  evtSource.addEventListener('queue_update', (e) => {
    try {
      const data = JSON.parse(e.data);
      if (data && data.queue) {
        updateQueueState(data.queue);
      }
    } catch (err) {
      console.error('[SSE] Error al procesar queue_update:', err);
    }
  });

  evtSource.addEventListener('library_updated', () => {
    notifyListeners('library_updated');
  });

  evtSource.onerror = () => {
    document.getElementById('headerStatusDot')?.style.setProperty('background-color', 'var(--color-warning)');
    evtSource.close();
    // Reintento en 3 segundos
    setTimeout(connectSSE, 3000);
  };
}

// initDockNavigation gestiona la conmutación ergonómica entre las 5 secciones principales.
function initDockNavigation() {
  const dockItems = document.querySelectorAll('.dock-item');
  const views = {
    library: document.getElementById('viewLibrary'),
    search: document.getElementById('viewSearch'),
    mixes: document.getElementById('viewMixes'),
    queue: document.getElementById('viewQueue'),
    settings: document.getElementById('viewSettings')
  };

  dockItems.forEach((item) => {
    item.addEventListener('click', () => {
      const targetTab = item.dataset.tab;
      dockItems.forEach((i) => i.classList.remove('active'));
      item.classList.add('active');

      Object.keys(views).forEach((vKey) => {
        if (views[vKey]) {
          views[vKey].classList.toggle('active', vKey === targetTab);
        }
      });

      state.ui.activeTab = targetTab;
      window.history.pushState({ tab: targetTab }, '', `#${targetTab}`);
    });
  });
}

// initHistoryHandling evita que el botón atrás de Android / iOS cierre la PWA accidentalmente.
function initHistoryHandling() {
  window.addEventListener('popstate', (e) => {
    // Si Now Playing está abierto, cerrarlo en lugar de salir
    if (state.ui.isNowPlayingOpen) {
      const modal = document.getElementById('nowPlayingModal');
      if (modal) {
        modal.classList.remove('open');
        state.ui.isNowPlayingOpen = false;
        notifyListeners('now_playing_toggled', false);
        return;
      }
    }

    if (e.state && e.state.tab) {
      const dockBtn = document.querySelector(`.dock-item[data-tab="${e.state.tab}"]`);
      if (dockBtn) dockBtn.click();
    }
  });
}

// initKeyboardShortcuts habilita controles desde teclado físico / mandos multimedia.
function initKeyboardShortcuts() {
  window.addEventListener('keydown', (e) => {
    if (e.target.tagName === 'INPUT' || e.target.tagName === 'TEXTAREA') return;

    switch (e.code) {
      case 'Space':
        e.preventDefault();
        api.toggle();
        break;
      case 'ArrowRight':
        api.next();
        break;
      case 'ArrowLeft':
        api.prev();
        break;
      case 'ArrowUp':
        e.preventDefault();
        api.stepVolume(5);
        break;
      case 'ArrowDown':
        e.preventDefault();
        api.stepVolume(-5);
        break;
    }
  });
}

// initWebStreamPlayer permite escuchar el audio del reproductor directamente en el móvil / navegador.
function initWebStreamPlayer() {
  const btn = document.getElementById('btnToggleWebStream');
  const audio = document.getElementById('webAudioPlayer');
  const btnText = document.getElementById('streamButtonText');
  const icon = document.getElementById('streamIndicatorIcon');
  if (!btn || !audio) return;

  btn.addEventListener('click', () => {
    if (!audio.paused && audio.src) {
      audio.pause();
      audio.src = '';
      audio.load();
      btn.style.background = 'rgba(255, 255, 255, 0.08)';
      if (btnText) btnText.textContent = 'Móvil';
      if (icon) icon.textContent = '📻';
    } else {
      btn.style.background = 'rgba(99, 102, 241, 0.4)';
      if (btnText) btnText.textContent = 'Conectando...';

      const streamSrc = `/api/stream?_t=${Date.now()}`;
      audio.src = streamSrc;
      audio.load();

      const playPromise = audio.play();
      if (playPromise !== undefined) {
        playPromise.then(() => {
          btn.style.background = 'var(--accent)';
          if (btnText) btnText.textContent = 'En Vivo';
          if (icon) icon.textContent = '🔊';
        }).catch((err) => {
          console.warn('[Stream] Intentando conexión directa a 4082:', err);
          audio.src = `http://${window.location.hostname}:4082/?_t=${Date.now()}`;
          audio.load();
          audio.play().then(() => {
            btn.style.background = 'var(--accent)';
            if (btnText) btnText.textContent = 'En Vivo (4082)';
            if (icon) icon.textContent = '🔊';
          }).catch((err2) => {
            console.error('[Stream] No se pudo reproducir stream:', err2);
            btn.style.background = 'rgba(255, 255, 255, 0.08)';
            if (btnText) btnText.textContent = 'Móvil';
            if (icon) icon.textContent = '📻';
          });
        });
      }
    }
  });
}

// registerServiceWorker instala el Service Worker y fuerza la actualización inmediata.
function registerServiceWorker() {
  if ('serviceWorker' in navigator) {
    navigator.serviceWorker.register('/sw.js?v=8').then((reg) => {
      reg.update();
    }).catch((err) => {
      console.warn('[PWA] Service Worker no registrado:', err);
    });
  }
}

