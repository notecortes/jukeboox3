// =====================================================================
// AuraBox - Main Application Entry Point & SSE Event Listener
// Multi-Device Real-Time Synchronization (<50ms) & PWA Bootstrap
// =====================================================================

import * as api from './api.js';
import { state, updatePlayerState, updateQueueState, notifyListeners } from './state.js';
import { initPlayer, showToast } from './player.js';
import { initGestures } from './gestures.js';
import { initQueue } from './queue.js';
import { initLyrics } from './lyrics.js';
import { initViews } from './views.js';

let syncRetryTimer = null;
let activeEventSource = null;
let sseReconnectTimer = null;
let sseRetryAttempt = 0;
let pageUnloading = false;

let streamDesired = false;
let streamReconnectTimer = null;
let streamStallWatchdog = null;
let streamRetryAttempt = 0;

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
  initLifecycleRecovery();

  // Establecer conexión bidireccional en tiempo real con Server-Sent Events
  connectSSE();

  // Registrar PWA Service Worker si es soportado
  registerServiceWorker();

  // Cargar estado inicial y cola con reintento automático si hubiera fallo transitorio
  syncPlayerAndQueue();
});

// closeSSEConnection cierra ordenadamente el canal SSE activo y limpia su temporizador.
function closeSSEConnection() {
  if (sseReconnectTimer) {
    clearTimeout(sseReconnectTimer);
    sseReconnectTimer = null;
  }
  if (activeEventSource) {
    activeEventSource.onopen = null;
    activeEventSource.onerror = null;
    activeEventSource.close();
    activeEventSource = null;
  }
}

// initLifecycleRecovery reconecta SSE y streaming web al despertar el dispositivo o recuperar red.
function initLifecycleRecovery() {
  const handleUnload = () => {
    pageUnloading = true;
    closeSSEConnection();
    clearStreamTimers();
  };

  window.addEventListener('pagehide', handleUnload);
  window.addEventListener('beforeunload', handleUnload);
  window.addEventListener('pageshow', (e) => {
    if (e.persisted) {
      pageUnloading = false;
      sseRetryAttempt = 0;
      connectSSE();
      if (streamDesired) {
        connectWebAudioStream(false);
      }
    }
  });

  window.addEventListener('online', () => {
    if (pageUnloading) return;
    if (!activeEventSource || activeEventSource.readyState === EventSource.CLOSED) {
      sseRetryAttempt = 0;
      connectSSE();
    } else {
      syncPlayerAndQueue();
    }
    if (streamDesired) {
      const audio = document.getElementById('webAudioPlayer');
      if (audio && (audio.error || audio.ended || audio.paused || audio.readyState < 2)) {
        connectWebAudioStream(false);
      }
    }
  });

  document.addEventListener('visibilitychange', () => {
    if (pageUnloading || document.visibilityState !== 'visible') return;
    if (!activeEventSource || activeEventSource.readyState === EventSource.CLOSED) {
      sseRetryAttempt = 0;
      connectSSE();
    } else {
      syncPlayerAndQueue();
    }
    if (streamDesired && state.player.status === 'play') {
      const audio = document.getElementById('webAudioPlayer');
      if (audio && (audio.error || audio.ended || audio.paused || audio.readyState < 2)) {
        connectWebAudioStream(false);
      }
    }
  });
}

// syncPlayerAndQueue sincroniza estado y cola desde REST como respaldo ante reconexiones.
function syncPlayerAndQueue() {
  if (syncRetryTimer) {
    clearTimeout(syncRetryTimer);
    syncRetryTimer = null;
  }

  const scheduleRetry = () => {
    if (!syncRetryTimer) {
      syncRetryTimer = setTimeout(() => {
        syncRetryTimer = null;
        syncPlayerAndQueue();
      }, 2000);
    }
  };

  api.getStatus()
    .then((st) => {
      if (st) updatePlayerState(st);
    })
    .catch(() => {
      scheduleRetry();
    });

  api.getQueue()
    .then((res) => {
      if (res && res.queue) updateQueueState(res.queue);
    })
    .catch(() => {
      scheduleRetry();
    });
}

