// =====================================================================
// AuraBox - Touch Gestures Manager (Mobile-First Ergonomics)
// Swipe-Up, Swipe-Down, Swipe-Horizontal & Queue Drag-and-Drop
// =====================================================================

import * as api from './api.js?v=21';
import { state, notifyListeners } from './state.js?v=21';
import { renderNowPlaying } from './player.js?v=21';

// initGestures configura los detectores de gestos táctiles para el Mini-Player y Now Playing.
export function initGestures() {
  setupMiniPlayerGestures();
  setupNowPlayingGestures();
}

// setupMiniPlayerGestures gestiona Swipe-Up para desplegar y Swipe-Horizontal para saltar pista.
function setupMiniPlayerGestures() {
  const miniPlayer = document.getElementById('miniPlayer');
  if (!miniPlayer) return;

  let startX = 0;
  let startY = 0;
  let startTime = 0;

  miniPlayer.addEventListener('touchstart', (e) => {
    if (e.target.closest('button')) return; // Permitir pulsación directa en botones
    const touch = e.touches[0];
    startX = touch.clientX;
    startY = touch.clientY;
    startTime = Date.now();
  }, { passive: true });

  miniPlayer.addEventListener('touchend', (e) => {
    if (e.target.closest('button')) return;
    const touch = e.changedTouches[0];
    const diffX = touch.clientX - startX;
    const diffY = touch.clientY - startY;
    const elapsed = Date.now() - startTime;

    // Distinguir entre Tap, Swipe Vertical y Swipe Horizontal
    const absX = Math.abs(diffX);
    const absY = Math.abs(diffY);

    if (absX < 15 && absY < 15 && elapsed < 350) {
      // Tap simple -> Abrir Now Playing
      openNowPlaying();
      return;
    }

    // Swipe Up (deslizar hacia arriba al menos 40px)
    if (diffY < -40 && absY > absX) {
      openNowPlaying();
      return;
    }

    // Swipe Horizontal (deslizar izquierda o derecha al menos 50px)
    if (absX > 50 && absX > absY) {
      if (diffX < 0) {
        // Deslizar izquierda -> Pista siguiente
        triggerNextWithFeedback(miniPlayer);
      } else {
        // Deslizar derecha -> Pista anterior
        triggerPrevWithFeedback(miniPlayer);
      }
    }
  }, { passive: true });
}

// setupNowPlayingGestures gestiona el gesto Swipe-Down en el modal completo para cerrarlo.
function setupNowPlayingGestures() {
  const modal = document.getElementById('nowPlayingModal');
  const grabber = document.getElementById('npGrabber');
  if (!modal || !grabber) return;

  let startY = 0;
  let startScrollTop = 0;

  modal.addEventListener('touchstart', (e) => {
    // Si se desliza en scrubber, slider de volumen o letras, no cerrar
    if (e.target.closest('input') || e.target.closest('.np-lyrics-container')) return;
    startY = e.touches[0].clientY;
    startScrollTop = modal.scrollTop;
  }, { passive: true });

  modal.addEventListener('touchend', (e) => {
    if (e.target.closest('input') || e.target.closest('.np-lyrics-container')) return;
    if (startScrollTop > 2) return;
    const diffY = e.changedTouches[0].clientY - startY;
    // Deslizamiento hacia abajo significativo (> 70px)
    if (diffY > 70) {
      closeNowPlaying();
    }
  }, { passive: true });

  // Cerrar al pulsar el grabber superior directamente
  grabber.addEventListener('click', closeNowPlaying);
}

// openNowPlaying despliega la vista Now Playing a pantalla completa.
export function openNowPlaying() {
  const modal = document.getElementById('nowPlayingModal');
  if (modal) {
    modal.classList.add('open');
    state.ui.isNowPlayingOpen = true;
    renderNowPlaying();
    notifyListeners('now_playing_toggled', true);
  }
}

// closeNowPlaying oculta el modal Now Playing y devuelve el foco a la vista anterior.
export function closeNowPlaying() {
  const modal = document.getElementById('nowPlayingModal');
  if (modal) {
    modal.classList.remove('open');
    state.ui.isNowPlayingOpen = false;
    notifyListeners('now_playing_toggled', false);
  }
}

// triggerNextWithFeedback dispara canción siguiente con animación optimista en el mini-player.
function triggerNextWithFeedback(el) {
  el.style.transform = 'translateX(-20px)';
  el.style.opacity = '0.7';
  setTimeout(() => {
    el.style.transform = 'translateX(0)';
    el.style.opacity = '1';
  }, 180);
  api.next();
}

// triggerPrevWithFeedback dispara canción anterior con animación optimista en el mini-player.
function triggerPrevWithFeedback(el) {
  el.style.transform = 'translateX(20px)';
  el.style.opacity = '0.7';
  setTimeout(() => {
    el.style.transform = 'translateX(0)';
    el.style.opacity = '1';
  }, 180);
  api.prev();
}

