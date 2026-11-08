package main

import (
	"compress/gzip"
	"encoding/json"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode"

	"aurabox/internal/db"
	"aurabox/internal/events"
	"aurabox/internal/lyrics"
	"aurabox/internal/mpd"
	"aurabox/internal/scanner"
	"aurabox/internal/smartmix"
)

// Server encapsula todas las dependencias y servicios activos de AuraBox.
type Server struct {
	database  *db.DB
	eventsHub *events.Hub
	player    *mpd.Client
	scanner   *scanner.LibraryScanner
	smartmix  *smartmix.SmartMixService
	lyrics    *lyrics.Service
	localDir  string
	nasDir    string
	usbDir    string
	coversDir string
	startTime time.Time
}

// main punto de entrada principal del servicio AuraBox en Linux / Raspberry Pi.
func main() {
	log.Println("==================================================")
	log.Println("           AuraBox - Hi-Fi Audio Jukebox          ")
	log.Println("==================================================")

	port := getEnv("PORT", "4080")
	adminPort := getEnv("ADMIN_PORT", "4081")
	mpdHost := getEnv("MPD_HOST", "localhost")
	mpdPort := getEnv("MPD_PORT", "6600")

	dataDir := getEnv("DATA_DIR", "./data")
	musicLocal := getEnv("MUSIC_LOCAL", "./music/local")
	musicNas := getEnv("MUSIC_NAS", "./music/nas")
	musicUsb := getEnv("MUSIC_USB", "./music/usb")
	dbPath := filepath.Join(dataDir, "aurabox.db")
	coversDir := filepath.Join(dataDir, "covers")

	_ = os.MkdirAll(dataDir, 0755)
	_ = os.MkdirAll(coversDir, 0755)

	// Inicializar SQLite
	database, err := db.InitDB(dbPath)
	if err != nil {
		log.Fatalf("[Error] No se pudo inicializar la base de datos SQLite: %v", err)
	}
	defer database.Close()
	log.Printf("[DB] Base de datos SQLite lista en: %s", dbPath)

	// Inicializar hub de eventos SSE en tiempo real
	hub := events.NewHub()

	// Inicializar controlador de audio MPD / ALSA
	player := mpd.NewClient(mpdHost, mpdPort, hub, database)

	// Inicializar escáner de biblioteca musical
	libScanner := scanner.NewLibraryScanner(database, hub, musicLocal, musicNas, musicUsb, coversDir)

	// Inicializar motor Smart Mix
	smService := smartmix.NewSmartMixService(database)

	// Inicializar servicio de letras LRCLIB
	lyrService := lyrics.NewService()

	srv := &Server{
		database:  database,
		eventsHub: hub,
		player:    player,
		scanner:   libScanner,
		smartmix:  smService,
		lyrics:    lyrService,
		localDir:  musicLocal,
		nasDir:    musicNas,
		usbDir:    musicUsb,
		coversDir: coversDir,
		startTime: time.Now(),
	}

	// Ejecutar escaneo inicial y reparación de duraciones en segundo plano
	libScanner.ScanLibraryAll()
	libScanner.RepairDurationsInBackground()

	// Enrutador HTTP principal
	mux := http.NewServeMux()
	srv.registerRoutes(mux)

	// Servidor Web principal en puerto 4080 con compresión Gzip, cabeceras seguras y soporte de streaming continuo
	httpServer := &http.Server{
		Addr:              ":" + port,
		Handler:           corsMiddleware(compressionAndSecurityMiddleware(mux)),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
		WriteTimeout:      0, // Sin corte abrupto para permitir transmisión SSE continua y streaming de audio
	}

	// Servidor secundario de métricas y administración en puerto 4081
	adminMux := http.NewServeMux()
	adminMux.HandleFunc("/health", srv.handleHealth)
	adminMux.HandleFunc("/metrics", srv.handleMetrics)
	adminServer := &http.Server{
		Addr:              ":" + adminPort,
		Handler:           adminMux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		log.Printf("[HTTP] Servidor de métricas y administración escuchando en http://0.0.0.0:%s", adminPort)
		if err := adminServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("[Admin] Advertencia servidor admin: %v", err)
		}
	}()

	go func() {
		log.Printf("[HTTP] AuraBox Web & API escuchando en http://0.0.0.0:%s", port)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("[HTTP] Error fatal al escuchar en puerto %s: %v", port, err)
		}
	}()

	// Captura de señales para apagado elegante
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	log.Println("[Sistema] Deteniendo AuraBox de manera ordenada...")
	_ = httpServer.Close()
	_ = adminServer.Close()
}

