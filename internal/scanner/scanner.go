package scanner

import (
	"bytes"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode"

	"aurabox/internal/db"
	"aurabox/internal/events"
)

const (
	scannerSchemaVersion = "2"
	maxCoverFileBytes    = 10 << 20
	maxCoverCacheEntries = 5000
)

var (
	catDirRegex        = regexp.MustCompile(`^(\d{2}\.)?([a-zA-ZáéíóúÁÉÍÓÚñÑ]+)$`)
	albumCleanRegex    = regexp.MustCompile(`(?i)\s*(\[[^\]]*\]|\((?:19|20)\d{2}[^)]*\)|FLAC|Lossless|Hunter|PMEDIA|FRG|320kbps|⭐️|\b\d+Bit[^\s]*)\s*`)
	albumYearPrefRegex = regexp.MustCompile(`^(?:19|20)\d{2}\s*[-–]\s*(.+)`)
	yearSearchRegex    = regexp.MustCompile(`\b(19\d{2}|20\d{2})\b`)
	trackDotRegex      = regexp.MustCompile(`^(\d+)\.\s*(.+)`)
)

// LibraryScanner analiza las carpetas de música local, NAS y USB e indexa metadatos audiófilos.
type LibraryScanner struct {
	database       *db.DB
	eventsHub      *events.Hub
	localDir       string
	nasDir         string
	usbDir         string
	coversDir      string
	scanning       bool
	repairing      atomic.Bool
	coverCache     map[string]string
	cacheMu        sync.RWMutex
	coverCopyMu    sync.Mutex
	mu             sync.Mutex
	onScanComplete func()
	watcherMu      sync.Mutex
	autoWatcher    *autoWatcher
}

// NewLibraryScanner inicializa el escáner de biblioteca musical.
func NewLibraryScanner(database *db.DB, hub *events.Hub, localDir, nasDir, usbDir, coversDir string) *LibraryScanner {
	_ = os.MkdirAll(coversDir, 0755)
	return &LibraryScanner{
		database:   database,
		eventsHub:  hub,
		localDir:   localDir,
		nasDir:     nasDir,
		usbDir:     usbDir,
		coversDir:  coversDir,
		coverCache: make(map[string]string),
	}
}

// SetOnScanComplete registra un callback opcional que se ejecuta tras finalizar un escaneo de biblioteca.
func (ls *LibraryScanner) SetOnScanComplete(fn func()) {
	ls.mu.Lock()
	ls.onScanComplete = fn
	ls.mu.Unlock()
}

