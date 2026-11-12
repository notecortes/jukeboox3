package db

import (
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// newTestDB crea una base de datos SQLite temporal aislada para cada caso de prueba.
func newTestDB(t *testing.T) *DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test_aurabox.db")
	d, err := InitDB(dbPath)
	if err != nil {
		t.Fatalf("InitDB falló: %v", err)
	}
	t.Cleanup(func() {
		_ = d.Close()
	})
	return d
}

// TestUpsertAndIndexedDurations comprueba que UpsertTrack asigna el ID real y GetIndexedTrackDurations indexa por ruta.
func TestUpsertAndIndexedDurations(t *testing.T) {
	d := newTestDB(t)

	tr := Track{
		Path:        "/music/local/The Beatles/Abbey Road/01 - Come Together.flac",
		Filename:    "01 - Come Together.flac",
		Title:       "Come Together",
		Artist:      "The Beatles",
		Album:       "Abbey Road",
		AlbumArtist: "The Beatles",
		Genre:       "Rock",
		Year:        1969,
		Duration:    259.5,
		TrackNumber: 1,
		Bitrate:     960,
		SampleRate:  96000,
		Format:      "FLAC",
		Source:      "local",
	}

	if err := d.UpsertTrack(&tr); err != nil {
		t.Fatalf("UpsertTrack falló: %v", err)
	}
	if tr.ID <= 0 {
		t.Fatalf("se esperaba un ID asignado tras UpsertTrack, recibido %d", tr.ID)
	}

	durations, err := d.GetIndexedTrackDurations()
	if err != nil {
		t.Fatalf("GetIndexedTrackDurations falló: %v", err)
	}
	if got := durations[tr.Path]; got != 259.5 {
		t.Fatalf("duración esperada 259.5, obtenida %v", got)
	}
}

// TestGetTracksByIDsPreservesOrderAndChunks verifica la fragmentación en bloques de 500 y el orden exacto de entrada.
func TestGetTracksByIDsPreservesOrderAndChunks(t *testing.T) {
	d := newTestDB(t)

	const total = 515
	batch := make([]*Track, 0, total)
	for i := 1; i <= total; i++ {
		batch = append(batch, &Track{
			Path:        fmt.Sprintf("/music/nas/artist/album/track_%03d.mp3", i),
			Filename:    fmt.Sprintf("track_%03d.mp3", i),
			Title:       fmt.Sprintf("Canción %03d", i),
			Artist:      "Artista Prueba",
			Album:       "Álbum Prueba",
			Duration:    180.0,
			TrackNumber: i,
			Format:      "MP3",
			Source:      "nas",
		})
	}
	if err := d.BatchUpsertTracks(batch); err != nil {
		t.Fatalf("BatchUpsertTracks falló: %v", err)
	}

	all, err := d.GetAllTracks()
	if err != nil || len(all) != total {
		t.Fatalf("se esperaban %d pistas, obtenidas %d (err=%v)", total, len(all), err)
	}

	// Construir una lista desordenada que cruce el límite de 500 elementos por bloque
	requestedIDs := make([]int64, 0, total)
	for i := total - 1; i >= 0; i-- {
		requestedIDs = append(requestedIDs, all[i].ID)
	}

	got, err := d.GetTracksByIDs(requestedIDs)
	if err != nil {
		t.Fatalf("GetTracksByIDs falló: %v", err)
	}
	if len(got) != len(requestedIDs) {
		t.Fatalf("longitud esperada %d, obtenida %d", len(requestedIDs), len(got))
	}
	for i, wantID := range requestedIDs {
		if got[i].ID != wantID {
			t.Fatalf("en índice %d se esperaba ID %d, obtenido %d", i, wantID, got[i].ID)
		}
	}
}