// registerRoutes configura las rutas REST, SSE y recursos estáticos.
func (s *Server) registerRoutes(mux *http.ServeMux) {
	// 1. Archivos estáticos del frontend PWA (HTML, CSS, JS, manifest)
	fs := http.FileServer(http.Dir("./web"))
	mux.Handle("/", noCacheMiddleware(fs))

	// 2. Caché de carátulas locales con cabeceras de persistencia inmutables (30 días)
	mux.Handle("/api/covers/", coversCacheMiddleware(http.StripPrefix("/api/covers/", http.FileServer(http.Dir(s.coversDir)))))

	// 3. Flujo en tiempo real Server-Sent Events (SSE)
	mux.HandleFunc("/api/events", s.eventsHub.ServeHTTP)

	// 4. API del Reproductor
	mux.HandleFunc("/api/status", s.handleStatus)
	mux.HandleFunc("/api/player/play", s.handlePlay)
	mux.HandleFunc("/api/player/pause", s.handlePause)
	mux.HandleFunc("/api/player/toggle", s.handleToggle)
	mux.HandleFunc("/api/player/next", s.handleNext)
	mux.HandleFunc("/api/player/prev", s.handlePrev)
	mux.HandleFunc("/api/player/seek", s.handleSeek)
	mux.HandleFunc("/api/player/volume", s.handleVolume)
	mux.HandleFunc("/api/player/volume/step", s.handleVolumeStep)
	mux.HandleFunc("/api/player/volume/max", s.handleVolumeMax)
	mux.HandleFunc("/api/player/output", s.handleSelectOutput)
	mux.HandleFunc("/api/player/config", s.handleAudioConfig)
	mux.HandleFunc("/api/player/sleeptimer", s.handleSleepTimer)

	// 5. API de Cola (Jukebox Queue)
	mux.HandleFunc("/api/queue", s.handleQueue)
	mux.HandleFunc("/api/queue/add", s.handleQueueAdd)
	mux.HandleFunc("/api/queue/move", s.handleQueueMove)
	mux.HandleFunc("/api/queue/remove", s.handleQueueRemove)
	mux.HandleFunc("/api/queue/clear", s.handleQueueClear)
	mux.HandleFunc("/api/queue/shuffle", s.handleQueueShuffle)

	// 6. API de Biblioteca
	mux.HandleFunc("/api/library/tracks", s.handleLibraryTracks)
	mux.HandleFunc("/api/library/recent", s.handleLibraryRecent)
	mux.HandleFunc("/api/library/albums", s.handleLibraryAlbums)
	mux.HandleFunc("/api/library/album", s.handleLibraryAlbumDetail)
	mux.HandleFunc("/api/library/artists", s.handleLibraryArtists)
	mux.HandleFunc("/api/library/artist/tracks", s.handleLibraryArtistTracks)
	mux.HandleFunc("/api/library/folders", s.handleLibraryFolders)
	mux.HandleFunc("/api/library/scan", s.handleLibraryScan)
	mux.HandleFunc("/api/library/favorite", s.handleFavoriteToggle)
	mux.HandleFunc("/api/library/favorites", s.handleGetFavorites)
	mux.HandleFunc("/api/library/favorites/play", s.handlePlayFavorites)
	mux.HandleFunc("/api/search", s.handleSearch)

	// 7. API de Smart Mix
	mux.HandleFunc("/api/smartmix", s.handleSmartMix)

	// 8. API de Letras LRCLIB
	mux.HandleFunc("/api/lyrics", s.handleLyrics)

	// 9. Emisoras de Radio, Stream y Ajustes
	mux.HandleFunc("/api/stream", s.handleAudioStream)
	mux.HandleFunc("/api/radios", s.handleRadios)
	mux.HandleFunc("/api/radios/play", s.handleRadioPlay)
	mux.HandleFunc("/api/settings", s.handleSettings)
	mux.HandleFunc("/api/settings/nas", s.handleNASConfig)
}

// noCacheMiddleware previene que el navegador almacene en caché HTML, JS o CSS desactualizados.
func noCacheMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache, no-store, must-revalidate")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
		next.ServeHTTP(w, r)
	})
}

// corsMiddleware permite solicitudes seguras desde navegadores en red local.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		if r.Method == "OPTIONS" {
			w.WriteHeader(http.StatusOK)
			return
		}
		next.ServeHTTP(w, r)
	})
}

var gzipPool = sync.Pool{
	New: func() interface{} {
		w, _ := gzip.NewWriterLevel(nil, gzip.BestSpeed)
		return w
	},
}

type gzipResponseWriter struct {
	http.ResponseWriter
	writer *gzip.Writer
}

func (g *gzipResponseWriter) Write(b []byte) (int, error) {
	return g.writer.Write(b)
}

