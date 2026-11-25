package main

import (
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/jpeg"
	_ "image/png"
	"io"
	"log"
	"math/rand"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode"

	"aurabox/internal/covers"
	"aurabox/internal/db"
	"aurabox/internal/events"
	"aurabox/internal/lyrics"
	"aurabox/internal/mpd"
	"aurabox/internal/podcasts"
	"aurabox/internal/scanner"
	"aurabox/internal/smartmix"
	"aurabox/internal/tlsutil"
)

// Server encapsula todas las dependencias y servicios activos de AuraBox.
type Server struct {
	database          *db.DB
	eventsHub         *events.Hub
	player            *mpd.Client
	scanner           *scanner.LibraryScanner
	smartmix          *smartmix.SmartMixService
	lyrics            *lyrics.Service
	covers            *covers.Service
	certManager       *tlsutil.CertManager
	streamClient      *http.Client
	httpClient        *http.Client
	streamUpstreamURL string
	localDir        string
	nasDir          string
	usbDir          string
	coversDir       string
	httpsPort       string
	httpsPublicPort string
	forceHTTPS      bool
	startTime       time.Time
	thumbOnce       sync.Once
	thumbSem        chan struct{}
	thumbMu         sync.Mutex
	thumbFlight     map[string]chan struct{}
	prewarmRunning  atomic.Bool
}

// newAudioStreamClient crea un cliente HTTP reutilizable para el proxy de stream MPD con timeouts seguros de conexión.
func newAudioStreamClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: (&net.Dialer{
				Timeout:   4 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
			ResponseHeaderTimeout: 6 * time.Second,
			IdleConnTimeout:       60 * time.Second,
		},
		Timeout: 0,
	}
}

// main punto de entrada principal del servicio AuraBox en Linux / Raspberry Pi.
func main() {
	log.Println("==================================================")
	log.Println("           AuraBox - Hi-Fi Audio Jukebox          ")
	log.Println("==================================================")

	port := getEnv("PORT", "4080")
	httpsPort := strings.TrimSpace(getEnv("HTTPS_PORT", "4443"))
	if strings.EqualFold(httpsPort, "off") || httpsPort == "0" {
		httpsPort = ""
	}
	httpsPublicPort := strings.TrimSpace(getEnv("HTTPS_PUBLIC_PORT", httpsPort))
	forceHTTPS := strings.EqualFold(getEnv("FORCE_HTTPS", "false"), "true")
	adminPort := getEnv("ADMIN_PORT", "4081")
	mpdSocket := getEnv("MPD_SOCKET", "/run/mpd/socket")
	mpdHost := getEnv("MPD_HOST", "localhost")
	mpdPort := getEnv("MPD_PORT", "6600")

	dataDir := getEnv("DATA_DIR", "./data")
	musicLocal := getEnv("MUSIC_LOCAL", "./music/local")
	musicNas := getEnv("MUSIC_NAS", "./music/nas")
	musicUsb := getEnv("MUSIC_USB", "./music/usb")
	dbPath := filepath.Join(dataDir, "aurabox.db")
	coversDir := filepath.Join(dataDir, "covers")
	certsDir := filepath.Join(dataDir, "certs")

	_ = os.MkdirAll(dataDir, 0755)
	_ = os.MkdirAll(coversDir, 0755)

	var certMgr *tlsutil.CertManager
	if httpsPort != "" {
		var tlsErr error
		certMgr, tlsErr = tlsutil.InitCertManager(
			certsDir,
			splitCSVTrim(getEnv("TLS_EXTRA_HOSTS", "")),
			getEnv("TLS_CERT_FILE", ""),
			getEnv("TLS_KEY_FILE", ""),
		)
		if tlsErr != nil {
			log.Printf("[HTTPS] Advertencia inicializando TLS (%v); se mantiene solo HTTP", tlsErr)
			httpsPort = ""
		} else {
			log.Printf("[HTTPS] Certificados TLS listos en %s (SANs: %s)", certsDir, strings.Join(certMgr.SANHosts(), ", "))
		}
	}

	// Inicializar SQLite
	database, err := db.InitDB(dbPath)
	if err != nil {
		log.Fatalf("[Error] No se pudo inicializar la base de datos SQLite: %v", err)
	}
	defer database.Close()
	log.Printf("[DB] Base de datos SQLite lista en: %s", dbPath)

	// Inicializar hub de eventos SSE en tiempo real
	hub := events.NewHub()

	// Inicializar controlador de audio MPD / ALSA (Unix socket preferido + fallback TCP)
	player := mpd.NewClient(mpdSocket, mpdHost, mpdPort, hub, database)
	defer player.Close()
	player.TriggerLibraryUpdate()

	// Hidratar estado y cola al instante cuando cualquier cliente abre el canal SSE
	hub.SetOnConnect(func(send func(string, interface{})) {
		st := player.GetState()
		q := player.GetQueue()
		send("player_state", st)
		send("queue_update", map[string]interface{}{
			"queue":  q,
			"length": len(q),
		})
	})

	// Inicializar escáner de biblioteca musical y mantener el índice MPD sincronizado tras cada barrido
	libScanner := scanner.NewLibraryScanner(database, hub, musicLocal, musicNas, musicUsb, coversDir)

	// Inicializar motor Smart Mix
	smService := smartmix.NewSmartMixService(database)

	// Conectar el generador de Autoplay / Radio infinita con Smart Mix y fallback de biblioteca
	player.SetAutoplayProvider(func(seedTrack db.Track) ([]db.Track, error) {
		artist := strings.TrimSpace(seedTrack.Artist)
		if artist != "" {
			// Nivel 1: Smart Mix combinando Last.fm / ListenBrainz con similitud local
			mixTracks, _, err := smService.GenerateMix(artist, 10)
			if err == nil && len(mixTracks) > 0 {
				return mixTracks, nil
			}
		}

		// Nivel 2: Si no hubo Smart Mix o falló, buscar por el mismo género musical
		if seedTrack.Genre != "" {
			genreTracks, gErr := database.GetTracksByGenre(seedTrack.Genre, 12)
			if gErr == nil && len(genreTracks) > 0 {
				return genreTracks, nil
			}
		}

		// Nivel 3: Fallback a temas aleatorios de la colección local
		return database.GetRandomTracks(12)
	})

	// Inicializar servicio de letras LRCLIB con caché persistente SQLite
	lyrService := lyrics.NewService()
	lyrService.SetStore(database)

	// Inicializar servicio de búsqueda de carátulas Cover Art Archive / MusicBrainz / iTunes / Deezer
	coversService := covers.NewService(coversDir)
	if player != nil {
		player.SetCoverFetcher(coversService.FetchAndCacheTrackCover)
	}

	generalHTTPClient := mpd.NewSafeExternalHTTPClient(15 * time.Second)

	srv := &Server{
		database:        database,
		eventsHub:       hub,
		player:          player,
		scanner:         libScanner,
		smartmix:        smService,
		lyrics:          lyrService,
		covers:          coversService,
		certManager:     certMgr,
		streamClient:    newAudioStreamClient(),
		httpClient:      generalHTTPClient,
		localDir:        musicLocal,
		nasDir:          musicNas,
		usbDir:          musicUsb,
		coversDir:       coversDir,
		httpsPort:       httpsPort,
		httpsPublicPort: httpsPublicPort,
		forceHTTPS:      forceHTTPS,
		startTime:       time.Now(),
	}

	prewarmCtx, prewarmCancel := context.WithCancel(context.Background())
	defer prewarmCancel()
	libScanner.SetOnScanComplete(func() {
		player.TriggerLibraryUpdate()
		go srv.PrewarmCoverThumbnails(prewarmCtx)
	})

	// Ejecutar escaneo incremental inicial (la reparación de duraciones corre tras indexar)
	libScanner.ScanLibraryAll()

	// Vigilar cambios automáticos en música local y unidades USB extraíbles sin intervención manual
	if watchErr := libScanner.StartAutoWatcher(3 * time.Second); watchErr != nil {
		log.Printf("[Watcher] Advertencia iniciando vigilancia automática: %v", watchErr)
	}
	defer libScanner.WaitForIdle()
	defer libScanner.StopAutoWatcher()

	// Enrutador HTTP principal
	mux := http.NewServeMux()
	srv.registerRoutes(mux)

	baseHandler := compressionAndSecurityMiddleware(mux)

	// Servidor Web HTTP en puerto 4080 (redirige si entra sin puerto :4080 o si FORCE_HTTPS=true)
	httpServer := &http.Server{
		Addr:              ":" + port,
		Handler:           corsMiddleware(srv.httpsRedirectMiddleware(baseHandler)),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       120 * time.Second,
		WriteTimeout:      0, // Sin corte abrupto para permitir transmisión SSE continua y streaming de audio
	}

	// Servidor Web HTTPS nativo en puerto 4443 con TLS 1.2+ y certificados SAN dinámicos
	var httpsServer *http.Server
	if httpsPort != "" && certMgr != nil {
		httpsServer = &http.Server{
			Addr:              ":" + httpsPort,
			Handler:           corsMiddleware(baseHandler),
			TLSConfig:         certMgr.TLSConfig(),
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       30 * time.Second,
			IdleTimeout:       120 * time.Second,
			WriteTimeout:      0,
		}
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

	if httpsServer != nil {
		go func() {
			ln, err := net.Listen("tcp", ":"+httpsPort)
			if err != nil {
				log.Printf("[HTTPS] No se pudo abrir puerto %s: %v", httpsPort, err)
				return
			}
			log.Printf("[HTTPS] AuraBox Web & API TLS escuchando en https://0.0.0.0:%s", httpsPort)
			tlsLn := tls.NewListener(ln, httpsServer.TLSConfig)
			if err := httpsServer.Serve(tlsLn); err != nil && err != http.ErrServerClosed {
				log.Printf("[HTTPS] Servidor TLS detenido: %v", err)
			}
		}()
	}

	// Captura de señales para apagado elegante
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop

	log.Println("[Sistema] Deteniendo AuraBox de manera ordenada...")
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	_ = httpServer.Shutdown(shutdownCtx)
	if httpsServer != nil {
		_ = httpsServer.Shutdown(shutdownCtx)
	}
	_ = adminServer.Shutdown(shutdownCtx)
}

// requireMethod restringe un endpoint a uno o más métodos HTTP permitidos respondiendo 405 Method Not Allowed si difiere.
func requireMethod(next http.HandlerFunc, methods ...string) http.HandlerFunc {
	allowHeader := strings.Join(methods, ", ")
	return func(w http.ResponseWriter, r *http.Request) {
		for _, m := range methods {
			if r.Method == m {
				next(w, r)
				return
			}
		}
		w.Header().Set("Allow", allowHeader)
		http.Error(w, "método no permitido", http.StatusMethodNotAllowed)
	}
}

// registerRoutes configura las rutas REST, SSE y recursos estáticos.
func (s *Server) registerRoutes(mux *http.ServeMux) {
	// 1. Archivos estáticos del frontend PWA (HTML, CSS, JS, manifest)
	fs := http.FileServer(http.Dir("./web"))
	mux.Handle("/", noCacheMiddleware(fs))

	// 2. Caché de carátulas locales y miniaturas optimizadas (240x240) con cabeceras de persistencia inmutables
	mux.HandleFunc("/api/covers/", s.handleCovers)

	// 3. Flujo en tiempo real Server-Sent Events (SSE)
	mux.HandleFunc("/api/events", requireMethod(s.eventsHub.ServeHTTP, http.MethodGet))

	// 4. API del Reproductor
	mux.HandleFunc("/api/status", requireMethod(s.handleStatus, http.MethodGet))
	mux.HandleFunc("/api/player/play", requireMethod(s.handlePlay, http.MethodPost))
	mux.HandleFunc("/api/player/pause", requireMethod(s.handlePause, http.MethodPost))
	mux.HandleFunc("/api/player/toggle", requireMethod(s.handleToggle, http.MethodPost))
	mux.HandleFunc("/api/player/next", requireMethod(s.handleNext, http.MethodPost))
	mux.HandleFunc("/api/player/prev", requireMethod(s.handlePrev, http.MethodPost))
	mux.HandleFunc("/api/player/seek", requireMethod(s.handleSeek, http.MethodPost))
	mux.HandleFunc("/api/player/repeat", requireMethod(s.handleRepeat, http.MethodPost))
	mux.HandleFunc("/api/player/volume", requireMethod(s.handleVolume, http.MethodPost))
	mux.HandleFunc("/api/player/volume/step", requireMethod(s.handleVolumeStep, http.MethodPost))
	mux.HandleFunc("/api/player/volume/max", requireMethod(s.handleVolumeMax, http.MethodPost))
	mux.HandleFunc("/api/player/output", requireMethod(s.handleSelectOutput, http.MethodPost))
	mux.HandleFunc("/api/player/dsp", requireMethod(s.handleSetDSP, http.MethodPost))
	mux.HandleFunc("/api/player/outputs/refresh", requireMethod(s.handleRefreshOutputs, http.MethodPost))
	mux.HandleFunc("/api/player/outputs/diagnostics", requireMethod(s.handleAudioDiagnostics, http.MethodGet))
	mux.HandleFunc("/api/player/config", requireMethod(s.handleAudioConfig, http.MethodPost))
	mux.HandleFunc("/api/player/sleeptimer", requireMethod(s.handleSleepTimer, http.MethodPost))
	mux.HandleFunc("/api/player/cover/auto-fetch", requireMethod(s.handlePlayerCoverAutoFetch, http.MethodPost, http.MethodGet))
	mux.HandleFunc("/api/player/autoplay", requireMethod(s.handlePlayerAutoplay, http.MethodPost))

	// 5. API de Cola (Jukebox Queue)
	mux.HandleFunc("/api/queue", requireMethod(s.handleQueue, http.MethodGet))
	mux.HandleFunc("/api/queue/add", requireMethod(s.handleQueueAdd, http.MethodPost))
	mux.HandleFunc("/api/queue/play", requireMethod(s.handleQueuePlay, http.MethodPost))
	mux.HandleFunc("/api/queue/move", requireMethod(s.handleQueueMove, http.MethodPost))
	mux.HandleFunc("/api/queue/remove", requireMethod(s.handleQueueRemove, http.MethodPost))
	mux.HandleFunc("/api/queue/clear", requireMethod(s.handleQueueClear, http.MethodPost))
	mux.HandleFunc("/api/queue/shuffle", requireMethod(s.handleQueueShuffle, http.MethodPost))

	// 6. API de Biblioteca
	mux.HandleFunc("/api/library/tracks", requireMethod(s.handleLibraryTracks, http.MethodGet))
	mux.HandleFunc("/api/library/recent", requireMethod(s.handleLibraryRecent, http.MethodGet))
	mux.HandleFunc("/api/library/albums", requireMethod(s.handleLibraryAlbums, http.MethodGet))
	mux.HandleFunc("/api/library/album", requireMethod(s.handleLibraryAlbumDetail, http.MethodGet))
	mux.HandleFunc("/api/library/album/cover/fetch", requireMethod(s.handleLibraryAlbumCoverFetch, http.MethodPost))
	mux.HandleFunc("/api/library/artists", requireMethod(s.handleLibraryArtists, http.MethodGet))
	mux.HandleFunc("/api/library/artist", requireMethod(s.handleLibraryArtistDetail, http.MethodGet))
	mux.HandleFunc("/api/library/artist/tracks", requireMethod(s.handleLibraryArtistTracks, http.MethodGet))
	mux.HandleFunc("/api/library/genres", requireMethod(s.handleLibraryGenres, http.MethodGet))
	mux.HandleFunc("/api/library/genres/albums", requireMethod(s.handleLibraryGenreAlbums, http.MethodGet))
	mux.HandleFunc("/api/library/folders", requireMethod(s.handleLibraryFolders, http.MethodGet))
	mux.HandleFunc("/api/library/scan", requireMethod(s.handleLibraryScan, http.MethodPost))
	mux.HandleFunc("/api/library/favorite", requireMethod(s.handleFavoriteToggle, http.MethodPost))
	mux.HandleFunc("/api/library/favorites", requireMethod(s.handleGetFavorites, http.MethodGet))
	mux.HandleFunc("/api/library/favorites/play", requireMethod(s.handlePlayFavorites, http.MethodPost))
	mux.HandleFunc("/api/search", requireMethod(s.handleSearch, http.MethodGet))

	// 7. API de Smart Mix
	mux.HandleFunc("/api/smartmix", requireMethod(s.handleSmartMix, http.MethodPost))
	mux.HandleFunc("/api/smartmix/themes", requireMethod(s.handleSmartMixThemes, http.MethodGet))

	// 8. API de Letras LRCLIB
	mux.HandleFunc("/api/lyrics", requireMethod(s.handleLyrics, http.MethodGet))

	// 9. Emisoras de Radio, Stream, Seguridad TLS y Ajustes
	mux.HandleFunc("/api/stream", requireMethod(s.handleAudioStream, http.MethodGet, http.MethodHead))
	mux.HandleFunc("/api/radios", requireMethod(s.handleRadios, http.MethodGet, http.MethodPost, http.MethodDelete))
	mux.HandleFunc("/api/radios/play", requireMethod(s.handleRadioPlay, http.MethodPost))
	mux.HandleFunc("/api/radios/favorite", requireMethod(s.handleRadioFavorite, http.MethodPost))
	mux.HandleFunc("/api/settings", requireMethod(s.handleSettings, http.MethodGet, http.MethodPost))
	mux.HandleFunc("/api/settings/nas", requireMethod(s.handleNASConfig, http.MethodGet, http.MethodPost))
	mux.HandleFunc("/api/security/status", requireMethod(s.handleSecurityStatus, http.MethodGet))
	mux.HandleFunc("/api/security/ca.crt", requireMethod(s.handleDownloadCACert, http.MethodGet, http.MethodHead))

	// 10. API de Podcasts, Búsqueda Online y OPML
	mux.HandleFunc("/api/podcasts", requireMethod(s.handlePodcasts, http.MethodGet))
	mux.HandleFunc("/api/podcasts/subscribe", requireMethod(s.handlePodcastSubscribe, http.MethodPost))
	mux.HandleFunc("/api/podcasts/unsubscribe", requireMethod(s.handlePodcastUnsubscribe, http.MethodDelete, http.MethodPost))
	mux.HandleFunc("/api/podcasts/favorite", requireMethod(s.handlePodcastFavorite, http.MethodPost))
	mux.HandleFunc("/api/podcasts/episodes", requireMethod(s.handlePodcastEpisodes, http.MethodGet))
	mux.HandleFunc("/api/podcasts/episodes/favorite", requireMethod(s.handlePodcastEpisodeFavorite, http.MethodPost))
	mux.HandleFunc("/api/podcasts/episodes/played", requireMethod(s.handlePodcastEpisodePlayed, http.MethodPost))
	mux.HandleFunc("/api/podcasts/play", requireMethod(s.handlePodcastPlay, http.MethodPost))
	mux.HandleFunc("/api/podcasts/search", requireMethod(s.handlePodcastSearch, http.MethodGet))
	mux.HandleFunc("/api/podcasts/opml/import", requireMethod(s.handlePodcastOPMLImport, http.MethodPost))
	mux.HandleFunc("/api/podcasts/opml/export", requireMethod(s.handlePodcastOPMLExport, http.MethodGet))
	mux.HandleFunc("/api/podcasts/refresh", requireMethod(s.handlePodcastsRefresh, http.MethodPost))
	mux.HandleFunc("/api/podcasts/progress", requireMethod(s.handlePodcastProgress, http.MethodPost))

	// 11. API de Playlists Personalizadas (M3U)
	mux.HandleFunc("/api/playlists", requireMethod(s.handlePlaylists, http.MethodGet, http.MethodPost))
	mux.HandleFunc("/api/playlists/detail", requireMethod(s.handlePlaylistDetail, http.MethodGet))
	mux.HandleFunc("/api/playlists/update", requireMethod(s.handlePlaylistUpdate, http.MethodPost))
	mux.HandleFunc("/api/playlists/delete", requireMethod(s.handlePlaylistDelete, http.MethodDelete, http.MethodPost))
	mux.HandleFunc("/api/playlists/tracks/add", requireMethod(s.handlePlaylistTracksAdd, http.MethodPost))
	mux.HandleFunc("/api/playlists/tracks/remove", requireMethod(s.handlePlaylistTracksRemove, http.MethodPost))
	mux.HandleFunc("/api/playlists/tracks/reorder", requireMethod(s.handlePlaylistTracksReorder, http.MethodPost))
	mux.HandleFunc("/api/playlists/play", requireMethod(s.handlePlaylistPlay, http.MethodPost))
	mux.HandleFunc("/api/playlists/export", requireMethod(s.handlePlaylistExport, http.MethodGet))
	mux.HandleFunc("/api/playlists/import", requireMethod(s.handlePlaylistImport, http.MethodPost))
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

// isSameOrigin valida que la cabecera Origin corresponda exactamente al mismo esquema HTTP/HTTPS y host de la petición.
func isSameOrigin(origin, reqHost string) bool {
	if origin == "" || reqHost == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return false
	}
	return u.Host != "" && strings.EqualFold(u.Host, reqHost)
}

// corsMiddleware restringe CORS estrictamente al mismo origen en red local sin exponer comodines globales.
func corsMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		origin := strings.TrimSpace(r.Header.Get("Origin"))
		sameOrigin := isSameOrigin(origin, r.Host)
		if sameOrigin {
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Add("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		}
		if r.Method == http.MethodOptions {
			if origin != "" && !sameOrigin {
				http.Error(w, "origen cruzado no permitido", http.StatusForbidden)
				return
			}
			w.WriteHeader(http.StatusNoContent)
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
	writer      *gzip.Writer
	decided     bool
	gzipEnabled bool
}

// Unwrap expone el ResponseWriter subyacente para compatibilidad con http.ResponseController.
func (g *gzipResponseWriter) Unwrap() http.ResponseWriter {
	return g.ResponseWriter
}

// isAlreadyCompressedContentType detecta MIME binarios ya comprimidos o multimedia donde gzip empeora CPU/latencia.
func isAlreadyCompressedContentType(ct string) bool {
	base := strings.ToLower(strings.TrimSpace(strings.Split(ct, ";")[0]))
	if base == "" {
		return false
	}
	if strings.HasPrefix(base, "image/") && base != "image/svg+xml" {
		return true
	}
	return strings.HasPrefix(base, "audio/") ||
		strings.HasPrefix(base, "video/") ||
		base == "application/octet-stream" ||
		base == "application/x-x509-ca-cert" ||
		base == "application/gzip" ||
		base == "application/zip"
}

// WriteHeader decide de forma diferida si activar gzip según código HTTP y Content-Type real de la respuesta.
func (g *gzipResponseWriter) WriteHeader(statusCode int) {
	if g.decided {
		return
	}
	g.decided = true

	shouldSkip := statusCode < 200 ||
		statusCode == http.StatusNoContent ||
		statusCode == http.StatusNotModified ||
		g.Header().Get("Content-Encoding") != "" ||
		isAlreadyCompressedContentType(g.Header().Get("Content-Type"))

	if !shouldSkip {
		gz := gzipPool.Get().(*gzip.Writer)
		gz.Reset(g.ResponseWriter)
		g.writer = gz
		g.gzipEnabled = true
		g.Header().Set("Content-Encoding", "gzip")
		g.Header().Del("Content-Length")
		g.Header().Add("Vary", "Accept-Encoding")
	}

	g.ResponseWriter.WriteHeader(statusCode)
}

// Write envía los bytes comprimidos o directos tras asegurar la decisión de cabecera.
func (g *gzipResponseWriter) Write(b []byte) (int, error) {
	if !g.decided {
		if g.Header().Get("Content-Type") == "" && len(b) > 0 {
			g.Header().Set("Content-Type", http.DetectContentType(b))
		}
		g.WriteHeader(http.StatusOK)
	}
	if g.gzipEnabled && g.writer != nil {
		return g.writer.Write(b)
	}
	return g.ResponseWriter.Write(b)
}

// Flush vacía el flujo gzip (si está activo) y el ResponseWriter subyacente.
func (g *gzipResponseWriter) Flush() {
	if g.gzipEnabled && g.writer != nil {
		_ = g.writer.Flush()
	}
	if f, ok := g.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// finish cierra el compresor gzip si se activó y devuelve la instancia al pool.
func (g *gzipResponseWriter) finish() {
	if g.gzipEnabled && g.writer != nil {
		_ = g.writer.Close()
		gzipPool.Put(g.writer)
		g.writer = nil
	}
}

// compressionAndSecurityMiddleware comprime respuestas textuales de forma segura y añade cabeceras CSP y de endurecimiento HTTP.
func compressionAndSecurityMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "SAMEORIGIN")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline' https://fonts.googleapis.com; font-src 'self' https://fonts.gstatic.com data:; img-src 'self' https: http: data:; connect-src 'self'; media-src 'self'; object-src 'none'; frame-ancestors 'self'; base-uri 'self'")

		// Omitir compresión en SSE, streaming binario, peticiones HEAD, Range o respuestas binarias prefijadas.
		if r.Method == http.MethodHead ||
			r.Header.Get("Range") != "" ||
			r.URL.Path == "/api/events" ||
			r.URL.Path == "/api/stream" ||
			strings.HasPrefix(r.URL.Path, "/api/covers/") ||
			r.URL.Path == "/api/security/ca.crt" ||
			!strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			next.ServeHTTP(w, r)
			return
		}

		gzw := &gzipResponseWriter{ResponseWriter: w}
		defer gzw.finish()
		next.ServeHTTP(gzw, r)
	})
}

