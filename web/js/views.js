// =====================================================================
// AuraBox - Views, Catalogs, A-Z Index & Context Menu Controller
// Album Grids, Artists, Search, Smart Mix Generator & Settings
// =====================================================================

import * as api from './api.js';
import { state, subscribe, notifyListeners, updatePlayerState } from './state.js';
import { escapeHtml, sanitizeCoverURL, sanitizeHTTPSRedirectURL } from './sanitize.js';
import { formatTime, renderSleepTimerUI, showToast, parseDSPBands, formatEQPresetString, renderEQSliders } from './player.js';
import { openNowPlaying } from './gestures.js';

// debounce retrasa la ejecución de una función para no sobrecargar el hilo UI con entradas continuas.
function debounce(fn, wait = 150) {
  let timeout;
  return function (...args) {
    clearTimeout(timeout);
    timeout = setTimeout(() => fn.apply(this, args), wait);
  };
}

// Estado reactivo de selección múltiple y vistas de biblioteca
let isSelectingAlbums = false;
let selectedAlbumIds = new Set();
let albumViewMode = localStorage.getItem('aurabox_album_view') || 'grid-4'; // 'grid-4' | 'grid-3' | 'list'
let albumSortBy = localStorage.getItem('aurabox_album_sort') || 'title'; // 'title' | 'year' | 'recent' | 'tracks'
let includeAllAlbums = localStorage.getItem('aurabox_include_all_albums') === 'true';
let albumFilterQuery = '';
let artistFilterQuery = '';
let recentMode = 'added';
const savedScrollTop = { albums: 0, artists: 0, artistDetail: 0, genres: 0, genreDetail: 0 };
let preservedAlbumsRenderLimit = 48;
let radioFilterQuery = '';
let radioGenreFilter = 'Todos';
let cachedRadios = [];
let hasLoadedRadios = false;
let isGeneratingMix = false;
let outputSwitchInFlight = false;
let outputRefreshInFlight = false;
let dspSwitchInFlight = false;
let lastDiagnosticsFetchAt = 0;
let cachedDiagnostics = null;
let isLoadingLibrary = false;
let pendingLibraryReload = null;
let lastProgressiveFetchAt = 0;

// saveScrollPosition guarda el desplazamiento actual del contenedor principal bajo una clave de vista.
function saveScrollPosition(key) {
  const mainEl = document.querySelector('.main-content');
  if (mainEl && key) {
    savedScrollTop[key] = mainEl.scrollTop || 0;
  }
}

// restoreScrollPosition recupera el desplazamiento guardado al volver desde una vista de detalle.
function restoreScrollPosition(key) {
  const targetTop = savedScrollTop[key] || 0;
  if (targetTop <= 0) return;
  requestAnimationFrame(() => {
    const mainEl = document.querySelector('.main-content');
    if (mainEl) mainEl.scrollTop = targetTop;
  });
}

// shouldDeferLibraryRender comprueba si el usuario interactúa con una vista que no conviene sobrescribir durante un escaneo progresivo.
function shouldDeferLibraryRender(isScanning = false) {
  if (isSelectingAlbums) return true;
  if (document.getElementById('albumTracksList') !== null) return true;
  if (document.getElementById('artistDetailView') !== null) return true;
  if (document.getElementById('genreDetailView') !== null) return true;
  if (state.ui.activeSubTab === 'folders') return true;
  if (document.activeElement && (document.activeElement.id === 'albumFilterInput' || document.activeElement.id === 'artistFilterInput')) return true;
  if (isScanning) {
    const mainEl = document.querySelector('.main-content');
    if (mainEl && mainEl.scrollTop > 120) return true;
  }
  return false;
}

// switchToLibrarySubTab cambia a Biblioteca y activa la sub-pestaña indicada sin perder sincronización visual.
function switchToLibrarySubTab(subtab) {
  document.querySelectorAll('.dock-item').forEach((item) => {
    item.classList.toggle('active', item.dataset.tab === 'library');
  });
  const views = ['viewLibrary', 'viewSearch', 'viewMixes', 'viewQueue', 'viewSettings'];
  views.forEach((id) => {
    const el = document.getElementById(id);
    if (el) el.classList.toggle('active', id === 'viewLibrary');
  });
  state.ui.activeTab = 'library';
  state.ui.activeSubTab = subtab;
  document.querySelectorAll('.sub-tab-btn').forEach((btn) => {
    btn.classList.toggle('active', btn.dataset.subtab === subtab);
  });
}

// initViews inicializa sub-pestañas, índice A-Z, buscador y panel de ajustes.
export function initViews() {
  bindSubTabs();
  bindAZIndex();
  bindSearch();
  bindSmartMixView();
  bindSettingsView();
  bindContextMenu();
  bindMultiSelectBar();

  // Cargar catálogo inicial
  loadLibraryData();

  const debouncedLibraryRefresh = debounce((payload) => {
    const isScanning = Boolean(payload && payload.scanning === true);
    const isFinalUpdate = (!payload || payload.scanning === false);
    const now = Date.now();
    // Si ya hay catálogo visible en pantalla, espaciar refrescos intermedios durante escaneos largos
    if (isScanning && state.library.albums.length >= 50 && now - lastProgressiveFetchAt < 10000) {
      return;
    }
    lastProgressiveFetchAt = now;
    loadLibraryData({ skipRender: shouldDeferLibraryRender(isScanning) });
    if (isFinalUpdate) {
      showToast('Biblioteca musical actualizada');
    }
  }, 800);

  subscribe((event, payload) => {
    if (event === 'library_updated') {
      debouncedLibraryRefresh(payload);
    } else if (event === 'player_updated') {
      renderAudioOutputs();
    } else if (event === 'trigger_artist_mix' && payload) {
      triggerSmartMix(payload);
    } else if (event === 'navigate_to_artist' && payload) {
      switchToLibrarySubTab('artists');
      openArtistDetail(payload);
    } else if (event === 'navigate_back_detail') {
      navigateBackFromDetail();
    }
  });
}

// loadLibraryData descarga álbumes, artistas, recientes y temáticas offline desde la API Go sin solapamientos.
export async function loadLibraryData(options = {}) {
  const { skipRender = false } = options;
  if (isLoadingLibrary) {
    pendingLibraryReload = { skipRender: Boolean(pendingLibraryReload?.skipRender && skipRender) };
    return;
  }
  isLoadingLibrary = true;
  try {
    const [albums, artists, recent, themes] = await Promise.all([
      api.getLibraryAlbums(includeAllAlbums),
      api.getLibraryArtists(),
      api.getLibraryRecent(30, recentMode),
      api.getSmartMixThemes().catch(() => [])
    ]);
    state.library.albums = albums || [];
    state.library.artists = artists || [];
    state.library.recentTracks = recent || [];
    state.library.themes = themes || [];

    if (!skipRender) {
      renderActiveSubTab();
    }
    populateSmartMixArtists();
    populateSmartMixThemes();
  } catch (err) {
    console.error('[Views] Error al cargar biblioteca:', err);
  } finally {
    isLoadingLibrary = false;
    if (pendingLibraryReload) {
      const nextOpts = pendingLibraryReload;
      pendingLibraryReload = null;
      loadLibraryData(nextOpts);
    }
  }
}

// bindSubTabs gestiona la navegación entre Álbumes, Artistas, Recientes y Carpetas.
function bindSubTabs() {
  const tabs = document.querySelectorAll('.sub-tab-btn');
  tabs.forEach((tab) => {
    tab.addEventListener('click', () => {
      tabs.forEach((t) => t.classList.remove('active'));
      tab.classList.add('active');
      state.ui.detailView = null;
      state.ui.activeSubTab = tab.dataset.subtab;
      renderActiveSubTab();
    });
  });
}

// renderActiveSubTab dibuja la vista correspondiente a la sub-pestaña activa.
function renderActiveSubTab(restoreScrollKey = '') {
  const container = document.getElementById('libraryContent');
  if (!container) return;

  state.ui.detailView = null;

  switch (state.ui.activeSubTab) {
    case 'albums':
      renderAlbumsGrid(container, restoreScrollKey === 'albums');
      break;
    case 'artists':
      renderArtistsList(container, restoreScrollKey === 'artists');
      break;
    case 'genres':
      renderGenresView(container, restoreScrollKey === 'genres');
      break;
    case 'recent':
      renderRecentTracks(container);
      break;
    case 'favorites':
      renderFavoritesView(container);
      break;
    case 'radios':
      renderRadiosView(container);
      break;
    case 'folders':
      renderFolderBrowser(container, '');
      break;
  }
}

// navigateBackFromDetail cierra la ficha actual de álbum, artista o género restaurando la vista previa y su scroll.
function navigateBackFromDetail() {
  const detail = state.ui.detailView;
  if (!detail) {
    renderActiveSubTab();
    return;
  }
  if (detail.type === 'album' && detail.backContext?.type === 'artist' && detail.backContext.name) {
    openArtistDetail(detail.backContext.name, { restoreScroll: true, skipPush: true });
    return;
  }
  if (detail.type === 'album' && detail.backContext?.type === 'genre' && detail.backContext.catValue) {
    openGenreDetail(detail.backContext.catType, detail.backContext.catValue, detail.backContext.label, { restoreScroll: true, skipPush: true });
    return;
  }
  const restoreKey = detail.type === 'artist' ? 'artists' : (detail.type === 'genre' ? 'genres' : 'albums');
  state.ui.detailView = null;
  renderActiveSubTab(restoreKey);
}

// cleanDisplayAlbumTitle simplifica nombres cargados de etiquetas técnicas para la interfaz.
function cleanDisplayAlbumTitle(raw) {
  if (!raw) return 'Álbum';
  return raw
    .replace(/\[(?:FLAC|PMEDIA|Lossless|Hunter|FRG|320kbps|24Bit[^\s]*|[^\]]*)\]/gi, '')
    .replace(/\((?:19|20)\d{2}[^)]*\)/gi, '')
    .replace(/⭐️/g, '')
    .trim();
}

// sortAlbums ordena el catálogo de álbumes según el criterio seleccionado por el usuario.
function sortAlbums(albums, mode) {
  const copy = [...albums];
  switch (mode) {
    case 'year':
      return copy.sort((a, b) => (b.year || 0) - (a.year || 0) || (a.title || '').localeCompare(b.title || '', 'es'));
    case 'recent':
      return copy.sort((a, b) => (b.added_seq || 0) - (a.added_seq || 0) || (b.id || 0) - (a.id || 0));
    case 'tracks':
      return copy.sort((a, b) => (b.track_count || 0) - (a.track_count || 0) || (a.title || '').localeCompare(b.title || '', 'es'));
    default:
      return copy.sort((a, b) => (a.title || '').localeCompare(b.title || '', 'es'));
  }
}

// getVisibleAlbums devuelve los álbumes filtrados y ordenados según las preferencias activas de la vista.
function getVisibleAlbums() {
  let albums = includeAllAlbums
    ? [...(state.library.albums || [])]
    : (state.library.albums || []).filter(a => Number(a.track_count || 0) > 5);

  if (albumFilterQuery) {
    const q = albumFilterQuery.toLowerCase();
    albums = albums.filter(a => (a.title && a.title.toLowerCase().includes(q)) || (a.artist && a.artist.toLowerCase().includes(q)));
  }
  return sortAlbums(albums, albumSortBy);
}

// renderAlbumsGrid dibuja la cuadrícula o lista compacta con ordenación, filtro, álbum aleatorio y toggle de EPs.
function renderAlbumsGrid(container, shouldRestoreScroll = false) {
  const albums = getVisibleAlbums();

  const isList = (albumViewMode === 'list');
  const is3Col = (albumViewMode === 'grid-3');
  const is4Col = (albumViewMode === 'grid-4' || albumViewMode === 'grid');
  const gridClass = isList ? 'albums-list' : `albums-grid ${is3Col ? 'density-3col' : 'density-4col'}`;

  // Si la barra ya existe en el DOM, solo actualizamos el contenedor para no perder el foco de escritura
  let albumsContainer = document.getElementById('albumsContainer');
  let filterInput = document.getElementById('albumFilterInput');

  if (!filterInput || !albumsContainer) {
    container.innerHTML = `
      <div class="albums-toolbar">
        <input type="text" id="albumFilterInput" placeholder="Filtrar álbumes..." value="${escapeHtml(albumFilterQuery)}" style="height:34px; padding:0 12px; font-size:0.82rem; border-radius:var(--radius-full); background:var(--bg-surface); border:1px solid var(--border-subtle); color:var(--text-primary); outline:none; flex:1; min-width:120px;"/>
        <div class="albums-toolbar-actions">
          <select id="albumSortSelect" class="album-sort-select" title="Ordenar álbumes">
            <option value="title" ${albumSortBy === 'title' ? 'selected' : ''}>A–Z</option>
            <option value="year" ${albumSortBy === 'year' ? 'selected' : ''}>Año</option>
            <option value="recent" ${albumSortBy === 'recent' ? 'selected' : ''}>Recientes</option>
            <option value="tracks" ${albumSortBy === 'tracks' ? 'selected' : ''}>Nº temas</option>
          </select>
          <button type="button" id="btnRandomAlbum" class="btn-secondary" style="height:34px; padding:0 10px; font-size:0.82rem; border-radius:var(--radius-full);" title="Reproducir un álbum al azar en 1 toque">
            🎲
          </button>
          <button type="button" id="btnToggleAllAlbums" class="btn-secondary btn-ep-toggle ${includeAllAlbums ? 'active' : ''}" style="height:34px; padding:0 10px; font-size:0.78rem; border-radius:var(--radius-full);" title="Mostrar solo discos largos (>5 temas) o incluir EPs y singles">
            ${includeAllAlbums ? 'Todos' : 'LPs (>5)'}
          </button>
          <button type="button" id="btnToggleAlbumView" class="btn-secondary" style="height:34px; padding:0 10px; font-size:0.82rem; border-radius:var(--radius-full);" title="Cambiar tamaño de carátulas / lista">
            ${is4Col ? '⊞ 4 Col' : is3Col ? '⊟ 3 Col' : '☰ Lista'}
          </button>
          <button type="button" id="btnToggleSelectAlbums" class="btn-secondary ${isSelectingAlbums ? 'active' : ''}" style="height:34px; padding:0 12px; font-size:0.82rem; border-radius:var(--radius-full); background:${isSelectingAlbums ? 'var(--accent)' : 'var(--bg-surface)'}; color:${isSelectingAlbums ? '#fff' : 'var(--text-secondary)'}; font-weight:600;">
            ${isSelectingAlbums ? '✓ Listo' : 'Seleccionar'}
          </button>
        </div>
      </div>
      <div class="${gridClass} ${isSelectingAlbums ? 'selection-mode' : ''}" id="albumsContainer"></div>
    `;

    filterInput = document.getElementById('albumFilterInput');
    albumsContainer = document.getElementById('albumsContainer');

    const debouncedFilter = debounce((val) => {
      albumFilterQuery = val;
      preservedAlbumsRenderLimit = 48;
      renderAlbumsGrid(container);
    }, 150);

    filterInput?.addEventListener('input', (e) => {
      debouncedFilter(e.target.value);
    });

    document.getElementById('albumSortSelect')?.addEventListener('change', (e) => {
      albumSortBy = e.target.value || 'title';
      localStorage.setItem('aurabox_album_sort', albumSortBy);
      preservedAlbumsRenderLimit = 48;
      renderAlbumsGrid(container);
    });

    document.getElementById('btnRandomAlbum')?.addEventListener('click', () => {
      playRandomAlbum(getVisibleAlbums());
    });

    document.getElementById('btnToggleAllAlbums')?.addEventListener('click', async (e) => {
      e.preventDefault();
      includeAllAlbums = !includeAllAlbums;
      localStorage.setItem('aurabox_include_all_albums', includeAllAlbums ? 'true' : 'false');
      preservedAlbumsRenderLimit = 48;
      try {
        const fetched = await api.getLibraryAlbums(includeAllAlbums);
        state.library.albums = fetched || [];
      } catch (err) {
        showToast('Error al cargar catálogo de álbumes');
      }
      renderAlbumsGrid(container);
    });

    document.getElementById('btnToggleAlbumView')?.addEventListener('click', (e) => {
      e.preventDefault();
      if (albumViewMode === 'grid-4' || albumViewMode === 'grid') albumViewMode = 'grid-3';
      else if (albumViewMode === 'grid-3') albumViewMode = 'list';
      else albumViewMode = 'grid-4';
      localStorage.setItem('aurabox_album_view', albumViewMode);
      renderAlbumsGrid(container);
    });

    document.getElementById('btnToggleSelectAlbums')?.addEventListener('click', (e) => {
      e.preventDefault();
      isSelectingAlbums = !isSelectingAlbums;
      if (!isSelectingAlbums) {
        selectedAlbumIds.clear();
        updateMultiSelectBar();
      }
      renderAlbumsGrid(container);
    });
  } else {
    albumsContainer.className = `${gridClass} ${isSelectingAlbums ? 'selection-mode' : ''}`;
    const btnToggle = document.getElementById('btnToggleAlbumView');
    if (btnToggle) {
      btnToggle.textContent = is4Col ? '⊞ 4 Col' : is3Col ? '⊟ 3 Col' : '☰ Lista';
    }
    const btnEp = document.getElementById('btnToggleAllAlbums');
    if (btnEp) {
      btnEp.classList.toggle('active', includeAllAlbums);
      btnEp.textContent = includeAllAlbums ? 'Todos' : 'LPs (>5)';
    }
    const btnSelect = document.getElementById('btnToggleSelectAlbums');
    if (btnSelect) {
      btnSelect.className = `btn-secondary ${isSelectingAlbums ? 'active' : ''}`;
      btnSelect.style.background = isSelectingAlbums ? 'var(--accent)' : 'var(--bg-surface)';
      btnSelect.style.color = isSelectingAlbums ? '#fff' : 'var(--text-secondary)';
      btnSelect.textContent = isSelectingAlbums ? '✓ Listo' : 'Seleccionar';
    }
  }

  // Renderizar álbumes en bloques progresivos (chunking)
  const initialCount = shouldRestoreScroll ? Math.max(48, preservedAlbumsRenderLimit) : 48;
  renderAlbumCardsChunked(albumsContainer, albums, isList, initialCount);
  updateMultiSelectBar();
  if (shouldRestoreScroll) {
    restoreScrollPosition('albums');
  }
}