func (g *gzipResponseWriter) Flush() {
	_ = g.writer.Flush()
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// compressionAndSecurityMiddleware comprime respuestas y añade cabeceras HTTP de seguridad.
func compressionAndSecurityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Endurecimiento con cabeceras de seguridad
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		w.Header().Set("X-XSS-Protection", "1; mode=block")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")

		// No comprimir SSE ni transmisión de audio en tiempo real
		if r.URL.Path == "/api/events" || r.URL.Path == "/api/stream" {
			next.ServeHTTP(w, r)
			return
		}

		if !strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}

		gz := gzipPool.Get().(*gzip.Writer)
		defer gzipPool.Put(gz)
		gz.Reset(w)
		defer gz.Close()

		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Del("Content-Length")

		gzw := &gzipResponseWriter{ResponseWriter: w, writer: gz}
		next.ServeHTTP(gzw, r)
	})
}

// coversCacheMiddleware aplica cabeceras de caché persistente (30 días) para carátulas.
func coversCacheMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=2592000, immutable")
		next.ServeHTTP(w, r)
	})
}

// isSafeMusicPath asegura que una ruta de archivo pertenezca a los directorios de música autorizados.
func (s *Server) isSafeMusicPath(p string) bool {
	if p == "" {
		return false
	}
	if strings.HasPrefix(p, "http://") || strings.HasPrefix(p, "https://") {
		return true
	}
	clean := filepath.Clean(p)
	return strings.HasPrefix(clean, s.localDir) ||
		strings.HasPrefix(clean, s.nasDir) ||
		strings.HasPrefix(clean, s.usbDir)
}

// jsonResponse serializa la estructura Go a JSON con el código HTTP correspondiente.
func jsonResponse(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

// handleStatus devuelve el estado completo del reproductor.
func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, http.StatusOK, s.player.GetState())
}