// resizeToThumbnail escala proporcionalmente una imagen para que quepa en maxDim x maxDim usando interpolación bilineal.
func resizeToThumbnail(src image.Image, maxDim int) image.Image {
	bounds := src.Bounds()
	w, h := bounds.Dx(), bounds.Dy()
	if w <= 0 || h <= 0 {
		return src
	}
	if w <= maxDim && h <= maxDim {
		return src
	}

	var newW, newH int
	if w >= h {
		newW = maxDim
		newH = int(float64(h) * float64(maxDim) / float64(w))
	} else {
		newH = maxDim
		newW = int(float64(w) * float64(maxDim) / float64(h))
	}
	if newW < 1 {
		newW = 1
	}
	if newH < 1 {
		newH = 1
	}

	dst := image.NewRGBA(image.Rect(0, 0, newW, newH))
	xRatio := float64(w) / float64(newW)
	yRatio := float64(h) / float64(newH)

	for y := 0; y < newH; y++ {
		srcY := float64(y) * yRatio
		yFloor := int(srcY)
		yDiff := srcY - float64(yFloor)
		yCeil := yFloor + 1
		if yCeil >= h {
			yCeil = h - 1
		}

		for x := 0; x < newW; x++ {
			srcX := float64(x) * xRatio
			xFloor := int(srcX)
			xDiff := srcX - float64(xFloor)
			xCeil := xFloor + 1
			if xCeil >= w {
				xCeil = w - 1
			}

			r1, g1, b1, a1 := src.At(bounds.Min.X+xFloor, bounds.Min.Y+yFloor).RGBA()
			r2, g2, b2, a2 := src.At(bounds.Min.X+xCeil, bounds.Min.Y+yFloor).RGBA()
			r3, g3, b3, a3 := src.At(bounds.Min.X+xFloor, bounds.Min.Y+yCeil).RGBA()
			r4, g4, b4, a4 := src.At(bounds.Min.X+xCeil, bounds.Min.Y+yCeil).RGBA()

			topR := float64(r1)*(1-xDiff) + float64(r2)*xDiff
			topG := float64(g1)*(1-xDiff) + float64(g2)*xDiff
			topB := float64(b1)*(1-xDiff) + float64(b2)*xDiff
			topA := float64(a1)*(1-xDiff) + float64(a2)*xDiff

			botR := float64(r3)*(1-xDiff) + float64(r4)*xDiff
			botG := float64(g3)*(1-xDiff) + float64(g4)*xDiff
			botB := float64(b3)*(1-xDiff) + float64(b4)*xDiff
			botA := float64(a3)*(1-xDiff) + float64(a4)*xDiff

			finR := uint8((topR*(1-yDiff) + botR*yDiff) / 257)
			finG := uint8((topG*(1-yDiff) + botG*yDiff) / 257)
			finB := uint8((topB*(1-yDiff) + botB*yDiff) / 257)
			finA := uint8((topA*(1-yDiff) + botA*yDiff) / 257)

			dst.SetRGBA(x, y, color.RGBA{R: finR, G: finG, B: finB, A: finA})
		}
	}

	return dst
}