// TestEmptySlicesAndFavorites verifica que las consultas vacías no devuelven nil y que ToggleFavorite persiste cambios.
func TestEmptySlicesAndFavorites(t *testing.T) {
	d := newTestDB(t)

	if tracks, err := d.GetAllTracks(); err != nil || tracks == nil || len(tracks) != 0 {
		t.Fatalf("GetAllTracks vacío debe devolver slice no nulo (len=%d, nil=%v, err=%v)", len(tracks), tracks == nil, err)
	}
	if albums, err := d.GetAlbums(); err != nil || albums == nil || len(albums) != 0 {
		t.Fatalf("GetAlbums vacío debe devolver slice no nulo")
	}
	if artists, err := d.GetArtists(); err != nil || artists == nil || len(artists) != 0 {
		t.Fatalf("GetArtists vacío debe devolver slice no nulo")
	}
	if favs, err := d.GetFavorites(); err != nil || favs == nil || len(favs) != 0 {
		t.Fatalf("GetFavorites vacío debe devolver slice no nulo")
	}

	tr := Track{
		Path:     "/music/local/Rosalía/El Mal Querer/01 - Malamente.flac",
		Filename: "01 - Malamente.flac",
		Title:    "Malamente",
		Artist:   "Rosalía",
		Album:    "El Mal Querer",
		Duration: 150.0,
		Format:   "FLAC",
		Source:   "local",
	}
	if err := d.UpsertTrack(&tr); err != nil {
		t.Fatalf("UpsertTrack falló: %v", err)
	}

	isFav, err := d.ToggleFavorite(tr.ID)
	if err != nil || !isFav {
		t.Fatalf("ToggleFavorite debería activar favorito (isFav=%v, err=%v)", isFav, err)
	}

	favs, err := d.GetFavorites()
	if err != nil || len(favs) != 1 || favs[0].ID != tr.ID {
		t.Fatalf("GetFavorites inesperado: %+v (err=%v)", favs, err)
	}
}

// TestConcurrentReadsDuringBatchUpsertWAL verifica que el pool WAL de lectura no se bloquea ni falla con escrituras concurrentes.
func TestConcurrentReadsDuringBatchUpsertWAL(t *testing.T) {
	d := newTestDB(t)

	var wg sync.WaitGroup
	errs := make(chan error, 64)

	// Escritor en lotes simulando un escaneo continuo
	wg.Add(1)
	go func() {
		defer wg.Done()
		for b := 0; b < 5; b++ {
			batch := make([]*Track, 0, 100)
			for i := 0; i < 100; i++ {
				batch = append(batch, &Track{
					Path:        fmt.Sprintf("/music/nas/band_%d/alb_%d/track_%02d.flac", i%5, b, i),
					Filename:    fmt.Sprintf("track_%02d.flac", i),
					Title:       fmt.Sprintf("Tema %d-%02d", b, i),
					Artist:      fmt.Sprintf("Banda %d", i%5),
					Album:       fmt.Sprintf("Disco %d", b),
					Genre:       "Indie",
					Duration:    200.0,
					TrackNumber: i + 1,
					Format:      "FLAC",
					Source:      "nas",
				})
			}
			if err := d.BatchUpsertTracks(batch); err != nil {
				errs <- fmt.Errorf("BatchUpsertTracks lote %d: %w", b, err)
				return
			}
		}
	}()

	// Lectores concurrentes ejecutando consultas de biblioteca mientras se escribe
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for k := 0; k < 20; k++ {
				if _, err := d.GetRecentTracks(20); err != nil {
					errs <- fmt.Errorf("GetRecentTracks: %w", err)
					return
				}
				if _, err := d.GetAlbums(); err != nil {
					errs <- fmt.Errorf("GetAlbums: %w", err)
					return
				}
				if _, err := d.GetArtists(); err != nil {
					errs <- fmt.Errorf("GetArtists: %w", err)
					return
				}
				if _, _, _, err := d.Search("Banda"); err != nil {
					errs <- fmt.Errorf("Search: %w", err)
					return
				}
			}
		}()
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}

	artists, err := d.GetArtists()
	if err != nil || len(artists) != 5 {
		t.Fatalf("se esperaban 5 artistas tras lotes deduplicados, obtenidos %d (err=%v)", len(artists), err)
	}
}

// TestInMemoryDBCompatibility asegura que las bases de datos efímeras :memory: mantienen lecturas y escrituras consistentes.
func TestInMemoryDBCompatibility(t *testing.T) {
	d, err := InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB(:memory:) falló: %v", err)
	}
	defer d.Close()

	if err := d.SetSetting("scanner_version", "2"); err != nil {
		t.Fatalf("SetSetting falló: %v", err)
	}
	if got := d.GetSetting("scanner_version", ""); got != "2" {
		t.Fatalf("GetSetting esperado '2', obtenido %q", got)
	}
}

