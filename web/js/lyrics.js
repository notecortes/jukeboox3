// =====================================================================
// AuraBox - LRCLIB Synchronized Lyrics Manager & Auto-Scroll
// Real-Time Time Tracking & One-Tap Seeking
// =====================================================================

import * as api from './api.js?v=21';
import { state, subscribe } from './state.js?v=21';
import { isLiveStreamTrack } from './player.js?v=21';

// initLyrics inicializa la sincronización de letras LRCLIB con el reproductor.
export function initLyrics() {
  subscribe((event) => {
    if (event === 'player_updated' || event === 'lyrics_view_opened') {
      checkAndFetchLyrics();
      syncActiveLyricLine();
    } else if (event === 'ambient_mode_entered') {
      syncActiveLyricLine(true);
    }
  });

  // Ticker de refresco frecuente para coincidencia exacta con el segundo actual
  setInterval(() => syncActiveLyricLine(false), 250);
}

// parseRadioStreamSong extrae artista y título desde un metadato ICY con formato "Artista - Canción".
function parseRadioStreamSong(streamTitle) {
  const clean = (streamTitle || '').trim();
  if (!clean) return null;
  const parts = clean.split(/\s+[-–—]\s+/);
  if (parts.length < 2) return null;
  const artist = parts[0].trim();
  const title = parts.slice(1).join(' - ').trim();
  if (!artist || !title) return null;
  return { artist, title };
}

// checkAndFetchLyrics consulta la API abierta LRCLIB si la pista ha cambiado.
async function checkAndFetchLyrics() {
  const track = state.player.current_track;
  if (!track) return;

  const isLive = isLiveStreamTrack(track);
  const streamTitle = isLive ? (state.player.stream_title || '').trim() : '';
  const trackKey = isLive
    ? `radio:${track.path || track.title}:${streamTitle}`
    : `${track.artist}-${track.title}`;
  if (state.lyrics.trackKey === trackKey) return;

  state.lyrics.trackKey = trackKey;
  state.lyrics.synced = [];
  state.lyrics.plain = '';
  state.lyrics.found = false;
  state.lyrics.activeIndex = -1;
  updateAmbientLyricDisplay('');

  const container = document.getElementById('npLyricsContainer');
  if (!container) return;

  let queryTitle = track.title;
  let queryArtist = track.artist;
  let queryAlbum = track.album;
  let queryDuration = track.duration;
  let queryPath = track.path;

  if (isLive) {
    const parsed = parseRadioStreamSong(streamTitle);
    if (!parsed) {
      container.innerHTML = '<div style="color:var(--text-muted); padding:40px 16px; font-size:0.9rem; line-height:1.5;">📡 Emisora de radio en directo.<br/>Cuando el flujo envíe «Artista - Canción», buscaremos la letra automáticamente.</div>';
      return;
    }
    queryTitle = parsed.title;
    queryArtist = parsed.artist;
    queryAlbum = '';
    queryDuration = 0;
    queryPath = '';
  }

  container.innerHTML = '<div style="color:var(--text-muted); padding:40px 0; font-size:0.9rem;">Buscando letras sincronizadas...</div>';

  try {
    const res = await api.getLyrics(queryTitle, queryArtist, queryAlbum, queryDuration, queryPath);
    if (res && res.found) {
      state.lyrics.found = true;
      state.lyrics.plain = res.plain || '';
      state.lyrics.synced = res.synced || [];
      renderLyricsLines();
    } else {
      container.innerHTML = '';
      const msg = document.createElement('div');
      msg.style.cssText = 'color:var(--text-muted); padding:40px 0; font-size:0.9rem;';
      msg.textContent = res?.plain || 'Letra no disponible para esta canción';
      container.appendChild(msg);
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

// updateAmbientLyricDisplay actualiza la línea actual mostrada en el modo marco musical horizontal.
function updateAmbientLyricDisplay(text) {
  const el = document.getElementById('npAmbientLyric');
  if (!el) return;
  const clean = (text || '').trim();
  if (!clean || clean === '♪') {
    el.textContent = '';
    el.classList.add('hidden');
  } else {
    el.textContent = clean;
    el.classList.remove('hidden');
  }
}

// syncActiveLyricLine detecta la línea correspondiente al segundo actual y realiza auto-scroll suave.
function syncActiveLyricLine(forceAmbientRefresh = false) {
  if (!state.lyrics.found || state.lyrics.synced.length === 0) {
    updateAmbientLyricDisplay('');
    return;
  }
  if (!forceAmbientRefresh && state.player.status !== 'play') return;

  const container = document.getElementById('npLyricsContainer');
  const lyricsOpen = Boolean(container && container.classList.contains('active'));
  if (!lyricsOpen && !state.ui.isNowPlayingOpen) return;

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

  if (activeIdx >= 0 && (activeIdx !== state.lyrics.activeIndex || forceAmbientRefresh)) {
    // Remover activo anterior
    if (state.lyrics.activeIndex >= 0 && state.lyrics.activeIndex !== activeIdx) {
      document.getElementById(`lyric-line-${state.lyrics.activeIndex}`)?.classList.remove('active');
    }

    state.lyrics.activeIndex = activeIdx;
    const activeItem = lines[activeIdx];
    const activeText = activeItem?.text ?? activeItem?.Text ?? '';
    updateAmbientLyricDisplay(activeText);

    if (lyricsOpen) {
      const currentEl = document.getElementById(`lyric-line-${activeIdx}`);
      if (currentEl) {
        currentEl.classList.add('active');
        // Auto-scroll centrado en la vista
        currentEl.scrollIntoView({ behavior: 'smooth', block: 'center' });
      }
    }
  }
}