// initThumbSync inicializa de forma segura el semáforo de concurrencia y mapa de deduplicación para miniaturas.
func (s *Server) initThumbSync() {
	s.thumbOnce.Do(func() {
		s.thumbSem = make(chan struct{}, 2)
		s.thumbFlight = make(map[string]chan struct{})
	})
}

// ensureThumbnail genera (de forma deduplicada y acotada en RAM) o reutiliza la miniatura 240x240 de una carátula.
// Devuelve la ruta del archivo a servir, si es miniatura JPEG y si el archivo existe en disco.
func (s *Server) ensureThumbnail(ctx context.Context, rawFilename string) (servePath string, isThumbFile bool, exists bool) {
	origPath := filepath.Join(s.coversDir, rawFilename)
	if strings.HasSuffix(strings.ToLower(rawFilename), ".svg") {
		if _, err := os.Stat(origPath); err != nil {
			return "", false, false
		}
		return origPath, false, true
	}

	thumbDir := filepath.Join(s.coversDir, "thumb")
	_ = os.MkdirAll(thumbDir, 0755)
	thumbPath := filepath.Join(thumbDir, rawFilename)

	// 1. Si la miniatura ya existe en disco, devolverla al instante sin CPU adicional
	if info, err := os.Stat(thumbPath); err == nil && info.Size() > 0 {
		return thumbPath, true, true
	}

	// 2. Evitar que múltiples peticiones decodifiquen la misma carátula grande en paralelo
	s.initThumbSync()
	for {
		s.thumbMu.Lock()
		waitCh, inFlight := s.thumbFlight[rawFilename]
		if !inFlight {
			waitCh = make(chan struct{})
			s.thumbFlight[rawFilename] = waitCh
			s.thumbMu.Unlock()
			break
		}
		s.thumbMu.Unlock()

		select {
		case <-waitCh:
			if info, err := os.Stat(thumbPath); err == nil && info.Size() > 0 {
				return thumbPath, true, true
			}
		case <-ctx.Done():
			return "", false, false
		}
	}
	defer func() {
		s.thumbMu.Lock()
		if ch, ok := s.thumbFlight[rawFilename]; ok {
			delete(s.thumbFlight, rawFilename)
			close(ch)
		}
		s.thumbMu.Unlock()
	}()

	if info, err := os.Stat(thumbPath); err == nil && info.Size() > 0 {
		return thumbPath, true, true
	}

	// Limitar decodificaciones simultáneas (máx. 2) para evitar picos OOM en Raspberry Pi
	select {
	case s.thumbSem <- struct{}{}:
		defer func() { <-s.thumbSem }()
	case <-ctx.Done():
		return "", false, false
	}

	origInfo, err := os.Stat(origPath)
	if err != nil {
		return "", false, false
	}
	if origInfo.Size() > 15<<20 {
		return origPath, false, true
	}

	origFile, err := os.Open(origPath)
	if err != nil {
		return "", false, false
	}
	defer origFile.Close()

	// Validar dimensiones antes de decodificar píxeles (protección anti-bomba de descompresión)
	cfg, _, cfgErr := image.DecodeConfig(origFile)
	if cfgErr != nil || cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > 6000 || cfg.Height > 6000 || int64(cfg.Width)*int64(cfg.Height) > 25_000_000 {
		return origPath, false, true
	}
	if _, seekErr := origFile.Seek(0, io.SeekStart); seekErr != nil {
		return origPath, false, true
	}

	img, _, err := image.Decode(origFile)
	if err != nil {
		return origPath, false, true
	}

	thumbImg := resizeToThumbnail(img, 240)
	tmpFile, err := os.CreateTemp(thumbDir, "thumb_*.tmp")
	if err == nil {
		tmpPath := tmpFile.Name()
		if encErr := jpeg.Encode(tmpFile, thumbImg, &jpeg.Options{Quality: 80}); encErr == nil {
			_ = tmpFile.Close()
			_ = os.Rename(tmpPath, thumbPath)
		} else {
			_ = tmpFile.Close()
			_ = os.Remove(tmpPath)
		}
	}

	if info, err := os.Stat(thumbPath); err == nil && info.Size() > 0 {
		return thumbPath, true, true
	}
	return origPath, false, true
}

// PrewarmCoverThumbnails genera de forma pausada en segundo plano las miniaturas faltantes tras un escaneo.
func (s *Server) PrewarmCoverThumbnails(ctx context.Context) {
	if s.coversDir == "" || !s.prewarmRunning.CompareAndSwap(false, true) {
		return
	}
	defer s.prewarmRunning.Store(false)

	entries, err := os.ReadDir(s.coversDir)
	if err != nil {
		return
	}

	thumbDir := filepath.Join(s.coversDir, "thumb")
	generated := 0
	for _, entry := range entries {
		if ctx.Err() != nil {
			return
		}
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		lower := strings.ToLower(name)
		if strings.HasSuffix(lower, ".tmp") || strings.HasSuffix(lower, ".svg") {
			continue
		}
		if !(strings.HasSuffix(lower, ".jpg") || strings.HasSuffix(lower, ".jpeg") || strings.HasSuffix(lower, ".png")) {
			continue
		}
		if info, err := os.Stat(filepath.Join(thumbDir, name)); err == nil && info.Size() > 0 {
			continue
		}

		if _, isThumb, exists := s.ensureThumbnail(ctx, name); exists && isThumb {
			generated++
		}

		// Pausa corta para no saturar la CPU ni la tarjeta SD/USB de la Raspberry Pi
		timer := time.NewTimer(60 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}

	if generated > 0 {
		log.Printf("[Covers] Pre-generadas %d miniaturas 240x240 en segundo plano", generated)
	}
}

// handleCovers sirve carátulas locales originales o miniaturas cacheadas en disco (240x240) protegiendo la RAM de la RPi.
func (s *Server) handleCovers(w http.ResponseWriter, r *http.Request) {
	relPath := strings.TrimPrefix(r.URL.Path, "/api/covers/")
	relPath = filepath.Clean(relPath)
	if relPath == "." || relPath == "/" || strings.Contains(relPath, "..") {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Cache-Control", "public, max-age=2592000, immutable")

	isThumb := r.URL.Query().Get("thumb") == "1" ||
		r.URL.Query().Get("size") == "thumb" ||
		strings.HasPrefix(relPath, "thumb/")

	rawFilename := filepath.Base(relPath)
	if rawFilename == "." || rawFilename == "/" {
		http.NotFound(w, r)
		return
	}

	origPath := filepath.Join(s.coversDir, rawFilename)
	if !isThumb || strings.HasSuffix(strings.ToLower(rawFilename), ".svg") {
		http.ServeFile(w, r, origPath)
		return
	}

	servePath, isThumbFile, exists := s.ensureThumbnail(r.Context(), rawFilename)
	if !exists {
		http.NotFound(w, r)
		return
	}
	if isThumbFile {
		w.Header().Set("Content-Type", "image/jpeg")
	}
	http.ServeFile(w, r, servePath)
}

// resolvePathThroughSymlinks resuelve rutas absolutas siguiendo symlinks incluso cuando el último nodo aún no existe en disco.
func resolvePathThroughSymlinks(target string) (string, error) {
	absPath, err := filepath.Abs(filepath.Clean(target))
	if err != nil {
		return "", err
	}
	if realPath, err := filepath.EvalSymlinks(absPath); err == nil {
		return realPath, nil
	}

	cur := absPath
	var tail []string
	for {
		parent := filepath.Dir(cur)
		base := filepath.Base(cur)
		tail = append(tail, base)
		if realParent, err := filepath.EvalSymlinks(parent); err == nil {
			res := realParent
			for i := len(tail) - 1; i >= 0; i-- {
				res = filepath.Join(res, tail[i])
			}
			return filepath.Clean(res), nil
		}
		if parent == cur {
			return absPath, nil
		}
		cur = parent
	}
}

// isSafeMusicPath asegura que una ruta de archivo o carpeta pertenezca físicamente a los directorios de música autorizados.
func (s *Server) isSafeMusicPath(p string) bool {
	if p == "" || strings.ContainsRune(p, '\x00') {
		return false
	}
	lower := strings.ToLower(strings.TrimSpace(p))
	if strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		return false
	}

	evalTarget, err := resolvePathThroughSymlinks(p)
	if err != nil {
		return false
	}

	roots := []string{s.localDir, s.nasDir, s.usbDir}
	for _, root := range roots {
		if strings.TrimSpace(root) == "" {
			continue
		}
		evalRoot, err := resolvePathThroughSymlinks(root)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(evalRoot, evalTarget)
		if err == nil && (rel == "." || (rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)))) {
			return true
		}
	}
	return false
}

const maxJSONBodyBytes int64 = 1 << 20

// decodeJSONBody decodifica un cuerpo JSON limitando su tamaño máximo a 1 MiB para evitar consumo excesivo de RAM.
func decodeJSONBody(w http.ResponseWriter, r *http.Request, dst interface{}) error {
	if r.Body == nil {
		return fmt.Errorf("cuerpo vacío")
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxJSONBodyBytes)
	dec := json.NewDecoder(r.Body)
	return dec.Decode(dst)
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

// handleRepeat activa o desactiva el modo de repetición de la cola de reproducción.
func (s *Server) handleRepeat(w http.ResponseWriter, r *http.Request) {
	enabledStr := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("enabled")))
	var enabled bool
	if enabledStr == "" {
		enabled = !s.player.GetState().Repeat
	} else {
		enabled = (enabledStr == "true" || enabledStr == "1")
	}
	s.player.SetRepeat(enabled)
	jsonResponse(w, http.StatusOK, s.player.GetState())
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
	if err := s.player.SelectAudioOutput(id); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	jsonResponse(w, http.StatusOK, s.player.GetState())
}

// handleRefreshOutputs re-escanea los dispositivos ALSA del host/contenedor y actualiza MPD en caliente.
func (s *Server) handleRefreshOutputs(w http.ResponseWriter, r *http.Request) {
	diag, err := s.player.RefreshAudioOutputs()
	if err != nil {
		log.Printf("[Audio] Error refrescando salidas: %v", err)
		http.Error(w, "no se pudieron actualizar las salidas de audio", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"state":       s.player.GetState(),
		"diagnostics": diag,
	})
}

// handleAudioDiagnostics devuelve las salidas de audio activas y la lectura cruda de tarjetas ALSA.
func (s *Server) handleAudioDiagnostics(w http.ResponseWriter, r *http.Request) {
	jsonResponse(w, http.StatusOK, s.player.GetAudioDiagnostics())
}

// handleAudioConfig ajusta ReplayGain y Crossfade en MPD tras validar lista blanca y rango permitido.
func (s *Server) handleAudioConfig(w http.ResponseWriter, r *http.Request) {
	rawRG := strings.TrimSpace(r.URL.Query().Get("replay_gain"))
	replayGain := "auto"
	if rawRG != "" {
		normRG, ok := mpd.NormalizeReplayGainMode(rawRG)
		if !ok {
			http.Error(w, "parámetro replay_gain inválido (permitidos: off, track, album, auto)", http.StatusBadRequest)
			return
		}
		replayGain = normRG
	}

	crossfadeStr := strings.TrimSpace(r.URL.Query().Get("crossfade"))
	cf := 0
	if crossfadeStr != "" {
		parsedCF, err := strconv.Atoi(crossfadeStr)
		if err != nil || parsedCF < 0 || parsedCF > 15 {
			http.Error(w, "parámetro crossfade inválido (0..15 segundos)", http.StatusBadRequest)
			return
		}
		cf = parsedCF
	}

	s.player.SetAudioConfig(replayGain, cf)

	if rawFade := strings.TrimSpace(r.URL.Query().Get("soft_fade")); rawFade != "" {
		enabled := (strings.EqualFold(rawFade, "true") || rawFade == "1")
		if !enabled && !strings.EqualFold(rawFade, "false") && rawFade != "0" {
			http.Error(w, "parámetro soft_fade inválido (true/false)", http.StatusBadRequest)
			return
		}
		s.player.SetSoftFade(enabled)
	}

	if rawFadeMs := strings.TrimSpace(r.URL.Query().Get("soft_fade_ms")); rawFadeMs != "" {
		ms, err := strconv.Atoi(rawFadeMs)
		if err != nil || ms < 300 || ms > 3000 {
			http.Error(w, "parámetro soft_fade_ms inválido (300..3000 ms)", http.StatusBadRequest)
			return
		}
		s.player.SetSoftFadeDuration(ms)
	}

	if rawAuto := strings.TrimSpace(r.URL.Query().Get("autoplay_similar")); rawAuto != "" {
		enabled := (strings.EqualFold(rawAuto, "true") || rawAuto == "1")
		s.player.SetAutoplayEnabled(enabled)
	}

	jsonResponse(w, http.StatusOK, s.player.GetState())
}

// handlePlayerAutoplay activa o desactiva la adición automática de música similar al terminar la cola.
func (s *Server) handlePlayerAutoplay(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Enabled *bool `json:"enabled"`
	}
	if err := decodeJSONBody(w, r, &payload); err != nil || payload.Enabled == nil {
		qEnabled := r.URL.Query().Get("enabled")
		if qEnabled != "" {
			en := strings.EqualFold(qEnabled, "true") || qEnabled == "1"
			s.player.SetAutoplayEnabled(en)
			jsonResponse(w, http.StatusOK, s.player.GetState())
			return
		}
		http.Error(w, "campo 'enabled' (booleano) requerido", http.StatusBadRequest)
		return
	}
	s.player.SetAutoplayEnabled(*payload.Enabled)
	jsonResponse(w, http.StatusOK, s.player.GetState())
}