// playRandomAlbum selecciona un disco al azar de la vista activa y comienza su reproducción inmediata.
async function playRandomAlbum(candidateAlbums) {
  const pool = Array.isArray(candidateAlbums) && candidateAlbums.length > 0
    ? candidateAlbums
    : (state.library.albums || []).filter(a => includeAllAlbums || Number(a.track_count || 0) > 5);

  if (pool.length === 0) {
    showToast('No hay álbumes disponibles para elegir al azar');
    return;
  }

  const pick = pool[Math.floor(Math.random() * pool.length)];
  try {
    await api.addToQueue({ album_id: pick.id, mode: 'replace', clear_first: true });
    showToast(`🎲 ${cleanDisplayAlbumTitle(pick.title)} — ${pick.artist || 'Artista'}`, 'Ver álbum', () => openAlbumDetail(pick.id));
  } catch (err) {
    showToast('No se pudo reproducir el álbum aleatorio');
  }
}

// renderAlbumCardsChunked renderiza álbumes progresivamente con IntersectionObserver evitando bloqueos en móviles.
function renderAlbumCardsChunked(container, albums, isList, initialBatchSize = 48) {
  container.innerHTML = '';
  if (albums.length === 0) {
    const emptyMsg = albumFilterQuery
      ? 'No se encontraron álbumes'
      : (includeAllAlbums ? 'No hay álbumes en la biblioteca' : 'No hay álbumes con más de 5 canciones');
    container.innerHTML = `<div style="text-align:center; padding:40px; color:var(--text-muted); grid-column:1/-1;">${emptyMsg}</div>`;
    return;
  }

  const BATCH_SIZE = 48;
  let currentIndex = 0;

  function appendBatch(batchSize = BATCH_SIZE) {
    const end = Math.min(currentIndex + batchSize, albums.length);
    const fragment = document.createDocumentFragment();

    for (let i = currentIndex; i < end; i++) {
      const album = albums[i];
      const isSelected = selectedAlbumIds.has(album.id);
      const cover = sanitizeCoverURL(album.cover_url);
      const displayTitle = cleanDisplayAlbumTitle(album.title);

      if (!isList) {
        const card = document.createElement('div');
        card.className = `album-card ${isSelected ? 'selected' : ''}`;
        if (album.title) {
          card.id = `album-${album.title.charAt(0).toUpperCase()}`;
        }

        card.innerHTML = `
          <div class="album-card-checkbox">${isSelected ? '✓' : ''}</div>
          <img class="album-card-cover" src="${escapeHtml(cover)}" alt="${escapeHtml(displayTitle)}" loading="lazy" decoding="async" data-fallback="/assets/default-cover.svg"/>
          <div class="album-card-title" title="${escapeHtml(album.title)}">${escapeHtml(displayTitle)}</div>
          <div class="album-card-artist" title="${escapeHtml(album.artist)}">${escapeHtml(album.artist || 'Artista')}</div>
        `;

        card.addEventListener('click', () => {
          if (isSelectingAlbums) {
            if (selectedAlbumIds.has(album.id)) {
              selectedAlbumIds.delete(album.id);
              card.classList.remove('selected');
              card.querySelector('.album-card-checkbox').textContent = '';
            } else {
              selectedAlbumIds.add(album.id);
              card.classList.add('selected');
              card.querySelector('.album-card-checkbox').textContent = '✓';
            }
            updateMultiSelectBar();
          } else {
            preservedAlbumsRenderLimit = Math.max(preservedAlbumsRenderLimit, currentIndex);
            openAlbumDetail(album.id);
          }
        });
        fragment.appendChild(card);
      } else {
        const row = document.createElement('div');
        row.className = `album-list-row ${isSelected ? 'selected' : ''}`;
        if (album.title) {
          row.id = `album-${album.title.charAt(0).toUpperCase()}`;
        }
        row.innerHTML = `
          <img class="album-list-cover" src="${escapeHtml(cover)}" alt="${escapeHtml(displayTitle)}" loading="lazy" decoding="async" data-fallback="/assets/default-cover.svg"/>
          <div class="album-list-info">
            <div class="album-list-title">${escapeHtml(displayTitle)}</div>
            <div class="album-list-artist">${escapeHtml(album.artist || 'Artista')} ${album.year > 0 ? '• ' + album.year : ''}</div>
          </div>
          <div style="font-size:0.75rem; color:var(--text-muted);">${album.track_count || ''} temas</div>
        `;

        row.addEventListener('click', () => {
          if (isSelectingAlbums) {
            if (selectedAlbumIds.has(album.id)) {
              selectedAlbumIds.delete(album.id);
              row.classList.remove('selected');
            } else {
              selectedAlbumIds.add(album.id);
              row.classList.add('selected');
            }
            updateMultiSelectBar();
          } else {
            preservedAlbumsRenderLimit = Math.max(preservedAlbumsRenderLimit, currentIndex);
            openAlbumDetail(album.id);
          }
        });
        fragment.appendChild(row);
      }
    }

    container.appendChild(fragment);
    currentIndex = end;

    if (currentIndex < albums.length) {
      ensureSentinel();
    }
  }

  let sentinel = null;
  let observer = null;

  function ensureSentinel() {
    if (sentinel && sentinel.parentNode) {
      sentinel.parentNode.removeChild(sentinel);
    }
    sentinel = document.createElement('div');
    sentinel.className = 'albums-scroll-sentinel';
    sentinel.style.cssText = 'height:30px; width:100%; grid-column:1/-1; pointer-events:none;';
    container.appendChild(sentinel);

    if ('IntersectionObserver' in window) {
      observer = new IntersectionObserver((entries) => {
        if (entries[0].isIntersecting) {
          observer.disconnect();
          appendBatch(BATCH_SIZE);
        }
      }, { rootMargin: '300px' });
      observer.observe(sentinel);
    } else {
      setTimeout(() => appendBatch(BATCH_SIZE), 100);
    }
  }

  appendBatch(initialBatchSize);
}

// updateMultiSelectBar actualiza la barra flotante de selección múltiple.
function updateMultiSelectBar() {
  const bar = document.getElementById('multiSelectBar');
  const countLabel = document.getElementById('multiSelectCount');
  if (!bar || !countLabel) return;

  const count = selectedAlbumIds.size;
  if (count > 0 && isSelectingAlbums) {
    bar.classList.remove('hidden');
    countLabel.textContent = `${count} álbum${count > 1 ? 'es' : ''} seleccionado${count > 1 ? 's' : ''}`;
  } else {
    bar.classList.add('hidden');
  }
}

// bindMultiSelectBar conecta los botones de acción para álbumes seleccionados.
function bindMultiSelectBar() {
  document.getElementById('btnMultiPlayNow')?.addEventListener('click', async () => {
    if (selectedAlbumIds.size === 0) return;
    const ids = Array.from(selectedAlbumIds);
    showToast(`Reproduciendo ${ids.length} álbumes seleccionados...`);
    await api.addToQueue({ album_ids: ids, mode: 'replace', clear_first: true });
    selectedAlbumIds.clear();
    isSelectingAlbums = false;
    updateMultiSelectBar();
    renderActiveSubTab();
  });

  document.getElementById('btnMultiAppend')?.addEventListener('click', async () => {
    if (selectedAlbumIds.size === 0) return;
    const ids = Array.from(selectedAlbumIds);
    await api.addToQueue({ album_ids: ids, mode: 'append' });
    showToast(`Añadidos ${ids.length} álbumes a la cola`);
    selectedAlbumIds.clear();
    isSelectingAlbums = false;
    updateMultiSelectBar();
    renderActiveSubTab();
  });

  document.getElementById('btnMultiCancel')?.addEventListener('click', () => {
    selectedAlbumIds.clear();
    isSelectingAlbums = false;
    updateMultiSelectBar();
    renderActiveSubTab();
  });
}

// openAlbumDetail carga y muestra los temas de un álbum conservando el historial y la posición de scroll anterior.
async function openAlbumDetail(albumId, backContext = null) {
  if (backContext?.type === 'artist') {
    saveScrollPosition('artistDetail');
  } else if (backContext?.type === 'genre') {
    saveScrollPosition('genreDetail');
  } else {
    saveScrollPosition('albums');
  }

  try {
    const data = await api.getLibraryAlbumDetail(albumId);
    const container = document.getElementById('libraryContent');
    if (!container || !data) return;

    const album = data.album;
    const tracks = data.tracks || [];
    state.ui.detailView = { type: 'album', id: albumId, backContext };
    window.history.pushState({ tab: 'library', detail: 'album' }, '', '#library');

    const backLabel = backContext?.type === 'artist'
      ? `← Volver a ${backContext.name}`
      : (backContext?.type === 'genre'
        ? `← Volver a ${backContext.label || backContext.catValue}`
        : '← Volver a Álbumes');
    const cover = sanitizeCoverURL(album.cover_url);
    const hasMissingCover = !album.cover_url || cover === '/assets/default-cover.svg';
    container.innerHTML = `
      <div style="margin-bottom:16px;">
        <button id="btnBackToAlbums" class="btn-secondary" style="margin-bottom:12px;">
          ${escapeHtml(backLabel)}
        </button>
        <div style="display:flex; gap:16px; align-items:center;">
          <img id="albumDetailCoverImg" src="${escapeHtml(cover)}" alt="${escapeHtml(album.title)}" loading="lazy" decoding="async" data-fallback="/assets/default-cover.svg" style="width:96px; height:96px; border-radius:var(--radius-md); object-fit:cover; box-shadow:0 8px 24px rgba(0,0,0,0.5);"/>
          <div style="min-width:0;">
            <h2 style="font-size:1.25rem; font-weight:700;">${escapeHtml(album.title)}</h2>
            <div id="albumDetailArtistLink" style="color:var(--accent-light); font-size:0.9rem; cursor:pointer; text-decoration:underline; text-decoration-style:dotted;">${escapeHtml(album.artist || 'Artista')}${album.year ? ' • ' + escapeHtml(album.year) : ''}</div>
            <div style="margin-top:8px; display:flex; flex-wrap:wrap; gap:8px;">
              <button id="btnPlayAlbumNow" class="btn-secondary" style="background:var(--accent); border-color:var(--accent); color:#fff;">
                ▶ Reproducir Álbum (${tracks.length} temas)
              </button>
              ${hasMissingCover ? `
                <button type="button" id="btnFetchOnlineCover" class="btn-secondary" style="font-size:0.8rem;" title="Descargar portada desde Cover Art Archive / MusicBrainz">
                  🌐 Buscar carátula online
                </button>
              ` : ''}
            </div>
          </div>
        </div>
      </div>
      <div class="tracks-list" id="albumTracksList"></div>
    `;

    const mainEl = document.querySelector('.main-content');
    if (mainEl) mainEl.scrollTop = 0;

    document.getElementById('btnBackToAlbums')?.addEventListener('click', () => navigateBackFromDetail());
    document.getElementById('albumDetailArtistLink')?.addEventListener('click', () => {
      if (album.artist) {
        switchToLibrarySubTab('artists');
        openArtistDetail(album.artist);
      }
    });
    document.getElementById('btnPlayAlbumNow')?.addEventListener('click', () => {
      api.addToQueue({ album_id: album.id, mode: 'replace', clear_first: true });
      showToast(`Reproduciendo álbum: ${album.title}`);
    });
    document.getElementById('btnFetchOnlineCover')?.addEventListener('click', async (e) => {
      const btn = e.currentTarget;
      btn.disabled = true;
      btn.textContent = 'Buscando portada...';
      try {
        const res = await api.fetchAlbumCoverOnline(album.id);
        const newCover = sanitizeCoverURL(res?.cover_url);
        const imgEl = document.getElementById('albumDetailCoverImg');
        if (imgEl && newCover && newCover !== '/assets/default-cover.svg') {
          imgEl.src = newCover;
          btn.textContent = '✓ Carátula guardada';
          showToast('Carátula descargada en caché local');
          loadLibraryData({ skipRender: true });
        } else {
          btn.disabled = false;
          btn.textContent = '🌐 Buscar carátula online';
          showToast('No se encontró carátula para este álbum');
        }
      } catch (err) {
        btn.disabled = false;
        btn.textContent = '🌐 Buscar carátula online';
        showToast('No se encontró carátula online para este disco');
      }
    });

    const list = document.getElementById('albumTracksList');
    tracks.forEach((track) => {
      const row = createTrackRow(track, () => {
        api.addToQueue({ track_ids: [track.id], mode: 'play_now' });
      });
      list.appendChild(row);
    });
  } catch (err) {
    showToast('Error al cargar álbum');
  }
}

