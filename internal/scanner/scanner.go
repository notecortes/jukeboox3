package scanner

import (
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
	"time"
	"unicode"

	"aurabox/internal/db"
	"aurabox/internal/events"
)

// LibraryScanner analiza las carpetas de música local, NAS y USB e indexa metadatos audiófilos.
type LibraryScanner struct {
	database  *db.DB
	eventsHub *events.Hub
	localDir  string
	nasDir    string
	usbDir    string
	coversDir  string
	scanning   bool
	coverCache map[string]string
	cacheMu    sync.RWMutex
	mu         sync.Mutex
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

// ScanLibraryAll inicia el barrido recursivo de todas las fuentes de almacenamiento configuradas.
func (ls *LibraryScanner) ScanLibraryAll() {
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

		log.Printf("[Scanner] Iniciando análisis integral de biblioteca con extracción de tags y metadatos reales...")
		start := time.Now()

		sources := []struct {
			name string
			path string
		}{
			{"local", ls.localDir},
			{"nas", ls.nasDir},
			{"usb", ls.usbDir},
		}

		totalScanned := 0
		for _, s := range sources {
			if s.path == "" {
				continue
			}
			if _, err := os.Stat(s.path); os.IsNotExist(err) {
				continue
			}
			count := ls.scanDirectory(s.path, s.name)
			totalScanned += count
		}

		elapsed := time.Since(start)
		log.Printf("[Scanner] Análisis finalizado: %d pistas indexadas correctamente en %v", totalScanned, elapsed)

		// Tras escanear, reparar duraciones de pistas pendientes en segundo plano
		ls.RepairDurationsInBackground()

		if ls.eventsHub != nil {
			ls.eventsHub.Broadcast("library_updated", map[string]interface{}{
				"tracks_count": totalScanned,
				"elapsed_ms":   elapsed.Milliseconds(),
			})
		}
	}()
}

// RepairDurationsInBackground recorre en segundo plano las canciones sin duración real calculada y las repara en paralelo.
func (ls *LibraryScanner) RepairDurationsInBackground() {
	go func() {
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

		results := make(chan updateItem, 1000)
		jobs := make(chan db.TrackPathID, 1000)

		// 8 workers concurrentes para procesar en paralelo sin saturar I/O
		var wg sync.WaitGroup
		for w := 0; w < 8; w++ {
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
		batch := make([]db.TrackPropertyUpdate, 0, 500)
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
			if len(batch) >= 500 {
				flush()
			}
		}
		flush()

		elapsed := time.Since(start)
		log.Printf("[Scanner] Finalizado cálculo de duraciones: %d canciones actualizadas en %v.", totalUpdated, elapsed)

		if totalUpdated > 0 && ls.eventsHub != nil {
			ls.eventsHub.Broadcast("library_updated", map[string]interface{}{
				"durations_repaired": totalUpdated,
			})
		}
	}()
}