// WaitForIdle espera a que terminen los barridos o reparaciones de duración en curso antes de cerrar recursos.
func (ls *LibraryScanner) WaitForIdle() {
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		ls.mu.Lock()
		busy := ls.scanning
		ls.mu.Unlock()
		if !busy && !ls.repairing.Load() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// ScanLibraryAll inicia un barrido incremental rápido de todas las fuentes configuradas.
func (ls *LibraryScanner) ScanLibraryAll() {
	ls.ScanLibrary(false)
}

// ScanLibrary inicia el escaneo paralelo en segundo plano; con force=true reindexa incluso pistas existentes.
func (ls *LibraryScanner) ScanLibrary(force bool) {
	ls.mu.Lock()
	if ls.scanning {
		ls.mu.Unlock()
		return
	}
	ls.scanning = true
	ls.mu.Unlock()

	go func() {
		defer func() {
			ls.mu.Lock()
			ls.scanning = false
			ls.mu.Unlock()
		}()

		forceScan := force
		if ls.database != nil && !forceScan {
			if ver := ls.database.GetSetting("scanner_version", ""); ver != scannerSchemaVersion {
				log.Printf("[Scanner] Actualizando índice a esquema de escáner v%s (reescaneo único)...", scannerSchemaVersion)
				forceScan = true
			}
		}

		var indexedDurations map[string]float64
		if !forceScan && ls.database != nil {
			indexedDurations, _ = ls.database.GetIndexedTrackDurations()
		}

		log.Printf("[Scanner] Iniciando análisis de biblioteca (incremental=%v, ya_indexadas=%d)...",
			!forceScan, len(indexedDurations))
		start := time.Now()

		sources := []struct {
			name string
			path string
		}{
			{"local", ls.localDir},
			{"nas", ls.nasDir},
			{"usb", ls.usbDir},
		}

		totalProcessed := 0
		skippedExisting := 0
		totalPruned := 0
		for _, s := range sources {
			if s.path == "" {
				continue
			}
			if _, err := os.Stat(s.path); os.IsNotExist(err) {
				continue
			}
			processed, skipped, seenPaths := ls.scanDirectory(s.path, s.name, indexedDurations, &totalProcessed)
			skippedExisting += skipped
			totalPruned += ls.pruneMissingSourceTracks(s.name, seenPaths)
			_ = processed
		}

		if ls.database != nil {
			_ = ls.database.SetSetting("scanner_version", scannerSchemaVersion)
		}

		elapsed := time.Since(start)
		log.Printf("[Scanner] Análisis finalizado: %d pistas nuevas/actualizadas (%d omitidas sin cambios, %d rutas obsoletas eliminadas) en %v",
			totalProcessed, skippedExisting, totalPruned, elapsed)

		ls.mu.Lock()
		onComplete := ls.onScanComplete
		ls.mu.Unlock()
		if onComplete != nil {
			onComplete()
		}

		// Tras escanear, reparar duraciones de pistas pendientes en segundo plano
		ls.RepairDurationsInBackground()

		if ls.eventsHub != nil {
			ls.eventsHub.Broadcast("library_updated", map[string]interface{}{
				"tracks_count": totalProcessed,
				"skipped":      skippedExisting,
				"pruned":       totalPruned,
				"elapsed_ms":   elapsed.Milliseconds(),
				"scanning":     false,
			})
		}
	}()
}

// RepairDurationsInBackground recorre en segundo plano las canciones sin duración real calculada y las repara en paralelo.
func (ls *LibraryScanner) RepairDurationsInBackground() {
	if !ls.repairing.CompareAndSwap(false, true) {
		return
	}

	go func() {
		defer ls.repairing.Store(false)

		tracks, err := ls.database.GetTracksNeedingDuration()
		if err != nil || len(tracks) == 0 {
			return
		}

		log.Printf("[Scanner] Calculando duración y calidad de audio real para %d canciones...", len(tracks))
		start := time.Now()

		type updateItem struct {
			id  int64
			dur float64
			br  int
			sr  int
		}

		results := make(chan updateItem, 256)
		jobs := make(chan db.TrackPathID, 256)

		// 4 workers concurrentes para equilibrar rendimiento y memoria (<256MB en RPi)
		var wg sync.WaitGroup
		for w := 0; w < 4; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for t := range jobs {
					dur, br, sr, err := ReadAudioProperties(t.Path)
					if err == nil && dur > 0 {
						results <- updateItem{id: t.ID, dur: dur, br: br, sr: sr}
					}
				}
			}()
		}

		// Encolador de tareas
		go func() {
			for _, t := range tracks {
				jobs <- t
			}
			close(jobs)
			wg.Wait()
			close(results)
		}()

		// Colector de resultados y persistencia en lotes
		batch := make([]db.TrackPropertyUpdate, 0, 100)
		totalUpdated := 0

		flush := func() {
			if len(batch) > 0 {
				_ = ls.database.BatchUpdateTrackProperties(batch)
				totalUpdated += len(batch)
				batch = batch[:0]
			}
		}

		for item := range results {
			batch = append(batch, db.TrackPropertyUpdate{
				ID:         item.id,
				Duration:   item.dur,
				Bitrate:    item.br,
				SampleRate: item.sr,
			})
			if len(batch) >= 100 {
				flush()
				// Ceder brevemente el lock de escritura SQLite para no retrasar favoritos o ajustes
				time.Sleep(15 * time.Millisecond)
			}
		}
		flush()

		elapsed := time.Since(start)
		log.Printf("[Scanner] Finalizado cálculo de duraciones: %d canciones actualizadas en %v.", totalUpdated, elapsed)

		if totalUpdated > 0 && ls.eventsHub != nil {
			ls.eventsHub.Broadcast("library_updated", map[string]interface{}{
				"durations_repaired": totalUpdated,
				"scanning":           false,
			})
		}
	}()
}

// pruneMissingSourceTracks elimina de SQLite rutas ya inexistentes en disco verificando con os.Stat y evitando borrados si NAS/USB está desmontado.
func (ls *LibraryScanner) pruneMissingSourceTracks(sourceName string, seenPaths map[string]struct{}) int {
	if ls.database == nil {
		return 0
	}
	// En NAS no podar si no se detectó ningún archivo (recurso de red temporalmente desconectado o desmontado).
	if sourceName == "nas" && len(seenPaths) == 0 {
		return 0
	}
	// En USB, podar solo si el directorio /music/usb existe vacío tras desmontar la unidad extraíble.
	if sourceName == "usb" && len(seenPaths) == 0 {
		entries, err := os.ReadDir(ls.usbDir)
		if err != nil || len(entries) > 0 {
			return 0
		}
	}
	refs, err := ls.database.GetTrackRefsBySource(sourceName)
	if err != nil || len(refs) == 0 {
		return 0
	}

	var staleIDs []int64
	for _, ref := range refs {
		if _, ok := seenPaths[ref.Path]; ok {
			continue
		}
		if _, statErr := os.Stat(ref.Path); os.IsNotExist(statErr) {
			staleIDs = append(staleIDs, ref.ID)
		}
	}
	if len(staleIDs) == 0 {
		return 0
	}

	deleted, err := ls.database.DeleteTracksByIDs(staleIDs)
	if err != nil {
		log.Printf("[Scanner] Advertencia al podar rutas obsoletas (%s): %v", sourceName, err)
		return 0
	}
	return int(deleted)
}