// openArtistDetail muestra la ficha completa de un artista (discografía, canciones y acciones rápidas).
async function openArtistDetail(artistName, options = {}) {
  const { restoreScroll = false, skipPush = false } = options;
  if (!artistName) return;

  if (!state.ui.detailView && state.ui.activeSubTab === 'artists') {
    saveScrollPosition('artists');
  }

  const container = document.getElementById('libraryContent');
  if (!container) return;

  try {
    const data = await api.getLibraryArtistDetail(artistName);
    const albums = data?.albums || [];
    const tracks = data?.tracks || [];
    const initial = artistName.charAt(0).toUpperCase();

    state.ui.detailView = { type: 'artist', name: artistName };
    if (!skipPush) {
      window.history.pushState({ tab: 'library', detail: 'artist', name: artistName }, '', '#library');
    }

    container.innerHTML = `
      <div id="artistDetailView" style="margin-bottom:16px;">
        <button id="btnBackToArtists" class="btn-secondary" style="margin-bottom:12px;">
          ← Volver a Artistas
        </button>

        <div style="display:flex; gap:14px; align-items:center; margin-bottom:14px;">
          <div class="artist-avatar" style="width:64px; height:64px; min-width:64px; font-size:1.5rem;">${escapeHtml(initial)}</div>
          <div style="min-width:0;">
            <h2 style="font-size:1.3rem; font-weight:700; word-break:break-word;">${escapeHtml(data?.artist || artistName)}</h2>
            <div style="font-size:0.84rem; color:var(--text-secondary); margin-top:2px;">
              ${albums.length} ${albums.length === 1 ? 'álbum' : 'álbumes'} • ${tracks.length} canciones
            </div>
          </div>
        </div>

        <div style="display:flex; flex-wrap:wrap; gap:8px; margin-bottom:18px;">
          <button type="button" id="btnPlayArtistAll" class="btn-secondary" style="background:var(--accent); border-color:var(--accent); color:#ffffff; font-weight:700;">
            ▶ Reproducir todo
          </button>
          <button type="button" id="btnShuffleArtistAll" class="btn-secondary">
            🔀 Aleatorio
          </button>
          <button type="button" id="btnArtistSmartMix" class="btn-secondary">
            ⚡ Smart Mix
          </button>
        </div>

        ${albums.length > 0 ? `
          <h3 class="settings-section-title" style="margin-bottom:10px;">Discografía</h3>
          <div class="artist-discography-grid" id="artistAlbumsGrid"></div>
        ` : ''}

        <h3 class="settings-section-title" style="margin-bottom:10px;">Todas las Canciones</h3>
        <div class="tracks-list" id="artistTracksList"></div>
      </div>
    `;

    if (restoreScroll) {
      restoreScrollPosition('artistDetail');
    } else {
      const mainEl = document.querySelector('.main-content');
      if (mainEl) mainEl.scrollTop = 0;
    }

    document.getElementById('btnBackToArtists')?.addEventListener('click', () => navigateBackFromDetail());
    document.getElementById('btnPlayArtistAll')?.addEventListener('click', async () => {
      await api.addToQueue({ artist: artistName, mode: 'replace', clear_first: true });
      showToast(`Reproduciendo todo de ${artistName}`);
    });
    document.getElementById('btnShuffleArtistAll')?.addEventListener('click', async () => {
      await api.addToQueue({ artist: artistName, mode: 'replace', clear_first: true, shuffle: true });
      showToast(`🔀 Reproduciendo ${artistName} en aleatorio`);
    });
    document.getElementById('btnArtistSmartMix')?.addEventListener('click', () => {
      triggerSmartMix(artistName);
    });

    const albGrid = document.getElementById('artistAlbumsGrid');
    if (albGrid) {
      albums.forEach((alb) => {
        const card = document.createElement('div');
        card.className = 'album-card';
        const cover = sanitizeCoverURL(alb.cover_url);
        const displayTitle = cleanDisplayAlbumTitle(alb.title);
        card.innerHTML = `
          <img class="album-card-cover" src="${escapeHtml(cover)}" alt="${escapeHtml(displayTitle)}" loading="lazy" decoding="async" data-fallback="/assets/default-cover.svg"/>
          <div class="album-card-title">${escapeHtml(displayTitle)}</div>
          <div class="album-card-artist">${alb.year > 0 ? alb.year + ' • ' : ''}${alb.track_count || 0} temas</div>
        `;
        card.addEventListener('click', () => {
          openAlbumDetail(alb.id, { type: 'artist', name: artistName });
        });
        albGrid.appendChild(card);
      });
    }

    const tracksList = document.getElementById('artistTracksList');
    if (tracksList) {
      tracks.forEach((track) => {
        const row = createTrackRow(track, () => {
          api.addToQueue({ track_ids: [track.id], mode: 'play_now' });
        });
        tracksList.appendChild(row);
      });
    }
  } catch (err) {
    showToast(`No se pudo cargar la ficha de ${artistName}`);
  }
}

// buildGenreCategoryCard construye una tarjeta interactiva de género o década con botón rápido de Smart Mix.
function buildGenreCategoryCard(cat) {
  const card = document.createElement('div');
  card.className = 'genre-card';
  const cover = sanitizeCoverURL(cat.cover_url);
  const albumLabel = cat.album_count === 1 ? '1 álbum' : `${cat.album_count} álbumes`;
  card.innerHTML = `
    <img class="genre-card-cover" src="${escapeHtml(cover)}" alt="${escapeHtml(cat.label)}" loading="lazy" decoding="async" data-fallback="/assets/default-cover.svg"/>
    <div class="genre-card-info">
      <div class="genre-card-title">${escapeHtml(cat.label)}</div>
      <div class="genre-card-meta">${escapeHtml(albumLabel)} • ${cat.track_count} temas</div>
    </div>
    <button type="button" class="genre-card-mix-btn" title="Crear Smart Mix de ${escapeHtml(cat.label)}">⚡</button>
  `;

  card.querySelector('.genre-card-mix-btn')?.addEventListener('click', (e) => {
    e.stopPropagation();
    triggerThemeMix(cat.type, cat.value, cat.label);
  });

  card.addEventListener('click', () => {
    saveScrollPosition('genres');
    openGenreDetail(cat.type, cat.value, cat.label);
  });
  return card;
}

// renderGenresView muestra todos los estilos musicales y décadas del catálogo para explorar álbumes.
async function renderGenresView(container, shouldRestoreScroll = false) {
  container.innerHTML = '<div style="text-align:center; padding:40px; color:var(--text-muted);">Cargando estilos y décadas...</div>';
  try {
    const data = await api.getLibraryGenres();
    const genres = data?.genres || [];
    const decades = data?.decades || [];

    if (genres.length === 0 && decades.length === 0) {
      container.innerHTML = '<div style="text-align:center; padding:40px; color:var(--text-muted);">No hay etiquetas de género o año registradas en tu música</div>';
      return;
    }

    container.innerHTML = `
      <div id="genresOverviewView">
        ${genres.length > 0 ? `
          <div class="settings-section-title" style="margin-bottom:10px;">Estilos y Géneros</div>
          <div class="genres-grid" id="genresGridList"></div>
        ` : ''}
        ${decades.length > 0 ? `
          <div class="settings-section-title" style="margin-bottom:10px;">Por Década</div>
          <div class="genres-grid" id="decadesGridList"></div>
        ` : ''}
      </div>
    `;

    const gGrid = document.getElementById('genresGridList');
    if (gGrid) {
      const frag = document.createDocumentFragment();
      genres.forEach((g) => frag.appendChild(buildGenreCategoryCard(g)));
      gGrid.appendChild(frag);
    }

    const dGrid = document.getElementById('decadesGridList');
    if (dGrid) {
      const frag = document.createDocumentFragment();
      decades.forEach((d) => frag.appendChild(buildGenreCategoryCard(d)));
      dGrid.appendChild(frag);
    }

    if (shouldRestoreScroll) {
      restoreScrollPosition('genres');
    }
  } catch (err) {
    container.innerHTML = '<div style="text-align:center; padding:40px; color:var(--color-danger);">Error al cargar géneros y décadas</div>';
  }
}

// openGenreDetail lista los álbumes filtrados de un género o década concreta con acceso a Smart Mix.
async function openGenreDetail(catType, catValue, label = '', options = {}) {
  const { restoreScroll = false, skipPush = false } = options;
  const container = document.getElementById('libraryContent');
  if (!container || !catValue) return;

  try {
    const data = await api.getLibraryGenreAlbums(catType || 'genre', catValue);
    const albums = data?.albums || [];
    const displayLabel = data?.label || label || catValue;

    state.ui.detailView = {
      type: 'genre',
      catType: catType || 'genre',
      catValue,
      label: displayLabel
    };
    if (!skipPush) {
      window.history.pushState({ tab: 'library', detail: 'genre', value: catValue }, '', '#library');
    }

    container.innerHTML = `
      <div id="genreDetailView" style="margin-bottom:16px;">
        <button id="btnBackToGenres" class="btn-secondary" style="margin-bottom:12px;">
          ← Volver a Géneros
        </button>

        <div style="display:flex; justify-content:space-between; align-items:center; flex-wrap:wrap; gap:10px; margin-bottom:16px;">
          <div>
            <h2 style="font-size:1.3rem; font-weight:700;">${escapeHtml(displayLabel)}</h2>
            <div style="font-size:0.84rem; color:var(--text-secondary); margin-top:2px;">
              ${albums.length} ${albums.length === 1 ? 'álbum' : 'álbumes'} disponibles
            </div>
          </div>
          <button type="button" id="btnGenreSmartMix" class="btn-secondary" style="background:var(--accent); border-color:var(--accent); color:#ffffff; font-weight:700;">
            ⚡ Smart Mix ${escapeHtml(displayLabel)}
          </button>
        </div>

        <div class="albums-grid density-3col" id="genreAlbumsGrid"></div>
      </div>
    `;

    if (restoreScroll) {
      restoreScrollPosition('genreDetail');
    } else {
      const mainEl = document.querySelector('.main-content');
      if (mainEl) mainEl.scrollTop = 0;
    }

    document.getElementById('btnBackToGenres')?.addEventListener('click', () => navigateBackFromDetail());
    document.getElementById('btnGenreSmartMix')?.addEventListener('click', () => {
      triggerThemeMix(catType || 'genre', catValue, displayLabel);
    });

    const grid = document.getElementById('genreAlbumsGrid');
    if (grid) {
      if (albums.length === 0) {
        grid.innerHTML = '<div style="text-align:center; padding:32px; color:var(--text-muted); grid-column:1/-1;">No hay álbumes completos en esta categoría</div>';
      } else {
        const frag = document.createDocumentFragment();
        albums.forEach((alb) => {
          const card = document.createElement('div');
          card.className = 'album-card';
          const cover = sanitizeCoverURL(alb.cover_url);
          const displayTitle = cleanDisplayAlbumTitle(alb.title);
          card.innerHTML = `
            <img class="album-card-cover" src="${escapeHtml(cover)}" alt="${escapeHtml(displayTitle)}" loading="lazy" decoding="async" data-fallback="/assets/default-cover.svg"/>
            <div class="album-card-title">${escapeHtml(displayTitle)}</div>
            <div class="album-card-artist">${escapeHtml(alb.artist || 'Artista')}${alb.year > 0 ? ' • ' + alb.year : ''}</div>
          `;
          card.addEventListener('click', () => {
            openAlbumDetail(alb.id, { type: 'genre', catType: catType || 'genre', catValue, label: displayLabel });
          });
          frag.appendChild(card);
        });
        grid.appendChild(frag);
      }
    }
  } catch (err) {
    showToast('No se pudieron cargar los álbumes de esta categoría');
  }
}

// renderArtistsList dibuja el listado de artistas con filtro instantáneo y navegación a su discografía.
function renderArtistsList(container, shouldRestoreScroll = false) {
  const allArtists = (state.library.artists || []).filter(a => a && a.name);
  if (allArtists.length === 0) {
    container.innerHTML = '<div style="text-align:center; padding:40px; color:var(--text-muted);">No hay artistas registrados</div>';
    return;
  }

  const q = artistFilterQuery.trim().toLowerCase();
  const artists = q ? allArtists.filter(a => a.name.toLowerCase().includes(q)) : allArtists;

  let filterInput = document.getElementById('artistFilterInput');
  let list = document.getElementById('artistsList');
  let countEl = document.getElementById('artistCountBadge');

  if (!filterInput || !list) {
    container.innerHTML = `
      <div class="artist-filter-bar">
        <input type="text" id="artistFilterInput" placeholder="Filtrar artistas..." value="${escapeHtml(artistFilterQuery)}" style="height:36px; padding:0 14px; font-size:0.84rem; border-radius:var(--radius-full); background:var(--bg-surface); border:1px solid var(--border-subtle); color:var(--text-primary); outline:none; flex:1; min-width:0;"/>
        <span class="badge-format" id="artistCountBadge" style="background:var(--bg-surface); color:var(--text-secondary); font-size:0.75rem; white-space:nowrap;">${artists.length} artistas</span>
      </div>
      <div class="artists-list" id="artistsList"></div>
    `;

    filterInput = document.getElementById('artistFilterInput');
    list = document.getElementById('artistsList');
    countEl = document.getElementById('artistCountBadge');

    const debouncedArtistFilter = debounce((val) => {
      artistFilterQuery = val;
      renderArtistsList(container);
    }, 150);

    filterInput?.addEventListener('input', (e) => {
      debouncedArtistFilter(e.target.value);
    });
  } else if (countEl) {
    countEl.textContent = `${artists.length} artistas`;
  }

  list.innerHTML = '';
  if (artists.length === 0) {
    list.innerHTML = '<div style="text-align:center; padding:32px; color:var(--text-muted);">No hay artistas que coincidan con el filtro</div>';
    return;
  }

  const fragment = document.createDocumentFragment();
  artists.forEach((art) => {
    const row = document.createElement('div');
    row.className = 'artist-row';
    const initial = art.name.charAt(0).toUpperCase();
    row.id = `artist-${initial}`;

    row.innerHTML = `
      <div class="artist-row-left">
        <div class="artist-avatar">${escapeHtml(initial)}</div>
        <div>
          <div class="artist-name">${escapeHtml(art.name)}</div>
          <div class="artist-meta">${art.track_count} temas • ${escapeHtml(art.genre || 'Música')}</div>
        </div>
      </div>
      <button class="btn-secondary btn-quick-mix" style="padding:4px 10px; font-size:0.75rem;">
        ⚡ Smart Mix
      </button>
    `;

    // Tap en botón rápido -> Generar mix instantáneo
    row.querySelector('.btn-quick-mix').addEventListener('click', (e) => {
      e.stopPropagation();
      triggerSmartMix(art.name);
    });

    // Tap en fila -> Abrir discografía y temas completos del artista
    row.addEventListener('click', () => {
      openArtistDetail(art.name);
    });

    fragment.appendChild(row);
  });
  list.appendChild(fragment);

  if (shouldRestoreScroll) {
    restoreScrollPosition('artists');
  }
}

// renderRecentTracks dibuja pistas recientes con selector entre Añadidas, Escuchadas y Más escuchadas.
function renderRecentTracks(container) {
  const modeOptions = [
    { id: 'added', label: 'Añadidas' },
    { id: 'played', label: 'Escuchadas' },
    { id: 'top', label: 'Top' }
  ];

  let chipsWrapper = document.getElementById('recentModeChips');
  let list = document.getElementById('recentTracksList');

  if (!chipsWrapper || !list) {
    container.innerHTML = `
      <div class="recent-mode-chips" id="recentModeChips">
        ${modeOptions.map(opt => `
          <button type="button" class="recent-mode-chip ${recentMode === opt.id ? 'active' : ''}" data-recent-mode="${opt.id}">
            ${opt.label}
          </button>
        `).join('')}
      </div>
      <div class="tracks-list" id="recentTracksList"></div>
    `;

    chipsWrapper = document.getElementById('recentModeChips');
    list = document.getElementById('recentTracksList');

    chipsWrapper?.querySelectorAll('.recent-mode-chip').forEach((btn) => {
      btn.addEventListener('click', async () => {
        const nextMode = btn.dataset.recentMode || 'added';
        if (nextMode === recentMode && state.library.recentTracks?.length > 0) return;
        recentMode = nextMode;
        chipsWrapper.querySelectorAll('.recent-mode-chip').forEach((b) => {
          b.classList.toggle('active', b.dataset.recentMode === recentMode);
        });
        list.innerHTML = '<div style="text-align:center; padding:28px; color:var(--text-muted);">Cargando pistas...</div>';
        try {
          const fresh = await api.getLibraryRecent(30, recentMode);
          state.library.recentTracks = fresh || [];
        } catch (err) {
          showToast('Error al cargar historial reciente');
        }
        renderRecentTracks(container);
      });
    });
  } else {
    chipsWrapper.querySelectorAll('.recent-mode-chip').forEach((b) => {
      b.classList.toggle('active', b.dataset.recentMode === recentMode);
    });
  }

  const tracks = state.library.recentTracks || [];
  list.innerHTML = '';
  if (tracks.length === 0) {
    const emptyText = recentMode === 'played'
      ? 'Aún no has reproducido pistas en AuraBox'
      : (recentMode === 'top' ? 'Aún no hay estadísticas de reproducción' : 'No hay pistas recientes');
    list.innerHTML = `<div style="text-align:center; padding:36px; color:var(--text-muted);">${emptyText}</div>`;
    return;
  }

  tracks.forEach((track) => {
    const row = createTrackRow(track, () => {
      api.addToQueue({ track_ids: [track.id], mode: 'play_now' });
    });
    list.appendChild(row);
  });
}