// handleSetDSP guarda el perfil de ecualizador DSP SoX para una salida de audio específica y recarga MPD si está activa.
func (s *Server) handleSetDSP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Output string `json:"output"`
		Preset string `json:"preset"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		if err := decodeJSONBody(w, r, &body); err != nil {
			http.Error(w, "cuerpo json inválido", http.StatusBadRequest)
			return
		}
	}

	outputName := strings.TrimSpace(body.Output)
	if outputName == "" {
		outputName = strings.TrimSpace(r.URL.Query().Get("output"))
	}
	preset := strings.TrimSpace(body.Preset)
	if preset == "" {
		preset = strings.TrimSpace(r.URL.Query().Get("preset"))
	}
	if preset == "" {
		http.Error(w, "parámetro preset requerido", http.StatusBadRequest)
		return
	}

	st, err := s.player.SetOutputDSPPreset(outputName, preset)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	jsonResponse(w, http.StatusOK, st)
}

// handleSleepTimer programa el temporizador de apagado por minutos o al finalizar la canción actual.
func (s *Server) handleSleepTimer(w http.ResponseWriter, r *http.Request) {
	mode := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("mode")))
	if mode == "track_end" {
		if err := s.player.StartSleepAfterTrack(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		jsonResponse(w, http.StatusOK, s.player.GetState())
		return
	}
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
	if err := decodeJSONBody(w, r, &payload); err != nil {
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
		cleanFolder := filepath.Clean(payload.Folder)
		if !s.isSafeMusicPath(cleanFolder) {
			http.Error(w, "acceso denegado: carpeta no permitida", http.StatusForbidden)
			return
		}
		folderTracks, err := s.database.GetTracksByPathPrefix(cleanFolder)
		if err == nil && len(folderTracks) > 0 {
			tracksToEnqueue = folderTracks
		} else {
			// Si no estaban aún en la base de datos, escanear la carpeta en disco
			tracksToEnqueue = s.scanner.ScanFolderTracks(cleanFolder)
		}
	} else if payload.Path != "" {
		cleanPath := filepath.Clean(payload.Path)
		if !s.isSafeMusicPath(cleanPath) {
			http.Error(w, "acceso denegado: ruta no permitida", http.StatusForbidden)
			return
		}
		fi, err := os.Lstat(cleanPath)
		if err == nil && fi.IsDir() {
			folderTracks, err := s.database.GetTracksByPathPrefix(cleanPath)
			if err == nil && len(folderTracks) > 0 {
				tracksToEnqueue = folderTracks
			} else {
				tracksToEnqueue = s.scanner.ScanFolderTracks(cleanPath)
			}
		} else if err == nil && fi.Mode().IsRegular() {
			// Es un archivo regular individual
			track, err := s.database.GetTrackByPath(cleanPath)
			if err == nil && track != nil {
				tracksToEnqueue = append(tracksToEnqueue, *track)
			} else {
				parsed := s.scanner.ParseAndSaveFile(cleanPath)
				if parsed != nil {
					tracksToEnqueue = append(tracksToEnqueue, *parsed)
				}
			}
		}
	} else if len(payload.FilePaths) > 0 {
		for _, fp := range payload.FilePaths {
			cleanFP := filepath.Clean(fp)
			if !s.isSafeMusicPath(cleanFP) {
				continue
			}
			track, err := s.database.GetTrackByPath(cleanFP)
			if err == nil && track != nil {
				tracksToEnqueue = append(tracksToEnqueue, *track)
			} else {
				parsed := s.scanner.ParseAndSaveFile(cleanFP)
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

	// Limpieza previa atómica si se especificó modo de reemplazo o ClearFirst
	if payload.ClearFirst || payload.Mode == "replace" {
		s.player.ReplaceQueue(tracksToEnqueue, true)
	} else {
		s.player.EnqueueTracks(tracksToEnqueue, payload.Mode)
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"status":   "ok",
		"enqueued": len(tracksToEnqueue),
	})
}

// handleQueuePlay salta directamente a reproducir la pista en el índice indicado de la cola.
func (s *Server) handleQueuePlay(w http.ResponseWriter, r *http.Request) {
	idxStr := r.URL.Query().Get("index")
	idx, err := strconv.Atoi(idxStr)
	if err != nil || idx < 0 {
		http.Error(w, "parámetro index inválido", http.StatusBadRequest)
		return
	}
	s.player.PlayIndex(idx)
	jsonResponse(w, http.StatusOK, s.player.GetState())
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
		log.Printf("[DB] Error listando pistas: %v", err)
		http.Error(w, "error al consultar la biblioteca", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, http.StatusOK, tracks)
}

// handleLibraryRecent devuelve pistas recientes (añadidas, escuchadas o más reproducidas según ?mode=added|played|top).
func (s *Server) handleLibraryRecent(w http.ResponseWriter, r *http.Request) {
	limitStr := r.URL.Query().Get("limit")
	limit, _ := strconv.Atoi(limitStr)
	if limit <= 0 {
		limit = 30
	} else if limit > 200 {
		limit = 200
	}
	mode := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("mode")))
	var tracks []db.Track
	var err error
	switch mode {
	case "played":
		tracks, err = s.database.GetRecentlyPlayedTracks(limit)
	case "top":
		tracks, err = s.database.GetMostPlayedTracks(limit)
	default:
		tracks, err = s.database.GetRecentTracks(limit)
	}
	if err != nil {
		log.Printf("[DB] Error listando recientes (mode=%q): %v", mode, err)
		http.Error(w, "error al consultar pistas recientes", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, http.StatusOK, tracks)
}

// handleLibraryAlbums devuelve los álbumes (> 5 pistas por defecto, o todos si ?include_all=true).
func (s *Server) handleLibraryAlbums(w http.ResponseWriter, r *http.Request) {
	includeAll := strings.EqualFold(strings.TrimSpace(r.URL.Query().Get("include_all")), "true") || r.URL.Query().Get("include_all") == "1"
	var albums []db.Album
	var err error
	if includeAll {
		albums, err = s.database.GetAlbumsFiltered(0)
	} else {
		albums, err = s.database.GetAlbums()
	}
	if err != nil {
		log.Printf("[DB] Error listando álbumes: %v", err)
		http.Error(w, "error al consultar álbumes", http.StatusInternalServerError)
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

// handleLibraryAlbumCoverFetch busca y descarga la carátula del álbum desde Cover Art Archive / MusicBrainz.
func (s *Server) handleLibraryAlbumCoverFetch(w http.ResponseWriter, r *http.Request) {
	if s.covers == nil {
		http.Error(w, "servicio de carátulas no inicializado", http.StatusServiceUnavailable)
		return
	}

	var albumID int64
	if idStr := strings.TrimSpace(r.URL.Query().Get("album_id")); idStr != "" {
		parsed, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil || parsed <= 0 {
			http.Error(w, "parámetro album_id inválido", http.StatusBadRequest)
			return
		}
		albumID = parsed
	} else {
		var payload struct {
			AlbumID int64 `json:"album_id"`
		}
		if err := decodeJSONBody(w, r, &payload); err != nil || payload.AlbumID <= 0 {
			http.Error(w, "album_id inválido o faltante", http.StatusBadRequest)
			return
		}
		albumID = payload.AlbumID
	}

	album, _, err := s.database.GetAlbumByID(albumID)
	if err != nil || album == nil {
		http.Error(w, "álbum no encontrado", http.StatusNotFound)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()

	coverURL, fetchErr := s.covers.FetchAndCacheAlbumCover(ctx, album.Title, album.Artist)
	if fetchErr != nil || coverURL == "" {
		log.Printf("[Covers] No se pudo obtener carátula online para %q (%s): %v", album.Title, album.Artist, fetchErr)
		http.Error(w, "no se encontró carátula en Cover Art Archive", http.StatusNotFound)
		return
	}

	if err := s.database.UpdateAlbumCover(album.Title, coverURL); err != nil {
		log.Printf("[DB] Error guardando carátula online de %q: %v", album.Title, err)
		http.Error(w, "error al guardar carátula en base de datos", http.StatusInternalServerError)
		return
	}

	if s.player != nil {
		s.player.UpdateCurrentTrackCoverIfAlbumMatches(album.Title, coverURL)
	}

	if s.eventsHub != nil {
		s.eventsHub.Broadcast("library_updated", map[string]interface{}{
			"scanning": false,
			"album_id": album.ID,
		})
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"album_id":  album.ID,
		"cover_url": coverURL,
		"source":    "coverartarchive",
	})
}

// handleLibraryArtists devuelve todos los artistas con sus totales.
func (s *Server) handleLibraryArtists(w http.ResponseWriter, r *http.Request) {
	artists, err := s.database.GetArtists()
	if err != nil {
		log.Printf("[DB] Error listando artistas: %v", err)
		http.Error(w, "error al consultar artistas", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, http.StatusOK, artists)
}

// handleLibraryArtistDetail devuelve la discografía y las canciones completas de un artista.
func (s *Server) handleLibraryArtistDetail(w http.ResponseWriter, r *http.Request) {
	artist := strings.TrimSpace(r.URL.Query().Get("name"))
	if artist == "" {
		http.Error(w, "parámetro name requerido", http.StatusBadRequest)
		return
	}
	albums, err := s.database.GetAlbumsByArtist(artist)
	if err != nil {
		log.Printf("[DB] Error listando álbumes de artista: %v", err)
		http.Error(w, "error al consultar discografía del artista", http.StatusInternalServerError)
		return
	}
	tracks, err := s.database.GetTracksByArtist(artist)
	if err != nil {
		log.Printf("[DB] Error listando pistas de artista: %v", err)
		http.Error(w, "error al consultar pistas del artista", http.StatusInternalServerError)
		return
	}
	if len(albums) == 0 && len(tracks) == 0 {
		http.Error(w, "artista no encontrado", http.StatusNotFound)
		return
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"artist": artist,
		"albums": albums,
		"tracks": tracks,
	})
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
		log.Printf("[DB] Error listando pistas de artista: %v", err)
		http.Error(w, "error al consultar pistas del artista", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, http.StatusOK, tracks)
}

// handleLibraryGenres devuelve el resumen navegable de géneros y décadas disponibles en la biblioteca.
func (s *Server) handleLibraryGenres(w http.ResponseWriter, r *http.Request) {
	genres, decades, err := s.database.GetCategoriesOverview()
	if err != nil {
		log.Printf("[DB] Error listando géneros y décadas: %v", err)
		http.Error(w, "error al consultar géneros", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"genres":  genres,
		"decades": decades,
	})
}

// handleLibraryGenreAlbums devuelve los álbumes filtrados por un género musical o una década concreta.
func (s *Server) handleLibraryGenreAlbums(w http.ResponseWriter, r *http.Request) {
	catType := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("type")))
	value := strings.TrimSpace(r.URL.Query().Get("value"))
	if catType == "" {
		catType = "genre"
	}
	if value == "" {
		http.Error(w, "parámetro value requerido", http.StatusBadRequest)
		return
	}

	var albums []db.Album
	var err error
	label := value

	switch catType {
	case "genre":
		albums, err = s.database.GetAlbumsByGenre(value)
	case "decade":
		cleanDec := strings.TrimSuffix(strings.ToLower(value), "s")
		decInt, parseErr := strconv.Atoi(cleanDec)
		if parseErr != nil {
			http.Error(w, "década inválida", http.StatusBadRequest)
			return
		}
		if decInt >= 0 && decInt < 100 {
			if decInt >= 40 {
				decInt += 1900
			} else {
				decInt += 2000
			}
		}
		decInt = (decInt / 10) * 10
		if decInt >= 1900 && decInt < 2000 {
			label = fmt.Sprintf("%02ds", decInt%100)
		} else {
			label = fmt.Sprintf("%ds", decInt)
		}
		albums, err = s.database.GetAlbumsByDecade(decInt)
	default:
		http.Error(w, "tipo de categoría inválido", http.StatusBadRequest)
		return
	}

	if err != nil {
		log.Printf("[DB] Error listando álbumes por categoría (%s=%q): %v", catType, value, err)
		http.Error(w, "error al consultar álbumes de la categoría", http.StatusInternalServerError)
		return
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"type":   catType,
		"value":  value,
		"label":  label,
		"albums": albums,
	})
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
		// Omitir archivos ocultos del sistema y enlaces simbólicos por seguridad
		if strings.HasPrefix(name, ".") || e.Type()&os.ModeSymlink != 0 {
			continue
		}
		full := filepath.Join(cleanSubpath, name)
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
		"path":    cleanSubpath,
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

// handleLibraryScan inicia un nuevo barrido de metadatos en segundo plano (?force=true para reindexado completo).
func (s *Server) handleLibraryScan(w http.ResponseWriter, r *http.Request) {
	force := (strings.ToLower(r.URL.Query().Get("force")) == "true" || r.URL.Query().Get("force") == "1")
	s.scanner.ScanLibrary(force)
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"status":  "scanning",
		"force":   force,
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
		log.Printf("[DB] Error alternando favorito %d: %v", id, err)
		http.Error(w, "no se pudo actualizar favorito", http.StatusInternalServerError)
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
		log.Printf("[DB] Error obteniendo favoritos: %v", err)
		http.Error(w, "error al consultar favoritos", http.StatusInternalServerError)
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
		_ = decodeJSONBody(w, r, &payload)
	}
	if payload.Mode == "" {
		payload.Mode = "play_now"
	}

	tracks, err := s.database.GetFavorites()
	if err != nil {
		log.Printf("[DB] Error reproduciendo favoritos: %v", err)
		http.Error(w, "error al cargar canciones favoritas", http.StatusInternalServerError)
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
		s.player.ReplaceQueue(tracks, true)
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

// handleSearch busca en tiempo real pistas, álbumes, artistas y emisoras de radio.
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query().Get("q")
	if strings.TrimSpace(q) == "" {
		jsonResponse(w, http.StatusOK, map[string]interface{}{
			"tracks":  []db.Track{},
			"albums":  []db.Album{},
			"artists": []db.Artist{},
			"radios":  []db.RadioStation{},
		})
		return
	}
	tracks, albums, artists, err := s.database.Search(q)
	if err != nil {
		log.Printf("[DB] Error en búsqueda: %v", err)
		http.Error(w, "error al ejecutar búsqueda", http.StatusInternalServerError)
		return
	}
	radios, _ := s.database.SearchRadios(q)
	if radios == nil {
		radios = []db.RadioStation{}
	}
	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"tracks":  tracks,
		"albums":  albums,
		"artists": artists,
		"radios":  radios,
	})
}

// handleSmartMixThemes devuelve los géneros y décadas disponibles localmente para crear mixes temáticos offline.
func (s *Server) handleSmartMixThemes(w http.ResponseWriter, r *http.Request) {
	themes, err := s.database.GetLibraryThemes()
	if err != nil {
		log.Printf("[SmartMix] Error consultando temas offline: %v", err)
		http.Error(w, "error al consultar géneros y décadas", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, http.StatusOK, themes)
}

// handleSmartMix genera una sesión Smart Mix (por artista o temático por género/década) y opcionalmente inicia su reproducción.
func (s *Server) handleSmartMix(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		Artist     string `json:"artist"`
		ThemeType  string `json:"theme_type"`
		ThemeValue string `json:"theme_value"`
		Count      int    `json:"count"`
		PlayNow    bool   `json:"play_now"`
	}
	if err := decodeJSONBody(w, r, &payload); err != nil {
		http.Error(w, "json inválido", http.StatusBadRequest)
		return
	}

	if payload.ThemeType != "" && payload.ThemeValue != "" {
		if payload.Count <= 0 {
			payload.Count = 30
		} else if payload.Count > 120 {
			payload.Count = 120
		}
		mixTracks, label, err := s.smartmix.GenerateThemeMix(payload.ThemeType, payload.ThemeValue, payload.Count)
		if err != nil {
			log.Printf("[SmartMix] Error generando mix temático (%s=%q): %v", payload.ThemeType, payload.ThemeValue, err)
			http.Error(w, "no se pudo generar el Smart Mix temático", http.StatusNotFound)
			return
		}
		if payload.PlayNow && len(mixTracks) > 0 {
			s.player.ReplaceQueue(mixTracks, true)
		}
		jsonResponse(w, http.StatusOK, map[string]interface{}{
			"seed_artist":     label,
			"theme_type":      payload.ThemeType,
			"theme_value":     payload.ThemeValue,
			"similar_artists": []string{},
			"tracks":          mixTracks,
			"count":           len(mixTracks),
		})
		return
	}

	if payload.Artist == "" {
		http.Error(w, "campo artist o theme_type/theme_value obligatorio", http.StatusBadRequest)
		return
	}
	if payload.Count <= 0 {
		payload.Count = 20
	} else if payload.Count > 100 {
		payload.Count = 100
	}

	mixTracks, similarArtists, err := s.smartmix.GenerateMix(payload.Artist, payload.Count)
	if err != nil {
		log.Printf("[SmartMix] Error generando mix para %q: %v", payload.Artist, err)
		http.Error(w, "no se pudo generar el Smart Mix para este artista", http.StatusInternalServerError)
		return
	}

	if payload.PlayNow && len(mixTracks) > 0 {
		s.player.ReplaceQueue(mixTracks, true)
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
		log.Printf("[Lyrics] Error obteniendo letras: %v", err)
		http.Error(w, "error al consultar letras", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, http.StatusOK, res)
}

// inferRadioFormat deduce el códec probable de una URL de radio cuando el cliente no especifica formato.
func inferRadioFormat(explicitFmt, streamURL string) string {
	fmtClean := strings.ToUpper(strings.TrimSpace(explicitFmt))
	if fmtClean != "" {
		return fmtClean
	}
	lower := strings.ToLower(strings.TrimSpace(streamURL))
	if idx := strings.IndexAny(lower, "?#"); idx >= 0 {
		lower = lower[:idx]
	}
	switch {
	case strings.HasSuffix(lower, ".m3u8"):
		return "HLS"
	case strings.HasSuffix(lower, ".mp3"), strings.Contains(lower, "/mp3/"), strings.HasSuffix(lower, "/mp3"):
		return "MP3"
	case strings.HasSuffix(lower, ".aac"), strings.HasSuffix(lower, ".aacp"), strings.Contains(lower, "/aac/"), strings.HasSuffix(lower, "/aac"):
		return "AAC"
	case strings.HasSuffix(lower, ".flac"), strings.Contains(lower, "/flac/"), strings.HasSuffix(lower, "/flac"):
		return "FLAC"
	case strings.HasSuffix(lower, ".ogg"), strings.HasSuffix(lower, ".oga"), strings.HasSuffix(lower, ".opus"):
		return "OGG"
	default:
		return "STREAM"
	}
}

// handleRadios gestiona la consulta, alta personalizada y borrado de emisoras de radio web.
func (s *Server) handleRadios(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		radios, err := s.database.GetRadios()
		if err != nil {
			log.Printf("[DB] Error listando radios: %v", err)
			http.Error(w, "error al consultar emisoras", http.StatusInternalServerError)
			return
		}
		jsonResponse(w, http.StatusOK, radios)

	case http.MethodPost:
		var payload struct {
			Name        string `json:"name"`
			StreamURL   string `json:"stream_url"`
			URL         string `json:"url"`
			LogoURL     string `json:"logo_url"`
			Genre       string `json:"genre"`
			Broadcaster string `json:"broadcaster"`
			Country     string `json:"country"`
			Bitrate     string `json:"bitrate"`
			Format      string `json:"format"`
		}
		if err := decodeJSONBody(w, r, &payload); err != nil {
			http.Error(w, "json inválido", http.StatusBadRequest)
			return
		}

		rawURL := strings.TrimSpace(payload.StreamURL)
		if rawURL == "" {
			rawURL = strings.TrimSpace(payload.URL)
		}
		cleanURL, err := mpd.ValidateHTTPStreamURL(rawURL)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		cleanLogo, err := mpd.ValidateLogoURL(payload.LogoURL)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}

		name := strings.TrimSpace(payload.Name)
		if name == "" || len(name) > 120 {
			http.Error(w, "el nombre de la emisora debe tener entre 1 y 120 caracteres", http.StatusBadRequest)
			return
		}

		station := db.RadioStation{
			Name:        name,
			StreamURL:   cleanURL,
			LogoURL:     cleanLogo,
			Genre:       strings.TrimSpace(payload.Genre),
			Broadcaster: strings.TrimSpace(payload.Broadcaster),
			Country:     strings.TrimSpace(payload.Country),
			Bitrate:     strings.TrimSpace(payload.Bitrate),
			Format:      inferRadioFormat(payload.Format, cleanURL),
		}
		if err := s.database.AddRadio(&station); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		jsonResponse(w, http.StatusCreated, station)

	case http.MethodDelete:
		idStr := strings.TrimSpace(r.URL.Query().Get("id"))
		id, err := strconv.ParseInt(idStr, 10, 64)
		if err != nil || id <= 0 {
			http.Error(w, "id de emisora inválido", http.StatusBadRequest)
			return
		}
		if err := s.database.DeleteRadio(id); err != nil {
			if strings.Contains(err.Error(), "no encontrada") {
				http.Error(w, err.Error(), http.StatusNotFound)
				return
			}
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		jsonResponse(w, http.StatusOK, map[string]string{"status": "deleted"})

	default:
		http.Error(w, "método no permitido", http.StatusMethodNotAllowed)
	}
}

// handleRadioFavorite conmuta el estado de favorita de una emisora web en SQLite.
func (s *Server) handleRadioFavorite(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ID int64 `json:"id"`
	}
	if r.Body != nil && r.ContentLength != 0 {
		_ = decodeJSONBody(w, r, &body)
	}
	if body.ID <= 0 {
		if idStr := r.URL.Query().Get("id"); idStr != "" {
			if idVal, err := strconv.ParseInt(idStr, 10, 64); err == nil {
				body.ID = idVal
			}
		}
	}
	if body.ID <= 0 {
		http.Error(w, "parámetro id de radio inválido", http.StatusBadRequest)
		return
	}

	newFav, err := s.database.ToggleRadioFavorite(body.ID)
	if err != nil {
		log.Printf("[Radio] Error alternando favorito para radio %d: %v", body.ID, err)
		http.Error(w, "error al actualizar favorita", http.StatusInternalServerError)
		return
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"id":       body.ID,
		"favorite": newFav,
		"status":   "ok",
	})
}

// handleRadioPlay valida/resuelve el flujo web de radio y comienza su reproducción inmediata.
func (s *Server) handleRadioPlay(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "método no permitido", http.StatusMethodNotAllowed)
		return
	}

	var payload struct {
		ID          int    `json:"id"`
		Name        string `json:"name"`
		StreamURL   string `json:"stream_url"`
		URL         string `json:"url"`
		LogoURL     string `json:"logo_url"`
		Genre       string `json:"genre"`
		Broadcaster string `json:"broadcaster"`
		Country     string `json:"country"`
		Bitrate     string `json:"bitrate"`
		Format      string `json:"format"`
	}
	if err := decodeJSONBody(w, r, &payload); err != nil {
		http.Error(w, "json inválido", http.StatusBadRequest)
		return
	}

	rawURL := strings.TrimSpace(payload.StreamURL)
	if rawURL == "" {
		rawURL = strings.TrimSpace(payload.URL)
	}
	if rawURL == "" && payload.ID > 0 && s.database != nil {
		if station, err := s.database.GetRadioByID(payload.ID); err == nil && station != nil {
			rawURL = station.StreamURL
			if payload.Name == "" {
				payload.Name = station.Name
			}
			if payload.LogoURL == "" {
				payload.LogoURL = station.LogoURL
			}
			if payload.Genre == "" {
				payload.Genre = station.Genre
			}
			if payload.Broadcaster == "" {
				payload.Broadcaster = station.Broadcaster
			}
			if payload.Country == "" {
				payload.Country = station.Country
			}
			if payload.Bitrate == "" {
				payload.Bitrate = station.Bitrate
			}
			if payload.Format == "" {
				payload.Format = station.Format
			}
		}
	}

	resolvedURLs, err := mpd.ResolveStreamURLs(rawURL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	coverVal, err := mpd.ValidateLogoURL(payload.LogoURL)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if coverVal == "" {
		coverVal = "/assets/default-cover.svg"
	}

	name := strings.TrimSpace(payload.Name)
	if name == "" {
		name = "Radio Web"
	}

	genre := strings.TrimSpace(payload.Genre)
	if genre == "" {
		genre = "Radio"
	}

	bitrateVal := 128
	if b, bErr := strconv.Atoi(strings.TrimSpace(payload.Bitrate)); bErr == nil && b > 0 {
		bitrateVal = b
	}

	formatVal := inferRadioFormat(payload.Format, resolvedURLs[0])

	radioTrack := db.Track{
		ID:       -1,
		Path:     resolvedURLs[0],
		Filename: name,
		Title:    name,
		Artist:   mpd.FormatRadioArtist(payload.Broadcaster, payload.Country),
		Album:    genre,
		Genre:    genre,
		Bitrate:  bitrateVal,
		Format:   formatVal,
		CoverURL: coverVal,
		Source:   "radio",
		Duration: 0,
	}

	s.player.ReplaceQueue([]db.Track{radioTrack}, true)

	jsonResponse(w, http.StatusOK, map[string]string{
		"status":     "playing_radio",
		"stream_url": resolvedURLs[0],
	})
}

// handleSettings lee o actualiza configuraciones permitidas del sistema bajo lista blanca estricta.
func (s *Server) handleSettings(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var payload map[string]string
		if err := decodeJSONBody(w, r, &payload); err != nil {
			http.Error(w, "json inválido", http.StatusBadRequest)
			return
		}
		if len(payload) == 0 {
			http.Error(w, "se requiere al menos un ajuste", http.StatusBadRequest)
			return
		}

		curState := s.player.GetState()
		newRG := curState.ReplayGain
		newCF := curState.Crossfade
		audioChanged := false

		for k, v := range payload {
			switch k {
			case "max_volume_limit":
				limit, err := strconv.Atoi(strings.TrimSpace(v))
				if err != nil || limit < 30 || limit > 100 {
					http.Error(w, "max_volume_limit inválido (30..100)", http.StatusBadRequest)
					return
				}
				s.player.SetMaxVolumeLimit(limit)
			case "replay_gain":
				rg, ok := mpd.NormalizeReplayGainMode(v)
				if !ok {
					http.Error(w, "replay_gain inválido (off, track, album, auto)", http.StatusBadRequest)
					return
				}
				newRG = rg
				audioChanged = true
			case "crossfade":
				cf, err := strconv.Atoi(strings.TrimSpace(v))
				if err != nil || cf < 0 || cf > 15 {
					http.Error(w, "crossfade inválido (0..15)", http.StatusBadRequest)
					return
				}
				newCF = cf
				audioChanged = true
			case "soft_fade":
				val := strings.ToLower(strings.TrimSpace(v))
				if val != "true" && val != "false" && val != "1" && val != "0" {
					http.Error(w, "soft_fade inválido (true/false)", http.StatusBadRequest)
					return
				}
				s.player.SetSoftFade(val == "true" || val == "1")
			case "autoplay_similar":
				val := strings.ToLower(strings.TrimSpace(v))
				if val != "true" && val != "false" && val != "1" && val != "0" {
					http.Error(w, "autoplay_similar inválido (true/false)", http.StatusBadRequest)
					return
				}
				s.player.SetAutoplayEnabled(val == "true" || val == "1")
			default:
				http.Error(w, "clave de ajuste no permitida", http.StatusBadRequest)
				return
			}
		}

		if audioChanged {
			s.player.SetAudioConfig(newRG, newCF)
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
		"soft_fade":        s.database.GetSetting("soft_fade", "true"),
		"autoplay_similar": s.database.GetSetting("autoplay_similar", "true"),
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
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})

	streamURL := s.streamUpstreamURL
	if streamURL == "" {
		mpdHost := getEnv("MPD_HOST", "localhost")
		streamURL = fmt.Sprintf("http://%s:4082/", mpdHost)
	}

	req, err := http.NewRequestWithContext(r.Context(), "GET", streamURL, nil)
	if err != nil {
		http.Error(w, "error al preparar stream", http.StatusInternalServerError)
		return
	}

	client := s.streamClient
	if client == nil {
		client = newAudioStreamClient()
	}

	var resp *http.Response
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		resp, lastErr = client.Do(req)
		if lastErr == nil && resp != nil && resp.StatusCode == http.StatusOK {
			break
		}
		if resp != nil {
			_ = resp.Body.Close()
			resp = nil
		}
		if attempt < 2 {
			select {
			case <-r.Context().Done():
				return
			case <-time.After(150 * time.Millisecond):
			}
		}
	}

	if resp == nil {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte("El stream de audio no está activo actualmente. Inicia una pista en AuraBox para escuchar."))
		return
	}
	defer resp.Body.Close()

	w.Header().Set("Content-Type", "audio/mpeg")
	w.Header().Set("Cache-Control", "no-cache, no-store")
	w.Header().Set("Pragma", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	flusher, canFlush := w.(http.Flusher)
	if canFlush {
		flusher.Flush()
	}

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

// hasControlChars verifica si una cadena de configuración contiene caracteres de control o saltos de línea peligrosos.
func hasControlChars(s string) bool {
	return strings.ContainsAny(s, "\r\n\x00")
}

// handleNASConfig gestiona la consulta y guardado seguro de parámetros NAS sin persistir contraseñas en claro en SQLite.
func (s *Server) handleNASConfig(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		var payload struct {
			Alias    string `json:"alias"`
			Host     string `json:"host"`
			Share    string `json:"share"`
			Username string `json:"username"`
			Password string `json:"password"`
			Options  string `json:"options"`
		}
		if err := decodeJSONBody(w, r, &payload); err != nil {
			http.Error(w, "json inválido", http.StatusBadRequest)
			return
		}

		for _, field := range []string{payload.Alias, payload.Host, payload.Share, payload.Username, payload.Options} {
			if hasControlChars(field) {
				http.Error(w, "parámetros NAS inválidos", http.StatusBadRequest)
				return
			}
		}

		if alias := strings.TrimSpace(payload.Alias); alias != "" {
			_ = s.database.SetSetting("nas_alias", alias)
		}
		if host := strings.TrimSpace(payload.Host); host != "" {
			_ = s.database.SetSetting("nas_host", host)
		}
		if share := strings.TrimSpace(payload.Share); share != "" {
			_ = s.database.SetSetting("nas_share", share)
		}
		if user := strings.TrimSpace(payload.Username); user != "" {
			_ = s.database.SetSetting("nas_user", user)
		}
		if opts := strings.TrimSpace(payload.Options); opts != "" {
			_ = s.database.SetSetting("nas_options", opts)
		}

		// Disparar re-escaneo reactivo sin almacenar nas_pass en texto plano dentro de SQLite.
		s.scanner.ScanLibraryAll()

		jsonResponse(w, http.StatusOK, map[string]string{
			"status":  "ok",
			"message": "Configuración NAS guardada (credenciales gestionadas vía .env del host). Re-escaneo iniciado.",
		})
		return
	}

	// GET
	nasHost := s.database.GetSetting("nas_host", getEnv("NAS_HOST", ""))
	nasShare := s.database.GetSetting("nas_share", getEnv("NAS_SHARE", "music"))
	nasUser := s.database.GetSetting("nas_user", getEnv("NAS_USER", ""))
	nasAlias := s.database.GetSetting("nas_alias", getEnv("NAS_ALIAS", "mi-nas"))
	nasOpts := s.database.GetSetting("nas_options", getEnv("NAS_OPTIONS", "vers=3.0,sec=ntlmssp,ro,iocharset=utf8,rsize=65536"))

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

// httpsRedirectMiddleware redirige HTTP a HTTPS si FORCE_HTTPS=true o si entra sin puerto explícito (puerto 80).
func (s *Server) httpsRedirectMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.certManager != nil && r.Host != "" {
			s.certManager.EnsureHost(r.Host)
		}

		if r.TLS == nil &&
			r.Method != http.MethodOptions &&
			s.httpsPort != "" &&
			s.certManager != nil &&
			r.Host != "" &&
			r.URL.Path != "/api/security/ca.crt" &&
			r.URL.Path != "/api/stream" {
			_, reqPort, splitErr := net.SplitHostPort(r.Host)
			arrivedWithoutAppPort := (splitErr != nil || reqPort == "" || reqPort == "80")
			if s.forceHTTPS || arrivedWithoutAppPort {
				target := buildHTTPSURLForHost(r.Host, s.httpsPublicPort, r.URL.RequestURI())
				if target != "" {
					http.Redirect(w, r, target, http.StatusTemporaryRedirect)
					return
				}
			}
		}

		next.ServeHTTP(w, r)
	})
}

// handleSecurityStatus informa sobre el estado TLS/HTTPS activo, URL segura y hosts cubiertos por el certificado.
func (s *Server) handleSecurityStatus(w http.ResponseWriter, r *http.Request) {
	if s.certManager != nil && r.Host != "" {
		s.certManager.EnsureHost(r.Host)
	}
	var sanHosts []string
	caReady := false
	if s.certManager != nil {
		sanHosts = s.certManager.SANHosts()
		caReady = s.certManager.HasCA()
	}

	reqHTTPS := (r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https"))
	httpsURL := ""
	if s.httpsPort != "" && s.certManager != nil && r.Host != "" {
		httpsURL = buildHTTPSURLForHost(r.Host, s.httpsPublicPort, "/")
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"https_enabled":     s.httpsPort != "" && s.certManager != nil,
		"https_port":        s.httpsPort,
		"https_public_port": s.httpsPublicPort,
		"force_https":       s.forceHTTPS,
		"request_is_https":  reqHTTPS,
		"ca_ready":          caReady,
		"san_hosts":         sanHosts,
		"https_url":         httpsURL,
	})
}

// handleDownloadCACert sirve la autoridad certificadora local en PEM para eliminar avisos en navegadores de red LAN.
func (s *Server) handleDownloadCACert(w http.ResponseWriter, r *http.Request) {
	if s.certManager == nil || !s.certManager.HasCA() {
		http.Error(w, "CA local no disponible (posible certificado externo configurado)", http.StatusNotFound)
		return
	}
	pemBytes := s.certManager.CACertPEM()
	w.Header().Set("Content-Type", "application/x-x509-ca-cert")
	w.Header().Set("Content-Disposition", `attachment; filename="aurabox-ca.crt"`)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(pemBytes)
}

// handlePlayerCoverAutoFetch busca online y actualiza la carátula para la pista actual o una pista indicada por ID.
func (s *Server) handlePlayerCoverAutoFetch(w http.ResponseWriter, r *http.Request) {
	if s.covers == nil {
		http.Error(w, "servicio de carátulas no configurado", http.StatusServiceUnavailable)
		return
	}

	var title, artist, album, trackPath string
	var trackID int64

	// Permitir consulta por track_id específico o usar la canción sonando actualmente
	if idStr := r.URL.Query().Get("track_id"); idStr != "" {
		if id, err := strconv.ParseInt(idStr, 10, 64); err == nil && id > 0 {
			trackID = id
			if tr, err := s.database.GetTrackByID(trackID); err == nil && tr != nil {
				title = tr.Title
				artist = tr.Artist
				album = tr.Album
				trackPath = tr.Path
			}
		}
	}

	if title == "" && s.player != nil {
		st := s.player.GetState()
		if st.CurrentTrack != nil {
			title = st.CurrentTrack.Title
			artist = st.CurrentTrack.Artist
			album = st.CurrentTrack.Album
			trackPath = st.CurrentTrack.Path
			trackID = st.CurrentTrack.ID
		}
	}

	if title == "" {
		http.Error(w, "no hay canción en reproducción ni track_id válido", http.StatusBadRequest)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()

	coverURL, err := s.covers.FetchAndCacheTrackCover(ctx, title, artist, album)
	if err != nil || coverURL == "" {
		log.Printf("[Covers] No se encontró portada online para %q (%s): %v", title, artist, err)
		http.Error(w, "no se encontró portada online para la canción", http.StatusNotFound)
		return
	}

	// Persistir en base de datos si la pista o álbum existen localmente
	if trackID > 0 {
		_ = s.database.UpdateTrackCover(trackID, coverURL)
	}
	if trackPath != "" {
		_ = s.database.UpdateTrackCoverByPath(trackPath, coverURL)
	}
	if album != "" && !strings.EqualFold(album, "Sencillos") && !strings.EqualFold(album, "Singles") {
		_ = s.database.UpdateAlbumCover(album, coverURL)
	}

	// Propagar en memoria a la cola y al reproductor activo
	if s.player != nil {
		s.player.UpdateTrackCoverInQueue(trackPath, coverURL)
		if album != "" {
			s.player.UpdateCurrentTrackCoverIfAlbumMatches(album, coverURL)
		}
	}

	if s.eventsHub != nil {
		if s.player != nil {
			s.eventsHub.Broadcast("player_state", s.player.GetState())
		}
		s.eventsHub.Broadcast("library_updated", map[string]interface{}{
			"cover_updated": true,
			"cover_url":     coverURL,
		})
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"status":    "ok",
		"cover_url": coverURL,
		"title":     title,
		"artist":    artist,
	})
}

// handlePodcasts devuelve la lista de podcasts suscritos con sus totales de episodios, opcionalmente filtrada por favoritos.
func (s *Server) handlePodcasts(w http.ResponseWriter, r *http.Request) {
	var podcastsList []db.Podcast
	var err error
	if r.URL.Query().Get("favorites") == "true" || r.URL.Query().Get("favorite") == "1" {
		podcastsList, err = s.database.GetFavoritePodcasts()
	} else {
		podcastsList, err = s.database.GetPodcasts()
	}
	if err != nil {
		log.Printf("[Podcasts] Error obteniendo lista: %v", err)
		http.Error(w, "error al consultar podcasts en base de datos", http.StatusInternalServerError)
		return
	}
	if podcastsList == nil {
		podcastsList = []db.Podcast{}
	}
	jsonResponse(w, http.StatusOK, podcastsList)
}

// handlePodcastFavorite conmuta el estado de favorito de un canal de podcast suscrito.
func (s *Server) handlePodcastFavorite(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		ID int64 `json:"id"`
	}
	if err := decodeJSONBody(w, r, &payload); err != nil || payload.ID <= 0 {
		http.Error(w, "id de podcast inválido", http.StatusBadRequest)
		return
	}

	fav, err := s.database.TogglePodcastFavorite(payload.ID)
	if err != nil {
		log.Printf("[Podcasts] Error conmutando favorito para podcast %d: %v", payload.ID, err)
		http.Error(w, "error al actualizar favorito", http.StatusInternalServerError)
		return
	}

	if s.eventsHub != nil {
		s.eventsHub.Broadcast("podcasts_updated", map[string]interface{}{
			"action":     "favorite_toggled",
			"podcast_id": payload.ID,
			"favorite":   fav,
		})
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"podcast_id": payload.ID,
		"favorite":   fav,
	})
}

// handlePodcastSubscribe registra un nuevo feed RSS de podcast y descarga su catálogo inicial de episodios.
func (s *Server) handlePodcastSubscribe(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		FeedURL string `json:"feed_url"`
	}
	if err := decodeJSONBody(w, r, &payload); err != nil || strings.TrimSpace(payload.FeedURL) == "" {
		http.Error(w, "feed_url requerido en formato JSON", http.StatusBadRequest)
		return
	}

	cleanURL, urlErr := mpd.ValidateHTTPStreamURL(payload.FeedURL)
	if urlErr != nil {
		http.Error(w, urlErr.Error(), http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	client := s.httpClient
	if client == nil {
		client = mpd.NewSafeExternalHTTPClient(15 * time.Second)
	}
	pod, episodes, err := podcasts.FetchAndParseFeed(ctx, client, cleanURL)
	if err != nil {
		log.Printf("[Podcasts] Error descargando feed %s: %v", cleanURL, err)
		http.Error(w, fmt.Sprintf("no se pudo descargar el feed: %v", err), http.StatusBadRequest)
		return
	}

	dbPod := &db.Podcast{
		Title:         pod.Title,
		Author:        pod.Author,
		FeedURL:       pod.FeedURL,
		Description:   pod.Description,
		CoverURL:      pod.CoverURL,
		Link:          pod.Link,
		LastBuildDate: pod.LastBuildDate,
		EpisodeCount:  len(episodes),
	}

	podID, err := s.database.SavePodcast(dbPod)
	if err != nil {
		log.Printf("[Podcasts] Error guardando podcast %s: %v", pod.Title, err)
		http.Error(w, "error al guardar podcast en base de datos", http.StatusInternalServerError)
		return
	}

	var dbEpisodes []db.PodcastEpisode
	for _, ep := range episodes {
		dbEpisodes = append(dbEpisodes, db.PodcastEpisode{
			PodcastID:     podID,
			GUID:          ep.GUID,
			Title:         ep.Title,
			Description:   ep.Description,
			EnclosureURL:  ep.EnclosureURL,
			EnclosureType: ep.EnclosureType,
			Duration:      ep.Duration,
			PubDate:       ep.PubDate,
		})
	}

	_ = s.database.SavePodcastEpisodes(podID, dbEpisodes)

	if s.eventsHub != nil {
		s.eventsHub.Broadcast("podcasts_updated", map[string]interface{}{
			"action":     "subscribed",
			"podcast_id": podID,
		})
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"status":        "subscribed",
		"podcast_id":    podID,
		"title":         pod.Title,
		"episode_count": len(episodes),
		"cover_url":     pod.CoverURL,
	})
}

// handlePodcastUnsubscribe elimina una suscripción a un podcast y borra sus episodios asociados.
func (s *Server) handlePodcastUnsubscribe(w http.ResponseWriter, r *http.Request) {
	var podID int64
	if idStr := r.URL.Query().Get("id"); idStr != "" {
		if id, err := strconv.ParseInt(idStr, 10, 64); err == nil && id > 0 {
			podID = id
		}
	}
	if podID <= 0 {
		var payload struct {
			ID int64 `json:"id"`
		}
		if err := decodeJSONBody(w, r, &payload); err == nil && payload.ID > 0 {
			podID = payload.ID
		}
	}

	if podID <= 0 {
		http.Error(w, "parámetro id de podcast requerido", http.StatusBadRequest)
		return
	}

	if err := s.database.DeletePodcast(podID); err != nil {
		log.Printf("[Podcasts] Error eliminando podcast %d: %v", podID, err)
		http.Error(w, "error al eliminar podcast", http.StatusInternalServerError)
		return
	}

	if s.eventsHub != nil {
		s.eventsHub.Broadcast("podcasts_updated", map[string]interface{}{
			"action":     "unsubscribed",
			"podcast_id": podID,
		})
	}

	jsonResponse(w, http.StatusOK, map[string]string{
		"status": "unsubscribed",
	})
}

// handlePodcastEpisodes devuelve los episodios indexados para un podcast específico.
func (s *Server) handlePodcastEpisodes(w http.ResponseWriter, r *http.Request) {
	idStr := r.URL.Query().Get("podcast_id")
	if idStr == "" {
		idStr = r.URL.Query().Get("id")
	}
	podID, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || podID <= 0 {
		http.Error(w, "parámetro podcast_id requerido", http.StatusBadRequest)
		return
	}

	pod, episodes, err := s.database.GetPodcastByID(podID)
	if err != nil || pod == nil {
		http.Error(w, "podcast no encontrado", http.StatusNotFound)
		return
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"podcast":  pod,
		"episodes": episodes,
		"total":    len(episodes),
	})
}

// handlePodcastEpisodeFavorite conmuta el estado de favorito de un episodio de podcast.
func (s *Server) handlePodcastEpisodeFavorite(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		EpisodeID int64 `json:"episode_id"`
		ID        int64 `json:"id"`
	}
	if err := decodeJSONBody(w, r, &payload); err != nil {
		http.Error(w, "json inválido", http.StatusBadRequest)
		return
	}
	epID := payload.EpisodeID
	if epID <= 0 {
		epID = payload.ID
	}
	if epID <= 0 {
		http.Error(w, "episode_id inválido", http.StatusBadRequest)
		return
	}

	fav, err := s.database.ToggleEpisodeFavorite(epID)
	if err != nil {
		log.Printf("[Podcasts] Error conmutando favorito para episodio %d: %v", epID, err)
		http.Error(w, "error al actualizar favorito de episodio", http.StatusInternalServerError)
		return
	}

	if s.eventsHub != nil {
		s.eventsHub.Broadcast("podcasts_updated", map[string]interface{}{
			"action":     "episode_favorite_toggled",
			"episode_id": epID,
			"favorite":   fav,
		})
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"episode_id": epID,
		"favorite":   fav,
	})
}

// handlePodcastEpisodePlayed conmuta o establece el estado de reproducción de un episodio de podcast.
func (s *Server) handlePodcastEpisodePlayed(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		EpisodeID int64 `json:"episode_id"`
		ID        int64 `json:"id"`
		Played    *bool `json:"played,omitempty"`
	}
	if err := decodeJSONBody(w, r, &payload); err != nil {
		http.Error(w, "json inválido", http.StatusBadRequest)
		return
	}
	epID := payload.EpisodeID
	if epID <= 0 {
		epID = payload.ID
	}
	if epID <= 0 {
		http.Error(w, "episode_id inválido", http.StatusBadRequest)
		return
	}

	var played bool
	var err error
	if payload.Played != nil {
		played = *payload.Played
		err = s.database.MarkEpisodePlayed(epID, played)
	} else {
		played, err = s.database.ToggleEpisodePlayed(epID)
	}

	if err != nil {
		log.Printf("[Podcasts] Error conmutando estado escuchado para episodio %d: %v", epID, err)
		http.Error(w, "error al actualizar estado de episodio", http.StatusInternalServerError)
		return
	}

	if s.eventsHub != nil {
		s.eventsHub.Broadcast("podcasts_updated", map[string]interface{}{
			"action":     "episode_played_toggled",
			"episode_id": epID,
			"played":     played,
		})
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"episode_id": epID,
		"played":     played,
	})
}

// handlePodcastPlay encola o reproduce inmediatamente un episodio en el motor MPD con reanudación de posición.
func (s *Server) handlePodcastPlay(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		EpisodeID int64   `json:"episode_id"`
		Mode      string  `json:"mode"` // "replace" (por defecto), "next", "append"
		FromStart bool    `json:"from_start,omitempty"`
		Position  float64 `json:"position,omitempty"`
	}
	if err := decodeJSONBody(w, r, &payload); err != nil || payload.EpisodeID <= 0 {
		http.Error(w, "episode_id inválido o faltante", http.StatusBadRequest)
		return
	}

	ep, pod, err := s.database.GetPodcastEpisodeByID(payload.EpisodeID)
	if err != nil || ep == nil {
		http.Error(w, "episodio no encontrado", http.StatusNotFound)
		return
	}

	coverVal := ep.PodcastCover
	if coverVal == "" && pod != nil {
		coverVal = pod.CoverURL
	}
	if coverVal == "" {
		coverVal = "/assets/default-cover.svg"
	}

	authorVal := pod.Author
	if authorVal == "" {
		authorVal = pod.Title
	}

	tr := db.Track{
		ID:          ep.ID,
		Path:        ep.EnclosureURL,
		Filename:    ep.Title,
		Title:       ep.Title,
		Artist:      authorVal,
		Album:       pod.Title,
		AlbumArtist: pod.Title,
		Genre:       "Podcast",
		Bitrate:     192,
		Format:      "MP3",
		CoverURL:    coverVal,
		Source:      "podcast",
		Duration:    ep.Duration,
	}

	mode := strings.ToLower(strings.TrimSpace(payload.Mode))
	enqueueMode := "replace"
	if mode == "next" {
		enqueueMode = "play_next"
	} else if mode == "append" {
		enqueueMode = "append"
	}

	// Calcular punto de reanudación automática si no se solicitó iniciar de cero
	seekPos := 0.0
	if payload.Position > 0 {
		seekPos = payload.Position
	} else if !payload.FromStart && ep.Progress > 5 && (!ep.Played || (ep.Duration > 0 && ep.Progress < ep.Duration-15)) {
		seekPos = ep.Progress
	}

	s.player.EnqueueTracks([]db.Track{tr}, enqueueMode)

	// Desplazar reproducción de forma no bloqueante tras el encolado
	if seekPos > 0 && enqueueMode == "replace" {
		go func(targetSec float64) {
			time.Sleep(350 * time.Millisecond)
			s.player.Seek(targetSec)
		}(seekPos)
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"status":     "playing",
		"episode_id": ep.ID,
		"title":      ep.Title,
		"podcast":    pod.Title,
		"stream_url": ep.EnclosureURL,
		"resumed":    seekPos > 0,
		"position":   seekPos,
	})
}

// handlePodcastSearch busca programas de podcast en el directorio abierto de Apple Podcasts.
func (s *Server) handlePodcastSearch(w http.ResponseWriter, r *http.Request) {
	q := strings.TrimSpace(r.URL.Query().Get("q"))
	if q == "" {
		jsonResponse(w, http.StatusOK, []podcasts.SearchResult{})
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	results, err := podcasts.SearchOnline(ctx, s.httpClient, q, 25)
	if err != nil {
		log.Printf("[Podcasts] Error en búsqueda online: %v", err)
		http.Error(w, "error al buscar podcasts en internet", http.StatusBadGateway)
		return
	}

	jsonResponse(w, http.StatusOK, results)
}

// handlePodcastOPMLImport procesa archivos o texto OPML y suscribe en segundo plano los podcasts descubiertos.
func (s *Server) handlePodcastOPMLImport(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 11<<20)
	var opmlData []byte

	// 1. Intentar como subida de archivo multipart
	if strings.Contains(r.Header.Get("Content-Type"), "multipart/form-data") {
		if err := r.ParseMultipartForm(10 << 20); err == nil {
			file, _, fErr := r.FormFile("file")
			if fErr == nil {
				defer file.Close()
				opmlData, _ = io.ReadAll(io.LimitReader(file, 10<<20))
			}
		}
	}

	// 2. Intentar como JSON {"opml": "<xml...>"} o texto plano en cuerpo
	if len(opmlData) == 0 {
		body, _ := io.ReadAll(io.LimitReader(r.Body, 10<<20))
		var jsonPayload struct {
			OPML string `json:"opml"`
		}
		if err := json.Unmarshal(body, &jsonPayload); err == nil && jsonPayload.OPML != "" {
			opmlData = []byte(jsonPayload.OPML)
		} else if len(body) > 0 {
			opmlData = body
		}
	}

	if len(opmlData) == 0 {
		http.Error(w, "archivo o contenido OPML vacío", http.StatusBadRequest)
		return
	}

	outlines, err := podcasts.ParseOPML(opmlData)
	if err != nil {
		log.Printf("[OPML] Error al interpretar archivo: %v", err)
		http.Error(w, fmt.Sprintf("archivo OPML no válido: %v", err), http.StatusBadRequest)
		return
	}

	if len(outlines) == 0 {
		http.Error(w, "no se encontraron feeds de podcast en el archivo OPML", http.StatusBadRequest)
		return
	}

	// Procesar importación en segundo plano con cliente HTTP anti-SSRF
	go func(feeds []podcasts.OPMLOutline) {
		imported := 0
		client := mpd.NewSafeExternalHTTPClient(15 * time.Second)
		for _, item := range feeds {
			safeFeedURL, urlErr := mpd.ValidateHTTPStreamURL(item.XMLURL)
			if urlErr != nil {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			pod, episodes, pErr := podcasts.FetchAndParseFeed(ctx, client, safeFeedURL)
			cancel()
			if pErr != nil || pod == nil {
				continue
			}

			dbPod := &db.Podcast{
				Title:         pod.Title,
				Author:        pod.Author,
				FeedURL:       pod.FeedURL,
				Description:   pod.Description,
				CoverURL:      pod.CoverURL,
				Link:          pod.Link,
				LastBuildDate: pod.LastBuildDate,
				EpisodeCount:  len(episodes),
			}
			podID, sErr := s.database.SavePodcast(dbPod)
			if sErr != nil || podID <= 0 {
				continue
			}

			var dbEpisodes []db.PodcastEpisode
			for _, ep := range episodes {
				dbEpisodes = append(dbEpisodes, db.PodcastEpisode{
					PodcastID:     podID,
					GUID:          ep.GUID,
					Title:         ep.Title,
					Description:   ep.Description,
					EnclosureURL:  ep.EnclosureURL,
					EnclosureType: ep.EnclosureType,
					Duration:      ep.Duration,
					PubDate:       ep.PubDate,
				})
			}
			_ = s.database.SavePodcastEpisodes(podID, dbEpisodes)
			imported++

			if s.eventsHub != nil {
				s.eventsHub.Broadcast("podcasts_updated", map[string]interface{}{
					"action":   "feed_imported",
					"imported": imported,
					"total":    len(feeds),
					"title":    pod.Title,
				})
			}
		}

		if s.eventsHub != nil {
			s.eventsHub.Broadcast("podcasts_updated", map[string]interface{}{
				"action":   "import_completed",
				"imported": imported,
				"total":    len(feeds),
			})
		}
		log.Printf("[OPML] Importación completada: %d de %d feeds registrados", imported, len(feeds))
	}(outlines)

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"status":           "importing",
		"total_discovered": len(outlines),
		"message":          fmt.Sprintf("Importando %d feeds en segundo plano", len(outlines)),
	})
}

// handlePodcastOPMLExport genera y descarga el archivo OPML con todas las suscripciones actuales de AuraBox.
func (s *Server) handlePodcastOPMLExport(w http.ResponseWriter, r *http.Request) {
	dbList, err := s.database.GetPodcasts()
	if err != nil {
		http.Error(w, "error al leer podcasts para exportar", http.StatusInternalServerError)
		return
	}

	podList := make([]podcasts.Podcast, 0, len(dbList))
	for _, p := range dbList {
		podList = append(podList, podcasts.Podcast{
			Title:   p.Title,
			FeedURL: p.FeedURL,
			Link:    p.Link,
		})
	}

	xmlBytes, err := podcasts.GenerateOPML(podList)
	if err != nil {
		http.Error(w, "error al generar documento OPML", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="aurabox-podcasts.opml"`)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(xmlBytes)
}