// scanDirectory recorre un árbol de directorios en paralelo (4 workers) y persiste en lotes con progreso SSE.
func (ls *LibraryScanner) scanDirectory(rootPath, sourceName string, indexed map[string]float64, totalCounter *int) (int, int, map[string]struct{}) {
	validExts := map[string]bool{
		".mp3":  true,
		".flac": true,
		".ogg":  true,
		".m4a":  true,
		".wav":  true,
		".opus": true,
		".aac":  true,
		".alac": true,
		".wma":  true,
	}

	type scanJob struct {
		path string
		info os.FileInfo
	}

	jobs := make(chan scanJob, 64)
	results := make(chan *db.Track, 128)
	seenPaths := make(map[string]struct{}, 128)

	var workersWg sync.WaitGroup
	const numWorkers = 4
	for w := 0; w < numWorkers; w++ {
		workersWg.Add(1)
		go func() {
			defer workersWg.Done()
			for j := range jobs {
				if t := ls.parseTrackMetadata(j.path, rootPath, sourceName, j.info); t != nil {
					results <- t
				}
			}
		}()
	}

	skipped := 0
	go func() {
		_ = filepath.Walk(rootPath, func(path string, info os.FileInfo, err error) error {
			if err != nil || info == nil || info.IsDir() || !info.Mode().IsRegular() {
				return nil
			}
			ext := strings.ToLower(filepath.Ext(path))
			if !validExts[ext] {
				return nil
			}
			seenPaths[path] = struct{}{}
			// Omitir archivos ya indexados con duración válida para arranques casi instantáneos
			if indexed != nil {
				if dur, ok := indexed[path]; ok && dur > 0 && dur != 240.0 {
					skipped++
					return nil
				}
			}
			jobs <- scanJob{path: path, info: info}
			return nil
		})
		close(jobs)
		workersWg.Wait()
		close(results)
	}()

	count := 0
	const initialBatchSize = 25
	const batchSize = 100
	batch := make([]*db.Track, 0, batchSize)
	firstBatchNotified := false
	lastNotify := time.Now()
	lastLoggedMilestone := 0

	flushBatch := func() {
		if len(batch) == 0 {
			return
		}
		if err := ls.database.BatchUpsertTracks(batch); err == nil {
			count += len(batch)
			if totalCounter != nil {
				*totalCounter += len(batch)
			}
			if count-lastLoggedMilestone >= 1000 {
				lastLoggedMilestone = (count / 1000) * 1000
				log.Printf("[Scanner] Progreso (%s): %d pistas indexadas...", sourceName, count)
			}
			// Enviar actualización progresiva en el primer lote o cada 5 segundos
			if ls.eventsHub != nil && (!firstBatchNotified || time.Since(lastNotify) >= 5*time.Second) {
				firstBatchNotified = true
				lastNotify = time.Now()
				soFar := count
				if totalCounter != nil {
					soFar = *totalCounter
				}
				ls.eventsHub.Broadcast("library_updated", map[string]interface{}{
					"tracks_count": soFar,
					"scanning":     true,
				})
			}
		} else {
			log.Printf("[Scanner] Advertencia al persistir lote de %d canciones: %v", len(batch), err)
		}
		batch = batch[:0]
	}

	for track := range results {
		batch = append(batch, track)
		limit := batchSize
		if !firstBatchNotified {
			limit = initialBatchSize
		}
		if len(batch) >= limit {
			flushBatch()
			// Pausa mínima para ceder prioridad a escrituras interactivas del usuario (favoritos, ajustes)
			time.Sleep(15 * time.Millisecond)
		}
	}
	flushBatch()

	return count, skipped, seenPaths
}