// renderFavoritesView dibuja la colección de pistas marcadas como favoritas con acciones de reproducción masiva.
async function renderFavoritesView(container) {
  container.innerHTML = '<div style="text-align:center; padding:40px; color:var(--text-muted);">Cargando canciones favoritas...</div>';
  try {
    const data = await api.getFavorites();
    const tracks = data.tracks || [];

    if (tracks.length === 0) {
      container.innerHTML = `
        <div style="text-align:center; padding:60px 20px; color:var(--text-secondary); max-width:400px; margin:0 auto;">
          <div style="font-size:3.5rem; color:#ff4b72; margin-bottom:16px; opacity:0.8;">♥</div>
          <h3 style="font-size:1.25rem; font-weight:700; color:var(--text-primary); margin-bottom:8px;">Aún no tienes canciones favoritas</h3>
          <p style="font-size:0.88rem; line-height:1.5; color:var(--text-muted); margin-bottom:24px;">
            Pulsa el corazón <b>♥</b> en el reproductor o en cualquier canción de tu biblioteca para guardarla en esta colección.
          </p>
          <button class="btn-primary" id="btnExploreLibrary" style="margin:0 auto; padding:10px 24px; font-size:0.9rem; border-radius:var(--radius-full); cursor:pointer;">
            Explorar Álbumes
          </button>
        </div>
      `;
      document.getElementById('btnExploreLibrary')?.addEventListener('click', () => {
        state.ui.activeSubTab = 'albums';
        document.querySelectorAll('.sub-tab-btn').forEach(b => {
          b.classList.toggle('active', b.dataset.subtab === 'albums');
        });
        renderAlbumsGrid(container);
      });
      return;
    }

    // Calcular duración acumulada de las canciones favoritas
    const totalSec = tracks.reduce((acc, t) => acc + (t.duration || 0), 0);
    const hours = Math.floor(totalSec / 3600);
    const mins = Math.floor((totalSec % 3600) / 60);
    const durationStr = hours > 0 ? `${hours} h ${mins} min` : `${mins} min`;

    container.innerHTML = `
      <div class="favorites-banner" style="background:linear-gradient(135deg, rgba(255, 75, 114, 0.16) 0%, rgba(138, 43, 226, 0.10) 100%); border:1px solid rgba(255, 75, 114, 0.25); border-radius:var(--radius-lg); padding:20px; margin-bottom:16px;">
        <div style="display:flex; align-items:center; gap:16px; margin-bottom:16px;">
          <div style="width:52px; height:52px; border-radius:var(--radius-md); background:linear-gradient(135deg, #ff4b72, #e02852); display:flex; align-items:center; justify-content:center; font-size:1.7rem; color:#fff; box-shadow:0 4px 16px rgba(255, 75, 114, 0.4);">
            ♥
          </div>
          <div style="flex:1; min-width:0;">
            <h2 style="font-size:1.25rem; font-weight:700; color:var(--text-primary); margin:0 0 4px 0;">Canciones Favoritas</h2>
            <div style="font-size:0.82rem; color:var(--text-secondary); font-weight:500;">
              ${tracks.length} ${tracks.length === 1 ? 'canción' : 'canciones'} • ${durationStr}
            </div>
          </div>
        </div>

        <div style="display:flex; gap:10px; flex-wrap:wrap;">
          <button class="btn-primary" id="btnPlayAllFavorites" style="flex:1; min-width:140px; height:42px; border-radius:var(--radius-full); font-size:0.88rem; font-weight:600; display:flex; align-items:center; justify-content:center; gap:8px; cursor:pointer;">
            ▶ <span>Reproducir todas</span>
          </button>
          <button class="btn-secondary" id="btnShuffleFavorites" style="height:42px; padding:0 16px; border-radius:var(--radius-full); font-size:0.88rem; font-weight:600; display:flex; align-items:center; gap:6px; cursor:pointer;">
            🔀 <span>Aleatorio</span>
          </button>
          <button class="btn-secondary" id="btnEnqueueFavorites" style="height:42px; padding:0 16px; border-radius:var(--radius-full); font-size:0.88rem; font-weight:600; display:flex; align-items:center; gap:6px; cursor:pointer;">
            ➕ <span>Encolar</span>
          </button>
        </div>
      </div>

      <div class="tracks-list" id="favoritesTracksList"></div>
    `;

    document.getElementById('btnPlayAllFavorites')?.addEventListener('click', async () => {
      try {
        await api.playFavorites(false, 'play_now');
        showToast('Reproduciendo canciones favoritas');
      } catch (err) {
        showToast('Error al reproducir favoritas', true);
      }
    });

    document.getElementById('btnShuffleFavorites')?.addEventListener('click', async () => {
      try {
        await api.playFavorites(true, 'play_now');
        showToast('Reproduciendo favoritas en modo aleatorio 🔀');
      } catch (err) {
        showToast('Error al barajar favoritas', true);
      }
    });

    document.getElementById('btnEnqueueFavorites')?.addEventListener('click', async () => {
      try {
        await api.playFavorites(false, 'append');
        showToast(`${tracks.length} canciones añadidas a la cola`);
      } catch (err) {
        showToast('Error al encolar favoritas', true);
      }
    });

    const list = document.getElementById('favoritesTracksList');
    tracks.forEach((track) => {
      const row = createTrackRow(track, () => {
        api.addToQueue({ track_ids: [track.id], mode: 'play_now' });
      });
      list.appendChild(row);
    });

  } catch (err) {
    container.innerHTML = `<div style="text-align:center; padding:40px; color:var(--color-danger);">Error al cargar favoritas: ${escapeHtml(err.message)}</div>`;
  }
}

// renderFolderBrowser renderiza el explorador de carpetas con migas de pan y controles de reproducción directa.
async function renderFolderBrowser(container, path = '') {
  container.innerHTML = '<div style="color:var(--text-muted); padding:20px;">Explorando directorios...</div>';
  try {
    const data = await api.getLibraryFolders(path);
    const items = data.items || [];
    const isRoot = !path || data.is_root;

    // Calcular migas de pan (breadcrumbs) evitando apuntar al directorio padre /music restringido
    let breadcrumbHtml = `<span class="folder-crumb" data-path="" style="color:var(--accent-light); font-weight:700; cursor:pointer;">🏠 Raíz</span>`;
    const parts = path ? path.split('/').filter(Boolean) : [];
    if (parts.length > 0) {
      let accumulated = '';
      parts.forEach((p, idx) => {
        accumulated += '/' + p;
        const isLast = (idx === parts.length - 1);
        const crumbTarget = (accumulated === '/music') ? '' : accumulated;
        breadcrumbHtml += ` <span style="color:var(--text-muted); margin:0 4px;">/</span> `;
        if (isLast) {
          breadcrumbHtml += `<span style="color:var(--text-primary); font-weight:700;">${escapeHtml(p)}</span>`;
        } else {
          breadcrumbHtml += `<span class="folder-crumb" data-path="${escapeHtml(crumbTarget)}" style="color:var(--accent-light); cursor:pointer;">${escapeHtml(p)}</span>`;
        }
      });
    }

    // Cabecera y barra de acciones
    container.innerHTML = `
      <div class="folder-browser-header" style="margin-bottom:14px;">
        <div style="display:flex; align-items:center; justify-content:space-between; margin-bottom:8px; flex-wrap:wrap; gap:8px;">
          <div style="font-size:0.85rem; line-height:1.6; word-break:break-all;">
            ${breadcrumbHtml}
          </div>
          ${path ? `<button id="btnBackFolder" class="btn-secondary" style="height:32px; padding:0 12px; font-size:0.8rem; border-radius:var(--radius-full);">← Subir un nivel</button>` : ''}
        </div>

        ${!isRoot ? `
          <div class="folder-actions-bar" style="display:flex; align-items:center; gap:8px; flex-wrap:wrap; background:var(--bg-surface-elevated); padding:10px 12px; border-radius:var(--radius-md); border:1px solid var(--border-subtle); margin-bottom:12px;">
            <button id="btnPlayAllFolder" class="btn-secondary" style="background:var(--accent); border-color:var(--accent); color:#ffffff; font-weight:700; font-size:0.82rem; padding:6px 14px; border-radius:var(--radius-full);" title="Reproducir inmediatamente todo el contenido">
              ▶ Reproducir Carpeta
            </button>
            <button id="btnShuffleFolder" class="btn-secondary" style="font-size:0.82rem; padding:6px 12px; border-radius:var(--radius-full);" title="Mezclar y reproducir carpeta">
              🔀 Mezclar
            </button>
            <button id="btnEnqueueFolder" class="btn-secondary" style="font-size:0.82rem; padding:6px 12px; border-radius:var(--radius-full);" title="Añadir carpeta a la cola de reproducción">
              ➕ Añadir a Cola
            </button>
          </div>
        ` : ''}
      </div>

      <div class="tracks-list" id="folderItemsList"></div>
    `;

    // Conectar eventos de migas de pan y subir nivel
    container.querySelectorAll('.folder-crumb').forEach((crumb) => {
      crumb.addEventListener('click', () => {
        renderFolderBrowser(container, crumb.dataset.path);
      });
    });

    if (path) {
      document.getElementById('btnBackFolder')?.addEventListener('click', () => {
        const parent = path.substring(0, path.lastIndexOf('/'));
        const nextPath = (parts.length <= 2 || parent === '/music' || parent === '') ? '' : parent;
        renderFolderBrowser(container, nextPath);
      });

      // Conectar botones de acción de carpeta completa
      const folderName = path.split('/').filter(Boolean).pop() || 'Carpeta';
      document.getElementById('btnPlayAllFolder')?.addEventListener('click', async () => {
        await api.playFolder(path, false);
        showToast(`▶ Reproduciendo carpeta: ${folderName}`, 'Ver en grande ⛶', openNowPlaying);
        openNowPlaying();
      });

      document.getElementById('btnShuffleFolder')?.addEventListener('click', async () => {
        await api.playFolder(path, true);
        showToast(`🔀 Mezclando carpeta: ${folderName}`, 'Ver en grande ⛶', openNowPlaying);
        openNowPlaying();
      });

      document.getElementById('btnEnqueueFolder')?.addEventListener('click', async () => {
        await api.enqueueFolder(path, false);
        showToast(`➕ Carpeta añadida a la cola: ${folderName}`);
      });
    }

    const list = document.getElementById('folderItemsList');
    if (items.length === 0) {
      list.innerHTML = '<div style="color:var(--text-muted); padding:32px 16px; text-align:center;">Esta carpeta no contiene archivos de audio</div>';
      return;
    }

    items.forEach((item) => {
      const row = document.createElement('div');
      row.className = 'track-row folder-item-row';
      row.style.cursor = 'pointer';
      const isDir = item.is_dir !== false;

      if (isDir) {
        // Fila de Directorio
        row.innerHTML = `
          <div style="font-size:1.2rem; min-width:28px; color:var(--accent-light);">📁</div>
          <div class="track-info">
            <div class="track-title" style="font-weight:600;">${escapeHtml(item.name)}</div>
            <div class="track-artist" style="font-size:0.75rem; color:var(--text-muted);">Carpeta de música</div>
          </div>
          <div class="track-right" style="gap:6px;">
            <button class="btn-icon btn-dir-play" style="width:34px; height:34px; min-width:34px; border-radius:var(--radius-full); background:rgba(99,102,241,0.15); border:1px solid var(--accent); color:var(--accent-light);" title="Reproducir carpeta directamente">
              ▶
            </button>
            <button class="btn-icon btn-dir-add" style="width:34px; height:34px; min-width:34px; border-radius:var(--radius-full); background:var(--bg-surface); border:1px solid var(--border-subtle); color:var(--text-secondary);" title="Añadir a la cola">
              ➕
            </button>
          </div>
        `;

        // Botón reproducir carpeta directa
        row.querySelector('.btn-dir-play')?.addEventListener('click', async (e) => {
          e.stopPropagation();
          await api.playFolder(item.path, false);
          showToast(`▶ Reproduciendo carpeta: ${item.name}`, 'Ver en grande ⛶', openNowPlaying);
          openNowPlaying();
        });

        // Botón encolar carpeta
        row.querySelector('.btn-dir-add')?.addEventListener('click', async (e) => {
          e.stopPropagation();
          await api.enqueueFolder(item.path, false);
          showToast(`➕ Añadida a cola: ${item.name}`);
        });

        // Clic en la fila navega dentro de la carpeta
        row.addEventListener('click', () => {
          renderFolderBrowser(container, item.path);
        });
      } else {
        // Fila de Archivo de Audio
        const ext = item.name.split('.').pop().toUpperCase();
        const cleanName = item.name.replace(/\.[^/.]+$/, '');

        row.innerHTML = `
          <div style="font-size:1.1rem; min-width:28px; color:var(--accent);">🎵</div>
          <div class="track-info">
            <div class="track-title" style="font-weight:500;">${escapeHtml(cleanName)}</div>
            <div class="track-artist" style="font-size:0.75rem; color:var(--text-muted);">${escapeHtml(item.name)}</div>
          </div>
          <div class="track-right" style="gap:6px;">
            <span class="badge-format" style="font-size:0.7rem; padding:2px 6px;">${escapeHtml(ext)}</span>
            <button class="btn-icon btn-file-play" style="width:34px; height:34px; min-width:34px; border-radius:var(--radius-full); background:rgba(99,102,241,0.15); border:1px solid var(--accent); color:var(--accent-light);" title="Reproducir ahora">
              ▶
            </button>
            <button class="btn-icon btn-file-add" style="width:34px; height:34px; min-width:34px; border-radius:var(--radius-full); background:var(--bg-surface); border:1px solid var(--border-subtle); color:var(--text-secondary);" title="Añadir a la cola">
              ➕
            </button>
          </div>
        `;

        // Botón play en el archivo
        row.querySelector('.btn-file-play')?.addEventListener('click', async (e) => {
          e.stopPropagation();
          await api.playFile(item.path);
          showToast(`▶ Reproduciendo: ${cleanName}`, 'Ver en grande ⛶', openNowPlaying);
          openNowPlaying();
        });

        // Botón añadir a la cola
        row.querySelector('.btn-file-add')?.addEventListener('click', async (e) => {
          e.stopPropagation();
          await api.enqueueFile(item.path);
          showToast(`➕ Añadido a cola: ${cleanName}`);
        });

        // Clic en toda la fila reproduce el archivo y abre Now Playing
        row.addEventListener('click', async () => {
          await api.playFile(item.path);
          showToast(`▶ Reproduciendo: ${cleanName}`, 'Ver en grande ⛶', openNowPlaying);
          openNowPlaying();
        });
      }

      list.appendChild(row);
    });
  } catch (err) {
    container.innerHTML = '<div style="color:var(--color-danger); padding:20px;">Error al explorar carpetas: ' + escapeHtml(err.message || 'Error de red') + '</div>';
  }
}

// createTrackRow construye el elemento visual de una pista con botón de favorito y menú contextual.
function createTrackRow(track, onPlayClick) {
  const row = document.createElement('div');
  row.className = 'track-row';

  const isFav = !!track.favorite;
  row.innerHTML = `
    <span class="track-number">${track.track_number || '•'}</span>
    <div class="track-info">
      <div class="track-title">${escapeHtml(track.title || 'Pista')}</div>
      <div class="track-artist">${escapeHtml(track.artist || 'Artista')}</div>
    </div>
    <div class="track-right">
      <button class="btn-icon btn-track-fav ${isFav ? 'active' : ''}" title="${isFav ? 'Quitar de favoritas' : 'Añadir a favoritas'}" style="width:32px; height:32px; min-width:32px; border:none; background:transparent; font-size:1.15rem; color:${isFav ? '#ff4b72' : 'var(--text-muted)'}; cursor:pointer; padding:0; display:flex; align-items:center; justify-content:center;">
        ${isFav ? '♥' : '♡'}
      </button>
      <span class="badge-format">${escapeHtml(track.format || 'FLAC')}</span>
      <span class="track-duration">${formatTime(track.duration)}</span>
      <button class="btn-icon btn-track-menu" style="width:32px; height:32px; min-width:32px; border:none; background:transparent;">
        ⋮
      </button>
    </div>
  `;

  row.addEventListener('click', (e) => {
    if (e.target.closest('.btn-track-menu') || e.target.closest('.btn-track-fav')) return;
    onPlayClick();
  });

  const favBtn = row.querySelector('.btn-track-fav');
  if (favBtn) {
    favBtn.addEventListener('click', async (e) => {
      e.stopPropagation();
      try {
        const res = await api.toggleFavorite(track.id);
        track.favorite = res.favorite;
        favBtn.classList.toggle('active', res.favorite);
        favBtn.innerHTML = res.favorite ? '♥' : '♡';
        favBtn.style.color = res.favorite ? '#ff4b72' : 'var(--text-muted)';
        favBtn.title = res.favorite ? 'Quitar de favoritas' : 'Añadir a favoritas';
        showToast(res.favorite ? 'Añadida a canciones favoritas ♥' : 'Quitada de favoritas');
        // Si estamos en la sub-pestaña de favoritos y se desmarca, refrescar la lista
        if (state.ui.activeSubTab === 'favorites') {
          const container = document.getElementById('libraryContent');
          if (container) renderFavoritesView(container);
        }
      } catch (err) {
        showToast('Error al actualizar favorita', true);
      }
    });
  }

  row.querySelector('.btn-track-menu').addEventListener('click', (e) => {
    e.stopPropagation();
    openContextMenu(track);
  });

  return row;
}

