// =====================================================================
// AuraBox - Player Controller, MediaSession & UI Synchronization
// Safeguard Volume, Timeline Scrubber & Ambient Background Rendering
// =====================================================================

import * as api from './api.js';
import { state, subscribe, notifyListeners } from './state.js';
import { openNowPlaying, closeNowPlaying } from './gestures.js';

// Estado de interacción con el scrubber para no interrumpir el arrastre táctil
let isScrubbing = false;

// initPlayer inicializa los escuchadores de controles, scrubber y MediaSession.
export function initPlayer() {
  bindMiniPlayerControls();
  bindNowPlayingControls();
  setupMediaSession();

  // Escuchar cambios reactivos en el estado
  subscribe((event) => {
    if (event === 'player_updated') {
      renderMiniPlayer();
      renderNowPlaying();
      updateMediaSession();
    }
  });

  // Ticker de animación fluido para la barra de progreso (4 veces por segundo)
  setInterval(tickProgress, 250);
}

// bindMiniPlayerControls conecta los botones de transporte del mini-player y apertura de Now Playing.
function bindMiniPlayerControls() {
  const miniPlayer = document.getElementById('miniPlayer');
  const btnPlay = document.getElementById('miniBtnPlay');
  const btnExpand = document.getElementById('btnMiniExpand');
  const btnHeaderNowPlaying = document.getElementById('btnHeaderNowPlaying');

  if (btnPlay) {
    btnPlay.addEventListener('click', (e) => {
      e.stopPropagation();
      optimisticTogglePlay();
    });
  }

  if (btnExpand) {
    btnExpand.addEventListener('click', (e) => {
      e.stopPropagation();
      openNowPlaying();
    });
  }

  if (miniPlayer) {
    miniPlayer.addEventListener('click', (e) => {
      if (e.target.closest('#miniBtnPlay') || e.target.closest('#btnMiniExpand')) return;
      openNowPlaying();
    });
  }

  if (btnHeaderNowPlaying) {
    btnHeaderNowPlaying.addEventListener('click', () => {
      openNowPlaying();
    });
  }
}

// bindNowPlayingControls conecta botones del modal completo (play, seek, volumen, letras, cerrar).
function bindNowPlayingControls() {
  const btnClose = document.getElementById('npBtnClose');
  const btnPlay = document.getElementById('npBtnPlay');
  const btnNext = document.getElementById('npBtnNext');
  const btnPrev = document.getElementById('npBtnPrev');
  const btnShuffle = document.getElementById('npBtnShuffle');
  const btnRepeat = document.getElementById('npBtnRepeat');
  const btnFav = document.getElementById('npBtnFav');
  const scrubber = document.getElementById('npTimelineScrubber');
  const volSlider = document.getElementById('npVolumeSlider');
  const btnVolMinus = document.getElementById('btnVolMinus');
  const btnVolPlus = document.getElementById('btnVolPlus');
  const btnLyrics = document.getElementById('npBtnLyrics');

  if (btnClose) btnClose.addEventListener('click', closeNowPlaying);
  if (btnPlay) btnPlay.addEventListener('click', optimisticTogglePlay);
  if (btnNext) btnNext.addEventListener('click', () => api.next());
  if (btnPrev) btnPrev.addEventListener('click', () => api.prev());
  if (btnShuffle) btnShuffle.addEventListener('click', () => api.shuffleQueue());
  if (btnRepeat) btnRepeat.addEventListener('click', toggleRepeat);

  if (btnFav) {
    btnFav.addEventListener('click', () => {
      if (state.player.current_track) {
        api.toggleFavorite(state.player.current_track.id).then((res) => {
          state.player.current_track.favorite = res.favorite;
          renderNowPlaying();
        });
      }
    });
  }

  // Deslizador de progreso de tiempo (Scrubber)
  if (scrubber) {
    scrubber.addEventListener('input', () => {
      isScrubbing = true;
      const val = parseFloat(scrubber.value);
      const elapsedLabel = document.getElementById('npTimeElapsed');
      if (elapsedLabel) elapsedLabel.textContent = formatTime(val);
      const dur = state.player.duration || 1;
      const miniFill = document.getElementById('miniProgressFill');
      if (miniFill) miniFill.style.width = `${Math.min(100, (val / dur) * 100)}%`;
    });

    scrubber.addEventListener('change', () => {
      const val = parseFloat(scrubber.value);
      state.player.elapsed_time = val;
      state.player.base_elapsed_time = val;
      state.player.last_sync_timestamp = (typeof performance !== 'undefined' ? performance.now() : Date.now());
      isScrubbing = false;
      api.seek(val);
    });
  }

  // Salvaguarda de volumen (Slider y botones por pasos)
  if (volSlider) {
    volSlider.addEventListener('input', () => {
      let val = parseInt(volSlider.value, 10);
      // Aplicar límite máximo de salvaguarda
      if (val > state.player.max_volume_limit) {
        val = state.player.max_volume_limit;
        volSlider.value = val;
      }
      api.setVolume(val);
    });
  }

  if (btnVolMinus) {
    btnVolMinus.addEventListener('click', () => api.stepVolume(-5));
  }
  if (btnVolPlus) {
    btnVolPlus.addEventListener('click', () => {
      if (state.player.volume + 5 > state.player.max_volume_limit) {
        showToast(`Límite de salvaguarda activo (${state.player.max_volume_limit}%)`);
        return;
      }
      api.stepVolume(5);
    });
  }

  if (btnLyrics) {
    btnLyrics.addEventListener('click', toggleLyricsView);
  }

  const badgeLyrics = document.getElementById('npBadgeLyrics');
  if (badgeLyrics) {
    badgeLyrics.addEventListener('click', toggleLyricsView);
  }

  const artContainer = document.getElementById('npArtworkContainer');
  if (artContainer) {
    artContainer.style.cursor = 'pointer';
    artContainer.title = 'Toca para ver u ocultar letras sincronizadas';
    artContainer.addEventListener('click', toggleLyricsView);
  }
}