// handlePodcastsRefresh actualiza los episodios de todos o de un podcast específico.
func (s *Server) handlePodcastsRefresh(w http.ResponseWriter, r *http.Request) {
	var podcastsList []db.Podcast
	var targetID int64

	if idStr := r.URL.Query().Get("id"); idStr != "" {
		if id, err := strconv.ParseInt(idStr, 10, 64); err == nil && id > 0 {
			targetID = id
		}
	} else if idStr := r.URL.Query().Get("podcast_id"); idStr != "" {
		if id, err := strconv.ParseInt(idStr, 10, 64); err == nil && id > 0 {
			targetID = id
		}
	}

	if targetID <= 0 && r.Body != nil {
		var payload struct {
			ID        int64 `json:"id"`
			PodcastID int64 `json:"podcast_id"`
		}
		if err := decodeJSONBody(w, r, &payload); err == nil {
			if payload.PodcastID > 0 {
				targetID = payload.PodcastID
			} else if payload.ID > 0 {
				targetID = payload.ID
			}
		}
	}

	if targetID > 0 {
		if p, _, err := s.database.GetPodcastByID(targetID); err == nil && p != nil {
			podcastsList = append(podcastsList, *p)
		}
	}

	if len(podcastsList) == 0 {
		var err error
		podcastsList, err = s.database.GetPodcasts()
		if err != nil {
			http.Error(w, "error al leer podcasts", http.StatusInternalServerError)
			return
		}
	}

	go func(list []db.Podcast) {
		client := mpd.NewSafeExternalHTTPClient(15 * time.Second)
		refreshed := 0
		for _, p := range list {
			safeFeedURL, urlErr := mpd.ValidateHTTPStreamURL(p.FeedURL)
			if urlErr != nil {
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			_, episodes, err := podcasts.FetchAndParseFeed(ctx, client, safeFeedURL)
			cancel()
			if err != nil {
				continue
			}

			var dbEpisodes []db.PodcastEpisode
			for _, ep := range episodes {
				dbEpisodes = append(dbEpisodes, db.PodcastEpisode{
					PodcastID:     p.ID,
					GUID:          ep.GUID,
					Title:         ep.Title,
					Description:   ep.Description,
					EnclosureURL:  ep.EnclosureURL,
					EnclosureType: ep.EnclosureType,
					Duration:      ep.Duration,
					PubDate:       ep.PubDate,
				})
			}
			_ = s.database.SavePodcastEpisodes(p.ID, dbEpisodes)
			refreshed++
		}
		if s.eventsHub != nil {
			s.eventsHub.Broadcast("podcasts_updated", map[string]interface{}{
				"action":    "refresh_completed",
				"refreshed": refreshed,
				"total":     len(list),
			})
		}
	}(podcastsList)

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"status":  "refreshing",
		"total":   len(podcastsList),
		"message": "Actualizando feeds en segundo plano",
	})
}

