// =====================================================================
// AuraBox - Jukebox Queue Controller
// Touch Drag & Drop Reordering & Swipe-to-Delete
// =====================================================================

import * as api from './api.js';
import { state, subscribe, updatePlayerState } from './state.js';
import { escapeHtml, sanitizeCoverURL } from './sanitize.js';
import { formatTime, isLiveStreamTrack, showToast } from './player.js';
import { openNowPlaying } from './gestures.js';

// initQueue configura los escuchadores de la vista de cola.
export function initQueue() {
  const btnClear = document.getElementById('btnQueueClear');
  const btnShuffle = document.getElementById('btnQueueShuffle');

  if (btnClear) {
    btnClear.addEventListener('click', () => {
      api.clearQueue();
      showToast('Cola vaciada');
    });
  }

  if (btnShuffle) {
    btnShuffle.addEventListener('click', () => {
      api.shuffleQueue();
      showToast('Barajado anti-repetición aplicado');
    });
  }

  subscribe((event) => {
    if (event === 'queue_updated' || event === 'player_updated') {
      renderQueueList();
    }
  });
}

// renderQueueList dibuja las canciones encoladas con indicadores de tiempo y manejadores de arrastre.
export function renderQueueList() {
  const container = document.getElementById('queueList');
  const totalCountEl = document.getElementById('queueTotalCount');
  if (!container) return;

  const queue = state.queue || [];
  if (totalCountEl) {
    let totalDur = 0;
    let liveCount = 0;
    queue.forEach((t) => {
      if (isLiveStreamTrack(t) && (!t.duration || t.duration <= 0)) {
        liveCount += 1;
      } else {
        totalDur += (t.duration || 0);
      }
    });
    if (queue.length > 0 && liveCount === queue.length) {
      totalCountEl.textContent = `${queue.length} radio${queue.length > 1 ? 's' : ''} en vivo`;
    } else {
      totalCountEl.textContent = `${queue.length} temas • ${formatTime(totalDur)}`;
    }
  }

  if (queue.length === 0) {
    container.innerHTML = `
      <div style="text-align:center; padding:48px 16px; color:var(--text-muted);">
        <p style="font-size:1.1rem; margin-bottom:8px;">La cola está vacía</p>
        <p style="font-size:0.85rem;">Explora álbumes o crea un Smart Mix para empezar a escuchar</p>
      </div>
    `;
    return;
  }

  container.innerHTML = '';

  // 1. Tarjeta Destacada "Ahora Suena" en formato grande
  if (state.player.current_track) {
    const cur = state.player.current_track;
    const curIsLive = isLiveStreamTrack(cur);
    const curHasKnownDur = (cur.duration && cur.duration > 0);
    const heroSubtitle = (curIsLive && state.player.stream_title)
      ? `🎙️ ${state.player.stream_title} • ${cur.artist || 'Radio en Directo'}`
      : `${cur.artist || 'Artista'} • ${cur.album || (curIsLive ? 'Radio Web' : 'Álbum')}`;
    const heroTimeLabel = (curIsLive && !curHasKnownDur)
      ? `${formatTime(state.player.elapsed_time || 0)} • EN VIVO`
      : `${formatTime(state.player.elapsed_time || 0)} / ${formatTime(cur.duration || 0)}`;

    const heroCard = document.createElement('div');
    heroCard.className = 'queue-hero-card';
    heroCard.style.cssText = `
      background: linear-gradient(135deg, rgba(99,102,241,0.22) 0%, rgba(15,23,42,0.85) 100%);
      border: 1px solid var(--accent);
      border-radius: var(--radius-lg);
      padding: 14px;
      margin-bottom: 16px;
      display: flex;
      flex-direction: column;
      gap: 12px;
      box-shadow: 0 8px 24px rgba(0,0,0,0.5), 0 0 20px rgba(99,102,241,0.25);
    `;

    heroCard.innerHTML = `
      <div style="display:flex; justify-content:space-between; align-items:center;">
        <span style="font-size:0.75rem; font-weight:800; color:var(--accent-light); text-transform:uppercase; letter-spacing:0.05em; display:flex; align-items:center; gap:6px;">
          <span style="width:8px; height:8px; border-radius:50%; background:#10b981; box-shadow:0 0 8px #10b981; display:inline-block;"></span>
          Ahora Suena en AuraBox
        </span>
        <button id="btnQueueOpenFullscreen" class="btn-secondary" style="font-size:0.75rem; padding:4px 10px; font-weight:700; background:var(--accent); border-color:var(--accent); color:#ffffff; border-radius:var(--radius-full);">
          ⛶ Ver Pantalla Completa
        </button>
      </div>

      <div style="display:flex; align-items:center; gap:14px; cursor:pointer;" id="queueHeroClickableArea">
        <img src="${escapeHtml(sanitizeCoverURL(cur.cover_url))}" data-fallback="/assets/default-cover.svg" style="width:68px; height:68px; min-width:68px; border-radius:var(--radius-md); object-fit:cover; box-shadow:0 4px 12px rgba(0,0,0,0.5);" alt="Carátula"/>
        <div style="flex:1; min-width:0;">
          <div style="font-size:1.05rem; font-weight:700; color:#ffffff; white-space:nowrap; overflow:hidden; text-overflow:ellipsis;">${escapeHtml(cur.title || 'Canción')}</div>
          <div style="font-size:0.88rem; color:var(--text-secondary); margin-top:2px; white-space:nowrap; overflow:hidden; text-overflow:ellipsis;">${escapeHtml(heroSubtitle)}</div>
          <div style="display:flex; align-items:center; gap:8px; margin-top:6px;">
            <span class="badge-format" style="font-size:0.7rem; padding:2px 8px;">${escapeHtml(cur.format || (curIsLive ? 'STREAM' : 'FLAC'))} ${escapeHtml(cur.bitrate || 128)}kbps</span>
            <span style="font-size:0.75rem; color:var(--text-muted);">${escapeHtml(heroTimeLabel)}</span>
          </div>
        </div>
      </div>
    `;

    heroCard.querySelector('#btnQueueOpenFullscreen')?.addEventListener('click', (e) => {
      e.stopPropagation();
      openNowPlaying();
    });

    heroCard.querySelector('#queueHeroClickableArea')?.addEventListener('click', () => {
      openNowPlaying();
    });

    container.appendChild(heroCard);
  }

  // 2. Separador de la lista encolada
  const queueTitleHeader = document.createElement('div');
  queueTitleHeader.style.cssText = 'font-size:0.8rem; font-weight:700; color:var(--text-muted); text-transform:uppercase; margin-bottom:8px; padding-left:4px;';
  queueTitleHeader.textContent = 'A continuación en la cola:';
  container.appendChild(queueTitleHeader);

  // 3. Pistas de la cola
  queue.forEach((track, index) => {
    const item = document.createElement('div');
    item.className = `queue-item ${index === state.player.current_index ? 'active' : ''}`;
    item.dataset.index = index;
    const itemIsLive = isLiveStreamTrack(track);
    const durText = (itemIsLive && (!track.duration || track.duration <= 0))
      ? 'EN VIVO'
      : formatTime(track.duration);

    item.innerHTML = `
      <div class="queue-drag-handle" title="Arrastrar para mover">
        <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2">
          <line x1="8" y1="6" x2="16" y2="6"/>
          <line x1="8" y1="12" x2="16" y2="12"/>
          <line x1="8" y1="18" x2="16" y2="18"/>
        </svg>
      </div>
      <div class="track-info">
        <div class="track-title">${escapeHtml(track.title)}</div>
        <div class="track-artist">${escapeHtml(track.artist)}</div>
      </div>
      <div class="track-right">
        <span class="track-duration">${durText}</span>
        <button class="btn-icon" style="width:32px; height:32px; min-width:32px; border:none; background:transparent;" title="Eliminar">
          <svg width="16" height="16" viewBox="0 0 24 24" fill="none" stroke="var(--text-muted)" stroke-width="2">
            <polyline points="3 6 5 6 21 6"/>
            <path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"/>
          </svg>
        </button>
      </div>
    `;

    // Botón eliminar directo
    const btnDel = item.querySelector('button');
    btnDel.addEventListener('click', (e) => {
      e.stopPropagation();
      removeQueueItemWithAnimation(item, index);
    });

    // Tap en el elemento para saltar a la posición exacta de la cola sin duplicar pistas
    item.addEventListener('click', () => {
      container.querySelectorAll('.queue-item.active').forEach((el) => el.classList.remove('active'));
      item.classList.add('active');
      api.playQueueIndex(index)
        .then((st) => {
          if (st) updatePlayerState(st);
        })
        .catch(() => {
          showToast('No se pudo reproducir la pista seleccionada');
        });
    });

    // Activar gestos táctiles de arrastre y swipe-to-delete
    attachQueueTouchGestures(item, index);

    container.appendChild(item);
  });
}