// optimisticTogglePlay ofrece retroalimentación visual inmediata (<100ms) antes de respuesta del servidor.
function optimisticTogglePlay() {
  const isPlaying = (state.player.status === 'play');
  state.player.status = isPlaying ? 'pause' : 'play';
  if (state.player.status === 'play') {
    state.player.last_sync_timestamp = (typeof performance !== 'undefined' ? performance.now() : Date.now());
    state.player.base_elapsed_time = state.player.elapsed_time || 0;
  }
  renderMiniPlayer();
  renderNowPlaying();
  api.toggle();
}

// renderMiniPlayer actualiza el widget flotante dockeado en el tercio inferior.
export function renderMiniPlayer() {
  const mini = document.getElementById('miniPlayer');
  if (!mini) return;

  const track = state.player.current_track;
  const isPlaying = (state.player.status === 'play');

  if (!track) {
    document.getElementById('miniTitle').textContent = 'AuraBox Jukebox';
    document.getElementById('miniArtist').textContent = 'Toca aquí para abrir Ahora Suena';
    document.getElementById('miniCover').src = '/assets/default-cover.svg';
    mini.classList.remove('hidden');
    mini.classList.remove('playing');
    return;
  }

  mini.classList.remove('hidden');
  mini.classList.toggle('playing', isPlaying);
  document.getElementById('miniTitle').textContent = track.title || 'Desconocido';
  document.getElementById('miniArtist').textContent = track.artist || 'Artista';

  const coverImg = document.getElementById('miniCover');
  coverImg.src = track.cover_url || '/assets/default-cover.svg';

  // Actualizar icono play / pausa en mini-player
  const icon = document.getElementById('miniPlayIcon');
  if (icon) {
    icon.innerHTML = isPlaying
      ? '<path d="M6 4h4v16H6zm8 0h4v16h-4z" fill="currentColor"/>'
      : '<path d="M8 5v14l11-7z" fill="currentColor"/>';
  }

  // Actualizar indicador en botón de cabecera superior
  const headerBtn = document.getElementById('btnHeaderNowPlaying');
  if (headerBtn) {
    if (isPlaying) {
      headerBtn.innerHTML = `<span>🎵</span> <span style="color:#34d399; font-weight:800;">●</span> <span>Ahora Suena</span>`;
      headerBtn.style.borderColor = 'var(--accent)';
      headerBtn.style.background = 'rgba(99,102,241,0.25)';
    } else {
      headerBtn.innerHTML = `<span>🎵</span> <span>Ahora Suena</span>`;
      headerBtn.style.borderColor = 'var(--border-subtle)';
      headerBtn.style.background = 'var(--bg-surface)';
    }
  }
}

