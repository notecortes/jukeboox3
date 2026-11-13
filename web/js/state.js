// =====================================================================
// AuraBox - Reactive Global Application State
// =====================================================================

export const state = {
  // Estado del reproductor de audio sincronizado desde el servidor
  player: {
    status: 'stop',
    current_track: null,
    current_index: -1,
    queue_length: 0,
    elapsed_time: 0,
    base_elapsed_time: 0,
    last_sync_timestamp: (typeof performance !== 'undefined' ? performance.now() : Date.now()),
    duration: 0,
    volume: 65,
    max_volume_limit: 85,
    repeat: false,
    random: false,
    crossfade: 2,
    replay_gain: 'auto',
    soft_fade: true,
    dsp_preset: 'flat',
    dsp_presets: {},
    audio_outputs: [],
    active_output: 'Jack 3.5mm Analógico',
    sleep_timer_sec: 0,
    sleep_timer_mode: '',
    stream_title: '',
    last_error: ''
  },

  // Cola de canciones (Jukebox Queue)
  queue: [],

  // Catálogo musical en memoria
  library: {
    tracks: [],
    recentTracks: [],
    albums: [],
    artists: [],
    themes: [],
    selectedAlbum: null,
    currentFolder: null
  },

  // Estado de vistas y navegación
  ui: {
    activeTab: 'library',
    activeSubTab: 'albums',
    detailView: null,
    isNowPlayingOpen: false,
    isLyricsViewOpen: false,
    contextMenuTarget: null,
    optimisticPlay: null
  },

  // Letras de canciones sincronizadas (LRCLIB)
  lyrics: {
    trackKey: '',
    synced: [],
    plain: '',
    found: false,
    activeIndex: -1
  },

  // Suscriptores para reactividad
  listeners: []
};

// subscribe registra una función callback que se ejecuta ante cualquier mutación de estado.
export function subscribe(callback) {
  state.listeners.push(callback);
  return () => {
    state.listeners = state.listeners.filter((cb) => cb !== callback);
  };
}

// notifyListeners notifica a todos los componentes suscritos para actualizar la UI.
export function notifyListeners(event, data) {
  state.listeners.forEach((callback) => {
    try {
      callback(event, data);
    } catch (err) {
      console.error('[State] Error en listener:', err);
    }
  });
}

// updatePlayerState fusiona el estado recibido por SSE o polling en el estado global.
export function updatePlayerState(newState) {
  if (newState && typeof newState.elapsed_time === 'number') {
    state.player.last_sync_timestamp = (typeof performance !== 'undefined' ? performance.now() : Date.now());
    state.player.base_elapsed_time = newState.elapsed_time;
  }
  if (newState && newState.dsp_presets !== undefined) {
    state.player.dsp_presets = (newState.dsp_presets && typeof newState.dsp_presets === 'object')
      ? { ...newState.dsp_presets }
      : {};
  }
  Object.assign(state.player, newState);
  notifyListeners('player_updated', state.player);
}

// updateQueueState actualiza la lista de la cola de reproducción.
export function updateQueueState(newQueue) {
  state.queue = newQueue;
  state.player.queue_length = newQueue.length;
  notifyListeners('queue_updated', state.queue);
}

