// =====================================================================
// AuraBox - REST API Client for Go Backend
// =====================================================================

const API_BASE = '/api';

// requestHelper ejecuta peticiones HTTP fetch con manejo de errores y JSON parsing.
async function requestHelper(endpoint, options = {}) {
  try {
    const res = await fetch(`${API_BASE}${endpoint}`, {
      headers: {
        'Content-Type': 'application/json',
        ...(options.headers || {})
      },
      ...options
    });
    if (!res.ok) {
      const errText = await res.text();
      throw new Error(`API Error [${res.status}]: ${errText}`);
    }
    return await res.json();
  } catch (err) {
    console.error(`[API] Fallo en ${endpoint}:`, err);
    throw err;
  }
}

// getStatus consulta el estado general del reproductor.
export function getStatus() {
  return requestHelper('/status');
}

// play inicia o reanuda la reproducción.
export function play() {
  return requestHelper('/player/play', { method: 'POST' });
}

// pause pausa la reproducción de audio.
export function pause() {
  return requestHelper('/player/pause', { method: 'POST' });
}

// toggle conmuta entre reproducir y pausar de forma instantánea.
export function toggle() {
  return requestHelper('/player/toggle', { method: 'POST' });
}

// next avanza a la siguiente canción en la cola.
export function next() {
  return requestHelper('/player/next', { method: 'POST' });
}

// prev salta a la canción anterior o reinicia la actual.
export function prev() {
  return requestHelper('/player/prev', { method: 'POST' });
}

// seek desplaza el cabezal de reproducción al segundo indicado.
export function seek(seconds) {
  return requestHelper(`/player/seek?seconds=${encodeURIComponent(seconds)}`, { method: 'POST' });
}

// setRepeat activa o desactiva el modo de repetición de la cola en el servidor.
export function setRepeat(enabled) {
  return requestHelper(`/player/repeat?enabled=${enabled ? 'true' : 'false'}`, { method: 'POST' });
}

// setVolume ajusta el volumen general respetando la salvaguarda de volumen.
export function setVolume(value) {
  return requestHelper(`/player/volume?value=${encodeURIComponent(value)}`, { method: 'POST' });
}

// stepVolume sube o baja el volumen por pasos directos (+/- delta).
export function stepVolume(delta) {
  return requestHelper(`/player/volume/step?delta=${encodeURIComponent(delta)}`, { method: 'POST' });
}

// setMaxVolumeLimit configura el tope de ganancia de protección auditiva.
export function setMaxVolumeLimit(limit) {
  return requestHelper(`/player/volume/max?limit=${encodeURIComponent(limit)}`, { method: 'POST' });
}

// selectAudioOutput conmuta la salida activa (Jack 3.5mm, USB DAC, I2S HAT, Stream).
export function selectAudioOutput(outputId) {
  return requestHelper(`/player/output?id=${encodeURIComponent(outputId)}`, { method: 'POST' });
}

// refreshAudioOutputs fuerza a MPD a re-escanear tarjetas ALSA / DAC USB conectados tras el arranque.
export function refreshAudioOutputs() {
  return requestHelper('/player/outputs/refresh', { method: 'POST' });
}

// setOutputDSPPreset asigna un perfil DSP/ecualizador SoX por salida de audio y recarga MPD si está activa.
export function setOutputDSPPreset(outputName, preset) {
  return requestHelper('/player/dsp', {
    method: 'POST',
    body: JSON.stringify({
      output: outputName || '',
      preset: preset || 'flat'
    })
  });
}

// getAudioDiagnostics consulta las salidas actuales en MPD y la lista ALSA (aplay -l) del contenedor.
export function getAudioDiagnostics() {
  return requestHelper('/player/outputs/diagnostics');
}

// setAudioConfig ajusta parámetros audiófilos de ReplayGain, Crossfade y Fade en pausa.
export function setAudioConfig(replayGain, crossfade, softFade = undefined) {
  let qs = `replay_gain=${encodeURIComponent(replayGain)}&crossfade=${encodeURIComponent(crossfade)}`;
  if (typeof softFade === 'boolean') {
    qs += `&soft_fade=${softFade ? 'true' : 'false'}`;
  }
  return requestHelper(`/player/config?${qs}`, { method: 'POST' });
}

// setSleepTimer programa el apagado automático de la música en N minutos.
export function setSleepTimer(minutes) {
  return requestHelper(`/player/sleeptimer?minutes=${encodeURIComponent(minutes)}`, { method: 'POST' });
}