// connectSSE mantiene el canal de eventos en tiempo real con instancia única y backoff exponencial.
function connectSSE() {
  if (pageUnloading) {
    return;
  }

  closeSSEConnection();

  const evtSource = new EventSource('/api/events');
  activeEventSource = evtSource;

  evtSource.addEventListener('connected', () => {
    if (activeEventSource !== evtSource) return;
    sseRetryAttempt = 0;
    console.log('[SSE] Conexión en tiempo real activa (<50ms latencia)');
    document.getElementById('headerStatusDot')?.style.setProperty('background-color', 'var(--color-success)');
    syncPlayerAndQueue();
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

  evtSource.addEventListener('library_updated', (e) => {
    let payload = null;
    if (e && e.data) {
      try {
        payload = JSON.parse(e.data);
      } catch (err) {
        payload = null;
      }
    }
    notifyListeners('library_updated', payload);
  });

  evtSource.onerror = () => {
    if (activeEventSource !== evtSource) return;
    document.getElementById('headerStatusDot')?.style.setProperty('background-color', 'var(--color-warning)');
    closeSSEConnection();
    if (pageUnloading) return;

    const delays = [1500, 3000, 5000, 8000, 10000];
    const waitMs = delays[Math.min(sseRetryAttempt, delays.length - 1)];
    sseRetryAttempt++;
    sseReconnectTimer = setTimeout(() => {
      sseReconnectTimer = null;
      connectSSE();
    }, waitMs);
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

// clearStreamTimers cancela cualquier temporizador activo de reconexión o bloqueo del audio web.
function clearStreamTimers() {
  if (streamReconnectTimer) {
    clearTimeout(streamReconnectTimer);
    streamReconnectTimer = null;
  }
  if (streamStallWatchdog) {
    clearTimeout(streamStallWatchdog);
    streamStallWatchdog = null;
  }
}

// updateWebStreamUI actualiza apariencia y texto del botón de escucha directa en el móvil.
function updateWebStreamUI(mode, labelOverride = '') {
  const btn = document.getElementById('btnToggleWebStream');
  const btnText = document.getElementById('streamButtonText');
  const icon = document.getElementById('streamIndicatorIcon');
  if (!btn) return;

  switch (mode) {
    case 'playing':
      btn.style.background = 'var(--accent)';
      if (btnText) btnText.textContent = labelOverride || 'En Vivo';
      if (icon) icon.textContent = '🔊';
      break;
    case 'connecting':
      btn.style.background = 'rgba(99, 102, 241, 0.4)';
      if (btnText) btnText.textContent = labelOverride || 'Conectando...';
      if (icon) icon.textContent = '⏳';
      break;
    case 'reconnecting':
      btn.style.background = 'rgba(245, 158, 11, 0.35)';
      if (btnText) btnText.textContent = labelOverride || 'Reconectando...';
      if (icon) icon.textContent = '🔄';
      break;
    default:
      btn.style.background = 'rgba(255, 255, 255, 0.08)';
      if (btnText) btnText.textContent = 'Móvil';
      if (icon) icon.textContent = '📻';
      break;
  }
}

// stopWebAudioStream detiene el reproductor HTML5 y retira el atributo src sin provocar peticiones a '/'.
function stopWebAudioStream() {
  const audio = document.getElementById('webAudioPlayer');
  streamDesired = false;
  streamRetryAttempt = 0;
  clearStreamTimers();

  if (audio) {
    audio.pause();
    audio.removeAttribute('src');
    audio.load();
  }
  updateWebStreamUI('idle');
}

// scheduleWebStreamReconnect programa un reintento escalonado tras un corte temporal del stream web.
function scheduleWebStreamReconnect() {
  if (!streamDesired || pageUnloading || streamReconnectTimer) {
    return;
  }
  if (typeof navigator !== 'undefined' && navigator.onLine === false) {
    updateWebStreamUI('reconnecting');
    return;
  }

  const delays = [1500, 3000, 5000, 8000];
  const waitMs = delays[Math.min(streamRetryAttempt, delays.length - 1)];
  streamRetryAttempt++;
  updateWebStreamUI('reconnecting');

  streamReconnectTimer = setTimeout(() => {
    streamReconnectTimer = null;
    if (streamDesired && !pageUnloading) {
      connectWebAudioStream(false);
    }
  }, waitMs);
}

// startStreamStallWatchdog vigila si el flujo MP3 queda congelado y recicla la conexión si no avanza.
function startStreamStallWatchdog() {
  const audio = document.getElementById('webAudioPlayer');
  if (!audio || !streamDesired || streamStallWatchdog) {
    return;
  }

  const snapshotTime = audio.currentTime || 0;
  streamStallWatchdog = setTimeout(() => {
    streamStallWatchdog = null;
    if (!streamDesired || pageUnloading) {
      return;
    }
    const stillStuck = audio.paused || (audio.currentTime <= snapshotTime + 0.05 && audio.readyState < 3);
    if (stillStuck && state.player.status === 'play') {
      console.warn('[Stream] Flujo bloqueado detectado por watchdog, reciclando conexión...');
      connectWebAudioStream(false);
    }
  }, 12000);
}

// connectWebAudioStream abre el flujo /api/stream (o :4082 en HTTP) con respaldo automático.
function connectWebAudioStream(allowDirectFallback = true) {
  const audio = document.getElementById('webAudioPlayer');
  if (!audio) return;

  clearStreamTimers();
  streamDesired = true;
  updateWebStreamUI('connecting');

  audio.src = `/api/stream?_t=${Date.now()}`;
  audio.load();

  const playPromise = audio.play();
  if (playPromise === undefined) {
    return;
  }

  playPromise.then(() => {
    streamRetryAttempt = 0;
    updateWebStreamUI('playing');
  }).catch((err) => {
    if (!streamDesired || pageUnloading) {
      return;
    }
    if (err && err.name === 'AbortError') {
      return;
    }
    if (err && err.name === 'NotAllowedError') {
      console.warn('[Stream] Reproducción bloqueada por política del navegador:', err);
      stopWebAudioStream();
      showToast('Pulsa de nuevo en Móvil para activar el audio');
      return;
    }

    if (allowDirectFallback && window.location.protocol !== 'https:') {
      console.warn('[Stream] Intentando conexión directa a 4082:', err);
      audio.src = `http://${window.location.hostname}:4082/?_t=${Date.now()}`;
      audio.load();
      audio.play().then(() => {
        streamRetryAttempt = 0;
        updateWebStreamUI('playing', 'En Vivo (4082)');
      }).catch((err2) => {
        console.error('[Stream] No se pudo reproducir stream directo:', err2);
        scheduleWebStreamReconnect();
      });
      return;
    }

    console.warn('[Stream] Corte o error de conexión en audio web, reintentando:', err);
    scheduleWebStreamReconnect();
  });
}

// initWebStreamPlayer permite escuchar el audio del reproductor directamente en el móvil con autorecuperación.
function initWebStreamPlayer() {
  const btn = document.getElementById('btnToggleWebStream');
  const audio = document.getElementById('webAudioPlayer');
  if (!btn || !audio) return;

  audio.addEventListener('playing', () => {
    if (!streamDesired) return;
    streamRetryAttempt = 0;
    clearStreamTimers();
    const isDirect = (audio.currentSrc || '').includes(':4082');
    updateWebStreamUI('playing', isDirect ? 'En Vivo (4082)' : 'En Vivo');
  });

  audio.addEventListener('timeupdate', () => {
    if (streamStallWatchdog) {
      clearTimeout(streamStallWatchdog);
      streamStallWatchdog = null;
    }
  });

  audio.addEventListener('waiting', () => {
    if (streamDesired && !pageUnloading) {
      startStreamStallWatchdog();
    }
  });

  audio.addEventListener('stalled', () => {
    if (streamDesired && !pageUnloading) {
      startStreamStallWatchdog();
    }
  });

  audio.addEventListener('ended', () => {
    if (!streamDesired || pageUnloading || !audio.hasAttribute('src')) return;
    console.warn('[Stream] Flujo finalizado inesperadamente, reconectando...');
    scheduleWebStreamReconnect();
  });

  audio.addEventListener('error', () => {
    if (!streamDesired || pageUnloading || !audio.hasAttribute('src')) return;
    console.warn('[Stream] Error en flujo de audio web, programando reconexión...');
    scheduleWebStreamReconnect();
  });

  btn.addEventListener('click', () => {
    if (streamDesired) {
      stopWebAudioStream();
    } else {
      connectWebAudioStream(true);
    }
  });
}

// registerServiceWorker instala el Service Worker y fuerza la actualización inmediata.
function registerServiceWorker() {
  if ('serviceWorker' in navigator) {
    navigator.serviceWorker.register('/sw.js?v=14').then((reg) => {
      reg.update();
    }).catch((err) => {
      console.warn('[PWA] Service Worker no registrado:', err);
    });
  }
}