// bindAZIndex conecta el índice lateral flotante para salto rápido en scroll.
function bindAZIndex() {
  const azContainer = document.getElementById('azIndexSlider');
  if (!azContainer) return;

  const letters = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ#'.split('');
  azContainer.innerHTML = '';

  letters.forEach((letter) => {
    const el = document.createElement('span');
    el.className = 'az-letter';
    el.textContent = letter;

    const jumpToLetter = () => {
      const target = document.getElementById(`album-${letter}`) || document.getElementById(`artist-${letter}`);
      if (target) {
        target.scrollIntoView({ behavior: 'smooth', block: 'start' });
      }
    };

    el.addEventListener('click', jumpToLetter);
    azContainer.appendChild(el);
  });
}

// bindSearch configura el buscador en tiempo real con debouncing de 200ms.
function bindSearch() {
  const input = document.getElementById('searchInput');
  const resultsContainer = document.getElementById('searchResults');
  if (!input || !resultsContainer) return;

  let debounceTimer;
  input.addEventListener('input', () => {
    clearTimeout(debounceTimer);
    const query = input.value.trim();
    if (query.length === 0) {
      resultsContainer.innerHTML = '';
      return;
    }

    debounceTimer = setTimeout(async () => {
      try {
        const res = await api.search(query);
        renderSearchResults(resultsContainer, res);
      } catch (err) {
        console.error('[Search] Error:', err);
      }
    }, 200);
  });
}

// renderSearchResults muestra canciones, álbumes, artistas y emisoras coincidentes con la búsqueda.
function renderSearchResults(container, data) {
  container.innerHTML = '';

  const hasTracks = (data.tracks?.length || 0) > 0;
  const hasAlbums = (data.albums?.length || 0) > 0;
  const hasArtists = (data.artists?.length || 0) > 0;
  const hasRadios = (data.radios?.length || 0) > 0;

  if (!hasTracks && !hasAlbums && !hasArtists && !hasRadios) {
    container.innerHTML = '<div style="text-align:center; padding:30px; color:var(--text-muted);">Sin resultados para esta búsqueda</div>';
    return;
  }

  // Emisoras de radio coincidentes
  if (hasRadios) {
    const header = document.createElement('h3');
    header.className = 'settings-section-title';
    header.textContent = `Emisoras de Radio (${data.radios.length})`;
    container.appendChild(header);

    const grid = document.createElement('div');
    grid.className = 'radios-grid';
    renderRadiosChunked(grid, data.radios, container);
    container.appendChild(grid);
  }

  // Pistas coincidentes
  if (hasTracks) {
    const header = document.createElement('h3');
    header.className = 'settings-section-title';
    header.style.marginTop = hasRadios ? '22px' : '0';
    header.textContent = 'Canciones';
    container.appendChild(header);

    const list = document.createElement('div');
    list.className = 'tracks-list';
    data.tracks.forEach((track) => {
      const row = createTrackRow(track, () => {
        api.addToQueue({ track_ids: [track.id], mode: 'play_now' });
      });
      list.appendChild(row);
    });
    container.appendChild(list);
  }

  // Álbumes coincidentes
  if (hasAlbums) {
    const header = document.createElement('h3');
    header.className = 'settings-section-title';
    header.style.marginTop = '18px';
    header.textContent = 'Álbumes';
    container.appendChild(header);

    const grid = document.createElement('div');
    grid.className = 'albums-grid';
    data.albums.forEach((album) => {
      const card = document.createElement('div');
      card.className = 'album-card';
      const cover = sanitizeCoverURL(album.cover_url);
      card.innerHTML = `
        <img class="album-card-cover" src="${escapeHtml(cover)}" alt="${escapeHtml(album.title)}" loading="lazy" decoding="async" data-fallback="/assets/default-cover.svg"/>
        <div class="album-card-title">${escapeHtml(album.title)}</div>
        <div class="album-card-artist">${escapeHtml(album.artist)}</div>
      `;
      card.addEventListener('click', () => {
        switchToLibrarySubTab('albums');
        openAlbumDetail(album.id);
      });
      grid.appendChild(card);
    });
    container.appendChild(grid);
  }

  // Artistas coincidentes
  if (hasArtists) {
    const header = document.createElement('h3');
    header.className = 'settings-section-title';
    header.style.marginTop = '18px';
    header.textContent = 'Artistas';
    container.appendChild(header);

    const list = document.createElement('div');
    list.className = 'artists-list';
    data.artists.forEach((art) => {
      const row = document.createElement('div');
      row.className = 'artist-row';
      const initial = (art.name || 'A').charAt(0).toUpperCase();
      row.innerHTML = `
        <div class="artist-row-left">
          <div class="artist-avatar">${escapeHtml(initial)}</div>
          <div>
            <div class="artist-name">${escapeHtml(art.name)}</div>
            <div class="artist-meta">${art.track_count || 0} temas</div>
          </div>
        </div>
        <button type="button" class="btn-secondary btn-quick-mix" style="padding:4px 10px; font-size:0.75rem;">
          ⚡ Smart Mix
        </button>
      `;
      row.querySelector('.btn-quick-mix')?.addEventListener('click', (e) => {
        e.stopPropagation();
        triggerSmartMix(art.name);
      });
      row.addEventListener('click', () => {
        switchToLibrarySubTab('artists');
        openArtistDetail(art.name);
      });
      list.appendChild(row);
    });
    container.appendChild(list);
  }
}

// bindSmartMixView configura el generador inteligente con autocompletado y chips táctiles para cualquier grupo.
function bindSmartMixView() {
  const input = document.getElementById('inputMixArtist');
  const suggestions = document.getElementById('mixSuggestions');
  const btnGen = document.getElementById('btnGenerateMix');

  if (input && suggestions) {
    input.addEventListener('input', () => {
      const q = input.value.trim().toLowerCase();
      if (!q) {
        suggestions.classList.remove('open');
        suggestions.innerHTML = '';
        return;
      }

      const matches = (state.library.artists || []).filter(a => a.name.toLowerCase().includes(q)).slice(0, 8);
      if (matches.length === 0) {
        suggestions.classList.remove('open');
        suggestions.innerHTML = '';
        return;
      }

      suggestions.innerHTML = matches.map(m => `
        <div class="mix-suggestion-item" data-artist="${escapeHtml(m.name)}">
          <b>${escapeHtml(m.name)}</b> <span style="font-size:0.75rem; color:var(--text-muted);">(${m.track_count} temas)</span>
        </div>
      `).join('');

      suggestions.querySelectorAll('.mix-suggestion-item').forEach(item => {
        item.addEventListener('click', (e) => {
          e.preventDefault();
          input.value = item.dataset.artist;
          suggestions.classList.remove('open');
          suggestions.innerHTML = '';
          triggerSmartMix(item.dataset.artist);
        });
      });
      suggestions.classList.add('open');
    });

    // Tecla Enter en teclado móvil o de sobremesa: evitar submit de página y lanzar mix
    input.addEventListener('keydown', (e) => {
      if (e.key === 'Enter') {
        e.preventDefault();
        input.blur();
        const artist = input.value.trim();
        if (artist) {
          suggestions.classList.remove('open');
          triggerSmartMix(artist);
        } else {
          showToast('Escribe o selecciona un grupo o artista');
        }
      }
    });

    document.addEventListener('click', (e) => {
      if (!input.contains(e.target) && !suggestions.contains(e.target)) {
        suggestions.classList.remove('open');
      }
    });
  }

  if (btnGen) {
    btnGen.addEventListener('click', (e) => {
      e.preventDefault();
      const artist = input ? input.value.trim() : '';
      if (!artist) {
        showToast('Escribe o selecciona un grupo o artista');
        return;
      }
      triggerSmartMix(artist);
    });
  }

  populateSmartMixArtists();
  populateSmartMixThemes();
}

// populateSmartMixArtists genera chips interactivos para los principales artistas del catálogo.
function populateSmartMixArtists() {
  const container = document.getElementById('mixArtistChips');
  if (!container) return;

  const topArtists = (state.library.artists || [])
    .filter(a => a.name && !a.name.startsWith('0') && a.name !== 'Artista Desconocido' && a.name !== 'Varios Artistas')
    .sort((a, b) => (b.track_count || 0) - (a.track_count || 0))
    .slice(0, 16);

  container.innerHTML = '';
  topArtists.forEach(art => {
    const chip = document.createElement('button');
    chip.type = 'button';
    chip.className = 'mix-chip';
    chip.textContent = `${art.name} (${art.track_count})`;
    chip.addEventListener('click', (e) => {
      e.preventDefault();
      const input = document.getElementById('inputMixArtist');
      if (input) input.value = art.name;
      triggerSmartMix(art.name);
    });
    container.appendChild(chip);
  });
}

// populateSmartMixThemes dibuja chips de géneros y décadas disponibles localmente para mixes 100% offline.
function populateSmartMixThemes() {
  const container = document.getElementById('mixThemeChips');
  if (!container) return;

  const themes = state.library.themes || [];
  container.innerHTML = '';
  if (themes.length === 0) {
    container.innerHTML = '<span style="font-size:0.78rem; color:var(--text-muted);">Escanea etiquetas de género o año en tu música para activar mixes temáticos</span>';
    return;
  }

  themes.forEach((theme) => {
    const chip = document.createElement('button');
    chip.type = 'button';
    chip.className = 'mix-chip mix-chip-theme';
    const icon = theme.type === 'decade' ? '🗓️' : '🎸';
    chip.textContent = `${icon} ${theme.label} (${theme.track_count})`;
    chip.addEventListener('click', (e) => {
      e.preventDefault();
      triggerThemeMix(theme.type, theme.value, theme.label);
    });
    container.appendChild(chip);
  });
}

// renderMixResultsCard dibuja el resumen interactivo y la lista de pistas del Smart Mix generado.
function renderMixResultsCard(resultsArea, res, titleLabel, subtitleLabel) {
  if (!resultsArea || !res) return;
  const similarChips = (res.similar_artists || []).map(a => `<span class="badge-format" style="background:rgba(99,102,241,0.18); color:var(--accent-light); margin:3px 2px;">${escapeHtml(a)}</span>`).join(' ');
  resultsArea.innerHTML = `
    <div class="mix-results-card">
      <div style="display:flex; justify-content:space-between; align-items:flex-start; margin-bottom:12px;">
        <div>
          <div style="font-size:0.75rem; text-transform:uppercase; color:var(--accent-light); font-weight:700;">Smart Mix Generado</div>
          <h3 style="font-size:1.2rem; font-weight:700; margin-top:2px;">${escapeHtml(titleLabel)}</h3>
          <div style="font-size:0.82rem; color:var(--text-secondary); margin-top:4px;">
            ${escapeHtml(subtitleLabel)}
          </div>
        </div>
        <span class="badge-format" style="background:var(--accent); color:#ffffff; font-weight:700; font-size:0.8rem;">▶ REPRODUCIENDO</span>
      </div>

      ${similarChips ? `
        <div style="margin-bottom:14px;">
          <div style="font-size:0.75rem; color:var(--text-muted); margin-bottom:6px; font-weight:600;">Artistas en esta sesión:</div>
          <div style="display:flex; flex-wrap:wrap; gap:4px;">
            ${similarChips}
          </div>
        </div>
      ` : ''}

      <div style="display:flex; gap:8px; margin-bottom:14px;">
        <button type="button" id="btnPlayMixNow" class="btn-secondary" style="flex:1; background:var(--accent); border-color:var(--accent); color:#ffffff; font-weight:700; justify-content:center;">
          ▶ Reiniciar Mix
        </button>
        <button type="button" id="btnAppendMixToQueue" class="btn-secondary" style="flex:1; justify-content:center;">
          ➕ Añadir a Cola
        </button>
      </div>

      <div style="font-size:0.75rem; font-weight:700; color:var(--text-muted); text-transform:uppercase; margin-bottom:8px;">Lista de temas remezclados:</div>
      <div class="tracks-list" style="max-height:300px; overflow-y:auto; border-radius:var(--radius-sm); border:1px solid var(--border-subtle); padding:4px;">
        ${res.tracks.map((t, idx) => `
          <div class="track-row" style="padding:6px 10px;">
            <span class="track-number" style="font-size:0.75rem;">${idx + 1}</span>
            <img src="${escapeHtml(sanitizeCoverURL(t.cover_url))}" alt="" loading="lazy" decoding="async" data-fallback="/assets/default-cover.svg" style="width:32px; height:32px; border-radius:4px; object-fit:cover; margin-right:8px;"/>
            <div class="track-info">
              <div class="track-title" style="font-size:0.85rem;">${escapeHtml(t.title)}</div>
              <div class="track-artist" style="font-size:0.72rem;">${escapeHtml(t.artist)} • ${escapeHtml(t.album)}</div>
            </div>
          </div>
        `).join('')}
      </div>
    </div>
  `;

  document.getElementById('btnPlayMixNow')?.addEventListener('click', async (e) => {
    e.preventDefault();
    await api.addToQueue({ track_ids: res.tracks.map(t => t.id), mode: 'replace', clear_first: true });
    showToast('Reproduciendo Smart Mix...');
  });

  document.getElementById('btnAppendMixToQueue')?.addEventListener('click', async (e) => {
    e.preventDefault();
    await api.addToQueue({ track_ids: res.tracks.map(t => t.id), mode: 'append' });
    showToast(`Añadidas ${res.tracks.length} canciones del Mix a la cola`);
  });
}

// triggerThemeMix genera una sesión offline balanceada por género o década desde el catálogo local.
async function triggerThemeMix(themeType, themeValue, label) {
  if (isGeneratingMix) return;
  isGeneratingMix = true;

  const resultsArea = document.getElementById('mixResultsArea');
  showToast(`Creando Smart Mix offline de ${label}...`);

  if (resultsArea) {
    resultsArea.innerHTML = `
      <div class="mix-results-card" style="text-align:center; padding:30px;">
        <div style="font-size:2rem; animation: pulse 1s infinite;">⚡</div>
        <div style="font-size:0.95rem; font-weight:700; margin-top:8px;">Generando sesión "${escapeHtml(label)}"...</div>
        <div style="font-size:0.8rem; color:var(--text-muted); margin-top:4px;">Remezclando pistas locales sin repetir artistas consecutivos</div>
      </div>
    `;
  }

  try {
    const res = await api.generateThemeMix(themeType, themeValue, 30, true);
    if (!res || !res.tracks || res.tracks.length === 0) {
      if (resultsArea) {
        resultsArea.innerHTML = '<div class="mix-results-card" style="color:var(--color-danger);">No se encontraron temas suficientes para esta temática</div>';
      }
      return;
    }

    showToast(`¡Mix "${label}" sonando! ${res.tracks.length} temas`);
    renderMixResultsCard(resultsArea, res, label, `${res.count} canciones locales intercaladas`);
  } catch (err) {
    showToast(`Error al crear mix temático: ${err.message}`);
    if (resultsArea) {
      resultsArea.innerHTML = `<div class="mix-results-card" style="color:var(--color-danger);">Error: ${escapeHtml(err.message)}</div>`;
    }
  } finally {
    isGeneratingMix = false;
  }
}

// triggerSmartMix busca 10 artistas similares al elegido, extrae 10 canciones de cada uno y los remezcla.
async function triggerSmartMix(artist) {
  if (isGeneratingMix) return;
  isGeneratingMix = true;

  const resultsArea = document.getElementById('mixResultsArea');
  showToast(`Buscando 10 artistas similares a ${artist}...`);

  if (resultsArea) {
    resultsArea.innerHTML = `
      <div class="mix-results-card" style="text-align:center; padding:30px;">
        <div style="font-size:2rem; animation: pulse 1s infinite;">⚡</div>
        <div style="font-size:0.95rem; font-weight:700; margin-top:8px;">Buscando 10 artistas similares a "${escapeHtml(artist)}"...</div>
        <div style="font-size:0.8rem; color:var(--text-muted); margin-top:4px;">Seleccionando hasta 10 temas por grupo y remezclando sin artistas consecutivos</div>
      </div>
    `;
  }

  try {
    const res = await api.generateSmartMix(artist, 10, true);
    if (!res || !res.tracks || res.tracks.length === 0) {
      if (resultsArea) {
        resultsArea.innerHTML = `<div class="mix-results-card" style="color:var(--color-danger);">No se encontraron temas suficientes en la biblioteca para generar el mix</div>`;
      }
      return;
    }

    showToast(`¡Smart Mix listo! ${res.tracks.length} temas remezclados`);
    renderMixResultsCard(
      resultsArea,
      res,
      `${res.seed_artist} & 10 Similares`,
      `${res.count} canciones de ${1 + (res.similar_artists?.length || 0)} artistas intercalados`
    );
  } catch (err) {
    showToast(`Error al crear mix: ${err.message}`);
    if (resultsArea) {
      resultsArea.innerHTML = `<div class="mix-results-card" style="color:var(--color-danger);">Error: ${escapeHtml(err.message)}</div>`;
    }
  } finally {
    isGeneratingMix = false;
  }
}