// setSleepAfterTrack programa la pausa automática justo al finalizar la pista actual.
export function setSleepAfterTrack() {
  return requestHelper('/player/sleeptimer?mode=track_end', { method: 'POST' });
}

// getQueue obtiene la cola de reproducción actual.
export function getQueue() {
  return requestHelper('/queue');
}

// addToQueue añade canciones a la cola (play_now, play_next, append, replace).
export function addToQueue(payload) {
  return requestHelper('/queue/add', {
    method: 'POST',
    body: JSON.stringify(payload)
  });
}

// playQueueIndex salta directamente a reproducir la pista en el índice indicado de la cola.
export function playQueueIndex(index) {
  return requestHelper(`/queue/play?index=${encodeURIComponent(index)}`, { method: 'POST' });
}

// moveInQueue reordena una pista dentro de la lista (Drag & Drop táctil).
export function moveInQueue(from, to) {
  return requestHelper(`/queue/move?from=${encodeURIComponent(from)}&to=${encodeURIComponent(to)}`, { method: 'POST' });
}

// removeFromQueue elimina una canción de la cola (Swipe-to-delete).
export function removeFromQueue(index) {
  return requestHelper(`/queue/remove?index=${encodeURIComponent(index)}`, { method: 'POST' });
}

// clearQueue vacía completamente la cola de reproducción.
export function clearQueue() {
  return requestHelper('/queue/clear', { method: 'POST' });
}

// shuffleQueue baraja la cola con algoritmo anti-repetición de artista.
export function shuffleQueue() {
  return requestHelper('/queue/shuffle', { method: 'POST' });
}

// getLibraryTracks consulta el listado de todas las canciones indexadas.
export function getLibraryTracks() {
  return requestHelper('/library/tracks');
}

// getLibraryRecent consulta las canciones recientes, escuchadas o más reproducidas según el modo indicado.
export function getLibraryRecent(limit = 30, mode = 'added') {
  return requestHelper(`/library/recent?limit=${encodeURIComponent(limit)}&mode=${encodeURIComponent(mode)}`);
}

// getLibraryAlbums obtiene el catálogo de álbumes discográficos (por defecto LPs > 5 temas).
export function getLibraryAlbums(includeAll = false) {
  const query = includeAll ? '?include_all=true' : '';
  return requestHelper(`/library/albums${query}`);
}

// getLibraryAlbumDetail obtiene pistas y datos detallados de un álbum por su ID.
export function getLibraryAlbumDetail(id) {
  return requestHelper(`/library/album?id=${encodeURIComponent(id)}`);
}

// fetchAlbumCoverOnline busca y descarga la portada de un álbum desde Cover Art Archive sin alterar el NAS.
export function fetchAlbumCoverOnline(albumId) {
  return requestHelper(`/library/album/cover/fetch?album_id=${encodeURIComponent(albumId)}`, {
    method: 'POST'
  });
}

// getLibraryArtists obtiene los artistas de la colección con conteo de temas.
export function getLibraryArtists() {
  return requestHelper('/library/artists');
}

// getLibraryArtistDetail obtiene discografía y pistas completas de un artista por nombre.
export function getLibraryArtistDetail(artistName) {
  return requestHelper(`/library/artist?name=${encodeURIComponent(artistName)}`);
}

// getLibraryArtistTracks obtiene todas las pistas de un artista por nombre.
export function getLibraryArtistTracks(artistName) {
  return requestHelper(`/library/artist/tracks?name=${encodeURIComponent(artistName)}`);
}

// getLibraryGenres obtiene el resumen navegable de géneros y décadas disponibles.
export function getLibraryGenres() {
  return requestHelper('/library/genres');
}

// getLibraryGenreAlbums devuelve los álbumes filtrados por un género musical o década.
export function getLibraryGenreAlbums(type, value) {
  return requestHelper(`/library/genres/albums?type=${encodeURIComponent(type)}&value=${encodeURIComponent(value)}`);
}

// getLibraryFolders explora el árbol de carpetas de almacenamiento (Local, NAS, USB).
export function getLibraryFolders(path = '') {
  return requestHelper(`/library/folders?path=${encodeURIComponent(path)}`);
}

// playFolder reproduce inmediatamente todo el contenido de una carpeta (reemplaza o reproduce ya).
export function playFolder(folderPath, shuffle = false) {
  return addToQueue({
    folder: folderPath,
    mode: 'replace',
    clear_first: true,
    shuffle: shuffle
  });
}