// handlePlay reanuda la reproducción.
func (s *Server) handlePlay(w http.ResponseWriter, r *http.Request) {
	s.player.Play()
	jsonResponse(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handlePause pausa la reproducción.
func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	s.player.Pause()
	jsonResponse(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleToggle conmuta entre play y pausa.
func (s *Server) handleToggle(w http.ResponseWriter, r *http.Request) {
	s.player.Toggle()
	jsonResponse(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleNext avanza a la pista siguiente.
func (s *Server) handleNext(w http.ResponseWriter, r *http.Request) {
	s.player.Next()
	jsonResponse(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handlePrev retrocede a la pista anterior.
func (s *Server) handlePrev(w http.ResponseWriter, r *http.Request) {
	s.player.Prev()
	jsonResponse(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleSeek cambia la posición actual en segundos.
func (s *Server) handleSeek(w http.ResponseWriter, r *http.Request) {
	secStr := r.URL.Query().Get("seconds")
	sec, err := strconv.ParseFloat(secStr, 64)
	if err != nil {
		http.Error(w, "parámetro seconds inválido", http.StatusBadRequest)
		return
	}
	s.player.Seek(sec)
	jsonResponse(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleVolume ajusta el volumen general con salvaguarda acústica.
func (s *Server) handleVolume(w http.ResponseWriter, r *http.Request) {
	valStr := r.URL.Query().Get("value")
	val, err := strconv.Atoi(valStr)
	if err != nil {
		http.Error(w, "parámetro value inválido", http.StatusBadRequest)
		return
	}
	s.player.SetVolume(val)
	jsonResponse(w, http.StatusOK, s.player.GetState())
}

// handleVolumeStep incrementa o decrementa el volumen por pasos fijos (+/- 5%).
func (s *Server) handleVolumeStep(w http.ResponseWriter, r *http.Request) {
	deltaStr := r.URL.Query().Get("delta")
	delta, err := strconv.Atoi(deltaStr)
	if err != nil {
		delta = 5
	}
	s.player.StepVolume(delta)
	jsonResponse(w, http.StatusOK, s.player.GetState())
}

// handleVolumeMax configura el tope de protección auditiva.
func (s *Server) handleVolumeMax(w http.ResponseWriter, r *http.Request) {
	limitStr := r.URL.Query().Get("limit")
	limit, err := strconv.Atoi(limitStr)
	if err != nil {
		http.Error(w, "parámetro limit inválido", http.StatusBadRequest)
		return
	}
	s.player.SetMaxVolumeLimit(limit)
	jsonResponse(w, http.StatusOK, s.player.GetState())
}

// handleSelectOutput conmuta la salida de audio (Jack / USB / I2S / Stream).
func (s *Server) handleSelectOutput(w http.ResponseWriter, r *http.Request) {
	idStr := r.URL.Query().Get("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		http.Error(w, "parámetro id inválido", http.StatusBadRequest)
		return
	}
	_ = s.player.SelectAudioOutput(id)
	jsonResponse(w, http.StatusOK, s.player.GetState())
}

// handleAudioConfig ajusta ReplayGain y Crossfade en MPD.
func (s *Server) handleAudioConfig(w http.ResponseWriter, r *http.Request) {
	replayGain := r.URL.Query().Get("replay_gain")
	crossfadeStr := r.URL.Query().Get("crossfade")
	cf, _ := strconv.Atoi(crossfadeStr)
	if replayGain == "" {
		replayGain = "auto"
	}
	s.player.SetAudioConfig(replayGain, cf)
	jsonResponse(w, http.StatusOK, s.player.GetState())
}

// handleSleepTimer programa el temporizador de apagado.
func (s *Server) handleSleepTimer(w http.ResponseWriter, r *http.Request) {
	minStr := r.URL.Query().Get("minutes")
	min, _ := strconv.Atoi(minStr)
	s.player.StartSleepTimer(min)
	jsonResponse(w, http.StatusOK, s.player.GetState())
}

// handleQueue devuelve la lista de pistas encoladas.
func (s *Server) handleQueue(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"queue":  s.player.GetQueue(),
		"state":  s.player.GetState(),
	})
}

// handleQueueAdd añade canciones a la cola según el modo ("play_now", "play_next", "append", "replace").
func (s *Server) handleQueueAdd(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		TrackIDs   []int64  `json:"track_ids"`
		AlbumID    int64    `json:"album_id"`
		AlbumIDs   []int64  `json:"album_ids"`
		Artist     string   `json:"artist"`
		Folder     string   `json:"folder"`
		Path       string   `json:"path"`
		FilePaths  []string `json:"file_paths"`
		Shuffle    bool     `json:"shuffle"`
		ClearFirst bool     `json:"clear_first"`
		Mode       string   `json:"mode"` // play_now, play_next, append, replace
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "cuerpo json inválido", http.StatusBadRequest)
		return
	}

	var tracksToEnqueue []db.Track

	if len(payload.TrackIDs) > 0 {
		tracks, err := s.database.GetTracksByIDs(payload.TrackIDs)
		if err == nil {
			tracksToEnqueue = append(tracksToEnqueue, tracks...)
		}
	} else if len(payload.AlbumIDs) > 0 {
		for _, albID := range payload.AlbumIDs {
			_, albTracks, err := s.database.GetAlbumByID(albID)
			if err == nil {
				tracksToEnqueue = append(tracksToEnqueue, albTracks...)
			}
		}
	} else if payload.AlbumID > 0 {
		_, albTracks, err := s.database.GetAlbumByID(payload.AlbumID)
		if err == nil {
			tracksToEnqueue = albTracks
		}
	} else if payload.Artist != "" {
		artTracks, err := s.database.GetTracksByArtist(payload.Artist)
		if err == nil {
			tracksToEnqueue = artTracks
		}
	} else if payload.Folder != "" {
		if !s.isSafeMusicPath(payload.Folder) {
			http.Error(w, "acceso denegado: carpeta no permitida", http.StatusForbidden)
			return
		}
		folderTracks, err := s.database.GetTracksByPathPrefix(payload.Folder)
		if err == nil && len(folderTracks) > 0 {
			tracksToEnqueue = folderTracks
		} else {
			// Si no estaban aún en la base de datos, escanear la carpeta en disco
			tracksToEnqueue = s.scanner.ScanFolderTracks(payload.Folder)
		}
	} else if payload.Path != "" {
		if !s.isSafeMusicPath(payload.Path) {
			http.Error(w, "acceso denegado: ruta no permitida", http.StatusForbidden)
			return
		}
		fi, err := os.Stat(payload.Path)
		if err == nil && fi.IsDir() {
			folderTracks, err := s.database.GetTracksByPathPrefix(payload.Path)
			if err == nil && len(folderTracks) > 0 {
				tracksToEnqueue = folderTracks
			} else {
				tracksToEnqueue = s.scanner.ScanFolderTracks(payload.Path)
			}
		} else {
			// Es un archivo individual
			track, err := s.database.GetTrackByPath(payload.Path)
			if err == nil && track != nil {
				tracksToEnqueue = append(tracksToEnqueue, *track)
			} else {
				parsed := s.scanner.ParseAndSaveFile(payload.Path)
				if parsed != nil {
					tracksToEnqueue = append(tracksToEnqueue, *parsed)
				}
			}
		}
	} else if len(payload.FilePaths) > 0 {
		for _, fp := range payload.FilePaths {
			if !s.isSafeMusicPath(fp) {
				continue
			}
			track, err := s.database.GetTrackByPath(fp)
			if err == nil && track != nil {
				tracksToEnqueue = append(tracksToEnqueue, *track)
			} else {
				parsed := s.scanner.ParseAndSaveFile(fp)
				if parsed != nil {
					tracksToEnqueue = append(tracksToEnqueue, *parsed)
				}
			}
		}
	}

	if len(tracksToEnqueue) == 0 {
		http.Error(w, "no se encontraron pistas para encolar", http.StatusNotFound)
		return
	}

	// Barajado opcional de pistas
	if payload.Shuffle && len(tracksToEnqueue) > 1 {
		rnd := rand.New(rand.NewSource(time.Now().UnixNano()))
		rnd.Shuffle(len(tracksToEnqueue), func(i, j int) {
			tracksToEnqueue[i], tracksToEnqueue[j] = tracksToEnqueue[j], tracksToEnqueue[i]
		})
	}

	// Limpieza previa si se especificó modo de reemplazo o ClearFirst
	if payload.ClearFirst || payload.Mode == "replace" {
		s.player.ClearQueue()
		s.player.EnqueueTracks(tracksToEnqueue, "play_now")
	} else {
		s.player.EnqueueTracks(tracksToEnqueue, payload.Mode)
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"status":   "ok",
		"enqueued": len(tracksToEnqueue),
	})
}

// handleQueueMove mueve una pista dentro de la cola (Drag & Drop táctil).
func (s *Server) handleQueueMove(w http.ResponseWriter, r *http.Request) {
	fromStr := r.URL.Query().Get("from")
	toStr := r.URL.Query().Get("to")
	from, err1 := strconv.Atoi(fromStr)
	to, err2 := strconv.Atoi(toStr)
	if err1 != nil || err2 != nil {
		http.Error(w, "parámetros from/to inválidos", http.StatusBadRequest)
		return
	}
	s.player.MoveInQueue(from, to)
	jsonResponse(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleQueueRemove elimina una canción de la cola (Swipe-to-delete).
func (s *Server) handleQueueRemove(w http.ResponseWriter, r *http.Request) {
	idxStr := r.URL.Query().Get("index")
	idx, err := strconv.Atoi(idxStr)
	if err != nil {
		http.Error(w, "parámetro index inválido", http.StatusBadRequest)
		return
	}
	s.player.RemoveFromQueue(idx)
	jsonResponse(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleQueueClear vacía la cola.
func (s *Server) handleQueueClear(w http.ResponseWriter, r *http.Request) {
	s.player.ClearQueue()
	jsonResponse(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleQueueShuffle baraja la lista evitando que el mismo artista suene dos veces seguidas.
func (s *Server) handleQueueShuffle(w http.ResponseWriter, r *http.Request) {
	s.player.AntiRepetitionShuffle()
	jsonResponse(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handleLibraryTracks devuelve todas las pistas registradas.
func (s *Server) handleLibraryTracks(w http.ResponseWriter, r *http.Request) {
	tracks, err := s.database.GetAllTracks()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, http.StatusOK, tracks)
}

// handleLibraryRecent devuelve las canciones más recientemente indexadas.
func (s *Server) handleLibraryRecent(w http.ResponseWriter, r *http.Request) {
	limitStr := r.URL.Query().Get("limit")
	limit, _ := strconv.Atoi(limitStr)
	if limit <= 0 {
		limit = 30
	}
	tracks, err := s.database.GetRecentTracks(limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, http.StatusOK, tracks)
}

// handleLibraryAlbums devuelve todos los álbumes.
func (s *Server) handleLibraryAlbums(w http.ResponseWriter, r *http.Request) {
	albums, err := s.database.GetAlbums()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, http.StatusOK, albums)
}

// handleLibraryAlbumDetail devuelve información de un álbum y sus temas.
func (s *Server) handleLibraryAlbumDetail(w http.ResponseWriter, r *http.Request) {
	idStr := r.URL.Query().Get("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "parámetro id inválido", http.StatusBadRequest)
		return
	}
	album, tracks, err := s.database.GetAlbumByID(id)
	if err != nil {
		http.Error(w, "álbum no encontrado", http.StatusNotFound)
		return
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"album":  album,
		"tracks": tracks,
	})
}

// handleLibraryArtists devuelve todos los artistas con sus totales.
func (s *Server) handleLibraryArtists(w http.ResponseWriter, r *http.Request) {
	artists, err := s.database.GetArtists()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, http.StatusOK, artists)
}

// handleLibraryArtistTracks devuelve las canciones de un artista.
func (s *Server) handleLibraryArtistTracks(w http.ResponseWriter, r *http.Request) {
	artist := r.URL.Query().Get("name")
	if artist == "" {
		http.Error(w, "parámetro name requerido", http.StatusBadRequest)
		return
	}
	tracks, err := s.database.GetTracksByArtist(artist)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, http.StatusOK, tracks)
}

// handleLibraryFolders explora el almacenamiento por carpetas (Local, NAS, USB), omitiendo archivos no musicales.
func (s *Server) handleLibraryFolders(w http.ResponseWriter, r *http.Request) {
	subpath := r.URL.Query().Get("path")
	if subpath == "" {
		// Mostrar raíces disponibles
		roots := []map[string]interface{}{
			{"name": "Música Local (/music/local)", "path": s.localDir, "source": "local", "is_dir": true},
			{"name": "Almacenamiento en Red NAS (/music/nas)", "path": s.nasDir, "source": "nas", "is_dir": true},
			{"name": "Unidades USB (/music/usb)", "path": s.usbDir, "source": "usb", "is_dir": true},
		}
		jsonResponse(w, http.StatusOK, map[string]interface{}{"is_root": true, "items": roots})
		return
	}

	cleanSubpath := filepath.Clean(subpath)
	if !s.isSafeMusicPath(cleanSubpath) {
		http.Error(w, "acceso denegado: ruta no permitida", http.StatusForbidden)
		return
	}

	entries, err := os.ReadDir(cleanSubpath)
	if err != nil {
		http.Error(w, "no se pudo leer el directorio", http.StatusInternalServerError)
		return
	}

	var dirItems []map[string]interface{}
	var fileItems []map[string]interface{}

	audioExts := map[string]bool{
		".mp3": true, ".flac": true, ".ogg": true, ".m4a": true,
		".wav": true, ".aac": true, ".opus": true, ".alac": true, ".wma": true,
	}

	for _, e := range entries {
		name := e.Name()
		// Omitir archivos y carpetas ocultas del sistema (ej: .directory, .folder.png, .DS_Store)
		if strings.HasPrefix(name, ".") {
			continue
		}
		full := filepath.Join(subpath, name)
		if e.IsDir() {
			dirItems = append(dirItems, map[string]interface{}{
				"name":   name,
				"path":   full,
				"is_dir": true,
			})
		} else {
			ext := strings.ToLower(filepath.Ext(name))
			if audioExts[ext] {
				fileItems = append(fileItems, map[string]interface{}{
					"name":   name,
					"path":   full,
					"is_dir": false,
				})
			}
		}
	}

	sort.Slice(dirItems, func(i, j int) bool {
		return naturalLess(dirItems[i]["name"].(string), dirItems[j]["name"].(string))
	})
	sort.Slice(fileItems, func(i, j int) bool {
		return naturalLess(fileItems[i]["name"].(string), fileItems[j]["name"].(string))
	})

	items := append(dirItems, fileItems...)
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"is_root": false,
		"path":    subpath,
		"items":   items,
	})
}

// naturalLess implementa ordenación natural sensible a números (1, 2, 9, 10 en vez de 1, 10, 2).
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

// handleLibraryScan inicia un nuevo barrido de metadatos en segundo plano.
func (s *Server) handleLibraryScan(w http.ResponseWriter, r *http.Request) {
	s.scanner.ScanLibraryAll()
	jsonResponse(w, http.StatusOK, map[string]string{
		"status":  "scanning",
		"message": "Escaneo de biblioteca iniciado en segundo plano",
	})
}

// handleFavoriteToggle alterna el estado de favorito de una canción.
func (s *Server) handleFavoriteToggle(w http.ResponseWriter, r *http.Request) {
	idStr := r.URL.Query().Get("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		http.Error(w, "parámetro id inválido", http.StatusBadRequest)
		return
	}
	isFav, err := s.database.ToggleFavorite(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"id":       id,
		"favorite": isFav,
	})
}

// handleGetFavorites devuelve la lista de canciones marcadas como favoritas por el usuario.
func (s *Server) handleGetFavorites(w http.ResponseWriter, r *http.Request) {
	tracks, err := s.database.GetFavorites()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if tracks == nil {
		tracks = []db.Track{}
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"tracks": tracks,
		"count":  len(tracks),
	})
}

// handlePlayFavorites encola o reproduce inmediatamente la colección completa de favoritos.
func (s *Server) handlePlayFavorites(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Mode    string `json:"mode"`    // "play_now" o "append"
		Shuffle bool   `json:"shuffle"` // true para barajar aleatoriamente
	}
	if r.Body != nil {
		_ = json.NewDecoder(r.Body).Decode(&payload)
	}
	if payload.Mode == "" {
		payload.Mode = "play_now"
	}

	tracks, err := s.database.GetFavorites()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if len(tracks) == 0 {
		http.Error(w, "no hay canciones favoritas guardadas", http.StatusBadRequest)
		return
	}

	if payload.Shuffle {
		r := rand.New(rand.NewSource(time.Now().UnixNano()))
		r.Shuffle(len(tracks), func(i, j int) {
			tracks[i], tracks[j] = tracks[j], tracks[i]
		})
	}

	if payload.Mode == "play_now" {
		s.player.ClearQueue()
		s.player.EnqueueTracks(tracks, "append")
	} else {
		s.player.EnqueueTracks(tracks, "append")
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"status":  "ok",
		"count":   len(tracks),
		"mode":    payload.Mode,
		"shuffle": payload.Shuffle,
	})
}

// handleSearch busca en tiempo real pistas, álbumes y artistas.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if strings.TrimSpace(q) == "" {
		jsonResponse(w, http.StatusOK, map[string]interface{}{
			"tracks":  []db.Track{},
			"albums":  []db.Album{},
			"artists": []db.Artist{},
		})
		return
	}
	tracks, albums, artists, err := s.database.Search(q)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"tracks":  tracks,
		"albums":  albums,
		"artists": artists,
	})
}