// isSpanishRadio identifica si una emisora pertenece a España según su campo país.
function isSpanishRadio(radio) {
  if (!radio || !radio.country) return false;
  const c = radio.country.trim().toLowerCase();
  return c === 'spain' || c === 'españa' || c === 'espana' || c === 'es';
}

// tuneRadioStation sintoniza una emisora validando la respuesta del servidor antes de abrir Now Playing.
async function tuneRadioStation(radio) {
  if (!radio) return;
  showToast(`Sintonizando ${radio.name}...`);
  try {
    await api.playRadio(radio);
    openNowPlaying();
  } catch (err) {
    showToast(`No se pudo sintonizar ${radio.name}: ${err.message || 'URL inválida'}`);
  }
}

// openRadiosCatalog navega directamente a Biblioteca -> Radios aplicando el filtro indicado.
function openRadiosCatalog(filterGenre = 'Todos') {
  radioGenreFilter = filterGenre;
  radioFilterQuery = '';
  document.querySelector('.dock-item[data-tab="library"]')?.click();
  state.ui.activeSubTab = 'radios';
  document.querySelectorAll('.sub-tab-btn').forEach((btn) => {
    btn.classList.toggle('active', btn.dataset.subtab === 'radios');
  });
  renderActiveSubTab();
  const mainContent = document.querySelector('.main-content');
  if (mainContent) mainContent.scrollTop = 0;
}

// handleSaveCustomRadio guarda una nueva emisora personalizada ingresada por el usuario y recarga la lista.
async function handleSaveCustomRadio(container) {
  const nameInput = document.getElementById('newRadioName');
  const urlInput = document.getElementById('newRadioUrl');
  const genreInput = document.getElementById('newRadioGenre');
  const countryInput = document.getElementById('newRadioCountry');
  const broadcasterInput = document.getElementById('newRadioBroadcaster');
  const formatSelect = document.getElementById('newRadioFormat');
  const submitBtn = document.getElementById('btnSubmitNewRadio');

  const name = nameInput ? nameInput.value.trim() : '';
  const streamURL = urlInput ? urlInput.value.trim() : '';
  if (!name || !streamURL) {
    showToast('Indica el nombre y la URL http/https de la emisora');
    return;
  }

  if (submitBtn) submitBtn.disabled = true;
  try {
    await api.addRadio({
      name,
      stream_url: streamURL,
      genre: (genreInput?.value.trim()) || 'General',
      country: (countryInput?.value.trim()) || 'Spain',
      broadcaster: broadcasterInput?.value.trim() || '',
      format: formatSelect?.value || ''
    });
    cachedRadios = (await api.getRadios()) || [];
    hasLoadedRadios = true;
    if (nameInput) nameInput.value = '';
    if (urlInput) urlInput.value = '';
    if (genreInput) genreInput.value = '';
    if (broadcasterInput) broadcasterInput.value = '';
    if (formatSelect) formatSelect.value = '';
    document.getElementById('addRadioPanel')?.classList.add('hidden');
    showToast(`Emisora guardada: ${name}`);
    renderRadiosView(container);
    loadRadioStations();
  } catch (err) {
    showToast(`Error al guardar emisora: ${err.message}`);
  } finally {
    if (submitBtn) submitBtn.disabled = false;
  }
}

// handleDeleteCustomRadio elimina una emisora añadida manualmente tras confirmación sencilla.
async function handleDeleteCustomRadio(radio, container) {
  if (!radio || !radio.id || radio.is_default) return;
  try {
    await api.deleteRadio(radio.id);
    cachedRadios = cachedRadios.filter((r) => r.id !== radio.id);
    showToast(`Emisora eliminada: ${radio.name}`);
    renderRadiosView(container);
    loadRadioStations();
  } catch (err) {
    showToast(`No se pudo eliminar: ${err.message}`);
  }
}

// renderRadiosView dibuja el catálogo completo de emisoras web con filtro de favoritos, España, buscador y alta personalizada.
async function renderRadiosView(container, forceRefresh = false) {
  if (!hasLoadedRadios || forceRefresh || !cachedRadios || cachedRadios.length === 0) {
    container.innerHTML = '<div style="text-align:center; padding:40px; color:var(--text-muted);">Cargando catálogo de radios web...</div>';
    try {
      cachedRadios = (await api.getRadios()) || [];
      hasLoadedRadios = true;
    } catch (err) {
      container.innerHTML = '<div style="text-align:center; padding:40px; color:var(--color-danger);">Error al cargar emisoras de radio</div>';
      return;
    }
  } else {
    // Sincronización en segundo plano para refrescar cambios si se añadieron emisoras en otra sesión
    api.getRadios().then((radios) => {
      if (Array.isArray(radios) && radios.length > 0) {
        cachedRadios = radios;
      }
    }).catch(() => {});
  }

  // Filtros rápidos incluyendo emisoras favoritas y emisoras españolas verificadas
  const popularGenres = ['Todos', '❤️ Favoritas', '🇪🇸 España', 'Rock', 'Alternative', 'Pop', 'Jazz', 'Classical', 'Electronic', 'Flamenco', 'News', 'Talk', 'Ambient', 'Blues', 'Folk', 'World'];

  // Filtrado reactivo
  const q = radioFilterQuery.trim().toLowerCase();
  let filtered = cachedRadios;
  if (radioGenreFilter === '❤️ Favoritas') {
    filtered = filtered.filter((r) => r.favorite);
  } else if (radioGenreFilter === '🇪🇸 España') {
    filtered = filtered.filter(isSpanishRadio);
  } else if (radioGenreFilter !== 'Todos') {
    filtered = filtered.filter((r) => (r.genre && r.genre.toLowerCase().includes(radioGenreFilter.toLowerCase())));
  }
  if (q) {
    filtered = filtered.filter((r) =>
      (r.name && r.name.toLowerCase().includes(q)) ||
      (r.genre && r.genre.toLowerCase().includes(q)) ||
      (r.broadcaster && r.broadcaster.toLowerCase().includes(q)) ||
      (r.country && r.country.toLowerCase().includes(q))
    );
  }

  let filterInput = document.getElementById('radioFilterInput');
  let listContainer = document.getElementById('radiosContainer');
  let countBadge = document.getElementById('radioCountBadge');

  if (!filterInput || !listContainer) {
    container.innerHTML = `
      <div class="radios-toolbar">
        <input type="text" id="radioFilterInput" placeholder="Buscar emisora, género o país..." value="${escapeHtml(radioFilterQuery)}" style="height:38px; padding:0 14px; font-size:0.85rem; border-radius:var(--radius-full); background:var(--bg-surface); border:1px solid var(--border-subtle); color:var(--text-primary); outline:none; flex:1; min-width:0;"/>
        <span class="badge-format" id="radioCountBadge" style="background:var(--bg-surface); color:var(--text-secondary); font-size:0.75rem; white-space:nowrap;">${filtered.length} radios</span>
        <button type="button" id="btnRefreshRadios" class="btn-secondary" title="Actualizar catálogo de radios" style="height:38px; width:38px; padding:0; display:flex; align-items:center; justify-content:center; border-radius:var(--radius-full); font-size:0.9rem;">
          🔄
        </button>
        <button type="button" id="btnToggleAddRadio" class="btn-secondary" style="height:38px; padding:0 12px; font-size:0.8rem; border-radius:var(--radius-full); font-weight:700; white-space:nowrap;">
          + Añadir
        </button>
      </div>

      <div id="addRadioPanel" class="add-radio-panel hidden">
        <div style="font-size:0.84rem; font-weight:700; margin-bottom:8px; color:var(--text-primary);">
          Añadir emisora web (.mp3, .aac, .m3u, .pls, .m3u8)
        </div>
        <form id="addRadioForm" class="add-radio-form">
          <input type="text" id="newRadioName" placeholder="Nombre de la emisora *" required />
          <input type="url" id="newRadioUrl" placeholder="URL del stream (https://... o .pls/.m3u/.m3u8) *" required />
          <div class="add-radio-row">
            <input type="text" id="newRadioGenre" placeholder="Género (ej: Rock, Noticias)" />
            <input type="text" id="newRadioCountry" placeholder="País (ej: Spain)" value="Spain" />
          </div>
          <div class="add-radio-row">
            <input type="text" id="newRadioBroadcaster" placeholder="Cadena (opcional)" />
            <select id="newRadioFormat">
              <option value="">Formato automático</option>
              <option value="MP3">MP3</option>
              <option value="AAC">AAC</option>
              <option value="HLS">HLS (.m3u8)</option>
              <option value="FLAC">FLAC</option>
              <option value="OGG">OGG</option>
            </select>
          </div>
          <div class="add-radio-actions">
            <button type="submit" id="btnSubmitNewRadio" class="btn-secondary add-radio-submit">💾 Guardar emisora</button>
            <button type="button" id="btnCancelNewRadio" class="btn-secondary">Cancelar</button>
          </div>
        </form>
      </div>

      <div class="radios-genre-chips" id="radioGenreChips">
        ${popularGenres.map((g) => `
          <button type="button" class="radio-genre-chip ${radioGenreFilter === g ? 'active' : ''}" data-genre="${escapeHtml(g)}">
            ${escapeHtml(g)}
          </button>
        `).join('')}
      </div>

      <div class="radios-grid" id="radiosContainer"></div>
    `;

    filterInput = document.getElementById('radioFilterInput');
    listContainer = document.getElementById('radiosContainer');
    countBadge = document.getElementById('radioCountBadge');

    const debouncedRadioFilter = debounce((val) => {
      radioFilterQuery = val;
      renderRadiosView(container);
    }, 150);

    filterInput?.addEventListener('input', (e) => {
      debouncedRadioFilter(e.target.value);
    });

    document.getElementById('btnRefreshRadios')?.addEventListener('click', async () => {
      showToast('Actualizando emisoras...');
      try {
        cachedRadios = (await api.getRadios()) || [];
        hasLoadedRadios = true;
        renderRadiosView(container);
        showToast(`${cachedRadios.length} emisoras sincronizadas`);
      } catch (err) {
        showToast('Error al refrescar radios');
      }
    });

    document.getElementById('btnToggleAddRadio')?.addEventListener('click', () => {
      document.getElementById('addRadioPanel')?.classList.toggle('hidden');
    });

    document.getElementById('btnCancelNewRadio')?.addEventListener('click', () => {
      document.getElementById('addRadioPanel')?.classList.add('hidden');
    });

    document.getElementById('addRadioForm')?.addEventListener('submit', (e) => {
      e.preventDefault();
      handleSaveCustomRadio(container);
    });

    document.querySelectorAll('#radioGenreChips .radio-genre-chip').forEach((btn) => {
      btn.addEventListener('click', (e) => {
        e.preventDefault();
        radioGenreFilter = btn.dataset.genre;
        document.querySelectorAll('#radioGenreChips .radio-genre-chip').forEach((b) => {
          b.classList.toggle('active', b.dataset.genre === radioGenreFilter);
        });
        renderRadiosView(container);
      });
    });
  } else {
    if (countBadge) countBadge.textContent = `${filtered.length} radios`;
    document.querySelectorAll('#radioGenreChips .radio-genre-chip').forEach((b) => {
      b.classList.toggle('active', b.dataset.genre === radioGenreFilter);
    });
  }

  // Renderizar tarjetas con chunking progresivo
  renderRadiosChunked(listContainer, filtered, container);
}

// renderRadiosChunked dibuja las emisoras en bloques de 40 elementos garantizando carga instantánea.
function renderRadiosChunked(container, radios, viewContainer) {
  container.innerHTML = '';
  if (radios.length === 0) {
    container.innerHTML = '<div style="text-align:center; padding:40px; color:var(--text-muted); grid-column:1/-1;">No se encontraron emisoras con los criterios actuales</div>';
    return;
  }

  const BATCH_SIZE = 40;
  let currentIndex = 0;

  function appendBatch() {
    const end = Math.min(currentIndex + BATCH_SIZE, radios.length);
    const fragment = document.createDocumentFragment();

    for (let i = currentIndex; i < end; i++) {
      const radio = radios[i];
      const card = document.createElement('div');
      card.className = 'radio-station-card';

      const subText = [radio.broadcaster, radio.country].filter(Boolean).join(' • ') || 'Emisora Web';
      const bitrateText = radio.bitrate ? `${radio.bitrate}k` : '';
      const formatText = radio.format || 'STREAM';
      const spanishBadge = isSpanishRadio(radio) ? '<span class="radio-badge-country">🇪🇸 España</span>' : '';
      const customBadge = (!radio.is_default) ? '<span class="radio-badge-genre">★ Propia</span>' : '';
      const favBadge = radio.favorite ? '<span class="radio-badge-genre" style="background:rgba(239,68,68,0.15); color:#ef4444; border-color:rgba(239,68,68,0.3);">❤️ Fav</span>' : '';
      const deleteBtn = (!radio.is_default && radio.id > 0)
        ? '<button type="button" class="radio-delete-btn" title="Eliminar emisora personalizada">🗑️</button>'
        : '';
      const favBtn = radio.id > 0
        ? `<button type="button" class="radio-fav-btn ${radio.favorite ? 'active' : ''}" title="${radio.favorite ? 'Quitar de favoritas' : 'Añadir a favoritas'}">${radio.favorite ? '❤️' : '🤍'}</button>`
        : '';

      const logoHtml = radio.logo_url
        ? `<img class="radio-card-logo-img" src="${escapeHtml(radio.logo_url)}" alt="${escapeHtml(radio.name)}" loading="lazy" decoding="async" onerror="this.onerror=null; this.parentElement.textContent='📻';" />`
        : '📻';

      card.innerHTML = `
        <div class="radio-card-icon">${logoHtml}</div>
        <div class="radio-card-info">
          <div class="radio-card-title">${escapeHtml(radio.name)}</div>
          <div class="radio-card-sub">${escapeHtml(subText)}</div>
          <div class="radio-card-badges">
            ${favBadge}
            ${spanishBadge}
            ${customBadge}
            ${radio.genre ? `<span class="radio-badge-genre">${escapeHtml(radio.genre.split(',')[0].trim())}</span>` : ''}
            <span class="radio-badge-format">${escapeHtml(formatText)} ${escapeHtml(bitrateText)}</span>
          </div>
        </div>
        <div class="radio-card-actions">
          ${favBtn}
          ${deleteBtn}
          <button type="button" class="radio-play-btn" title="Reproducir">▶</button>
        </div>
      `;

      const favBtnEl = card.querySelector('.radio-fav-btn');
      if (favBtnEl) {
        favBtnEl.addEventListener('click', async (e) => {
          e.stopPropagation();
          e.preventDefault();
          const prevFav = !!radio.favorite;
          const nextFav = !prevFav;
          radio.favorite = nextFav;
          favBtnEl.classList.toggle('active', nextFav);
          favBtnEl.textContent = nextFav ? '❤️' : '🤍';
          favBtnEl.title = nextFav ? 'Quitar de favoritas' : 'Añadir a favoritas';
          showToast(nextFav ? `❤️ Añadida a favoritas: ${radio.name}` : `🤍 Quitada de favoritas: ${radio.name}`);

          // Sincronizar en cachedRadios
          const matchedCached = cachedRadios.find((r) => r.id === radio.id);
          if (matchedCached) {
            matchedCached.favorite = nextFav;
          }

          try {
            const res = await api.toggleRadioFavorite(radio.id);
            if (res && typeof res.favorite === 'boolean') {
              radio.favorite = res.favorite;
              if (matchedCached) matchedCached.favorite = res.favorite;
              favBtnEl.classList.toggle('active', res.favorite);
              favBtnEl.textContent = res.favorite ? '❤️' : '🤍';
            }
          } catch (err) {
            radio.favorite = prevFav;
            if (matchedCached) matchedCached.favorite = prevFav;
            favBtnEl.classList.toggle('active', prevFav);
            favBtnEl.textContent = prevFav ? '❤️' : '🤍';
            showToast(`Error al marcar favorita: ${err.message}`);
          }

          // Si estamos filtrando activamente por Favoritas, re-renderizar para actualizar la lista
          if (radioGenreFilter === '❤️ Favoritas' && viewContainer) {
            renderRadiosView(viewContainer);
          }
        });
      }

      const delBtnEl = card.querySelector('.radio-delete-btn');
      if (delBtnEl) {
        delBtnEl.addEventListener('click', (e) => {
          e.stopPropagation();
          handleDeleteCustomRadio(radio, viewContainer);
        });
      }

      card.addEventListener('click', (e) => {
        if (e.target.closest('.radio-delete-btn') || e.target.closest('.radio-fav-btn')) return;
        e.preventDefault();
        tuneRadioStation(radio);
      });

      fragment.appendChild(card);
    }

    container.appendChild(fragment);
    currentIndex = end;

    if (currentIndex < radios.length) {
      ensureRadioSentinel();
    }
  }

  let sentinel = null;
  let observer = null;

  function ensureRadioSentinel() {
    if (sentinel && sentinel.parentNode) {
      sentinel.parentNode.removeChild(sentinel);
    }
    sentinel = document.createElement('div');
    sentinel.className = 'radios-scroll-sentinel';
    sentinel.style.cssText = 'height:30px; width:100%; grid-column:1/-1; pointer-events:none;';
    container.appendChild(sentinel);

    if ('IntersectionObserver' in window) {
      observer = new IntersectionObserver((entries) => {
        if (entries[0].isIntersecting) {
          observer.disconnect();
          appendBatch();
        }
      }, { rootMargin: '300px' });
      observer.observe(sentinel);
    } else {
      setTimeout(appendBatch, 100);
    }
  }

  appendBatch();
}