// renderNowPlaying actualiza los elementos visuales de la vista expandida y los controles globales del reproductor.
export function renderNowPlaying() {
  const track = state.player.current_track;
  const isPlaying = (state.player.status === 'play');
  const maxLimit = state.player.max_volume_limit || 85;

  // Sincronizar botón principal de play / pausa
  const playIcon = document.getElementById('npPlayIcon');
  if (playIcon) {
    playIcon.innerHTML = isPlaying
      ? '<path d="M6 19h4V5H6v14zm8-14v14h4V5h-4z" fill="currentColor"/>'
      : '<path d="M8 5v14l11-7z" fill="currentColor"/>';
  }

  // Sincronizar botón de repetición
  const btnRepeat = document.getElementById('npBtnRepeat');
  if (btnRepeat) {
    btnRepeat.classList.toggle('active', !!state.player.repeat);
  }

  // Sincronizar slider de volumen y salvaguarda auditiva en Ajustes
  const volSlider = document.getElementById('npVolumeSlider');
  if (volSlider) {
    volSlider.max = maxLimit;
    if (document.activeElement !== volSlider) {
      volSlider.value = state.player.volume;
    }
  }

  const settingMaxVol = document.getElementById('settingMaxVol');
  const labelMaxVol = document.getElementById('labelMaxVol');
  if (settingMaxVol && document.activeElement !== settingMaxVol) {
    settingMaxVol.value = maxLimit;
  }
  if (labelMaxVol) {
    labelMaxVol.textContent = `${maxLimit}%`;
  }

  // Actualizar insignia de salida de audio activa en cabecera
  const outBadge = document.getElementById('headerActiveOutput');
  if (outBadge) {
    outBadge.textContent = state.player.active_output || 'Audio';
  }

  if (!track) {
    document.getElementById('npTitle').textContent = 'AuraBox Hi-Fi';
    document.getElementById('npArtist').textContent = 'Sin reproducción activa • Elige un álbum o carpeta';
    document.getElementById('npCover').src = '/assets/default-cover.svg';
    const badge = document.getElementById('npHifiBadge');
    if (badge) badge.textContent = 'En Espera';
    const btnFav = document.getElementById('npBtnFav');
    if (btnFav) btnFav.classList.remove('active');
    return;
  }

  document.getElementById('npTitle').textContent = track.title || 'Canción';
  document.getElementById('npArtist').textContent = track.artist || 'Artista';

  const coverSrc = track.cover_url || '/assets/default-cover.svg';
  document.getElementById('npCover').src = coverSrc;

  // Fondo adaptativo con desenfoque basado en la carátula
  const bg = document.getElementById('npAmbientBg');
  if (bg) {
    bg.style.backgroundImage = `url('${coverSrc}')`;
  }

  // Badge Hi-Fi técnico
  const badge = document.getElementById('npHifiBadge');
  if (badge) {
    let rateKhz = track.sample_rate ? Math.round(track.sample_rate / 1000) : 44;
    badge.textContent = `${track.format || 'FLAC'} ${rateKhz}kHz • ${track.bitrate || 320}kbps`;
  }

  // Corazón de favorito
  const btnFav = document.getElementById('npBtnFav');
  if (btnFav) {
    btnFav.classList.toggle('active', !!track.favorite);
  }

  // Actualizar marcas de duración
  const dur = (track && track.duration) || state.player.duration || 0;
  const durationLabel = document.getElementById('npTimeDuration');
  if (durationLabel && dur > 0) {
    durationLabel.textContent = formatTime(dur);
  }
  const scrubber = document.getElementById('npTimelineScrubber');
  if (scrubber && dur > 0) {
    scrubber.max = dur;
  }

  tickProgress();
}

// tickProgress avanza la barra de tiempo en tiempo real entre eventos SSE.
export function tickProgress() {
  const dur = state.player.duration || (state.player.current_track?.duration) || 0;

  if (state.player.status === 'play' && dur > 0) {
    const now = (typeof performance !== 'undefined' ? performance.now() : Date.now());
    const lastSync = state.player.last_sync_timestamp || now;
    const deltaSec = Math.max(0, (now - lastSync) / 1000);
    const calculated = (state.player.base_elapsed_time || 0) + deltaSec;
    state.player.elapsed_time = Math.min(dur, calculated);
  }

  const elapsed = state.player.elapsed_time || 0;
  const pct = dur > 0 ? Math.min(100, Math.max(0, (elapsed / dur) * 100)) : 0;

  const miniFill = document.getElementById('miniProgressFill');
  if (miniFill) miniFill.style.width = `${pct}%`;

  if (!isScrubbing) {
    const scrubber = document.getElementById('npTimelineScrubber');
    const elapsedLabel = document.getElementById('npTimeElapsed');
    const durationLabel = document.getElementById('npTimeDuration');

    if (scrubber) {
      if (dur > 0) scrubber.max = dur;
      scrubber.value = elapsed;
    }
    if (elapsedLabel) elapsedLabel.textContent = formatTime(elapsed);
    if (durationLabel && dur > 0) durationLabel.textContent = formatTime(dur);
  }
}