// handleSmartMix genera una sesión Smart Mix de un artista y opcionalmente inicia su reproducción.
func (s *Server) handleSmartMix(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Artist  string `json:"artist"`
		Count   int    `json:"count"`
		PlayNow bool   `json:"play_now"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "json inválido", http.StatusBadRequest)
		return
	}

	if payload.Artist == "" {
		http.Error(w, "campo artist es obligatorio", http.StatusBadRequest)
		return
	}
	if payload.Count <= 0 {
		payload.Count = 20
	}

	mixTracks, similarArtists, err := s.smartmix.GenerateMix(payload.Artist, payload.Count)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if payload.PlayNow && len(mixTracks) > 0 {
		s.player.ClearQueue()
		s.player.EnqueueTracks(mixTracks, "append")
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"seed_artist":     payload.Artist,
		"similar_artists": similarArtists,
		"tracks":          mixTracks,
		"count":           len(mixTracks),
	})
}

// handleLyrics devuelve letras sincronizadas (LRCLIB) para la vista Now Playing.
func (s *Server) handleLyrics(w http.ResponseWriter, r *http.Request) {
	title := r.URL.Query().Get("title")
	artist := r.URL.Query().Get("artist")
	album := r.URL.Query().Get("album")
	durStr := r.URL.Query().Get("duration")
	audioPath := r.URL.Query().Get("path")
	if audioPath != "" && !s.isSafeMusicPath(audioPath) {
		audioPath = "" // Prevenir path traversal en búsqueda de letras
	}
	dur, _ := strconv.ParseFloat(durStr, 64)

	if title == "" || artist == "" {
		http.Error(w, "title y artist requeridos", http.StatusBadRequest)
		return
	}

	res, err := s.lyrics.GetLyrics(title, artist, album, dur, audioPath)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, http.StatusOK, res)
}

// handleRadios devuelve la lista de emisoras de streaming configuradas.
func (s *Server) handleRadios(w http.ResponseWriter, r *http.Request) {
	radios, err := s.database.GetRadios()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	jsonResponse(w, http.StatusOK, radios)
}

// handleRadioPlay encola y comienza la reproducción de un flujo web de radio.
func (s *Server) handleRadioPlay(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Name        string `json:"name"`
		StreamURL   string `json:"stream_url"`
		LogoURL     string `json:"logo_url"`
		Genre       string `json:"genre"`
		Broadcaster string `json:"broadcaster"`
		Country     string `json:"country"`
		Bitrate     string `json:"bitrate"`
		Format      string `json:"format"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, "json inválido", http.StatusBadRequest)
		return
	}

	artistInfo := payload.Broadcaster
	if payload.Country != "" {
		if artistInfo != "" {
			artistInfo += " (" + payload.Country + ")"
		} else {
			artistInfo = payload.Country
		}
	}
	if artistInfo == "" {
		artistInfo = "Radio en Directo"
	}

	bitrateVal := 128
	if b, err := strconv.Atoi(payload.Bitrate); err == nil && b > 0 {
		bitrateVal = b
	}

	formatVal := payload.Format
	if formatVal == "" {
		formatVal = "STREAM"
	}

	coverVal := payload.LogoURL
	if coverVal == "" {
		coverVal = "/assets/default-cover.svg"
	}

	radioTrack := db.Track{
		ID:         -1,
		Path:       payload.StreamURL,
		Filename:   payload.Name,
		Title:      payload.Name,
		Artist:     artistInfo,
		Album:      payload.Genre,
		Genre:      payload.Genre,
		Bitrate:    bitrateVal,
		Format:     formatVal,
		CoverURL:   coverVal,
		Duration:   0,
	}

	s.player.ClearQueue()
	s.player.EnqueueTracks([]db.Track{radioTrack}, "append")
	s.player.Play()

	jsonResponse(w, http.StatusOK, map[string]string{"status": "playing_radio"})
}