// attachQueueTouchGestures implementa deslizamiento lateral para borrar y reordenamiento táctil.
function attachQueueTouchGestures(el, index) {
  let startX = 0;
  let startY = 0;
  let isSwiping = false;

  el.addEventListener('touchstart', (e) => {
    startX = e.touches[0].clientX;
    startY = e.touches[0].clientY;
    isSwiping = false;
  }, { passive: true });

  el.addEventListener('touchmove', (e) => {
    const diffX = e.touches[0].clientX - startX;
    const diffY = e.touches[0].clientY - startY;

    // Si el movimiento es horizontal hacia la izquierda, animar deslizamiento
    if (Math.abs(diffX) > Math.abs(diffY) && diffX < -20) {
      isSwiping = true;
      el.style.transform = `translateX(${diffX}px)`;
      el.style.opacity = `${1 - Math.min(1, Math.abs(diffX) / 250)}`;
    }
  }, { passive: true });

  el.addEventListener('touchend', (e) => {
    if (!isSwiping) return;
    const diffX = e.changedTouches[0].clientX - startX;
    if (diffX < -100) {
      // Eliminar si superó el umbral
      removeQueueItemWithAnimation(el, index);
    } else {
      // Restaurar posición si no llegó al umbral
      el.style.transform = 'translateX(0)';
      el.style.opacity = '1';
    }
  }, { passive: true });
}

// removeQueueItemWithAnimation aplica una animación de salida antes de invocar la API.
function removeQueueItemWithAnimation(el, index) {
  el.style.transform = 'translateX(-100%)';
  el.style.opacity = '0';
  setTimeout(() => {
    api.removeFromQueue(index);
    showToast('Pista eliminada de la cola');
  }, 180);
}