// parseTrackMetadata extrae título, artista, álbum, formato y carátula usando tags embebidos y fallback heurístico inteligente.
func (ls *LibraryScanner) parseTrackMetadata(filePath, rootPath, sourceName string, info os.FileInfo) *db.Track {
	filename := filepath.Base(filePath)
	ext := strings.ToLower(filepath.Ext(filePath))
	formatName := strings.ToUpper(strings.TrimPrefix(ext, "."))

	// Detectar o copiar carátula de la carpeta contenedora
	dir := filepath.Dir(filePath)
	coverURL := ls.findOrCacheCover(dir)

	var artist, album, title, genre, albumArtist string
	var trackNumber int = 1
	var year int = 0
	var sampleRate int = 44100
	var bitrate int = 320
	var duration float64 = 0.0

	// 1. Intentar leer etiquetas embebidas reales (Vorbis Comments para FLAC/OGG, ID3v2 para MP3)
	// Nota: ReadAudioTags ya calcula duración, bitrate y sample rate de MP3/FLAC/WAV sin reabrir el fichero.
	tagMeta := ReadAudioTags(filePath)
	if tagMeta != nil {
		if tagMeta.Artist != "" {
			artist = cleanMetadataString(tagMeta.Artist)
		}
		if tagMeta.Album != "" {
			album = cleanMetadataString(tagMeta.Album)
		}
		if tagMeta.Title != "" {
			title = cleanMetadataString(tagMeta.Title)
		}
		if tagMeta.AlbumArtist != "" {
			albumArtist = cleanMetadataString(tagMeta.AlbumArtist)
		}
		if tagMeta.Genre != "" {
			genre = cleanMetadataString(tagMeta.Genre)
		}
		if tagMeta.Year > 0 {
			year = tagMeta.Year
		}
		if tagMeta.TrackNumber > 0 {
			trackNumber = tagMeta.TrackNumber
		}
		if tagMeta.Duration > 0 {
			duration = tagMeta.Duration
		}
		if tagMeta.Bitrate > 0 {
			bitrate = tagMeta.Bitrate
		}
		if tagMeta.SampleRate > 0 {
			sampleRate = tagMeta.SampleRate
		}
	}

	// Si aún no se pudo calcular duración por stream VBR/CBR, estimar con tamaño del archivo
	if duration <= 0 && info != nil && info.Size() > 0 && bitrate > 0 {
		duration = float64(info.Size()*8) / float64(bitrate*1000)
	}

	// 2. Preparar heurística jerárquica sobre los directorios para completar metadatos ausentes
	relPath := filepath.Base(filePath)
	if rootPath != "" {
		if rel, err := filepath.Rel(rootPath, filePath); err == nil && !strings.HasPrefix(rel, "..") {
			relPath = rel
		}
	}
	segments := strings.Split(filepath.ToSlash(relPath), "/")

	isCategory := false
	detectedCategoryGenre := ""

	if len(segments) > 1 {
		m := catDirRegex.FindStringSubmatch(segments[0])
		if len(m) > 2 {
			isCategory = true
			detectedCategoryGenre = capitalizeWords(m[2])
			if detectedCategoryGenre == "Clasica" {
				detectedCategoryGenre = "Clásica"
			}
		}
	}

	if genre == "" {
		if isCategory {
			genre = detectedCategoryGenre
		} else {
			genre = "Música"
		}
	}

	fallbackArtist := "Artista Desconocido"
	fallbackAlbum := "Colección"

	if isCategory && len(segments) >= 4 {
		// /music/nas/04.indie/izal/2018 - Autoterapia/01 - Autoterapia.flac
		fallbackArtist = cleanArtistName(segments[1])
		fallbackAlbum = cleanAlbumName(segments[2])
		year = extractYear(segments[2], year)
	} else if isCategory && len(segments) == 3 {
		// /music/nas/01.clasica/Ludovico Einaudi/01 - Divenire.mp3 (sin subcarpeta de álbum)
		fallbackArtist = cleanArtistName(segments[1])
		fallbackAlbum = "Sencillos"
	} else if isCategory && len(segments) == 2 {
		// /music/nas/05.recopilatorios/01 - Tema.mp3
		fallbackArtist = "Varios Artistas"
		fallbackAlbum = detectedCategoryGenre
	} else if len(segments) >= 3 {
		// /music/local/The Beatles/Abbey Road/01 - Come Together.flac
		fallbackArtist = cleanArtistName(segments[0])
		fallbackAlbum = cleanAlbumName(segments[1])
		year = extractYear(segments[1], year)
	} else if len(segments) == 2 {
		fallbackArtist = cleanArtistName(segments[0])
		fallbackAlbum = "Sencillos"
	}

	if artist == "" || strings.HasPrefix(artist, "0") {
		artist = fallbackArtist
	}
	if album == "" {
		album = fallbackAlbum
	}

	// 3. Extracción y refinamiento del Título y Número de Pista si viene del nombre de archivo
	rawBaseName := strings.TrimSuffix(filename, filepath.Ext(filename))
	if title == "" {
		parsedTrack, parsedArtist, parsedTitle := parseFilename(rawBaseName)
		if parsedTrack > 0 {
			trackNumber = parsedTrack
		}
		if parsedArtist != "" && (artist == "Artista Desconocido" || artist == "Varios Artistas") {
			artist = cleanArtistName(parsedArtist)
		}
		if parsedTitle != "" {
			title = parsedTitle
		} else {
			title = rawBaseName
		}
	}

	// 4. Parámetros de Calidad Hi-Fi según el contenedor si no vinieron en las etiquetas
	if bitrate <= 0 || sampleRate <= 0 {
		switch formatName {
		case "FLAC":
			if bitrate <= 0 {
				bitrate = 960
			}
			if sampleRate <= 0 {
				sampleRate = 96000
			}
		case "MP3":
			if bitrate <= 0 {
				bitrate = 320
			}
			if sampleRate <= 0 {
				sampleRate = 44100
			}
		case "WAV":
			if bitrate <= 0 {
				bitrate = 1411
			}
			if sampleRate <= 0 {
				sampleRate = 44100
			}
		case "OGG", "OPUS":
			if bitrate <= 0 {
				bitrate = 256
			}
			if sampleRate <= 0 {
				sampleRate = 48000
			}
		}
	}

	if albumArtist == "" {
		albumArtist = artist
	}

	if coverURL == "" {
		coverURL = ls.findOrCacheEmbeddedCover(dir, album, filePath)
	}

	return &db.Track{
		Path:        filePath,
		Filename:    filename,
		Title:       title,
		Artist:      artist,
		Album:       album,
		AlbumArtist: albumArtist,
		Genre:       genre,
		Year:        year,
		Duration:    duration,
		TrackNumber: trackNumber,
		Bitrate:     bitrate,
		SampleRate:  sampleRate,
		Format:      formatName,
		Source:      sourceName,
		CoverURL:    coverURL,
		BPM:         120.0,
	}
}

