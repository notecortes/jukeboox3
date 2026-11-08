// =====================================================================
// AuraBox - LRCLIB Synchronized Lyrics Manager & Auto-Scroll
// Real-Time Time Tracking & One-Tap Seeking
// =====================================================================

import * as api from './api.js';
import { state, subscribe } from './state.js';

// initLyrics inicializa la sincronización de letras LRCLIB con el reproductor.
export function initLyrics() {
  subscribe((event) => {
    if (event === 'player_updated' || event === 'lyrics_view_opened') {
      checkAndFetchLyrics();
      syncActiveLyricLine();
    }
  });

  // Ticker de refresco frecuente para coincidencia exacta con el segundo actual
  setInterval(syncActiveLyricLine, 250);
}

// checkAndFetchLyrics consulta la API abierta LRCLIB si la pista ha cambiado.
async function checkAndFetchLyrics() {
  const track = state.player.current_track;
  if (!track) return;

  const trackKey = `${track.artist}-${track.title}`;
  if (state.lyrics.trackKey === trackKey) return;

  state.lyrics.trackKey = trackKey;
  state.lyrics.synced = [];
  state.lyrics.plain = '';
  state.lyrics.found = false;
  state.lyrics.activeIndex = -1;

  const container = document.getElementById('npLyricsContainer');
  if (!container) return;
  container.innerHTML = '<div style="color:var(--text-muted); padding:40px 0; font-size:0.9rem;">Buscando letras sincronizadas...</div>';

  try {
    const res = await api.getLyrics(track.title, track.artist, track.album, track.duration, track.path);
    if (res && res.found) {
      state.lyrics.found = true;
      state.lyrics.plain = res.plain || '';
      state.lyrics.synced = res.synced || [];
      renderLyricsLines();
    } else {
      container.innerHTML = `<div style="color:var(--text-muted); padding:40px 0; font-size:0.9rem;">${res?.plain || 'Letra no disponible para esta canción'}</div>`;
    }
  } catch (err) {
    container.innerHTML = '<div style="color:var(--text-muted); padding:40px 0; font-size:0.9rem;">No se pudieron cargar letras en este momento</div>';
  }
}

// renderLyricsLines renderiza las líneas en el contenedor permitiendo salto de tiempo al hacer tap.
function renderLyricsLines() {
  const container = document.getElementById('npLyricsContainer');
  if (!container) return;
  container.innerHTML = '';

  if (state.lyrics.synced.length === 0) {
    // Si sólo hay texto plano no sincronizado
    const plainEl = document.createElement('div');
    plainEl.style.whiteSpace = 'pre-wrap';
    plainEl.style.lineHeight = '1.8';
    plainEl.style.fontSize = '0.95rem';
    plainEl.style.color = 'var(--text-secondary)';
    plainEl.textContent = state.lyrics.plain;
    container.appendChild(plainEl);
    return;
  }

  // Renderizar líneas con marca de tiempo sincronizada
  state.lyrics.synced.forEach((item, index) => {
    const lineEl = document.createElement('div');
    lineEl.className = 'lyric-line';
    lineEl.id = `lyric-line-${index}`;
    const text = item.text ?? item.Text ?? '♪';
    const time = item.time ?? item.Time ?? 0;
    lineEl.textContent = text || '♪';
    lineEl.dataset.time = time;

    // Tap en la línea para desplazarse inmediatamente a ese punto del audio
    lineEl.addEventListener('click', () => {
      api.seek(time);
    });

    container.appendChild(lineEl);
  });
}

// syncActiveLyricLine detecta la línea correspondiente al segundo actual y realiza auto-scroll suave.
function syncActiveLyricLine() {
  if (!state.lyrics.found || state.lyrics.synced.length === 0) return;
  if (state.player.status !== 'play') return;

  // Optimización de recursos: no procesar scroll si la vista de letras está oculta
  const container = document.getElementById('npLyricsContainer');
  if (!container || !container.classList.contains('active')) return;

  const currentTime = state.player.elapsed_time || 0;
  const lines = state.lyrics.synced;
  let activeIdx = -1;

  for (let i = 0; i < lines.length; i++) {
    const lineTime = lines[i].time ?? lines[i].Time ?? 0;
    if (lineTime <= currentTime) {
      activeIdx = i;
    } else {
      break;
    }
  }

  if (activeIdx !== state.lyrics.activeIndex && activeIdx >= 0) {
    // Remover activo anterior
    if (state.lyrics.activeIndex >= 0) {
      document.getElementById(`lyric-line-${state.lyrics.activeIndex}`)?.classList.remove('active');
    }

    state.lyrics.activeIndex = activeIdx;
    const currentEl = document.getElementById(`lyric-line-${activeIdx}`);
    if (currentEl) {
      currentEl.classList.add('active');
      // Auto-scroll centrado en la vista
      currentEl.scrollIntoView({ behavior: 'smooth', block: 'center' });
    }
  }
}
