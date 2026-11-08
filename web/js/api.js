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

// setAudioConfig ajusta parámetros audiófilos de ReplayGain y Crossfade.
export function setAudioConfig(replayGain, crossfade) {
  return requestHelper(`/player/config?replay_gain=${encodeURIComponent(replayGain)}&crossfade=${encodeURIComponent(crossfade)}`, { method: 'POST' });
}

// setSleepTimer programa el apagado automático de la música en N minutos.
export function setSleepTimer(minutes) {
  return requestHelper(`/player/sleeptimer?minutes=${encodeURIComponent(minutes)}`, { method: 'POST' });
}

// getQueue obtiene la cola de reproducción actual.
export function getQueue() {
  return requestHelper('/queue');
}

// addToQueue añade canciones a la cola (play_now, play_next, append).
export function addToQueue(payload) {
  return requestHelper('/queue/add', {
    method: 'POST',
    body: JSON.stringify(payload)
  });
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

// getLibraryRecent consulta las canciones añadidas recientemente.
export function getLibraryRecent(limit = 30) {
  return requestHelper(`/library/recent?limit=${encodeURIComponent(limit)}`);
}

// getLibraryAlbums obtiene el catálogo de álbumes discográficos.
export function getLibraryAlbums() {
  return requestHelper('/library/albums');
}

// getLibraryAlbumDetail obtiene pistas y datos detallados de un álbum por su ID.
export function getLibraryAlbumDetail(id) {
  return requestHelper(`/library/album?id=${encodeURIComponent(id)}`);
}

// getLibraryArtists obtiene los artistas de la colección con conteo de temas.
export function getLibraryArtists() {
  return requestHelper('/library/artists');
}

// getLibraryArtistTracks obtiene todas las pistas de un artista por nombre.
export function getLibraryArtistTracks(artistName) {
  return requestHelper(`/library/artist/tracks?name=${encodeURIComponent(artistName)}`);
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

// triggerLibraryScan solicita un re-escaneo reactivo de las carpetas de música.
export function triggerLibraryScan() {
  return requestHelper('/library/scan', { method: 'POST' });
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