// TestEmbeddedRadiosSeededAndCRUD verifica el sembrado automático de emisoras embebidas (incluidas españolas) y el alta/borrado de radios personalizadas.
func TestEmbeddedRadiosSeededAndCRUD(t *testing.T) {
	d, err := InitDB(":memory:")
	if err != nil {
		t.Fatalf("InitDB(:memory:) falló: %v", err)
	}
	defer d.Close()

	radios, err := d.GetRadios()
	if err != nil {
		t.Fatalf("GetRadios falló: %v", err)
	}
	if len(radios) < 280 {
		t.Fatalf("se esperaban al menos 280 emisoras sembradas desde default_radios.json embebido, obtenidas %d", len(radios))
	}

	spanishCount := 0
	var foundRNE bool
	var defaultStation RadioStation
	for _, r := range radios {
		if strings.EqualFold(r.Country, "Spain") {
			spanishCount++
		}
		if r.Name == "RNE Radio Nacional" {
			foundRNE = true
		}
		if r.IsDefault && defaultStation.ID == 0 {
			defaultStation = r
		}
	}
	if spanishCount < 45 || !foundRNE {
		t.Fatalf("catálogo español incompleto (spanishCount=%d, foundRNE=%v)", spanishCount, foundRNE)
	}

	custom := RadioStation{
		Name:        "Mi Radio Indie Test",
		StreamURL:   "https://example.org/stream/live.pls",
		Genre:       "Indie",
		Broadcaster: "Indie Lab",
		Country:     "Spain",
		Bitrate:     "192",
		Format:      "STREAM",
	}
	if err := d.AddRadio(&custom); err != nil {
		t.Fatalf("AddRadio falló: %v", err)
	}
	if custom.ID <= 0 || custom.IsDefault {
		t.Fatalf("emisora personalizada mal guardada: %+v", custom)
	}

	fetched, err := d.GetRadioByStreamURL(custom.StreamURL)
	if err != nil || fetched.ID != custom.ID {
		t.Fatalf("GetRadioByStreamURL falló: err=%v fetched=%+v", err, fetched)
	}

	dupDefault := custom
	dupDefault.Name = "Intento Sobrescribir"
	dupDefault.StreamURL = defaultStation.StreamURL
	if err := d.AddRadio(&dupDefault); err == nil {
		t.Fatalf("AddRadio no debería sobrescribir una emisora integrada existente (%s)", defaultStation.Name)
	}

	if err := d.DeleteRadio(defaultStation.ID); err == nil {
		t.Fatalf("DeleteRadio no debería borrar una emisora predeterminada (ID=%d)", defaultStation.ID)
	}
	if err := d.DeleteRadio(custom.ID); err != nil {
		t.Fatalf("DeleteRadio personalizada falló: %v", err)
	}
	if _, err := d.GetRadioByStreamURL(custom.StreamURL); err == nil {
		t.Fatalf("GetRadioByStreamURL debería fallar tras eliminar la emisora")
	}
}

// TestLikeWildcardEscapingInSearchAndPathPrefix valida que % y _ se interpreten de forma literal en búsquedas LIKE y prefijos.
func TestLikeWildcardEscapingInSearchAndPathPrefix(t *testing.T) {
	d := newTestDB(t)

	samples := []*Track{
		{
			Path:     "/music/local/100%_Live/01.flac",
			Filename: "01.flac",
			Title:    "100% Pure Love",
			Artist:   "Artist_One",
			Album:    "Special 100%",
			Format:   "FLAC",
			Source:   "local",
		},
		{
			Path:     "/music/local/100X_Other/02.flac",
			Filename: "02.flac",
			Title:    "1000 Nights",
			Artist:   "ArtistXOne",
			Album:    "Normal Album",
			Format:   "FLAC",
			Source:   "local",
		},
	}
	for _, tr := range samples {
		if err := d.UpsertTrack(tr); err != nil {
			t.Fatalf("UpsertTrack falló: %v", err)
		}
	}

	foundTracks, _, _, err := d.Search("100%")
	if err != nil {
		t.Fatalf("Search('100%%') falló: %v", err)
	}
	if len(foundTracks) != 1 || foundTracks[0].Title != "100% Pure Love" {
		t.Fatalf("Search('100%%') debía devolver solo coincidencia literal, obtuvo %+v", foundTracks)
	}

	_, _, foundArtists, err := d.Search("Artist_One")
	if err != nil {
		t.Fatalf("Search('Artist_One') falló: %v", err)
	}
	if len(foundArtists) != 1 || foundArtists[0].Name != "Artist_One" {
		t.Fatalf("Search('Artist_One') debía coincidir solo con '_', obtuvo %+v", foundArtists)
	}

	prefTracks, err := d.GetTracksByPathPrefix("/music/local/100%_Live")
	if err != nil {
		t.Fatalf("GetTracksByPathPrefix falló: %v", err)
	}
	if len(prefTracks) != 1 || prefTracks[0].Path != "/music/local/100%_Live/01.flac" {
		t.Fatalf("GetTracksByPathPrefix debía devolver solo carpeta literal, obtuvo %+v", prefTracks)
	}
}