// handleSettings lee o actualiza configuraciones del sistema.
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		var payload map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, "json inválido", http.StatusBadRequest)
			return
		}
		for k, v := range payload {
			_ = s.database.SetSetting(k, v)
		}
		jsonResponse(w, http.StatusOK, map[string]string{"status": "updated"})
		return
	}

	// GET
	jsonResponse(w, http.StatusOK, map[string]string{
		"max_volume_limit": s.database.GetSetting("max_volume_limit", "85"),
		"player_volume":    s.database.GetSetting("player_volume", "65"),
		"replay_gain":      s.database.GetSetting("replay_gain", "auto"),
		"crossfade":        s.database.GetSetting("crossfade", "2"),
	})
}

// handleHealth proporciona un punto de comprobación de salud en el puerto 4081.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"status":  "healthy",
		"service": "AuraBox Core",
		"uptime":  time.Since(s.startTime).String(),
	})
}

// handleMetrics entrega métricas ligeras para supervisión.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	st := s.player.GetState()
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"player_status":  st.Status,
		"queue_length":   st.QueueLength,
		"current_volume": st.Volume,
		"uptime_seconds": int(time.Since(s.startTime).Seconds()),
	})
}

// handleAudioStream retransmite el flujo de audio HTTP desde MPD hacia el navegador web.
func (s *Server) handleAudioStream(w http.ResponseWriter, r *http.Request) {
	mpdHost := getEnv("MPD_HOST", "localhost")
	streamURL := fmt.Sprintf("http://%s:4082/", mpdHost)

	req, err := http.NewRequestWithContext(r.Context(), "GET", streamURL, nil)
	if err != nil {
		http.Error(w, "error al preparar stream", http.StatusInternalServerError)
		return
	}

	client := &http.Client{Timeout: 0}
	resp, err := client.Do(req)
	if err != nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("El stream de audio no está activo actualmente. Inicia una pista en AuraBox para escuchar."))
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("Connection", "close")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	flusher, canFlush := w.(http.Flusher)
	buf := make([]byte, 4096)
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			_, writeErr := w.Write(buf[:n])
			if writeErr != nil {
				return
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if readErr != nil {
			return
		}
	}
}

