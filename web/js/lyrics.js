// =====================================================================
// AuraBox - LRCLIB Synchronized Lyrics Manager & Auto-Scroll
// Real-Time Time Tracking & One-Tap Seeking
// =====================================================================

import * as api from './api.js';
import { state, subscribe } from './state.js';
import { isLiveStreamTrack } from './player.js';

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

  // Al volver la pantalla a primer plano, sincronizar letras si están visibles
  document.addEventListener('visibilitychange', () => {
    if (document.visibilityState === 'visible') {
      checkAndFetchLyrics();
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

let lyricsViewMode = 'karaoke';
try {
  lyricsViewMode = localStorage.getItem('aurabox_lyrics_mode') || 'karaoke';
} catch (e) {}

// getLyricLineInfo obtiene de forma segura el texto y tiempo de una línea de letra.
function getLyricLineInfo(item) {
  if (!item) return { text: '', time: 0 };
  const text = item.text ?? item.Text ?? '';
  const time = item.time ?? item.Time ?? 0;
  return { text: (text || '').trim(), time };
}

// escapeHTML sanitiza texto para incrustación segura en el DOM.
function escapeHTML(str) {
  return String(str || '')
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/"/g, '&quot;');
}

// setLyricsViewMode alterna entre el modo karaoke de 3 líneas y la lista completa desplazable.
function setLyricsViewMode(mode) {
  lyricsViewMode = (mode === 'scroll') ? 'scroll' : 'karaoke';
  try {
    localStorage.setItem('aurabox_lyrics_mode', lyricsViewMode);
  } catch (e) {}

  const btnKaraoke = document.getElementById('btnLyricsModeKaraoke');
  const btnScroll = document.getElementById('btnLyricsModeScroll');
  const viewKaraoke = document.getElementById('lyricsViewKaraoke');
  const viewScroll = document.getElementById('lyricsViewScroll');

  if (btnKaraoke && btnScroll && viewKaraoke && viewScroll) {
    btnKaraoke.classList.toggle('active', lyricsViewMode === 'karaoke');
    btnScroll.classList.toggle('active', lyricsViewMode === 'scroll');
    viewKaraoke.classList.toggle('hidden', lyricsViewMode !== 'karaoke');
    viewScroll.classList.toggle('hidden', lyricsViewMode !== 'scroll');
    syncActiveLyricLine(true);
  }
}

// updatePanelKaraokeDisplay actualiza las 3 líneas (anterior, actual y siguiente) en el panel Now Playing.
function updatePanelKaraokeDisplay(activeIdx, lines) {
  const container = document.getElementById('panelKaraoke3Lines');
  if (!container || !lines || lines.length === 0) return;

  const prevEl = document.getElementById('pkLinePrev');
  const activeEl = document.getElementById('pkLineActive');
  const nextEl = document.getElementById('pkLineNext');
  if (!prevEl || !activeEl || !nextEl) return;

  if (activeIdx < 0 || activeIdx >= lines.length) {
    const first = getLyricLineInfo(lines[0]);
    prevEl.textContent = '';
    prevEl.classList.add('empty');
    activeEl.textContent = first.text || '♪';
    activeEl.classList.remove('empty');
    activeEl.dataset.time = first.time;
    const second = lines.length > 1 ? getLyricLineInfo(lines[1]) : null;
    nextEl.textContent = second ? (second.text || '♪') : '';
    nextEl.classList.toggle('empty', !second);
    if (second) nextEl.dataset.time = second.time;
    return;
  }

  // Línea anterior
  if (activeIdx > 0) {
    const prev = getLyricLineInfo(lines[activeIdx - 1]);
    prevEl.textContent = prev.text || '♪';
    prevEl.dataset.time = prev.time;
    prevEl.classList.remove('empty');
  } else {
    prevEl.textContent = '';
    prevEl.classList.add('empty');
    delete prevEl.dataset.time;
  }

  // Línea activa
  const active = getLyricLineInfo(lines[activeIdx]);
  activeEl.textContent = active.text || '♪';
  activeEl.dataset.time = active.time;
  activeEl.classList.remove('empty');

  // Línea siguiente
  if (activeIdx + 1 < lines.length) {
    const next = getLyricLineInfo(lines[activeIdx + 1]);
    nextEl.textContent = next.text || '♪';
    nextEl.dataset.time = next.time;
    nextEl.classList.remove('empty');
  } else {
    nextEl.textContent = '';
    nextEl.classList.add('empty');
    delete nextEl.dataset.time;
  }
}

// updateAmbientLyricDisplay actualiza las 3 líneas sincronizadas en el modo marco musical horizontal/ambiental.
function updateAmbientLyricDisplay(activeIdx, lines) {
  const el = document.getElementById('npAmbientLyric');
  if (!el) return;

  if (activeIdx === '' || !lines || lines.length === 0 || typeof activeIdx !== 'number' || activeIdx < 0) {
    el.innerHTML = '';
    el.classList.add('hidden');
    return;
  }

  const prev = activeIdx > 0 ? getLyricLineInfo(lines[activeIdx - 1]) : null;
  const active = getLyricLineInfo(lines[activeIdx]);
  const next = (activeIdx + 1 < lines.length) ? getLyricLineInfo(lines[activeIdx + 1]) : null;

  if (!active.text || active.text === '♪') {
    el.innerHTML = '';
    el.classList.add('hidden');
    return;
  }

  el.classList.remove('hidden');
  el.innerHTML = `
    <div class="karaoke-3lines-container ambient">
      ${prev ? `<div class="karaoke-line karaoke-prev" data-time="${prev.time}">${escapeHTML(prev.text)}</div>` : '<div class="karaoke-line karaoke-prev empty"></div>'}
      <div class="karaoke-line karaoke-active" data-time="${active.time}">${escapeHTML(active.text)}</div>
      ${next ? `<div class="karaoke-line karaoke-next" data-time="${next.time}">${escapeHTML(next.text)}</div>` : '<div class="karaoke-line karaoke-next empty"></div>'}
    </div>
  `;

  // Añadir eventos click para salto instantáneo al tocar cualquiera de las 3 líneas
  el.querySelectorAll('.karaoke-line[data-time]').forEach((lineEl) => {
    lineEl.addEventListener('click', (e) => {
      e.stopPropagation();
      const t = parseFloat(lineEl.dataset.time);
      if (!isNaN(t)) {
        api.seek(t);
      }
    });
  });
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
    plainEl.style.padding = '20px 12px';
    plainEl.textContent = state.lyrics.plain;
    container.appendChild(plainEl);
    return;
  }

  // Barra de herramientas para cambiar de vista (Karaoke 3 líneas vs Lista completa)
  const toolbar = document.createElement('div');
  toolbar.className = 'lyrics-mode-toolbar';
  toolbar.innerHTML = `
    <button class="btn-lyrics-mode ${lyricsViewMode === 'karaoke' ? 'active' : ''}" id="btnLyricsModeKaraoke" type="button" title="Modo Karaoke (3 líneas)">
      <span>🎤</span> Karaoke (3 líneas)
    </button>
    <button class="btn-lyrics-mode ${lyricsViewMode === 'scroll' ? 'active' : ''}" id="btnLyricsModeScroll" type="button" title="Lista completa desplazable">
      <span>📜</span> Lista completa
    </button>
  `;
  container.appendChild(toolbar);

  toolbar.querySelector('#btnLyricsModeKaraoke')?.addEventListener('click', (e) => {
    e.stopPropagation();
    setLyricsViewMode('karaoke');
  });
  toolbar.querySelector('#btnLyricsModeScroll')?.addEventListener('click', (e) => {
    e.stopPropagation();
    setLyricsViewMode('scroll');
  });

  // Vista 1: Modo Karaoke con 3 líneas prominentes
  const viewKaraoke = document.createElement('div');
  viewKaraoke.id = 'lyricsViewKaraoke';
  viewKaraoke.className = `lyrics-view-karaoke ${lyricsViewMode === 'karaoke' ? '' : 'hidden'}`;
  viewKaraoke.innerHTML = `
    <div class="karaoke-3lines-container" id="panelKaraoke3Lines">
      <div class="karaoke-line karaoke-prev empty" id="pkLinePrev"></div>
      <div class="karaoke-line karaoke-active" id="pkLineActive">♪</div>
      <div class="karaoke-line karaoke-next empty" id="pkLineNext"></div>
    </div>
  `;

  // Asignar listeners de salto en las 3 líneas
  viewKaraoke.querySelectorAll('.karaoke-line').forEach((lineEl) => {
    lineEl.addEventListener('click', (e) => {
      e.stopPropagation();
      const t = parseFloat(lineEl.dataset.time);
      if (!isNaN(t)) {
        api.seek(t);
      }
    });
  });
  container.appendChild(viewKaraoke);

  // Vista 2: Lista completa desplazable con auto-scroll
  const viewScroll = document.createElement('div');
  viewScroll.id = 'lyricsViewScroll';
  viewScroll.className = `lyrics-view-scroll ${lyricsViewMode === 'scroll' ? '' : 'hidden'}`;

  state.lyrics.synced.forEach((item, index) => {
    const lineEl = document.createElement('div');
    lineEl.className = 'lyric-line';
    lineEl.id = `lyric-line-${index}`;
    const { text, time } = getLyricLineInfo(item);
    lineEl.textContent = text || '♪';
    lineEl.dataset.time = time;

    lineEl.addEventListener('click', (e) => {
      e.stopPropagation();
      api.seek(time);
    });

    viewScroll.appendChild(lineEl);
  });
  container.appendChild(viewScroll);

  // Sincronizar inmediatamente la línea actual en ambas vistas
  updatePanelKaraokeDisplay(state.lyrics.activeIndex, state.lyrics.synced);
}

// syncActiveLyricLine detecta la línea correspondiente al segundo actual y realiza auto-scroll suave.
function syncActiveLyricLine(forceAmbientRefresh = false) {
  if (document.visibilityState === 'hidden') return;
  if (!state.lyrics.found || state.lyrics.synced.length === 0) {
    updateAmbientLyricDisplay(-1, []);
    return;
  }
  if (!forceAmbientRefresh && state.player.status !== 'play') return;

  const container = document.getElementById('npLyricsContainer');
  const lyricsOpen = Boolean(container && container.classList.contains('active'));
  const isAmbient = Boolean(state.ui.isAmbientMode);
  if (!lyricsOpen && !isAmbient) return;

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
    // Remover activo anterior en lista desplazable
    if (state.lyrics.activeIndex >= 0 && state.lyrics.activeIndex !== activeIdx) {
      document.getElementById(`lyric-line-${state.lyrics.activeIndex}`)?.classList.remove('active');
    }

    state.lyrics.activeIndex = activeIdx;

    // Actualizar vista Karaoke de 3 líneas en pantalla ambiental / marco musical
    updateAmbientLyricDisplay(activeIdx, lines);

    // Actualizar vista Karaoke de 3 líneas en panel Now Playing
    updatePanelKaraokeDisplay(activeIdx, lines);

    if (lyricsOpen && lyricsViewMode === 'scroll') {
      const currentEl = document.getElementById(`lyric-line-${activeIdx}`);
      if (currentEl) {
        currentEl.classList.add('active');
        // Auto-scroll centrado en la vista
        currentEl.scrollIntoView({ behavior: 'smooth', block: 'center' });
      }
    }
  }
}