// scanDirectory recorre un árbol de directorios e indexa cada archivo de audio compatible en lotes atómicos.
func (ls *LibraryScanner) scanDirectory(rootPath, sourceName string) int {
	validExts := map[string]bool{
		".mp3":  true,
		".flac": true,
		".ogg":  true,
		".m4a":  true,
		".wav":  true,
		".opus": true,
	}

	count := 0
	batchSize := 150
	batch := make([]*db.Track, 0, batchSize)

	flushBatch := func() {
		if len(batch) > 0 {
			if err := ls.database.BatchUpsertTracks(batch); err == nil {
				count += len(batch)
			} else {
				log.Printf("[Scanner] Advertencia al persistir lote de %d canciones: %v", len(batch), err)
			}
			batch = batch[:0]
		}
	}

	_ = filepath.Walk(rootPath, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || info.IsDir() {
			return nil
		}

		ext := strings.ToLower(filepath.Ext(path))
		if !validExts[ext] {
			return nil
		}

		track := ls.parseTrackMetadata(path, rootPath, sourceName, info)
		if track != nil {
			batch = append(batch, track)
			if len(batch) >= batchSize {
				flushBatch()
			}
		}
		return nil
	})

	flushBatch()
	return count
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

	// Si aún no tenemos duración, intentar leer propiedades de audio nativas
	if duration <= 0 {
		if dur, br, sr, err := ReadAudioProperties(filePath); err == nil && dur > 0 {
			duration = dur
			if br > 0 {
				bitrate = br
			}
			if sr > 0 {
				sampleRate = sr
			}
		}
	}

	// Si aún no se pudo calcular por stream VBR/CBR, estimar con tamaño del archivo
	if duration <= 0 && info != nil && info.Size() > 0 && bitrate > 0 {
		duration = float64(info.Size()*8) / float64(bitrate*1000)
	}

	// 2. Si faltan metadatos clave, aplicar heurística jerárquica sobre los directorios
	relPath, _ := filepath.Rel(rootPath, filePath)
	segments := strings.Split(filepath.ToSlash(relPath), "/")

	// Reconocer carpetas de categoría raíz (ej: "04.indie", "07.rock", "01.clasica", "02.soundtrack")
	catRegex := regexp.MustCompile(`^(\d{2}\.)?([a-zA-ZáéíóúÁÉÍÓÚñÑ]+)$`)
	isCategory := false
	detectedCategoryGenre := ""

	if len(segments) > 0 {
		m := catRegex.FindStringSubmatch(segments[0])
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

	if artist == "" || strings.HasPrefix(artist, "0") {
		if isCategory && len(segments) >= 3 {
			// /music/nas/04.indie/izal/2018 - Autoterapia/01 - Autoterapia.flac
			artist = cleanArtistName(segments[1])
			if album == "" {
				album = cleanAlbumName(segments[2])
				year = extractYear(segments[2], year)
			}
		} else if isCategory && len(segments) == 2 {
			// /music/nas/05.recopilatorios/RockFM/...
			artist = "Varios Artistas"
			if album == "" {
				album = cleanAlbumName(segments[1])
			}
		} else if len(segments) >= 3 {
			// /music/local/The Beatles/Abbey Road/01 - Come Together.flac
			artist = cleanArtistName(segments[0])
			if album == "" {
				album = cleanAlbumName(segments[1])
				year = extractYear(segments[1], year)
			}
		} else if len(segments) == 2 {
			artist = cleanArtistName(segments[0])
			if album == "" {
				album = "Sencillos"
			}
		} else {
			artist = "Artista Desconocido"
			if album == "" {
				album = "Colección"
			}
		}
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
	reClean := regexp.MustCompile(`(?i)\s*(\[[^\]]*\]|\((?:19|20)\d{2}[^)]*\)|FLAC|Lossless|Hunter|PMEDIA|FRG|320kbps|⭐️|\b\d+Bit[^\s]*)\s*`)
	cleaned := reClean.ReplaceAllString(raw, " ")
	cleaned = strings.TrimSpace(cleaned)

	// Si el álbum empieza por año "2018 - Nombre", quitar el prefijo
	rePrefix := regexp.MustCompile(`^(?:19|20)\d{2}\s*[-–]\s*(.+)`)
	if m := rePrefix.FindStringSubmatch(cleaned); len(m) > 1 {
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
	re := regexp.MustCompile(`\b(19\d{2}|20\d{2})\b`)
	m := re.FindString(name)
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
	reDot := regexp.MustCompile(`^(\d+)\.\s*(.+)`)
	if m := reDot.FindStringSubmatch(base); len(m) > 2 {
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

// findOrCacheCover localiza una imagen de carátula local y la enlaza a la URL estática con caché en memoria.
func (ls *LibraryScanner) findOrCacheCover(dir string) string {
	ls.cacheMu.RLock()
	if cached, ok := ls.coverCache[dir]; ok {
		ls.cacheMu.RUnlock()
		return cached
	}
	ls.cacheMu.RUnlock()

	candidates := []string{
		"cover.jpg", "cover.png", "folder.jpg", "folder.png",
		"front.jpg", "front.png", "Cover.jpg", "Folder.jpg",
		"Front.jpg", "artwork.jpg",
	}

	result := ""
	for _, cand := range candidates {
		candPath := filepath.Join(dir, cand)
		if info, err := os.Stat(candPath); err == nil && !info.IsDir() {
			hash := md5.Sum([]byte(dir))
			hashStr := hex.EncodeToString(hash[:])
			destName := hashStr + filepath.Ext(cand)
			destPath := filepath.Join(ls.coversDir, destName)

			if _, err := os.Stat(destPath); os.IsNotExist(err) {
				_ = copyFile(candPath, destPath)
			}
			result = fmt.Sprintf("/api/covers/%s", destName)
			break
		}
	}

	ls.cacheMu.Lock()
	ls.coverCache[dir] = result
	ls.cacheMu.Unlock()

	return result
}

// copyFile copia un archivo físico de imagen hacia la caché estática de AuraBox.
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()

	_, err = io.Copy(out, in)
	return err
}

// ParseAndSaveFile procesa un archivo de audio específico en disco y lo guarda en la base de datos.
func (ls *LibraryScanner) ParseAndSaveFile(filePath string) *db.Track {
	info, err := os.Stat(filePath)
	if err != nil || info.IsDir() {
		return nil
	}

	source := "local"
	if strings.HasPrefix(filePath, ls.nasDir) {
		source = "nas"
	} else if strings.HasPrefix(filePath, ls.usbDir) {
		source = "usb"
	}

	track := ls.parseTrackMetadata(filePath, "", source, info)
	if track != nil {
		_ = ls.database.UpsertTrack(track)
	}
	return track
}

// ScanFolderTracks recorre una carpeta en disco y extrae todas las pistas de audio encontradas.
func (ls *LibraryScanner) ScanFolderTracks(folderPath string) []db.Track {
	var tracks []db.Track
	source := "local"
	if strings.HasPrefix(folderPath, ls.nasDir) {
		source = "nas"
	} else if strings.HasPrefix(folderPath, ls.usbDir) {
		source = "usb"
	}

	supportedExts := map[string]bool{
		".mp3": true, ".flac": true, ".ogg": true, ".m4a": true,
		".wav": true, ".aac": true, ".opus": true, ".alac": true,
	}

	_ = filepath.Walk(folderPath, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if !supportedExts[ext] {
			return nil
		}
		t := ls.parseTrackMetadata(path, folderPath, source, info)
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