// describeAudioOutput asigna una etiqueta legible y descripción técnica a cada interfaz detectada por MPD.
function describeAudioOutput(output) {
  const plugin = (output.plugin || '').toLowerCase();
  const nameLower = (output.name || '').toLowerCase();
  const activePreset = (state.player.dsp_presets && state.player.dsp_presets[output.name]) || '';
  const presetLabel = activePreset.startsWith('eq:') ? 'EQ Personalizado' : activePreset;
  const dspTag = plugin === 'pipe' ? (presetLabel ? ` • DSP SoX (${presetLabel})` : ' • DSP SoX activo') : '';

  if (plugin === 'httpd') {
    return {
      badge: 'STREAM',
      isDAC: false,
      description: 'Stream Hi-Fi 320kbps vía /api/stream'
    };
  }
  if (plugin === 'pulse') {
    return {
      badge: 'PULSE',
      isDAC: false,
      description: 'Servidor de audio PulseAudio del sistema'
    };
  }
  if (plugin === 'null') {
    return {
      badge: 'NULL',
      isDAC: false,
      description: 'Salida silenciosa de respaldo (sin tarjeta física)'
    };
  }
  if (nameLower.includes('i2s') || nameLower.includes('hifiberry') || nameLower.includes('allo') || nameLower.includes('iqaudio') || nameLower.includes('justboom') || nameLower.includes('pisound')) {
    return {
      badge: plugin === 'pipe' ? 'I2S • DSP' : 'I2S',
      isDAC: true,
      description: `DAC Hardware I2S HAT detectado en ALSA${dspTag}`
    };
  }
  if (nameLower.includes('usb') || nameLower.includes('dac') || nameLower.includes('codec') || nameLower.includes('topping') || nameLower.includes('fiio') || nameLower.includes('smsl')) {
    return {
      badge: plugin === 'pipe' ? 'USB • DSP' : 'USB',
      isDAC: true,
      description: `DAC USB externo Plug & Play detectado por ALSA${dspTag}`
    };
  }
  if (nameLower.includes('hdmi') || nameLower.includes('vc4')) {
    return {
      badge: plugin === 'pipe' ? 'HDMI • DSP' : 'HDMI',
      isDAC: false,
      description: `Salida de audio digital HDMI${dspTag}`
    };
  }
  if (nameLower.includes('jack') || nameLower.includes('bcm2835') || nameLower.includes('headphone') || nameLower.includes('analóg') || nameLower.includes('analog')) {
    return {
      badge: plugin === 'pipe' ? 'JACK • DSP' : 'JACK',
      isDAC: false,
      description: `Salida analógica integrada de hardware${dspTag}`
    };
  }
  if (plugin === 'pipe') {
    return {
      badge: 'DSP',
      isDAC: false,
      description: 'Salida ALSA procesada mediante perfil DSP SoX'
    };
  }
  return {
    badge: plugin ? plugin.toUpperCase() : 'ALSA',
    isDAC: false,
    description: 'Interfaz de audio detectada automáticamente'
  };
}

// renderAudioDiagnostics muestra ayuda contextual y tarjetas ALSA crudas si no se detecta ningún DAC externo/HAT.
function renderAudioDiagnostics(diag, outputs) {
  const diagEl = document.getElementById('audioOutputsDiagnostics');
  if (!diagEl) return;

  const list = Array.isArray(outputs) ? outputs : (state.player.audio_outputs || []);
  const hasDAC = (diag && typeof diag.has_dac === 'boolean')
    ? diag.has_dac
    : list.some((o) => describeAudioOutput(o).isDAC);

  if (hasDAC) {
    diagEl.innerHTML = '';
    return;
  }

  const rawLines = (diag && diag.alsa_raw)
    ? diag.alsa_raw.split('\n').filter((l) => l.startsWith('card ')).join('\n')
    : '';

  diagEl.innerHTML = `
    <div style="margin-top:8px; padding:10px 12px; border-radius:var(--radius-sm); background:rgba(255,255,255,0.03); border:1px dashed var(--border-subtle); font-size:0.78rem; color:var(--text-secondary); line-height:1.45;">
      <div style="font-weight:700; color:var(--text-primary); margin-bottom:4px;">¿No aparece tu DAC?</div>
      <div>• <b>DAC USB:</b> enciéndelo/conéctalo a la Raspberry Pi y pulsa <b>🔄 Redetectar DAC</b>.</div>
      <div>• <b>DAC I2S HAT</b> (HiFiBerry, Allo Boss, IQaudio…): en Raspberry Pi OS nuevo añade su <code>dtoverlay=</code> en <code>/boot/firmware/config.txt</code> y reinicia.</div>
      ${rawLines ? `<pre style="margin:8px 0 0; padding:6px 8px; border-radius:4px; background:rgba(0,0,0,0.28); font-size:0.7rem; color:var(--text-muted); white-space:pre-wrap; overflow-x:auto;">${escapeHtml(rawLines)}</pre>` : ''}
    </div>
  `;
}

// ensureAudioDiagnosticsLoaded consulta una sola vez (con caché de 15s) los detalles ALSA si no hay DAC visible.
async function ensureAudioDiagnosticsLoaded(outputs) {
  const hasDAC = (outputs || []).some((o) => describeAudioOutput(o).isDAC);
  if (hasDAC) {
    renderAudioDiagnostics(cachedDiagnostics, outputs);
    return;
  }

  renderAudioDiagnostics(cachedDiagnostics, outputs);
  const now = Date.now();
  if (now - lastDiagnosticsFetchAt < 15000) return;
  lastDiagnosticsFetchAt = now;

  try {
    cachedDiagnostics = await api.getAudioDiagnostics();
    renderAudioDiagnostics(cachedDiagnostics, state.player.audio_outputs);
  } catch (_) {
    // Silencioso en caso de reconexión transitoria
  }
}

// renderAudioOutputs dibuja dinámicamente las tarjetas de hardware real reportado por MPD sin IDs fijos.
function renderAudioOutputs(force = false) {
  const container = document.getElementById('audioOutputsList');
  if (!container) return;

  const outputs = Array.isArray(state.player.audio_outputs) ? state.player.audio_outputs : [];
  if (outputs.length === 0) {
    if (!force && container.dataset.sig === 'empty') return;
    container.dataset.sig = 'empty';
    container.innerHTML = `
      <div style="font-size:0.85rem; color:var(--text-muted); padding:6px 2px;">
        Buscando salidas de audio disponibles en MPD...
      </div>
    `;
    renderAudioDiagnostics(cachedDiagnostics, outputs);
    return;
  }

  const sig = JSON.stringify(outputs.map((o) => [o.id, o.name, o.plugin, !!o.enabled]));
  if (!force && container.dataset.sig === sig) {
    return;
  }
  container.dataset.sig = sig;
  container.innerHTML = '';

  outputs.forEach((output) => {
    const meta = describeAudioOutput(output);
    const opt = document.createElement('div');
    opt.className = `output-option ${output.enabled ? 'active' : ''}`;
    opt.dataset.outputid = String(output.id);
    opt.innerHTML = `
      <div style="min-width:0; padding-right:10px;">
        <div class="output-name">${escapeHtml(output.name)}</div>
        <div style="font-size:0.75rem; color:var(--text-muted);">${escapeHtml(meta.description)}</div>
      </div>
      <span class="badge-format">${escapeHtml(meta.badge)}</span>
    `;

    opt.addEventListener('click', async () => {
      if (outputSwitchInFlight) return;
      outputSwitchInFlight = true;
      try {
        const updatedState = await api.selectAudioOutput(output.id);
        if (updatedState) {
          updatePlayerState(updatedState);
          renderAudioOutputs(true);
        }
        showToast(`Salida conmutada a: ${output.name}`);
      } catch (err) {
        showToast(`No se pudo cambiar la salida: ${err.message}`);
      } finally {
        outputSwitchInFlight = false;
      }
    });

    container.appendChild(opt);
  });

  ensureAudioDiagnosticsLoaded(outputs);
}

// runForcedScan lanza un re-escaneo completo bloqueando pulsaciones repetidas mientras inicia.
async function runForcedScan(btn, startMsg) {
  if (btn && btn.disabled) return;
  if (btn) btn.disabled = true;
  showToast(startMsg);
  try {
    await api.triggerLibraryScan(true);
  } catch (err) {
    showToast(`Error al iniciar escaneo: ${err.message}`);
  } finally {
    if (btn) btn.disabled = false;
  }
}

// bindSettingsView gestiona selectores dinámicos de audio, salvaguarda de volumen y configuración del NAS.
function bindSettingsView() {
  // Renderizar inicialmente salidas de audio detectadas
  renderAudioOutputs(true);

  // Botón para redetectar DAC USB y salidas ALSA en caliente sin reiniciar Docker
  const btnRefreshOutputs = document.getElementById('btnRefreshOutputs');
  btnRefreshOutputs?.addEventListener('click', async () => {
    if (outputRefreshInFlight) return;
    outputRefreshInFlight = true;
    const origText = btnRefreshOutputs.textContent;
    btnRefreshOutputs.disabled = true;
    btnRefreshOutputs.textContent = 'Detectando...';
    try {
      const res = await api.refreshAudioOutputs();
      if (res && res.diagnostics) {
        cachedDiagnostics = res.diagnostics;
        lastDiagnosticsFetchAt = Date.now();
      }
      if (res && res.state) {
        updatePlayerState(res.state);
      }
      renderAudioOutputs(true);
      renderAudioDiagnostics(cachedDiagnostics, state.player.audio_outputs);
      const hasDAC = cachedDiagnostics ? cachedDiagnostics.has_dac : false;
      showToast(hasDAC ? 'DAC detectado y salidas actualizadas' : 'Salidas ALSA re-escaneadas');
    } catch (err) {
      showToast(`Error al redetectar salidas: ${err.message}`);
    } finally {
      btnRefreshOutputs.disabled = false;
      btnRefreshOutputs.textContent = origText;
      outputRefreshInFlight = false;
    }
  });

  // Escaneo completo manual de biblioteca
  const btnRescanLib = document.getElementById('btnRescanLibrary');
  btnRescanLib?.addEventListener('click', () => {
    runForcedScan(btnRescanLib, 'Iniciando re-escaneo completo de música...');
  });

  // Perfiles de ecualización / DSP y ecualizador gráfico de 5 bandas por salida activa
  const applyDSPSelection = async (presetOrEq, successLabel) => {
    if (dspSwitchInFlight) return;
    const activeOutput = state.player.active_output || '';
    if (!activeOutput || activeOutput === 'Detectando audio...') {
      showToast('Espera a que se detecte la salida de audio activa');
      return;
    }

    const dspButtons = document.querySelectorAll('#dspPresetSelector .dsp-opt');
    const btnApplyEQ = document.getElementById('btnApplyCustomEQ');
    const btnResetEQ = document.getElementById('btnResetEQ');

    dspSwitchInFlight = true;
    dspButtons.forEach((b) => { b.disabled = true; });
    if (btnApplyEQ) btnApplyEQ.disabled = true;
    if (btnResetEQ) btnResetEQ.disabled = true;

    const descEl = document.getElementById('dspPresetDesc');
    if (descEl) descEl.textContent = 'Aplicando ecualizador DSP en MPD...';

    try {
      const updatedState = await api.setOutputDSPPreset(activeOutput, presetOrEq);
      if (updatedState) {
        updatePlayerState(updatedState);
        renderAudioOutputs(true);
      }
      showToast(successLabel || 'Ecualizador DSP aplicado');
    } catch (err) {
      showToast(`No se pudo aplicar DSP: ${err.message}`);
      updatePlayerState({});
    } finally {
      dspButtons.forEach((b) => { b.disabled = false; });
      if (btnApplyEQ) btnApplyEQ.disabled = false;
      if (btnResetEQ) btnResetEQ.disabled = false;
      dspSwitchInFlight = false;
    }
  };

  const getSliderBands = () => {
    const bands = [0, 0, 0, 0, 0];
    document.querySelectorAll('#eqBandsList .eq-band-slider').forEach((sl) => {
      const idx = parseInt(sl.dataset.band || '0', 10);
      const val = parseInt(sl.value || '0', 10);
      if (idx >= 0 && idx < 5 && !Number.isNaN(val)) {
        bands[idx] = Math.max(-12, Math.min(12, val));
      }
    });
    return bands;
  };

  const dspButtons = document.querySelectorAll('#dspPresetSelector .dsp-opt');
  dspButtons.forEach((btn) => {
    btn.addEventListener('click', () => {
      const key = btn.dataset.dsp || 'flat';
      if (key === 'custom') {
        const eqStr = formatEQPresetString(getSliderBands());
        applyDSPSelection(eqStr, 'Curva EQ personalizada aplicada');
        return;
      }
      renderEQSliders(parseDSPBands(key));
      applyDSPSelection(key, `Perfil DSP aplicado: ${btn.textContent.trim()}`);
    });
  });

  document.querySelectorAll('#eqBandsList .eq-band-slider').forEach((slider) => {
    slider.addEventListener('input', () => {
      const idx = parseInt(slider.dataset.band || '0', 10);
      const val = parseInt(slider.value || '0', 10);
      const label = document.querySelector(`#eqBandsList [data-band-val="${idx}"]`);
      if (label) {
        label.textContent = `${val > 0 ? '+' : ''}${val} dB`;
      }
      dspButtons.forEach((b) => {
        b.classList.toggle('active', b.dataset.dsp === 'custom');
      });
    });
  });

  document.getElementById('btnApplyCustomEQ')?.addEventListener('click', () => {
    const eqStr = formatEQPresetString(getSliderBands());
    applyDSPSelection(eqStr, eqStr === 'flat' ? 'Ecualizador restablecido a Plano Hi-Fi' : 'Curva EQ de 5 bandas aplicada');
  });

  document.getElementById('btnResetEQ')?.addEventListener('click', () => {
    renderEQSliders([0, 0, 0, 0, 0]);
    applyDSPSelection('flat', 'Ecualizador restablecido a Plano Hi-Fi (Bit-Perfect)');
  });

  // Límite máximo de salvaguarda de volumen
  const maxVolSlider = document.getElementById('settingMaxVol');
  const maxVolLabel = document.getElementById('labelMaxVol');
  if (maxVolSlider) {
    const initialMax = state.player.max_volume_limit || 85;
    maxVolSlider.value = initialMax;
    if (maxVolLabel) maxVolLabel.textContent = `${initialMax}%`;

    maxVolSlider.addEventListener('input', () => {
      if (maxVolLabel) {
        maxVolLabel.textContent = `${maxVolSlider.value}%`;
      }
    });

    maxVolSlider.addEventListener('change', async () => {
      const limit = parseInt(maxVolSlider.value, 10);
      try {
        const updated = await api.setMaxVolumeLimit(limit);
        if (updated) updatePlayerState(updated);
        showToast(`Límite máximo de volumen ajustado al ${limit}%`);
      } catch (err) {
        showToast(`Error al ajustar límite: ${err.message}`);
      }
    });
  }

  // Crossfade en caliente (0..15 segundos)
  const crossfadeSlider = document.getElementById('settingCrossfade');
  const crossfadeLabel = document.getElementById('labelCrossfade');
  if (crossfadeSlider) {
    const initialCF = Number(state.player.crossfade || 0);
    crossfadeSlider.value = initialCF;
    if (crossfadeLabel) crossfadeLabel.textContent = `${initialCF}s`;

    crossfadeSlider.addEventListener('input', () => {
      if (crossfadeLabel) crossfadeLabel.textContent = `${crossfadeSlider.value}s`;
    });

    crossfadeSlider.addEventListener('change', async () => {
      const cf = parseInt(crossfadeSlider.value, 10);
      const rg = (state.player.replay_gain || 'auto').toLowerCase();
      try {
        const updated = await api.setAudioConfig(rg, cf);
        if (updated) updatePlayerState(updated);
        showToast(cf === 0 ? 'Crossfade desactivado (Gapless puro)' : `Crossfade ajustado a ${cf}s`);
      } catch (err) {
        showToast(`Error al ajustar Crossfade: ${err.message}`);
      }
    });
  }

  // Selector ReplayGain en vivo (auto / track / album / off)
  const rgButtons = document.querySelectorAll('#replayGainSelector .rg-opt');
  rgButtons.forEach((btn) => {
    btn.addEventListener('click', async () => {
      const rg = btn.dataset.rg || 'auto';
      const cf = Number(state.player.crossfade || 0);
      rgButtons.forEach((b) => b.classList.toggle('active', b === btn));
      try {
        const updated = await api.setAudioConfig(rg, cf);
        if (updated) updatePlayerState(updated);
        showToast(`ReplayGain ajustado a: ${btn.textContent}`);
      } catch (err) {
        showToast(`Error al cambiar ReplayGain: ${err.message}`);
      }
    });
  });

  // Interruptor Fade In / Fade Out suave al pausar y reanudar + duración configurable
  const softFadeCheckbox = document.getElementById('settingSoftFade');
  if (softFadeCheckbox) {
    if (typeof state.player.soft_fade === 'boolean') {
      softFadeCheckbox.checked = state.player.soft_fade;
    }
    softFadeCheckbox.addEventListener('change', async () => {
      const enabled = softFadeCheckbox.checked;
      const rg = (state.player.replay_gain || 'auto').toLowerCase();
      const cf = Number(state.player.crossfade || 0);
      try {
        const updated = await api.setAudioConfig(rg, cf, enabled);
        if (updated) updatePlayerState(updated);
        showToast(enabled ? 'Fade In / Fade Out en pausa activado' : 'Pausa instantánea (sin fade)');
      } catch (err) {
        softFadeCheckbox.checked = !enabled;
        showToast(`Error al actualizar fade en pausa: ${err.message}`);
      }
    });
  }

  const fadeDurationButtons = document.querySelectorAll('#softFadeDurationSelector .rg-opt');
  fadeDurationButtons.forEach((btn) => {
    btn.addEventListener('click', async () => {
      const ms = parseInt(btn.dataset.fadeMs || '1100', 10);
      const rg = (state.player.replay_gain || 'auto').toLowerCase();
      const cf = Number(state.player.crossfade || 0);
      fadeDurationButtons.forEach((b) => b.classList.toggle('active', b === btn));
      try {
        const updated = await api.setAudioConfig(rg, cf, undefined, ms);
        if (updated) updatePlayerState(updated);
        showToast(`Duración de fundido ajustada a ${(ms / 1000).toFixed(1)} s`);
      } catch (err) {
        showToast(`Error al cambiar duración de fundido: ${err.message}`);
      }
    });
  });

  // Temporizador de apagado (Sleep Timer)
  const sleepButtons = document.querySelectorAll('.btn-sleep-timer');
  sleepButtons.forEach((btn) => {
    btn.addEventListener('click', async () => {
      try {
        if (btn.dataset.sleepMode === 'track_end') {
          const updated = await api.setSleepAfterTrack();
          if (updated) updatePlayerState(updated);
          renderSleepTimerUI();
          showToast('La música se pausará al terminar la canción actual');
        } else {
          const mins = parseInt(btn.dataset.minutes, 10);
          const updated = await api.setSleepTimer(mins);
          if (updated) updatePlayerState(updated);
          renderSleepTimerUI();
          if (mins > 0) {
            showToast(`Temporizador programado para apagarse en ${mins} minutos`);
          } else {
            showToast('Temporizador de apagado desactivado');
          }
        }
      } catch (err) {
        showToast(err.message || 'No se pudo configurar el temporizador');
      }
    });
  });
  renderSleepTimerUI();

  // Cargar emisoras de radio, configuración del NAS y estado HTTPS
  loadRadioStations();
  loadNASConfig();
  loadSecurityStatus();

  // Guardar configuración del NAS
  document.getElementById('btnSaveNAS')?.addEventListener('click', async () => {
    const alias = document.getElementById('nasInputAlias')?.value.trim();
    const host = document.getElementById('nasInputHost')?.value.trim();
    const share = document.getElementById('nasInputShare')?.value.trim();
    const username = document.getElementById('nasInputUser')?.value.trim();
    const password = document.getElementById('nasInputPass')?.value.trim();
    const options = document.getElementById('nasInputOptions')?.value.trim();

    showToast('Guardando configuración y conectando al NAS...');
    try {
      await api.saveNASConfig({ alias, host, share, username, password, options });
      showToast('Configuración NAS guardada. Re-escaneando...');
      loadNASConfig();
    } catch (err) {
      showToast(`Error: ${err.message}`);
    }
  });

  // Re-escanear NAS completo
  const btnRescanNAS = document.getElementById('btnRescanNAS');
  btnRescanNAS?.addEventListener('click', () => {
    runForcedScan(btnRescanNAS, 'Iniciando re-escaneo completo del almacenamiento NAS...');
  });
}