// cleanArtistName normaliza nombres de artistas y bandas conocidos para dar aspecto profesional.
func cleanArtistName(raw string) string {
	raw = strings.TrimSpace(raw)
	lower := strings.ToLower(raw)

	// Mapeo canónico para agrupaciones comunes
	canonicals := map[string]string{
		"izal":               "IZAL",
		"the cure":           "The Cure",
		"la habitación roja": "La Habitación Roja",
		"la habitacion roja": "La Habitación Roja",
		"lori meyers":        "Lori Meyers",
		"sidonie":            "Sidonie",
		"ultraligera":        "Ultraligera",
		"viva suecia":        "Viva Suecia",
		"yves tumor":         "Yves Tumor",
		"vetusta morla":      "Vetusta Morla",
		"love of lesbians":   "Love of Lesbian",
		"love of lesbian":    "Love of Lesbian",
		"linkin park":        "Linkin Park",
		"the cult":           "The Cult",
		"madonna":            "Madonna",
		"nujabes":            "Nujabes",
		"david bowie":        "David Bowie",
		"frank sinatra":      "Frank Sinatra",
		"sia":                "Sia",
		"max richter":        "Max Richter",
		"ludovico":           "Ludovico Einaudi",
		"ludovico einaudi":   "Ludovico Einaudi",
		"octave one":         "Octave One",
		"ólafur arnalds":     "Ólafur Arnalds",
		"olafur arnalds":     "Ólafur Arnalds",
		"rosalia":            "Rosalía",
		"rosalía":            "Rosalía",
		"the beatles":        "The Beatles",
		"miles davis":        "Miles Davis",
		"pink floyd":         "Pink Floyd",
		"queen":              "Queen",
		"led zeppelin":       "Led Zeppelin",
	}

	if c, ok := canonicals[lower]; ok {
		return c
	}

	// Si tiene formato "Nombre Apellido", capitalizar palabras
	return capitalizeWords(raw)
}

// cleanAlbumName elimina tags como [FLAC], (1993), [PMEDIA] del título del álbum para que luzca estético.
func cleanAlbumName(raw string) string {
	raw = strings.TrimSpace(raw)
	// Quitar sufijos entre corchetes o paréntesis con etiquetas técnicas
	cleaned := albumCleanRegex.ReplaceAllString(raw, " ")
	cleaned = strings.TrimSpace(cleaned)

	// Si el álbum empieza por año "2018 - Nombre", quitar el prefijo
	if m := albumYearPrefRegex.FindStringSubmatch(cleaned); len(m) > 1 {
		cleaned = strings.TrimSpace(m[1])
	}

	if cleaned == "" {
		return raw
	}
	return cleaned
}

// extractYear busca un año de 4 dígitos en el nombre del directorio o álbum.
func extractYear(name string, defaultYear int) int {
	if defaultYear > 0 {
		return defaultYear
	}
	m := yearSearchRegex.FindString(name)
	if m != "" {
		if y, err := strconv.Atoi(m); err == nil {
			return y
		}
	}
	return 0
}