// toggleRepeat alterna el modo de repetición en memoria y en el demonio MPD.
function toggleRepeat() {
  const nextRepeat = !state.player.repeat;
  state.player.repeat = nextRepeat;
  document.getElementById('npBtnRepeat')?.classList.toggle('active', nextRepeat);
  showToast(nextRepeat ? 'Repetición activada' : 'Repetición desactivada');
  api.setRepeat(nextRepeat).catch(() => {
    state.player.repeat = !nextRepeat;
    document.getElementById('npBtnRepeat')?.classList.toggle('active', !nextRepeat);
    showToast('Error al cambiar modo repetición');
  });
}

// toggleLyricsView conmuta entre la carátula y la vista de letras sincronizadas.
function toggleLyricsView() {
  const lyricsContainer = document.getElementById('npLyricsContainer');
  const artworkContainer = document.getElementById('npArtworkContainer');
  const btn = document.getElementById('npBtnLyrics');
  const badge = document.getElementById('npBadgeLyrics');

  const showLyrics = !lyricsContainer.classList.contains('active');
  lyricsContainer.classList.toggle('active', showLyrics);
  artworkContainer.style.display = showLyrics ? 'none' : 'block';
  btn?.classList.toggle('active', showLyrics);
  if (badge) {
    badge.textContent = showLyrics ? '🖼️ Carátula' : '🎤 Letras';
  }

  if (showLyrics) {
    notifyListeners('lyrics_view_opened', state.player.current_track);
  }
}

// setupMediaSession integra la API de navegación nativa en Android / iOS / Wearables.
function setupMediaSession() {
  if (!('mediaSession' in navigator)) return;

  navigator.mediaSession.setActionHandler('play', () => api.play());
  navigator.mediaSession.setActionHandler('pause', () => api.pause());
  navigator.mediaSession.setActionHandler('previoustrack', () => api.prev());
  navigator.mediaSession.setActionHandler('nexttrack', () => api.next());
  navigator.mediaSession.setActionHandler('seekto', (details) => {
    if (details.seekTime !== undefined) api.seek(details.seekTime);
  });
}

// updateMediaSession sincroniza carátula y metadatos con la pantalla de bloqueo.
function updateMediaSession() {
  if (!('mediaSession' in navigator) || !state.player.current_track) return;
  const track = state.player.current_track;

  navigator.mediaSession.metadata = new MediaMetadata({
    title: track.title,
    artist: track.artist,
    album: track.album,
    artwork: [
      { src: track.cover_url || '/assets/default-cover.svg', sizes: '512x512', type: 'image/png' }
    ]
  });
  navigator.mediaSession.playbackState = (state.player.status === 'play') ? 'playing' : 'paused';
}

// formatTime convierte segundos numéricos al formato MM:SS.
export function formatTime(seconds) {
  if (!seconds || isNaN(seconds)) return '0:00';
  const mins = Math.floor(seconds / 60);
  const secs = Math.floor(seconds % 60);
  return `${mins}:${secs < 10 ? '0' : ''}${secs}`;
}

// showToast muestra un aviso flotante momentáneo con soporte opcional de botón de acción.
export function showToast(message, actionLabel = '', actionFn = null) {
  let toast = document.getElementById('appToast');
  if (!toast) {
    toast = document.createElement('div');
    toast.id = 'appToast';
    toast.className = 'toast';
    document.getElementById('app').appendChild(toast);
  }
  toast.innerHTML = '';
  const textSpan = document.createElement('span');
  textSpan.textContent = message;
  toast.appendChild(textSpan);

  if (actionLabel && actionFn) {
    const actBtn = document.createElement('button');
    actBtn.className = 'toast-action-btn';
    actBtn.textContent = actionLabel;
    actBtn.style.cssText = 'background:var(--accent); border:none; color:#ffffff; font-size:0.75rem; font-weight:700; padding:4px 10px; border-radius:12px; margin-left:8px; cursor:pointer;';
    actBtn.addEventListener('click', (e) => {
      e.stopPropagation();
      actionFn();
      toast.classList.remove('show');
    });
    toast.appendChild(actBtn);
  }

  toast.classList.add('show');
  clearTimeout(toast._timer);
  toast._timer = setTimeout(() => {
    toast.classList.remove('show');
  }, actionLabel ? 3800 : 2200);
}