// enqueueFolder añade todas las canciones de una carpeta al final de la cola activa.
export function enqueueFolder(folderPath, shuffle = false) {
  return addToQueue({
    folder: folderPath,
    mode: 'append',
    shuffle: shuffle
  });
}

// playFile reproduce de inmediato un archivo de audio específico en disco.
export function playFile(filePath) {
  return addToQueue({
    path: filePath,
    mode: 'play_now'
  });
}

// enqueueFile añade un archivo de audio específico al final de la cola activa.
export function enqueueFile(filePath) {
  return addToQueue({
    path: filePath,
    mode: 'append'
  });
}

// triggerLibraryScan solicita un re-escaneo reactivo (incremental o forzado) de las carpetas de música.
export function triggerLibraryScan(force = false) {
  const query = force ? '?force=true' : '';
  return requestHelper(`/library/scan${query}`, { method: 'POST' });
}

// toggleFavorite alterna el marcado de favorito (corazón) de una pista.
export function toggleFavorite(trackId) {
  return requestHelper(`/library/favorite?id=${encodeURIComponent(trackId)}`, { method: 'POST' });
}

// getFavorites obtiene todas las pistas marcadas como favoritas en la biblioteca.
export function getFavorites() {
  return requestHelper('/library/favorites');
}

// playFavorites encola o reproduce la colección completa de favoritas con o sin barajado.
export function playFavorites(shuffle = false, mode = 'play_now') {
  return requestHelper('/library/favorites/play', {
    method: 'POST',
    body: JSON.stringify({ mode, shuffle })
  });
}

// search ejecuta búsquedas instantáneas en pistas, álbumes y artistas.
export function search(query) {
  return requestHelper(`/search?q=${encodeURIComponent(query)}`);
}

// generateSmartMix solicita un mix balanceado de un artista semilla (40/60).
export function generateSmartMix(artist, count = 20, playNow = true) {
  return requestHelper('/smartmix', {
    method: 'POST',
    body: JSON.stringify({ artist, count, play_now: playNow })
  });
}

// getSmartMixThemes obtiene los géneros y décadas locales para mixes temáticos 100% offline.
export function getSmartMixThemes() {
  return requestHelper('/smartmix/themes');
}

// generateThemeMix genera una sesión Smart Mix offline por género o década.
export function generateThemeMix(themeType, themeValue, count = 30, playNow = true) {
  return requestHelper('/smartmix', {
    method: 'POST',
    body: JSON.stringify({
      theme_type: themeType,
      theme_value: themeValue,
      count,
      play_now: playNow
    })
  });
}

// getLyrics consulta letras sincronizadas en la API abierta LRCLIB.
export function getLyrics(title, artist, album = '', duration = 0, path = '') {
  const params = { title, artist, album, duration: Math.round(duration) };
  if (path) params.path = path;
  const q = new URLSearchParams(params);
  return requestHelper(`/lyrics?${q.toString()}`);
}

// getRadios obtiene las emisoras de radio por internet configuradas.
export function getRadios() {
  return requestHelper('/radios');
}

// addRadio guarda una nueva emisora personalizada (.mp3, .aac, .m3u, .pls, .m3u8) en el catálogo.
export function addRadio(radio) {
  return requestHelper('/radios', {
    method: 'POST',
    body: JSON.stringify(radio)
  });
}

// deleteRadio elimina una emisora personalizada añadida por el usuario según su ID.
export function deleteRadio(id) {
  return requestHelper(`/radios?id=${encodeURIComponent(id)}`, { method: 'DELETE' });
}

// playRadio arranca la reproducción de una emisora web.
export function playRadio(radio) {
  return requestHelper('/radios/play', {
    method: 'POST',
    body: JSON.stringify(radio)
  });
}

// getSettings lee las configuraciones persistentes del reproductor.
export function getSettings() {
  return requestHelper('/settings');
}

// saveSettings guarda parámetros de configuración en el servidor.
export function saveSettings(settingsMap) {
  return requestHelper('/settings', {
    method: 'POST',
    body: JSON.stringify(settingsMap)
  });
}

// getNASConfig consulta el estado y credenciales actuales del recurso de red NAS.
export function getNASConfig() {
  return requestHelper('/settings/nas');
}

// saveNASConfig guarda credenciales del NAS e inicia re-escaneo de música en red.
export function saveNASConfig(nasPayload) {
  return requestHelper('/settings/nas', {
    method: 'POST',
    body: JSON.stringify(nasPayload)
  });
}

// getSecurityStatus consulta el estado TLS/HTTPS, la URL segura disponible y la CA descargable.
export function getSecurityStatus() {
  return requestHelper('/security/status');
}