// parseFilename descompone nombres como "01 - Titulo", "08 - IZAL - Jenna Fischer", "U2 - One.mp3"
func parseFilename(base string) (trackNum int, artist string, title string) {
	trackNum = 1
	parts := strings.Split(base, " - ")

	if len(parts) == 3 {
		// "08 - IZAL - Jenna Fischer"
		if num, err := strconv.Atoi(strings.TrimSpace(parts[0])); err == nil {
			trackNum = num
		}
		artist = strings.TrimSpace(parts[1])
		title = strings.TrimSpace(parts[2])
		return
	} else if len(parts) == 2 {
		// "¿Es '01 - Titulo' o 'Artista - Canción'?"
		first := strings.TrimSpace(parts[0])
		if num, err := strconv.Atoi(first); err == nil {
			trackNum = num
			title = strings.TrimSpace(parts[1])
			return
		}
		// 'U2 - One'
		artist = first
		title = strings.TrimSpace(parts[1])
		return
	}

	// Manejo de "14. Jeanne"
	if m := trackDotRegex.FindStringSubmatch(base); len(m) > 2 {
		if num, err := strconv.Atoi(m[1]); err == nil {
			trackNum = num
		}
		title = strings.TrimSpace(m[2])
		return
	}

	title = base
	return
}

// capitalizeWords pone en mayúscula la primera letra de cada palabra respetando preposiciones.
func capitalizeWords(s string) string {
	words := strings.Fields(s)
	lowerExceptions := map[string]bool{
		"de": true, "del": true, "la": true, "las": true, "el": true, "los": true,
		"y": true, "o": true, "en": true, "a": true, "the": true, "of": true, "and": true,
	}

	for i, w := range words {
		lowerW := strings.ToLower(w)
		if i > 0 && lowerExceptions[lowerW] {
			words[i] = lowerW
			continue
		}
		runes := []rune(w)
		if len(runes) > 0 {
			runes[0] = unicode.ToUpper(runes[0])
			words[i] = string(runes)
		}
	}
	return strings.Join(words, " ")
}

// cleanMetadataString elimina caracteres no imprimibles o espacios residuales.
func cleanMetadataString(s string) string {
	return strings.TrimSpace(strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return -1
	}, s))
}

// findOrCacheCover localiza una imagen de carátula local y la enlaza a la URL estática con caché en memoria segura para concurrencia.
func (ls *LibraryScanner) findOrCacheCover(dir string) string {
	ls.cacheMu.RLock()
	if cached, ok := ls.coverCache[dir]; ok {
		ls.cacheMu.RUnlock()
		return cached
	}
	ls.cacheMu.RUnlock()

	// Serializar búsqueda y copia cuando una carpeta no está aún cacheada para evitar lecturas SMB duplicadas
	ls.coverCopyMu.Lock()
	defer ls.coverCopyMu.Unlock()

	ls.cacheMu.RLock()
	if cached, ok := ls.coverCache[dir]; ok {
		ls.cacheMu.RUnlock()
		return cached
	}
	ls.cacheMu.RUnlock()

	preferredNames := []string{
		"cover.jpg", "cover.jpeg", "cover.png", "cover.webp",
		"folder.jpg", "folder.jpeg", "folder.png", "folder.webp",
		"front.jpg", "front.jpeg", "front.png", "front.webp",
		"album.jpg", "album.jpeg", "album.png", "album.webp",
		"artwork.jpg", "artwork.jpeg", "artwork.png",
	}

	result := ""
	if entries, err := os.ReadDir(dir); err == nil {
		filesByLower := make(map[string]string, len(entries))
		for _, entry := range entries {
			if !entry.IsDir() {
				name := entry.Name()
				filesByLower[strings.ToLower(name)] = name
			}
		}

		tryCacheImage := func(actualName string) string {
			candPath := filepath.Join(dir, actualName)
			candInfo, statErr := os.Lstat(candPath)
			if statErr != nil || !candInfo.Mode().IsRegular() || candInfo.Size() <= 0 || candInfo.Size() > maxCoverFileBytes {
				return ""
			}
			hash := md5.Sum([]byte(dir))
			hashStr := hex.EncodeToString(hash[:])
			ext := strings.ToLower(filepath.Ext(actualName))
			destName := hashStr + ext
			destPath := filepath.Join(ls.coversDir, destName)

			if _, err := os.Stat(destPath); os.IsNotExist(err) {
				if copyErr := copyFile(candPath, destPath); copyErr != nil {
					return ""
				}
			}
			return fmt.Sprintf("/api/covers/%s", destName)
		}

		for _, pref := range preferredNames {
			if actualName, ok := filesByLower[pref]; ok {
				if cachedURL := tryCacheImage(actualName); cachedURL != "" {
					result = cachedURL
					break
				}
			}
		}

		// Fallback inteligente: si no hay nombre canónico (cover/folder/front), aprovechar cualquier imagen válida en la carpeta
		if result == "" {
			var otherImages []string
			for lowerName, actualName := range filesByLower {
				ext := filepath.Ext(lowerName)
				if ext == ".jpg" || ext == ".jpeg" || ext == ".png" || ext == ".webp" {
					otherImages = append(otherImages, actualName)
				}
			}
			sort.Strings(otherImages)
			for _, imgName := range otherImages {
				if cachedURL := tryCacheImage(imgName); cachedURL != "" {
					result = cachedURL
					break
				}
			}
		}
	}

	ls.cacheMu.Lock()
	if len(ls.coverCache) >= maxCoverCacheEntries {
		clear(ls.coverCache)
	}
	ls.coverCache[dir] = result
	ls.cacheMu.Unlock()

	return result
}