// handleNASConfig gestiona la consulta y guardado de credenciales del recurso de red NAS.
func (s *Server) handleNASConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == "POST" {
		var payload struct {
			Alias    string `json:"alias"`
			Host     string `json:"host"`
			Share    string `json:"share"`
			Username string `json:"username"`
			Password string `json:"password"`
			Options  string `json:"options"`
		}
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			http.Error(w, "json inválido", http.StatusBadRequest)
			return
		}

		if payload.Alias != "" {
			_ = s.database.SetSetting("nas_alias", payload.Alias)
		}
		if payload.Host != "" {
			_ = s.database.SetSetting("nas_host", payload.Host)
		}
		if payload.Share != "" {
			_ = s.database.SetSetting("nas_share", payload.Share)
		}
		if payload.Username != "" {
			_ = s.database.SetSetting("nas_user", payload.Username)
		}
		if payload.Password != "" {
			_ = s.database.SetSetting("nas_pass", payload.Password)
		}
		if payload.Options != "" {
			_ = s.database.SetSetting("nas_options", payload.Options)
		}

		// Disparar re-escaneo reactivo
		s.scanner.ScanLibraryAll()

		jsonResponse(w, http.StatusOK, map[string]string{
			"status":  "ok",
			"message": "Configuración NAS guardada con éxito. Re-escaneo iniciado.",
		})
		return
	}

	// GET
	nasHost := s.database.GetSetting("nas_host", getEnv("NAS_HOST", ""))
	nasShare := s.database.GetSetting("nas_share", getEnv("NAS_SHARE", "music"))
	nasUser := s.database.GetSetting("nas_user", getEnv("NAS_USER", ""))
	nasAlias := s.database.GetSetting("nas_alias", getEnv("NAS_ALIAS", "mi-nas"))
	nasOpts := s.database.GetSetting("nas_options", getEnv("NAS_OPTIONS", "vers=3.0,sec=ntlmssp,ro,iocharset=utf8"))

	// Verificar si la carpeta /music/nas contiene archivos reales
	mounted := false
	var dirCount int
	if entries, err := os.ReadDir(s.nasDir); err == nil && len(entries) > 0 {
		mounted = true
		dirCount = len(entries)
	}

	pathDisplay := "Sin configurar (.env)"
	if nasHost != "" {
		pathDisplay = fmt.Sprintf("\\\\%s\\%s\\", nasHost, nasShare)
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"alias":       nasAlias,
		"host":        nasHost,
		"share":       nasShare,
		"username":    nasUser,
		"options":     nasOpts,
		"mounted":     mounted,
		"items_found": dirCount,
		"path":        pathDisplay,
	})
}

// getEnv obtiene una variable de entorno o recurre a un valor por defecto.
func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}
