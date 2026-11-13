// =====================================================================
// AuraBox - Player Controller, MediaSession & UI Synchronization
// Safeguard Volume, Timeline Scrubber & Ambient Background Rendering
// =====================================================================

import * as api from './api.js';
import { state, subscribe, notifyListeners } from './state.js';
import { sanitizeCoverURL } from './sanitize.js';
import { openNowPlaying, closeNowPlaying } from './gestures.js';

// Estado de interacción con el scrubber para no interrumpir el arrastre táctil
let isScrubbing = false;
let lastShownPlayerError = '';
let wakeLockEnabled = (typeof localStorage !== 'undefined' && localStorage.getItem('aurabox_wake_lock') === 'true');
let wakeLockSentinel = null;
let lastSleepSyncAt = 0;
let ambientModeEnabled = (typeof localStorage !== 'undefined' ? localStorage.getItem('aurabox_ambient_mode') !== 'false' : true);
let ambientTimeoutSec = (() => {
  if (typeof localStorage === 'undefined') return 60;
  const raw = parseInt(localStorage.getItem('aurabox_ambient_timeout_sec') || '60', 10);
  return [10, 15, 30, 60, 120].includes(raw) ? raw : 60;
})();
let ambientOrientation = (typeof localStorage !== 'undefined' && localStorage.getItem('aurabox_ambient_orientation') === 'landscape')
  ? 'landscape'
  : 'all';
let ambientInactivityTimer = null;
let swallowAmbientWakeClick = false;

// isLiveStreamTrack determina si una pista corresponde a una radio web o flujo HTTP en directo.
export function isLiveStreamTrack(track) {
  if (!track) return false;
  if (track.source === 'radio') return true;
  const p = (track.path || '').toLowerCase();
  return p.startsWith('http://') || p.startsWith('https://');
}

// formatLiveSubtitle construye el subtítulo con la canción ICY activa o el nombre de la emisora.
function formatLiveSubtitle(track, streamTitle, includeStation = true) {
  const icy = (streamTitle || '').trim();
  const stationArtist = (track && track.artist) ? track.artist : 'Radio en Directo';
  if (icy) {
    return includeStation ? `🎙️ ${icy} • ${stationArtist}` : `🎙️ ${icy}`;
  }
  return `📡 ${stationArtist}`;
}