// handlePodcastProgress guarda los segundos reproducidos y el estado de visto de un episodio.
func (s *Server) handlePodcastProgress(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		EpisodeID int64    `json:"episode_id"`
		Progress  float64  `json:"progress"`
		Played    *bool    `json:"played,omitempty"`
	}
	if err := decodeJSONBody(w, r, &payload); err != nil || payload.EpisodeID <= 0 {
		http.Error(w, "episode_id inválido", http.StatusBadRequest)
		return
	}

	if payload.Progress >= 0 {
		_ = s.database.UpdateEpisodeProgress(payload.EpisodeID, payload.Progress)
	}
	if payload.Played != nil {
		_ = s.database.MarkEpisodePlayed(payload.EpisodeID, *payload.Played)
	}

	jsonResponse(w, http.StatusOK, map[string]string{
		"status": "updated",
	})
}

// buildHTTPSURLForHost construye la URL HTTPS segura conservando IP/host y evitando redirecciones abiertas.
func buildHTTPSURLForHost(rawHost, publicPort, reqURI string) string {
	hostOnly := strings.TrimSpace(rawHost)
	if hostOnly == "" {
		return ""
	}
	if h, _, err := net.SplitHostPort(hostOnly); err == nil {
		hostOnly = h
	}
	hostOnly = strings.TrimPrefix(hostOnly, "[")
	hostOnly = strings.TrimSuffix(hostOnly, "]")
	if hostOnly == "" || strings.ContainsAny(hostOnly, " \t\r\n/@\\") {
		return ""
	}
	if !strings.HasPrefix(reqURI, "/") {
		reqURI = "/"
	}

	authority := hostOnly
	if strings.Contains(hostOnly, ":") {
		authority = "[" + hostOnly + "]"
	}
	if publicPort != "" && publicPort != "443" {
		authority = net.JoinHostPort(hostOnly, publicPort)
	}
	return "https://" + authority + reqURI
}