// TestAlbumsRequireMoreThanFiveTracks verifica que GetAlbums y Search solo listan álbumes con más de 5 canciones (> 5), manteniendo las pistas individuales accesibles.
func TestAlbumsRequireMoreThanFiveTracks(t *testing.T) {
	d := newTestDB(t)

	insertAlbumTracks := func(albumName string, count int) int64 {
		var firstID int64
		batch := make([]*Track, 0, count)
		for i := 1; i <= count; i++ {
			batch = append(batch, &Track{
				Path:        fmt.Sprintf("/music/local/band/%s/%02d.flac", strings.ReplaceAll(albumName, " ", "_"), i),
				Filename:    fmt.Sprintf("%02d.flac", i),
				Title:       fmt.Sprintf("%s Track %02d", albumName, i),
				Artist:      "Filter Band",
				Album:       albumName,
				Duration:    180.0,
				TrackNumber: i,
				Format:      "FLAC",
				Source:      "local",
			})
		}
		if err := d.BatchUpsertTracks(batch); err != nil {
			t.Fatalf("BatchUpsertTracks (%s) falló: %v", albumName, err)
		}
		all, err := d.GetAllTracks()
		if err != nil {
			t.Fatalf("GetAllTracks falló: %v", err)
		}
		for _, tr := range all {
			if tr.Album == albumName {
				firstID = tr.ID
				break
			}
		}
		return firstID
	}

	singleID := insertAlbumTracks("Filter Single", 1)
	epID := insertAlbumTracks("Filter EP5", 5)
	lp6ID := insertAlbumTracks("Filter LP6", 6)
	lp10ID := insertAlbumTracks("Filter LP10", 10)

	albums, err := d.GetAlbums()
	if err != nil {
		t.Fatalf("GetAlbums falló: %v", err)
	}
	if len(albums) != 2 {
		t.Fatalf("se esperaban exactamente 2 álbumes (> 5 temas), obtenidos %d: %+v", len(albums), albums)
	}

	gotTitles := map[string]int{
		albums[0].Title: albums[0].TrackCount,
		albums[1].Title: albums[1].TrackCount,
	}
	if gotTitles["Filter LP6"] != 6 || gotTitles["Filter LP10"] != 10 {
		t.Fatalf("títulos/conteos inesperados en GetAlbums: %+v", gotTitles)
	}

	// En Search, los álbumes pequeños no deben aparecer en la sección de álbumes, pero sus pistas sí en Canciones.
	foundTracks, foundAlbums, _, err := d.Search("Filter")
	if err != nil {
		t.Fatalf("Search('Filter') falló: %v", err)
	}
	if len(foundAlbums) != 2 {
		t.Fatalf("Search debía devolver solo 2 álbumes (> 5 temas), obtuvo %d: %+v", len(foundAlbums), foundAlbums)
	}
	for _, a := range foundAlbums {
		if a.TrackCount <= 5 {
			t.Fatalf("álbum %q con %d temas no debería aparecer en Search", a.Title, a.TrackCount)
		}
	}
	if len(foundTracks) == 0 {
		t.Fatalf("Search debía seguir encontrando las pistas individuales")
	}

	// La consulta directa por ID sigue funcionando para pistas existentes (compatibilidad de API).
	if singleID == 0 || epID == 0 || lp6ID == 0 || lp10ID == 0 {
		t.Fatalf("IDs inválidos: single=%d ep=%d lp6=%d lp10=%d", singleID, epID, lp6ID, lp10ID)
	}
	if _, epTracks, err := d.GetAlbumByID(epID); err != nil || len(epTracks) != 5 {
		t.Fatalf("GetAlbumByID(epID) debía seguir devolviendo sus 5 pistas, err=%v len=%d", err, len(epTracks))
	}
}