// initPlayer inicializa los escuchadores de controles, scrubber y MediaSession.
export function initPlayer() {
  bindMiniPlayerControls();
  bindNowPlayingControls();
  bindNowPlayingSleepControls();
  initWakeLock();
  initAmbientScreen();
  setupMediaSession();

  // Escuchar cambios reactivos en el estado
  subscribe((event, payload) => {
    if (event === 'player_updated') {
      lastSleepSyncAt = Date.now();
      const errMsg = (state.player.last_error || '').trim();
      if (errMsg && errMsg !== lastShownPlayerError) {
        lastShownPlayerError = errMsg;
        showToast(`⚠️ ${errMsg}`);
      } else if (!errMsg) {
        lastShownPlayerError = '';
      }
      renderMiniPlayer();
      renderNowPlaying();
      syncWakeLockWithPlayback();
      updateMediaSession();
    } else if (event === 'now_playing_toggled') {
      if (payload) {
        resetAmbientTimer();
      } else {
        exitAmbientMode(false);
      }
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
      const track = state.player.current_track;
      if (track && !isLiveStreamTrack(track) && track.id > 0) {
        api.toggleFavorite(track.id).then((res) => {
          state.player.current_track.favorite = res.favorite;
          renderNowPlaying();
        });
      }
    });
  }

  // Deslizador de progreso de tiempo (Scrubber)
  if (scrubber) {
    scrubber.addEventListener('input', () => {
      if (scrubber.disabled) return;
      isScrubbing = true;
      const val = parseFloat(scrubber.value);
      const elapsedLabel = document.getElementById('npTimeElapsed');
      if (elapsedLabel) elapsedLabel.textContent = formatTime(val);
      const dur = state.player.duration || 1;
      const miniFill = document.getElementById('miniProgressFill');
      if (miniFill) miniFill.style.width = `${Math.min(100, (val / dur) * 100)}%`;
    });

    scrubber.addEventListener('change', () => {
      if (scrubber.disabled) {
        isScrubbing = false;
        return;
      }
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
      const volDbfs = document.getElementById('npVolDbfs');
      if (volDbfs) {
        volDbfs.textContent = val > 0 ? `${Math.round(20 * Math.log10(val / 100))} dB` : '-∞ dB';
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

  // Acción en 1 toque: Crear Smart Mix del artista que está sonando
  const btnQuickMix = document.getElementById('npBtnQuickMix');
  if (btnQuickMix) {
    btnQuickMix.addEventListener('click', (e) => {
      e.stopPropagation();
      const track = state.player.current_track;
      if (!track || isLiveStreamTrack(track) || !track.artist) return;
      notifyListeners('trigger_artist_mix', track.artist);
    });
  }

  // Tap en nombre del artista: abrir ficha del artista en Biblioteca
  const npArtist = document.getElementById('npArtist');
  if (npArtist) {
    npArtist.addEventListener('click', () => {
      const track = state.player.current_track;
      if (!track || isLiveStreamTrack(track) || !track.artist) return;
      closeNowPlaying();
      notifyListeners('navigate_to_artist', track.artist);
    });
  }
}

// bindNowPlayingSleepControls conecta el popover rápido del temporizador de apagado dentro de Now Playing.
function bindNowPlayingSleepControls() {
  const wrapper = document.getElementById('npSleepWrapper');
  const btnSleep = document.getElementById('npBtnSleep');
  const popover = document.getElementById('npSleepPopover');
  if (!wrapper || !btnSleep || !popover) return;

  btnSleep.addEventListener('click', (e) => {
    e.stopPropagation();
    popover.classList.toggle('hidden');
  });

  popover.querySelectorAll('.np-sleep-opt').forEach((opt) => {
    opt.addEventListener('click', async (e) => {
      e.stopPropagation();
      popover.classList.add('hidden');
      try {
        if (opt.dataset.sleepMode === 'track_end') {
          const updated = await api.setSleepAfterTrack();
          if (updated) Object.assign(state.player, updated);
          showToast('La música se pausará al terminar esta canción');
        } else {
          const mins = parseInt(opt.dataset.minutes, 10);
          const updated = await api.setSleepTimer(mins);
          if (updated) Object.assign(state.player, updated);
          showToast(mins > 0 ? `Apagado programado en ${mins} minutos` : 'Temporizador cancelado');
        }
        lastSleepSyncAt = Date.now();
        renderSleepTimerUI();
      } catch (err) {
        showToast(err.message || 'No se pudo programar el temporizador');
      }
    });
  });

  document.addEventListener('click', (e) => {
    if (!wrapper.contains(e.target)) {
      popover.classList.add('hidden');
    }
  });
}

// initWakeLock configura el modo atril para tablet (Screen Wake Lock API) manteniendo la pantalla activa al reproducir.
function initWakeLock() {
  const checkbox = document.getElementById('settingWakeLock');
  const statusEl = document.getElementById('wakeLockStatusText');
  if (!checkbox) return;

  checkbox.checked = wakeLockEnabled;
  if (!('wakeLock' in navigator)) {
    if (statusEl) {
      statusEl.textContent = 'Requiere HTTPS y navegador compatible para bloquear el reposo de pantalla.';
    }
    checkbox.addEventListener('change', () => {
      wakeLockEnabled = checkbox.checked;
      localStorage.setItem('aurabox_wake_lock', wakeLockEnabled ? 'true' : 'false');
      if (wakeLockEnabled) {
        showToast('Modo Atril guardado (requiere acceso por HTTPS para activarse)');
      }
    });
    return;
  }

  checkbox.addEventListener('change', () => {
    wakeLockEnabled = checkbox.checked;
    localStorage.setItem('aurabox_wake_lock', wakeLockEnabled ? 'true' : 'false');
    syncWakeLockWithPlayback();
    showToast(wakeLockEnabled ? 'Modo Atril en Tablet activado' : 'Modo Atril en Tablet desactivado');
  });

  document.addEventListener('visibilitychange', () => {
    if (document.visibilityState === 'visible') {
      syncWakeLockWithPlayback();
    }
  });

  syncWakeLockWithPlayback();
}

// syncWakeLockWithPlayback adquiere o libera el bloqueo de pantalla según el estado de reproducción y visibilidad.
async function syncWakeLockWithPlayback() {
  const statusEl = document.getElementById('wakeLockStatusText');
  if (!('wakeLock' in navigator)) return;

  const shouldHold = wakeLockEnabled && state.player.status === 'play' && document.visibilityState === 'visible';
  if (shouldHold) {
    if (!wakeLockSentinel) {
      try {
        wakeLockSentinel = await navigator.wakeLock.request('screen');
        wakeLockSentinel.addEventListener('release', () => {
          wakeLockSentinel = null;
        });
        if (statusEl) {
          statusEl.textContent = 'Activo: pantalla encendida mientras suena música.';
        }
      } catch (_) {
        if (statusEl) {
          statusEl.textContent = 'En espera: se activará al interactuar o usar HTTPS.';
        }
      }
    }
  } else {
    if (wakeLockSentinel) {
      try {
        await wakeLockSentinel.release();
      } catch (_) {
        // Ignorar errores al liberar sentinel ya expirado
      }
      wakeLockSentinel = null;
    }
    if (statusEl) {
      statusEl.textContent = wakeLockEnabled
        ? 'Preparado: mantendrá la pantalla encendida durante la reproducción.'
        : 'Evita que la pantalla se apague mientras suena música.';
    }
  }
}

// isAmbientViewportAllowed comprueba si el tamaño/orientación actual admite Modo Marco Musical según preferencias.
function isAmbientViewportAllowed() {
  if (ambientOrientation === 'landscape') {
    return window.matchMedia('(orientation: landscape) and (min-width: 560px)').matches;
  }
  return true;
}

// enterAmbientModeNow activa de inmediato el Modo Marco Musical en Now Playing.
export function enterAmbientModeNow() {
  const modal = document.getElementById('nowPlayingModal');
  if (!modal || !state.ui.isNowPlayingOpen) return;
  if (ambientInactivityTimer) {
    clearTimeout(ambientInactivityTimer);
    ambientInactivityTimer = null;
  }
  modal.classList.add('ambient-mode');
  notifyListeners('ambient_mode_entered', true);
}

// exitAmbientMode restaura los controles completos de Now Playing saliendo del modo marco musical.
function exitAmbientMode(restartTimer = true) {
  const modal = document.getElementById('nowPlayingModal');
  if (modal && modal.classList.contains('ambient-mode')) {
    modal.classList.remove('ambient-mode');
  }
  if (ambientInactivityTimer) {
    clearTimeout(ambientInactivityTimer);
    ambientInactivityTimer = null;
  }
  if (restartTimer && state.ui.isNowPlayingOpen) {
    resetAmbientTimer();
  }
}

// resetAmbientTimer reinicia la cuenta atrás configurada de inactividad para entrar en modo marco musical.
function resetAmbientTimer() {
  if (ambientInactivityTimer) {
    clearTimeout(ambientInactivityTimer);
    ambientInactivityTimer = null;
  }
  if (!ambientModeEnabled || !state.ui.isNowPlayingOpen || !isAmbientViewportAllowed()) {
    return;
  }
  ambientInactivityTimer = setTimeout(() => {
    ambientInactivityTimer = null;
    if (!ambientModeEnabled || !state.ui.isNowPlayingOpen || !isAmbientViewportAllowed()) return;
    enterAmbientModeNow();
  }, ambientTimeoutSec * 1000);
}

// syncAmbientSettingsUI sincroniza los selectores de parámetros del Modo Marco Musical en Ajustes.
function syncAmbientSettingsUI() {
  const checkbox = document.getElementById('settingAmbientMode');
  if (checkbox) {
    checkbox.checked = ambientModeEnabled;
  }
  document.querySelectorAll('#ambientTimeoutSelector .rg-opt').forEach((btn) => {
    const val = parseInt(btn.dataset.ambientSec || '60', 10);
    btn.classList.toggle('active', val === ambientTimeoutSec);
  });
  document.querySelectorAll('#ambientOrientationSelector .rg-opt').forEach((btn) => {
    btn.classList.toggle('active', (btn.dataset.ambientOrient || 'all') === ambientOrientation);
  });
}

// initAmbientScreen gestiona el modo marco musical configurable y el acceso directo desde Now Playing.
function initAmbientScreen() {
  syncAmbientSettingsUI();

  const checkbox = document.getElementById('settingAmbientMode');
  if (checkbox) {
    checkbox.addEventListener('change', () => {
      ambientModeEnabled = checkbox.checked;
      localStorage.setItem('aurabox_ambient_mode', ambientModeEnabled ? 'true' : 'false');
      if (!ambientModeEnabled) {
        exitAmbientMode(false);
        showToast('Modo Marco Musical automático desactivado');
      } else {
        resetAmbientTimer();
        showToast(`Modo Marco Musical activado (${ambientTimeoutSec}s)`);
      }
    });
  }

  document.querySelectorAll('#ambientTimeoutSelector .rg-opt').forEach((btn) => {
    btn.addEventListener('click', () => {
      const sec = parseInt(btn.dataset.ambientSec || '60', 10);
      if (![10, 15, 30, 60, 120].includes(sec)) return;
      ambientTimeoutSec = sec;
      localStorage.setItem('aurabox_ambient_timeout_sec', String(sec));
      syncAmbientSettingsUI();
      resetAmbientTimer();
      showToast(`Marco Musical automático tras ${sec}s sin tocar`);
    });
  });

  document.querySelectorAll('#ambientOrientationSelector .rg-opt').forEach((btn) => {
    btn.addEventListener('click', () => {
      const orient = (btn.dataset.ambientOrient === 'landscape') ? 'landscape' : 'all';
      ambientOrientation = orient;
      localStorage.setItem('aurabox_ambient_orientation', orient);
      syncAmbientSettingsUI();
      if (!isAmbientViewportAllowed()) {
        exitAmbientMode(false);
      } else {
        resetAmbientTimer();
      }
      showToast(orient === 'landscape'
        ? 'Marco Musical limitado a pantalla horizontal'
        : 'Marco Musical activo en vertical y horizontal');
    });
  });

  const btnManualAmbient = document.getElementById('npBtnAmbient');
  if (btnManualAmbient) {
    btnManualAmbient.addEventListener('click', (e) => {
      e.stopPropagation();
      enterAmbientModeNow();
      showToast('Modo Marco Musical activado • Toca la pantalla para volver');
    });
  }

  const modal = document.getElementById('nowPlayingModal');
  if (!modal) return;

  const handleActivity = (e) => {
    if (!state.ui.isNowPlayingOpen) return;
    if (e.target && e.target.closest('#npBtnAmbient')) return;
    if (modal.classList.contains('ambient-mode')) {
      swallowAmbientWakeClick = true;
      setTimeout(() => {
        swallowAmbientWakeClick = false;
      }, 400);
      exitAmbientMode(true);
      if (e.cancelable && e.type !== 'keydown') {
        e.stopPropagation();
      }
      return;
    }
    resetAmbientTimer();
  };

  modal.addEventListener('pointerdown', handleActivity, { capture: true, passive: true });
  modal.addEventListener('touchstart', handleActivity, { capture: true, passive: true });
  modal.addEventListener('wheel', handleActivity, { capture: true, passive: true });
  modal.addEventListener('click', (e) => {
    if (swallowAmbientWakeClick) {
      e.preventDefault();
      e.stopPropagation();
    }
  }, { capture: true });

  window.addEventListener('keydown', () => {
    if (state.ui.isNowPlayingOpen) {
      exitAmbientMode(true);
    }
  });

  const handleViewportChange = () => {
    if (!isAmbientViewportAllowed() || !state.ui.isNowPlayingOpen) {
      exitAmbientMode(false);
    } else {
      resetAmbientTimer();
    }
  };
  window.addEventListener('resize', handleViewportChange);
  window.addEventListener('orientationchange', handleViewportChange);
}

// renderSleepTimerUI actualiza el distintivo del temporizador en Now Playing y en Ajustes.
export function renderSleepTimerUI() {
  const btnSleep = document.getElementById('npBtnSleep');
  const badge = document.getElementById('npSleepBadge');
  const settingsStatus = document.getElementById('settingsSleepStatus');

  const mode = state.player.sleep_timer_mode || '';
  const sec = Math.max(0, Math.floor(Number(state.player.sleep_timer_sec || 0)));

  if (mode === 'track_end') {
    btnSleep?.classList.add('active');
    if (badge) {
      badge.textContent = 'Fin pista';
      badge.classList.remove('hidden');
    }
    if (settingsStatus) {
      settingsStatus.textContent = 'Activo: se pausará al terminar la canción actual';
    }
  } else if (sec > 0) {
    const label = formatTime(sec);
    btnSleep?.classList.add('active');
    if (badge) {
      badge.textContent = label;
      badge.classList.remove('hidden');
    }
    if (settingsStatus) {
      settingsStatus.textContent = `Activo: apagado automático en ${label}`;
    }
  } else {
    btnSleep?.classList.remove('active');
    if (badge) {
      badge.textContent = '';
      badge.classList.add('hidden');
    }
    if (settingsStatus) {
      settingsStatus.textContent = 'Temporizador inactivo';
    }
  }
}

// DSP_PRESET_BANDS define las curvas orientativas de 5 bandas [60Hz, 250Hz, 1kHz, 4kHz, 12kHz] para cada perfil.
export const DSP_PRESET_BANDS = {
  flat: [0, 0, 0, 0, 0],
  night: [4, 1, 0, 1, 2],
  voice: [-4, -1, 2, 4, 1],
  livingroom: [3, 1, 0, 2, 2],
  bass: [6, -1, 0, 0, 1]
};

// DSP_PRESET_DESCRIPTIONS detalla la respuesta acústica de cada perfil DSP soportado por SoX.
const DSP_PRESET_DESCRIPTIONS = {
  flat: 'Señal directa bit-perfect ALSA sin alterar muestras ni ecualización.',
  night: 'Realce de graves y agudos a bajo volumen con compresión suave de picos.',
  voice: 'Limpia subgraves y realza claridad vocal e instrumentos acústicos.',
  livingroom: 'Escena cálida con presencia en sala para altavoces domésticos.',
  bass: 'Refuerzo cálido en subgraves para cajas compactas o auriculares abiertos.',
  custom: 'Curva gráfica personalizada de 5 bandas (±12 dB) procesada a 32-bit en SoX.'
};

// parseDSPBands extrae los 5 valores de ganancia en dB desde un perfil DSP o curva "eq:b1,b2,b3,b4,b5".
export function parseDSPBands(preset) {
  const clean = String(preset || 'flat').trim().toLowerCase();
  if (clean.startsWith('eq:')) {
    const parts = clean.slice(3).split(',').map((n) => {
      const v = parseInt(n.trim(), 10);
      if (Number.isNaN(v)) return 0;
      return Math.max(-12, Math.min(12, v));
    });
    if (parts.length === 5) return parts;
  }
  return [...(DSP_PRESET_BANDS[clean] || DSP_PRESET_BANDS.flat)];
}

// formatEQPresetString serializa 5 bandas en formato backend "eq:b1,b2,b3,b4,b5" o "flat" si están a cero.
export function formatEQPresetString(bands) {
  const clean = [0, 1, 2, 3, 4].map((idx) => {
    const v = parseInt(bands?.[idx] ?? 0, 10);
    if (Number.isNaN(v)) return 0;
    return Math.max(-12, Math.min(12, v));
  });
  if (clean.every((v) => v === 0)) return 'flat';
  return `eq:${clean.join(',')}`;
}

// renderEQSliders actualiza visualmente los 5 deslizadores del ecualizador y sus lecturas en dB.
export function renderEQSliders(bands) {
  const safe = Array.isArray(bands) && bands.length === 5 ? bands : [0, 0, 0, 0, 0];
  document.querySelectorAll('#eqBandsList .eq-band-slider').forEach((slider) => {
    const idx = parseInt(slider.dataset.band || '0', 10);
    const val = safe[idx] ?? 0;
    slider.value = String(val);
    const label = document.querySelector(`#eqBandsList [data-band-val="${idx}"]`);
    if (label) {
      label.textContent = `${val > 0 ? '+' : ''}${val} dB`;
    }
  });
}

// syncSettingsAudioControls mantiene sincronizados Crossfade, ReplayGain, Fade, Marco Musical y EQ DSP entre dispositivos.
export function syncSettingsAudioControls() {
  const cfSlider = document.getElementById('settingCrossfade');
  const cfLabel = document.getElementById('labelCrossfade');
  const cf = Number(state.player.crossfade || 0);
  if (cfSlider && document.activeElement !== cfSlider) {
    cfSlider.value = cf;
  }
  if (cfLabel) {
    cfLabel.textContent = `${cf}s`;
  }

  const rg = (state.player.replay_gain || 'auto').toLowerCase();
  document.querySelectorAll('#replayGainSelector .rg-opt').forEach((btn) => {
    btn.classList.toggle('active', btn.dataset.rg === rg);
  });

  const softFadeChk = document.getElementById('settingSoftFade');
  if (softFadeChk && typeof state.player.soft_fade === 'boolean') {
    softFadeChk.checked = state.player.soft_fade;
  }
  const fadeMs = Number(state.player.soft_fade_ms || 1100);
  document.querySelectorAll('#softFadeDurationSelector .rg-opt').forEach((btn) => {
    const val = parseInt(btn.dataset.fadeMs || '1100', 10);
    btn.classList.toggle('active', val === fadeMs);
  });

  const activeOut = state.player.active_output || 'Sin salida';
  const outputs = Array.isArray(state.player.audio_outputs) ? state.player.audio_outputs : [];
  const activeObj = outputs.find((o) => o.name === activeOut || o.enabled);
  const activePlugin = (activeObj?.plugin || '').toLowerCase();
  const isNonDSPOutput = (activePlugin === 'pulse' || activePlugin === 'null' || activePlugin === 'httpd');

  const presetMap = (state.player.dsp_presets && typeof state.player.dsp_presets === 'object')
    ? state.player.dsp_presets
    : {};
  const rawPreset = (presetMap[activeOut] || state.player.dsp_preset || 'flat').toLowerCase();
  const uiPresetKey = rawPreset.startsWith('eq:') ? 'custom' : rawPreset;

  const dspOutLabel = document.getElementById('dspActiveOutputName');
  if (dspOutLabel) {
    dspOutLabel.textContent = activeOut;
  }

  const compatHint = document.getElementById('dspCompatHint');
  if (compatHint) {
    if (activePlugin === 'pulse') {
      compatHint.textContent = 'Nota: Estás usando la salida PulseAudio del host. En la Raspberry Pi (salidas ALSA, Jack 3.5mm, DAC USB o I2S HAT), el DSP SoX procesa directamente las 5 bandas y perfiles.';
      compatHint.classList.remove('hidden');
    } else if (activePlugin === 'null') {
      compatHint.textContent = 'Nota: No hay tarjeta física ALSA detectada; selecciona un DAC USB, Jack o HDMI para aplicar ecualización DSP.';
      compatHint.classList.remove('hidden');
    } else {
      compatHint.textContent = '';
      compatHint.classList.add('hidden');
    }
  }

  const dspModeBadge = document.getElementById('dspModeBadge');
  if (dspModeBadge) {
    const isDSPEnabled = rawPreset !== 'flat';
    dspModeBadge.textContent = isNonDSPOutput
      ? (activePlugin ? activePlugin.toUpperCase() : 'NO ALSA')
      : (isDSPEnabled ? 'DSP SoX' : 'Bit-Perfect');
    dspModeBadge.style.background = (!isNonDSPOutput && isDSPEnabled) ? 'rgba(99, 102, 241, 0.2)' : '';
    dspModeBadge.style.color = (!isNonDSPOutput && isDSPEnabled) ? 'var(--accent-light)' : '';
  }

  document.querySelectorAll('#dspPresetSelector .dsp-opt').forEach((btn) => {
    btn.classList.toggle('active', btn.dataset.dsp === uiPresetKey);
  });

  const dspDesc = document.getElementById('dspPresetDesc');
  if (dspDesc) {
    dspDesc.textContent = DSP_PRESET_DESCRIPTIONS[uiPresetKey] || DSP_PRESET_DESCRIPTIONS.flat;
  }

  const eqContainer = document.getElementById('eqBandsList');
  if (!eqContainer || !eqContainer.contains(document.activeElement)) {
    renderEQSliders(parseDSPBands(rawPreset));
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
  const isLiveMini = isLiveStreamTrack(track);
  document.getElementById('miniTitle').textContent = track.title || 'Desconocido';
  document.getElementById('miniArtist').textContent = isLiveMini
    ? formatLiveSubtitle(track, state.player.stream_title, false)
    : (track.artist || 'Artista');

  const coverImg = document.getElementById('miniCover');
  if (coverImg) {
    coverImg.removeAttribute('data-fallback-applied');
    coverImg.dataset.fallback = '/assets/default-cover.svg';
    coverImg.src = sanitizeCoverURL(track.cover_url);
  }

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

  const npModal = document.getElementById('nowPlayingModal');
  if (npModal) {
    npModal.classList.toggle('playing', isPlaying);
  }

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

  const volDbfs = document.getElementById('npVolDbfs');
  if (volDbfs) {
    const vol = Number(state.player.volume) || 0;
    volDbfs.textContent = vol > 0 ? `${Math.round(20 * Math.log10(vol / 100))} dB` : '-∞ dB';
  }

  const settingMaxVol = document.getElementById('settingMaxVol');
  const labelMaxVol = document.getElementById('labelMaxVol');
  if (settingMaxVol && document.activeElement !== settingMaxVol) {
    settingMaxVol.value = maxLimit;
  }
  if (labelMaxVol) {
    labelMaxVol.textContent = `${maxLimit}%`;
  }

  // Actualizar insignia de salida de audio activa en cabecera y ajustes audio/sleep
  const outBadge = document.getElementById('headerActiveOutput');
  if (outBadge) {
    outBadge.textContent = state.player.active_output || 'Audio';
  }
  syncSettingsAudioControls();
  renderSleepTimerUI();

  const btnQuickMix = document.getElementById('npBtnQuickMix');
  const npArtistEl = document.getElementById('npArtist');

  if (!track) {
    document.getElementById('npTitle').textContent = 'AuraBox Hi-Fi';
    if (npArtistEl) {
      npArtistEl.textContent = 'Sin reproducción activa • Elige un álbum o carpeta';
      npArtistEl.classList.remove('clickable');
    }
    if (btnQuickMix) btnQuickMix.style.display = 'none';
    document.getElementById('npCover').src = '/assets/default-cover.svg';
    const badge = document.getElementById('npHifiBadge');
    if (badge) badge.textContent = 'En Espera';
    const btnFav = document.getElementById('npBtnFav');
    if (btnFav) {
      btnFav.style.display = 'flex';
      btnFav.classList.remove('active');
    }
    const elapsedLabel = document.getElementById('npTimeElapsed');
    const durationLabel = document.getElementById('npTimeDuration');
    const scrubber = document.getElementById('npTimelineScrubber');
    if (elapsedLabel) elapsedLabel.textContent = '0:00';
    if (durationLabel) durationLabel.textContent = '0:00';
    if (scrubber) {
      scrubber.disabled = false;
      scrubber.style.opacity = '';
      scrubber.value = 0;
    }
    return;
  }

  const isLive = isLiveStreamTrack(track);

  document.getElementById('npTitle').textContent = track.title || 'Canción';
  if (npArtistEl) {
    npArtistEl.textContent = isLive
      ? formatLiveSubtitle(track, state.player.stream_title, true)
      : (track.artist || 'Artista');
    npArtistEl.classList.toggle('clickable', !isLive && Boolean(track.artist));
  }
  if (btnQuickMix) {
    btnQuickMix.style.display = (!isLive && track.artist) ? 'flex' : 'none';
  }

  const coverSrc = sanitizeCoverURL(track.cover_url);
  const npCover = document.getElementById('npCover');
  if (npCover) {
    npCover.removeAttribute('data-fallback-applied');
    npCover.dataset.fallback = '/assets/default-cover.svg';
    npCover.src = coverSrc;
  }

  // Fondo adaptativo con desenfoque basado en la carátula validada sin riesgo de inyección CSS
  const bg = document.getElementById('npAmbientBg');
  if (bg) {
    bg.style.backgroundImage = `url("${coverSrc}")`;
  }

  // Badge Hi-Fi técnico o transmisión en directo
  const badge = document.getElementById('npHifiBadge');
  if (badge) {
    if (isLive) {
      const parts = ['EN VIVO', track.format || 'STREAM'];
      if (track.sample_rate) parts.push(`${Math.round(track.sample_rate / 1000)}kHz`);
      if (track.bitrate) parts.push(`${track.bitrate}kbps`);
      badge.textContent = parts.join(' • ');
    } else {
      const rateKhz = track.sample_rate ? Math.round(track.sample_rate / 1000) : 44;
      badge.textContent = `${track.format || 'FLAC'} ${rateKhz}kHz • ${track.bitrate || 320}kbps`;
    }
  }

  // Ocultar corazón en emisoras en directo (no tienen ID de pista en tabla tracks)
  const btnFav = document.getElementById('npBtnFav');
  if (btnFav) {
    btnFav.style.display = isLive ? 'none' : 'flex';
    btnFav.classList.toggle('active', !isLive && !!track.favorite);
  }

  // Actualizar marcas de duración y bloqueo del scrubber en directos
  const dur = (track && track.duration) || state.player.duration || 0;
  const durationLabel = document.getElementById('npTimeDuration');
  if (durationLabel) {
    if (dur > 0) {
      durationLabel.textContent = formatTime(dur);
    } else if (isLive) {
      durationLabel.textContent = 'EN VIVO';
    } else {
      durationLabel.textContent = '0:00';
    }
  }
  const scrubber = document.getElementById('npTimelineScrubber');
  if (scrubber) {
    if (isLive && dur <= 0) {
      scrubber.disabled = true;
      scrubber.max = 1;
      scrubber.value = 1;
      scrubber.style.opacity = '0.45';
    } else {
      scrubber.disabled = false;
      scrubber.style.opacity = '';
      if (dur > 0) scrubber.max = dur;
    }
  }

  tickProgress();
}

// tickProgress avanza la barra de tiempo en tiempo real entre eventos SSE.
export function tickProgress() {
  const isLive = isLiveStreamTrack(state.player.current_track);
  const dur = state.player.duration || (state.player.current_track?.duration) || 0;

  if (state.player.status === 'play' && (dur > 0 || isLive)) {
    const now = (typeof performance !== 'undefined' ? performance.now() : Date.now());
    const lastSync = state.player.last_sync_timestamp || now;
    const deltaSec = Math.max(0, (now - lastSync) / 1000);
    const calculated = (state.player.base_elapsed_time || 0) + deltaSec;
    state.player.elapsed_time = dur > 0 ? Math.min(dur, calculated) : calculated;
  }

  const elapsed = state.player.elapsed_time || 0;
  const pct = dur > 0
    ? Math.min(100, Math.max(0, (elapsed / dur) * 100))
    : (isLive && state.player.status === 'play' ? 100 : 0);

  const miniFill = document.getElementById('miniProgressFill');
  if (miniFill) miniFill.style.width = `${pct}%`;

  if (!isScrubbing) {
    const scrubber = document.getElementById('npTimelineScrubber');
    const elapsedLabel = document.getElementById('npTimeElapsed');
    const durationLabel = document.getElementById('npTimeDuration');

    if (scrubber) {
      if (isLive && dur <= 0) {
        scrubber.max = 1;
        scrubber.value = 1;
      } else {
        if (dur > 0) scrubber.max = dur;
        scrubber.value = elapsed;
      }
    }
    if (elapsedLabel) elapsedLabel.textContent = formatTime(elapsed);
    if (durationLabel) {
      if (dur > 0) {
        durationLabel.textContent = formatTime(dur);
      } else if (isLive) {
        durationLabel.textContent = 'EN VIVO';
      } else {
        durationLabel.textContent = '0:00';
      }
    }
  }

  // Actualizar cuenta atrás visual suave cuando el temporizador por minutos está activo
  if (state.player.sleep_timer_mode === 'minutes' && state.player.sleep_timer_sec > 0 && lastSleepSyncAt > 0) {
    const nowMs = Date.now();
    if (nowMs - lastSleepSyncAt >= 1000) {
      const dec = Math.floor((nowMs - lastSleepSyncAt) / 1000);
      lastSleepSyncAt += dec * 1000;
      state.player.sleep_timer_sec = Math.max(0, state.player.sleep_timer_sec - dec);
      renderSleepTimerUI();
    }
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
    if (!isLiveStreamTrack(state.player.current_track) && details.seekTime !== undefined) {
      api.seek(details.seekTime);
    }
  });
}

// updateMediaSession sincroniza carátula y metadatos con la pantalla de bloqueo.
function updateMediaSession() {
  if (!('mediaSession' in navigator) || !state.player.current_track) return;
  const track = state.player.current_track;
  const isLive = isLiveStreamTrack(track);
  const icy = (state.player.stream_title || '').trim();

  navigator.mediaSession.metadata = new MediaMetadata({
    title: (isLive && icy) ? icy : track.title,
    artist: (isLive && icy) ? track.title : track.artist,
    album: isLive ? (track.artist || 'Radio en Directo') : track.album,
    artwork: [
      { src: sanitizeCoverURL(track.cover_url), sizes: '512x512', type: 'image/png' }
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