// splitCSVTrim divide listas separadas por comas eliminando espacios y entradas vacías.
func splitCSVTrim(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	var out []string
	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		if trimmed != "" {
			out = append(out, trimmed)
		}
	}
	return out
}

// getEnv obtiene una variable de entorno o recurre a un valor por defecto.
func getEnv(key, defaultVal string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultVal
}

// handlePlaylists gestiona el listado general (GET) y la creación de una nueva playlist (POST).
func (s *Server) handlePlaylists(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		playlists, err := s.database.GetAllPlaylists()
		if err != nil {
			log.Printf("[Playlists] Error listando playlists: %v", err)
			http.Error(w, "error al consultar playlists", http.StatusInternalServerError)
			return
		}
		jsonResponse(w, http.StatusOK, playlists)
		return
	}

	// Creación de nueva playlist
	var payload struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := decodeJSONBody(w, r, &payload); err != nil {
		payload.Name = r.FormValue("name")
		payload.Description = r.FormValue("description")
	}

	cleanName := strings.TrimSpace(payload.Name)
	if cleanName == "" {
		http.Error(w, "el nombre de la playlist no puede estar vacío", http.StatusBadRequest)
		return
	}

	pl, err := s.database.CreatePlaylist(cleanName, payload.Description)
	if err != nil {
		log.Printf("[Playlists] Error creando playlist %q: %v", cleanName, err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if s.eventsHub != nil {
		s.eventsHub.Broadcast("playlists_updated", map[string]interface{}{
			"action":      "create",
			"playlist_id": pl.ID,
			"name":        pl.Name,
		})
	}

	jsonResponse(w, http.StatusCreated, pl)
}

// handlePlaylistDetail devuelve la información detallada de una playlist y su listado ordenado de canciones.
func (s *Server) handlePlaylistDetail(w http.ResponseWriter, r *http.Request) {
	idStr := r.URL.Query().Get("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "identificador de playlist inválido", http.StatusBadRequest)
		return
	}

	detail, err := s.database.GetPlaylistByID(id)
	if err != nil {
		log.Printf("[Playlists] Error consultando playlist %d: %v", id, err)
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}

	jsonResponse(w, http.StatusOK, detail)
}

// handlePlaylistUpdate actualiza el nombre y la descripción de una playlist existente.
func (s *Server) handlePlaylistUpdate(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		ID          int64  `json:"id"`
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := decodeJSONBody(w, r, &payload); err != nil {
		id, _ := strconv.ParseInt(r.FormValue("id"), 10, 64)
		payload.ID = id
		payload.Name = r.FormValue("name")
		payload.Description = r.FormValue("description")
	}

	if payload.ID <= 0 {
		idStr := r.URL.Query().Get("id")
		payload.ID, _ = strconv.ParseInt(idStr, 10, 64)
	}

	if payload.ID <= 0 {
		http.Error(w, "identificador de playlist inválido", http.StatusBadRequest)
		return
	}

	cleanName := strings.TrimSpace(payload.Name)
	if cleanName == "" {
		http.Error(w, "el nombre de la playlist no puede estar vacío", http.StatusBadRequest)
		return
	}

	if err := s.database.UpdatePlaylist(payload.ID, cleanName, payload.Description); err != nil {
		log.Printf("[Playlists] Error actualizando playlist %d: %v", payload.ID, err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if s.eventsHub != nil {
		s.eventsHub.Broadcast("playlists_updated", map[string]interface{}{
			"action":      "update",
			"playlist_id": payload.ID,
			"name":        cleanName,
		})
	}

	jsonResponse(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handlePlaylistDelete elimina definitivamente una lista de reproducción.
func (s *Server) handlePlaylistDelete(w http.ResponseWriter, r *http.Request) {
	idStr := r.URL.Query().Get("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		var payload struct {
			ID int64 `json:"id"`
		}
		if errJSON := decodeJSONBody(w, r, &payload); errJSON == nil && payload.ID > 0 {
			id = payload.ID
		}
	}

	if id <= 0 {
		http.Error(w, "identificador de playlist inválido", http.StatusBadRequest)
		return
	}

	if err := s.database.DeletePlaylist(id); err != nil {
		log.Printf("[Playlists] Error eliminando playlist %d: %v", id, err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if s.eventsHub != nil {
		s.eventsHub.Broadcast("playlists_updated", map[string]interface{}{
			"action":      "delete",
			"playlist_id": id,
		})
	}

	jsonResponse(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handlePlaylistTracksAdd añade una o varias pistas a una lista de reproducción.
func (s *Server) handlePlaylistTracksAdd(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		PlaylistID int64   `json:"playlist_id"`
		TrackID    int64   `json:"track_id"`
		TrackIDs   []int64 `json:"track_ids"`
	}
	if err := decodeJSONBody(w, r, &payload); err != nil {
		http.Error(w, "cuerpo de petición JSON inválido", http.StatusBadRequest)
		return
	}

	if payload.PlaylistID <= 0 {
		http.Error(w, "identificador de playlist requerido", http.StatusBadRequest)
		return
	}

	trackIDs := payload.TrackIDs
	if payload.TrackID > 0 {
		trackIDs = append(trackIDs, payload.TrackID)
	}

	if len(trackIDs) == 0 {
		http.Error(w, "no se han indicado pistas para agregar", http.StatusBadRequest)
		return
	}

	added, err := s.database.AddTracksToPlaylist(payload.PlaylistID, trackIDs)
	if err != nil {
		log.Printf("[Playlists] Error agregando pistas a playlist %d: %v", payload.PlaylistID, err)
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	if s.eventsHub != nil {
		s.eventsHub.Broadcast("playlists_updated", map[string]interface{}{
			"action":      "add_tracks",
			"playlist_id": payload.PlaylistID,
			"added":       added,
		})
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"status": "ok",
		"added":  added,
	})
}

// handlePlaylistTracksRemove retira una canción de la lista según su posición ordinal.
func (s *Server) handlePlaylistTracksRemove(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		PlaylistID int64 `json:"playlist_id"`
		Position   int   `json:"position"`
	}
	if err := decodeJSONBody(w, r, &payload); err != nil {
		plID, _ := strconv.ParseInt(r.URL.Query().Get("playlist_id"), 10, 64)
		pos, _ := strconv.Atoi(r.URL.Query().Get("position"))
		payload.PlaylistID = plID
		payload.Position = pos
	}

	if payload.PlaylistID <= 0 || payload.Position < 0 {
		http.Error(w, "parámetros de playlist o posición inválidos", http.StatusBadRequest)
		return
	}

	if err := s.database.RemoveTrackFromPlaylist(payload.PlaylistID, payload.Position); err != nil {
		log.Printf("[Playlists] Error eliminando pista en pos %d de playlist %d: %v", payload.Position, payload.PlaylistID, err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if s.eventsHub != nil {
		s.eventsHub.Broadcast("playlists_updated", map[string]interface{}{
			"action":      "remove_track",
			"playlist_id": payload.PlaylistID,
			"position":    payload.Position,
		})
	}

	jsonResponse(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handlePlaylistTracksReorder modifica el orden de una canción trasladándola de una posición a otra.
func (s *Server) handlePlaylistTracksReorder(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		PlaylistID int64 `json:"playlist_id"`
		From       int   `json:"from"`
		To         int   `json:"to"`
	}
	if err := decodeJSONBody(w, r, &payload); err != nil {
		plID, _ := strconv.ParseInt(r.URL.Query().Get("playlist_id"), 10, 64)
		from, _ := strconv.Atoi(r.URL.Query().Get("from"))
		to, _ := strconv.Atoi(r.URL.Query().Get("to"))
		payload.PlaylistID = plID
		payload.From = from
		payload.To = to
	}

	if payload.PlaylistID <= 0 || payload.From < 0 || payload.To < 0 {
		http.Error(w, "parámetros de reordenación inválidos", http.StatusBadRequest)
		return
	}

	if err := s.database.ReorderPlaylistTrack(payload.PlaylistID, payload.From, payload.To); err != nil {
		log.Printf("[Playlists] Error reordenando pista en playlist %d (%d -> %d): %v", payload.PlaylistID, payload.From, payload.To, err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if s.eventsHub != nil {
		s.eventsHub.Broadcast("playlists_updated", map[string]interface{}{
			"action":      "reorder",
			"playlist_id": payload.PlaylistID,
			"from":        payload.From,
			"to":          payload.To,
		})
	}

	jsonResponse(w, http.StatusOK, map[string]string{"status": "ok"})
}

// handlePlaylistPlay carga y reproduce inmediatamente (o anexa a la cola) las canciones de una playlist.
func (s *Server) handlePlaylistPlay(w http.ResponseWriter, r *http.Request) {
	var payload struct {
		ID      int64  `json:"id"`
		Mode    string `json:"mode"`    // "play_now" o "append"
		Shuffle bool   `json:"shuffle"` // true para barajar aleatoriamente
	}
	if err := decodeJSONBody(w, r, &payload); err != nil {
		id, _ := strconv.ParseInt(r.URL.Query().Get("id"), 10, 64)
		payload.ID = id
		payload.Mode = r.URL.Query().Get("mode")
		payload.Shuffle = (r.URL.Query().Get("shuffle") == "true")
	}

	if payload.ID <= 0 {
		idStr := r.URL.Query().Get("id")
		payload.ID, _ = strconv.ParseInt(idStr, 10, 64)
	}

	if payload.ID <= 0 {
		http.Error(w, "identificador de playlist inválido", http.StatusBadRequest)
		return
	}

	if payload.Mode == "" {
		payload.Mode = "play_now"
	}

	detail, err := s.database.GetPlaylistByID(payload.ID)
	if err != nil {
		log.Printf("[Playlists] Error al cargar canciones de la playlist %d: %v", payload.ID, err)
		http.Error(w, "playlist no encontrada", http.StatusNotFound)
		return
	}

	if len(detail.Tracks) == 0 {
		http.Error(w, "la playlist seleccionada no contiene ninguna canción", http.StatusBadRequest)
		return
	}

	tracksToPlay := make([]db.Track, len(detail.Tracks))
	copy(tracksToPlay, detail.Tracks)

	if payload.Shuffle {
		rnd := rand.New(rand.NewSource(time.Now().UnixNano()))
		rnd.Shuffle(len(tracksToPlay), func(i, j int) {
			tracksToPlay[i], tracksToPlay[j] = tracksToPlay[j], tracksToPlay[i]
		})
	}

	if payload.Mode == "play_now" {
		s.player.ReplaceQueue(tracksToPlay, true)
	} else {
		s.player.EnqueueTracks(tracksToPlay, "append")
	}

	jsonResponse(w, http.StatusOK, map[string]interface{}{
		"status":  "ok",
		"count":   len(tracksToPlay),
		"mode":    payload.Mode,
		"shuffle": payload.Shuffle,
	})
}

// handlePlaylistExport genera la descarga del archivo estándar M3U8 con la lista de pistas.
func (s *Server) handlePlaylistExport(w http.ResponseWriter, r *http.Request) {
	idStr := r.URL.Query().Get("id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil || id <= 0 {
		http.Error(w, "identificador de playlist requerido", http.StatusBadRequest)
		return
	}

	filename, content, err := s.database.ExportPlaylistM3U(id)
	if err != nil {
		log.Printf("[Playlists] Error exportando playlist %d a M3U: %v", id, err)
		http.Error(w, "error al generar archivo M3U", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "audio/x-mpegurl; charset=utf-8")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	_, _ = w.Write([]byte(content))
}

// handlePlaylistImport procesa un archivo M3U/M3U8 subido por multipart o JSON y crea una nueva playlist.
func (s *Server) handlePlaylistImport(w http.ResponseWriter, r *http.Request) {
	var name string
	var m3uContent string

	contentType := r.Header.Get("Content-Type")
	if strings.Contains(contentType, "multipart/form-data") {
		// Límite estricto de 11 MB para formularios M3U protegiendo RAM en Raspberry Pi
		r.Body = http.MaxBytesReader(w, r.Body, 11<<20)
		if err := r.ParseMultipartForm(10 << 20); err != nil {
			http.Error(w, "error al procesar formulario multipart", http.StatusBadRequest)
			return
		}
		name = r.FormValue("name")
		file, _, err := r.FormFile("file")
		if err != nil {
			http.Error(w, "archivo M3U no proporcionado en campo 'file'", http.StatusBadRequest)
			return
		}
		defer file.Close()

		rawBytes, err := io.ReadAll(io.LimitReader(file, 10<<20))
		if err != nil {
			http.Error(w, "error leyendo el archivo M3U", http.StatusInternalServerError)
			return
		}
		m3uContent = string(rawBytes)
	} else {
		var payload struct {
			Name    string `json:"name"`
			Content string `json:"content"`
		}
		if err := decodeJSONBody(w, r, &payload); err != nil {
			http.Error(w, "cuerpo de petición JSON inválido", http.StatusBadRequest)
			return
		}
		name = payload.Name
		m3uContent = payload.Content
	}

	if strings.TrimSpace(m3uContent) == "" {
		http.Error(w, "el contenido del archivo M3U no puede estar vacío", http.StatusBadRequest)
		return
	}

	pl, matchedCount, err := s.database.ImportPlaylistM3U(name, m3uContent)
	if err != nil {
		log.Printf("[Playlists] Error importando M3U: %v", err)
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	if s.eventsHub != nil {
		s.eventsHub.Broadcast("playlists_updated", map[string]interface{}{
			"action":         "import",
			"playlist_id":    pl.ID,
			"name":           pl.Name,
			"matched_tracks": matchedCount,
		})
	}

	jsonResponse(w, http.StatusCreated, map[string]interface{}{
		"status":         "ok",
		"playlist":       pl,
		"matched_tracks": matchedCount,
	})
}