// loadNASConfig lee la configuración activa del NAS y actualiza la tarjeta de estado.
async function loadNASConfig() {
  try {
    const data = await api.getNASConfig();
    if (!data) return;

    if (data.alias && document.getElementById('nasInputAlias')) {
      document.getElementById('nasInputAlias').value = data.alias;
      document.getElementById('nasCardAlias').textContent = data.alias;
    }
    if (data.host && document.getElementById('nasInputHost')) {
      document.getElementById('nasInputHost').value = data.host;
    }
    if (data.share && document.getElementById('nasInputShare')) {
      document.getElementById('nasInputShare').value = data.share;
    }
    if (data.username && document.getElementById('nasInputUser')) {
      document.getElementById('nasInputUser').value = data.username;
    }
    if (data.options && document.getElementById('nasInputOptions')) {
      document.getElementById('nasInputOptions').value = data.options;
    }
    if (data.path && document.getElementById('nasCardPath')) {
      document.getElementById('nasCardPath').textContent = data.path;
    }

    const statusCard = document.getElementById('nasStatusCard');
    if (statusCard && data.mounted) {
      statusCard.style.borderColor = 'var(--color-success)';
    }
  } catch (err) {
    console.warn('[NAS] No se pudo leer configuración:', err);
  }
}

// loadSecurityStatus consulta el estado HTTPS del servidor y actualiza los controles de certificado/redirección.
async function loadSecurityStatus() {
  const titleEl = document.getElementById('securityStatusTitle');
  const badgeEl = document.getElementById('securityProtocolBadge');
  const descEl = document.getElementById('securityStatusDesc');
  const cardEl = document.getElementById('securityStatusCard');
  const btnOpenHTTPS = document.getElementById('btnOpenHTTPS');
  const btnDownloadCA = document.getElementById('btnDownloadCACert');
  if (!titleEl || !badgeEl || !descEl) return;

  try {
    const sec = await api.getSecurityStatus();
    if (!sec) return;

    const isHTTPS = window.location.protocol === 'https:' || Boolean(sec.request_is_https);
    if (isHTTPS) {
      titleEl.textContent = 'Conexión cifrada activa (HTTPS)';
      badgeEl.textContent = 'HTTPS 🔒';
      badgeEl.style.background = 'rgba(16,185,129,0.15)';
      badgeEl.style.color = 'var(--color-success)';
      badgeEl.style.borderColor = 'rgba(16,185,129,0.3)';
      if (cardEl) cardEl.style.borderLeft = '4px solid var(--color-success)';
      descEl.innerHTML = 'Tu tráfico y credenciales del NAS viajan cifrados por TLS. ' +
        'Para eliminar el aviso inicial del navegador y permitir instalación PWA completa, instala el certificado raíz <code>aurabox-ca.crt</code> como CA de confianza en este dispositivo.';
      if (btnOpenHTTPS) btnOpenHTTPS.style.display = 'none';
    } else if (sec.https_enabled && sec.https_url) {
      const safeHTTPS = sanitizeHTTPSRedirectURL(sec.https_url);
      titleEl.textContent = 'Conectado por HTTP sin cifrar';
      badgeEl.textContent = 'HTTP';
      if (cardEl) cardEl.style.borderLeft = '4px solid var(--accent)';
      descEl.innerHTML = `AuraBox ya escucha por HTTPS en <code>${escapeHtml(safeHTTPS || sec.https_url)}</code>. ` +
        'Para entrar poniendo solo la IP de la Raspberry Pi (<code>80/443 → 4080/4443</code>), ejecuta <code>./scripts/setup-https-pi.sh</code> en la Pi.';
      if (btnOpenHTTPS && safeHTTPS) {
        btnOpenHTTPS.href = safeHTTPS;
        btnOpenHTTPS.style.display = 'flex';
      } else if (btnOpenHTTPS) {
        btnOpenHTTPS.style.display = 'none';
      }
    } else {
      titleEl.textContent = 'Modo solo HTTP activo (sin HTTPS)';
      badgeEl.textContent = 'HTTP';
      if (cardEl) cardEl.style.borderLeft = '4px solid var(--text-muted)';
      descEl.innerHTML = 'Estás entrando directamente por HTTP sin cifrado ni avisos de certificado. ' +
        'Si quieres reactivar HTTPS cifrado e instalación PWA, ejecuta <code>./scripts/setup-https-pi.sh</code> en la Raspberry Pi.';
      if (btnOpenHTTPS) btnOpenHTTPS.style.display = 'none';
    }

    if (btnDownloadCA) {
      btnDownloadCA.style.display = sec.ca_ready ? 'flex' : 'none';
    }
  } catch (err) {
    console.warn('[Security] No se pudo consultar estado HTTPS:', err);
  }
}

// loadRadioStations carga emisoras web destacadas (priorizando España) y accesos rápidos en Ajustes.
async function loadRadioStations() {
  const container = document.getElementById('radioStationsList');
  if (!container) return;

  try {
    const radios = (await api.getRadios()) || [];
    cachedRadios = radios;
    hasLoadedRadios = true;
    const spanishRadios = radios.filter(isSpanishRadio);

    const priorityNames = [
      'RNE Radio Nacional',
      'RNE Radio 3',
      'RNE Radio Clásica',
      'Cadena SER',
      'COPE Nacional',
      'Onda Cero',
      'LOS40',
      'Rock FM España'
    ];
    const preview = [];
    const seenIds = new Set();
    priorityNames.forEach((targetName) => {
      const match = radios.find((r) => r.name === targetName);
      if (match && !seenIds.has(match.stream_url)) {
        seenIds.add(match.stream_url);
        preview.push(match);
      }
    });
    spanishRadios.forEach((r) => {
      if (preview.length < 8 && !seenIds.has(r.stream_url)) {
        seenIds.add(r.stream_url);
        preview.push(r);
      }
    });
    radios.forEach((r) => {
      if (preview.length < 8 && !seenIds.has(r.stream_url)) {
        seenIds.add(r.stream_url);
        preview.push(r);
      }
    });

    container.innerHTML = `
      <div style="font-size:0.85rem; color:var(--text-secondary); margin-bottom:10px; line-height:1.45;">
        <b>${radios.length}</b> emisoras de alta fidelidad configuradas (<b>${spanishRadios.length}</b> de España).
      </div>
      <div style="display:flex; flex-wrap:wrap; gap:8px; margin-bottom:12px;">
        <button type="button" id="btnSettingsOpenSpanishRadios" class="btn-secondary" style="flex:1; min-width:140px; height:36px; justify-content:center; font-size:0.78rem;">
          🇪🇸 Ver emisoras España (${spanishRadios.length})
        </button>
        <button type="button" id="btnSettingsOpenAllRadios" class="btn-secondary" style="flex:1; min-width:140px; height:36px; justify-content:center; font-size:0.78rem;">
          📻 Catálogo completo
        </button>
      </div>
    `;

    document.getElementById('btnSettingsOpenSpanishRadios')?.addEventListener('click', () => {
      openRadiosCatalog('🇪🇸 España');
    });
    document.getElementById('btnSettingsOpenAllRadios')?.addEventListener('click', () => {
      openRadiosCatalog('Todos');
    });

    preview.forEach((r) => {
      const card = document.createElement('div');
      card.className = 'output-option';
      const flagPrefix = isSpanishRadio(r) ? '🇪🇸 ' : '';
      card.innerHTML = `
        <div style="display:flex; align-items:center; gap:10px; min-width:0;">
          <div style="font-size:1.3rem;">📻</div>
          <div style="min-width:0;">
            <div class="output-name">${flagPrefix}${escapeHtml(r.name)}</div>
            <div style="font-size:0.75rem; color:var(--text-muted);">${escapeHtml(r.genre)} • ${escapeHtml(r.format || 'STREAM')} ${escapeHtml(r.bitrate)}k</div>
          </div>
        </div>
        <button type="button" class="btn-secondary" style="padding:4px 10px; font-size:0.75rem;">▶ Sintonizar</button>
      `;
      card.addEventListener('click', (e) => {
        e.preventDefault();
        tuneRadioStation(r);
      });
      container.appendChild(card);
    });
  } catch (err) {
    console.error('[Radios] Error:', err);
  }
}

// bindContextMenu conecta las acciones rápidas del Bottom Sheet (⋯).
function bindContextMenu() {
  const modal = document.getElementById('contextMenuModal');
  const btnClose = document.getElementById('btnCloseContextMenu');
  if (!modal) return;

  if (btnClose) {
    btnClose.addEventListener('click', closeContextMenu);
  }

  modal.addEventListener('click', (e) => {
    if (e.target === modal) closeContextMenu();
  });

  // Acciones
  document.getElementById('cmenuPlayNow')?.addEventListener('click', () => {
    if (state.ui.contextMenuTarget) {
      api.addToQueue({ track_ids: [state.ui.contextMenuTarget.id], mode: 'play_now' });
      showToast('Reproduciendo pista ahora');
    }
    closeContextMenu();
  });

  document.getElementById('cmenuPlayNext')?.addEventListener('click', () => {
    if (state.ui.contextMenuTarget) {
      api.addToQueue({ track_ids: [state.ui.contextMenuTarget.id], mode: 'play_next' });
      showToast('Pista añadida a continuación');
    }
    closeContextMenu();
  });

  document.getElementById('cmenuAppend')?.addEventListener('click', () => {
    if (state.ui.contextMenuTarget) {
      api.addToQueue({ track_ids: [state.ui.contextMenuTarget.id], mode: 'append' });
      showToast('Pista encolada al final');
    }
    closeContextMenu();
  });

  document.getElementById('cmenuSmartMix')?.addEventListener('click', () => {
    if (state.ui.contextMenuTarget) {
      triggerSmartMix(state.ui.contextMenuTarget.artist);
    }
    closeContextMenu();
  });

  document.getElementById('cmenuToggleFav')?.addEventListener('click', async () => {
    if (state.ui.contextMenuTarget) {
      const target = state.ui.contextMenuTarget;
      try {
        const res = await api.toggleFavorite(target.id);
        target.favorite = res.favorite;
        showToast(res.favorite ? 'Añadida a canciones favoritas ♥' : 'Quitada de favoritas');
        if (state.ui.activeSubTab === 'favorites') {
          const container = document.getElementById('libraryContent');
          if (container) renderFavoritesView(container);
        }
      } catch (err) {
        showToast('Error al actualizar favorita', true);
      }
    }
    closeContextMenu();
  });
}

// openContextMenu despliega el menú contextual de opciones para una pista.
function openContextMenu(track) {
  state.ui.contextMenuTarget = track;
  const modal = document.getElementById('contextMenuModal');
  const title = document.getElementById('contextMenuTitle');
  const favText = document.getElementById('cmenuToggleFavText');
  const favIcon = document.getElementById('cmenuFavIcon');

  if (modal && title) {
    title.textContent = `${track.title} - ${track.artist}`;
    if (favText) {
      favText.textContent = track.favorite ? 'Quitar de favoritas' : 'Marcar como favorita';
    }
    if (favIcon) {
      favIcon.textContent = track.favorite ? '💔' : '♥';
    }
    modal.classList.add('active');
  }
}

// closeContextMenu cierra el menú contextual inferior.
function closeContextMenu() {
  const modal = document.getElementById('contextMenuModal');
  if (modal) {
    modal.classList.remove('active');
    state.ui.contextMenuTarget = null;
  }
}