// findOrCacheEmbeddedCover extrae y cachea la carátula incrustada en etiquetas cuando la carpeta carece de archivos de imagen.
func (ls *LibraryScanner) findOrCacheEmbeddedCover(dir, album, filePath string) string {
	extLower := strings.ToLower(filepath.Ext(filePath))
	if extLower != ".mp3" && extLower != ".flac" {
		return ""
	}

	normAlbum := strings.ToLower(strings.TrimSpace(album))
	cacheKey := "emb:" + dir + "\x00" + normAlbum
	if normAlbum == "" || normAlbum == "sencillos" || normAlbum == "colección" {
		cacheKey = "emb-file:" + filePath
	}

	ls.cacheMu.RLock()
	if cached, ok := ls.coverCache[cacheKey]; ok {
		ls.cacheMu.RUnlock()
		return cached
	}
	ls.cacheMu.RUnlock()

	ls.coverCopyMu.Lock()
	defer ls.coverCopyMu.Unlock()

	ls.cacheMu.RLock()
	if cached, ok := ls.coverCache[cacheKey]; ok {
		ls.cacheMu.RUnlock()
		return cached
	}
	ls.cacheMu.RUnlock()

	hash := md5.Sum([]byte(cacheKey))
	hashStr := hex.EncodeToString(hash[:])
	result := ""

	// Reutilizar si ya se extrajo en un escaneo previo sin reabrir el audio por red
	for _, candidateExt := range []string{".jpg", ".png", ".webp", ".gif", ".bmp"} {
		destName := hashStr + candidateExt
		destPath := filepath.Join(ls.coversDir, destName)
		if info, err := os.Stat(destPath); err == nil && info.Size() > 0 {
			result = fmt.Sprintf("/api/covers/%s", destName)
			break
		}
	}

	if result == "" {
		imgBytes, imgExt := ExtractEmbeddedCover(filePath, defaultMaxEmbeddedCoverBytes)
		if len(imgBytes) > 0 && imgExt != "" {
			if err := os.MkdirAll(ls.coversDir, 0755); err == nil {
				destName := hashStr + imgExt
				destPath := filepath.Join(ls.coversDir, destName)
				tmpDst := destPath + ".tmp"
				if writeErr := os.WriteFile(tmpDst, imgBytes, 0644); writeErr == nil {
					if renErr := os.Rename(tmpDst, destPath); renErr == nil {
						result = fmt.Sprintf("/api/covers/%s", destName)
					} else {
						_ = os.Remove(tmpDst)
					}
				}
			}
		}
	}

	ls.cacheMu.Lock()
	if len(ls.coverCache) >= maxCoverCacheEntries {
		clear(ls.coverCache)
	}
	ls.coverCache[cacheKey] = result
	ls.cacheMu.Unlock()

	return result
}

// isSupportedImageMagic valida las firmas mágicas estándar de imágenes JPEG, PNG, GIF, WebP o BMP.
func isSupportedImageMagic(header []byte) bool {
	if len(header) >= 3 && header[0] == 0xFF && header[1] == 0xD8 && header[2] == 0xFF {
		return true // JPEG
	}
	if len(header) >= 8 && bytes.Equal(header[:8], []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}) {
		return true // PNG
	}
	if len(header) >= 4 && (bytes.Equal(header[:4], []byte("GIF8")) || (len(header) >= 12 && bytes.Equal(header[:4], []byte("RIFF")) && bytes.Equal(header[8:12], []byte("WEBP")))) {
		return true // GIF / WebP
	}
	if len(header) >= 2 && header[0] == 'B' && header[1] == 'M' {
		return true // BMP
	}
	return false
}

// copyFile copia de forma segura y acotada una carátula física validando su cabecera hacia la caché estática de AuraBox.
func copyFile(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxCoverFileBytes {
		return fmt.Errorf("carátula inválida o fuera de tamaño permitido")
	}

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	var header [12]byte
	n, err := io.ReadFull(in, header[:])
	if err != nil && err != io.ErrUnexpectedEOF {
		return err
	}
	if !isSupportedImageMagic(header[:n]) {
		return fmt.Errorf("formato binario de carátula no reconocido")
	}

	tmpDst := dst + ".tmp"
	out, err := os.OpenFile(tmpDst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		return err
	}

	if _, wErr := out.Write(header[:n]); wErr != nil {
		_ = out.Close()
		_ = os.Remove(tmpDst)
		return wErr
	}

	remainingLimit := int64(maxCoverFileBytes - n)
	if _, copyErr := io.Copy(out, io.LimitReader(in, remainingLimit)); copyErr != nil {
		_ = out.Close()
		_ = os.Remove(tmpDst)
		return copyErr
	}
	if closeErr := out.Close(); closeErr != nil {
		_ = os.Remove(tmpDst)
		return closeErr
	}
	return os.Rename(tmpDst, dst)
}

// detectSourceAndRoot determina la fuente (local/nas/usb) y su directorio raíz para preservar jerarquías de carpetas.
func (ls *LibraryScanner) detectSourceAndRoot(targetPath string) (string, string) {
	if ls.nasDir != "" && strings.HasPrefix(targetPath, ls.nasDir) {
		return "nas", ls.nasDir
	}
	if ls.usbDir != "" && strings.HasPrefix(targetPath, ls.usbDir) {
		return "usb", ls.usbDir
	}
	if ls.localDir != "" && strings.HasPrefix(targetPath, ls.localDir) {
		return "local", ls.localDir
	}
	return "local", ""
}

// ParseAndSaveFile procesa un archivo de audio específico en disco y lo guarda en la base de datos.
func (ls *LibraryScanner) ParseAndSaveFile(filePath string) *db.Track {
	info, err := os.Lstat(filePath)
	if err != nil || info.IsDir() || !info.Mode().IsRegular() {
		return nil
	}

	source, rootDir := ls.detectSourceAndRoot(filePath)
	track := ls.parseTrackMetadata(filePath, rootDir, source, info)
	if track != nil {
		_ = ls.database.UpsertTrack(track)
	}
	return track
}

// ScanFolderTracks recorre una carpeta en disco y extrae todas las pistas de audio encontradas.
func (ls *LibraryScanner) ScanFolderTracks(folderPath string) []db.Track {
	var tracks []db.Track
	source, rootDir := ls.detectSourceAndRoot(folderPath)
	if rootDir == "" {
		rootDir = folderPath
	}

	supportedExts := map[string]bool{
		".mp3": true, ".flac": true, ".ogg": true, ".m4a": true,
		".wav": true, ".aac": true, ".opus": true, ".alac": true, ".wma": true,
	}

	_ = filepath.Walk(folderPath, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() || !info.Mode().IsRegular() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if !supportedExts[ext] {
			return nil
		}
		t := ls.parseTrackMetadata(path, rootDir, source, info)
		if t != nil {
			_ = ls.database.UpsertTrack(t)
			tracks = append(tracks, *t)
		}
		return nil
	})

	// Ordenación natural por número de pista o nombre de archivo (1, 2, ... 9, 10)
	sort.Slice(tracks, func(i, j int) bool {
		if tracks[i].TrackNumber > 0 && tracks[j].TrackNumber > 0 && tracks[i].TrackNumber != tracks[j].TrackNumber {
			return tracks[i].TrackNumber < tracks[j].TrackNumber
		}
		return naturalLess(tracks[i].Filename, tracks[j].Filename)
	})

	return tracks
}

// naturalLess compara dos cadenas considerando fragmentos numéricos de forma natural.
func naturalLess(s1, s2 string) bool {
	i, j := 0, 0
	r1, r2 := []rune(s1), []rune(s2)
	for i < len(r1) && j < len(r2) {
		if unicode.IsDigit(r1[i]) && unicode.IsDigit(r2[j]) {
			start1 := i
			for i < len(r1) && unicode.IsDigit(r1[i]) {
				i++
			}
			start2 := j
			for j < len(r2) && unicode.IsDigit(r2[j]) {
				j++
			}
			n1, _ := strconv.ParseUint(string(r1[start1:i]), 10, 64)
			n2, _ := strconv.ParseUint(string(r2[start2:j]), 10, 64)
			if n1 != n2 {
				return n1 < n2
			}
		} else {
			c1 := unicode.ToLower(r1[i])
			c2 := unicode.ToLower(r2[j])
			if c1 != c2 {
				return c1 < c2
			}
			i++
			j++
		}
	}
	return len(r1) < len(r2)
}
