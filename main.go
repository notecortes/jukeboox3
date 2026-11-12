package main

import (
	"compress/gzip"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
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
	"syscall"
	"time"
	"unicode"

	"aurabox/internal/db"
	"aurabox/internal/events"
	"aurabox/internal/lyrics"
	"aurabox/internal/mpd"
	"aurabox/internal/scanner"
	"aurabox/internal/smartmix"
	"aurabox/internal/tlsutil"
)

// Server encapsula todas las dependencias y servicios activos de AuraBox.
type Server struct {
	database        *db.DB
	eventsHub       *events.Hub
	player          *mpd.Client
	scanner         *scanner.LibraryScanner
	smartmix        *smartmix.SmartMixService
	lyrics          *lyrics.Service
	certManager       *tlsutil.CertManager
	streamClient      *http.Client
	streamUpstreamURL string
	localDir        string
	nasDir          string
	usbDir          string
	coversDir       string
	httpsPort       string
	httpsPublicPort string
	forceHTTPS      bool
	startTime       time.Time
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

	// Inicializar escáner de biblioteca musical
	libScanner := scanner.NewLibraryScanner(database, hub, musicLocal, musicNas, musicUsb, coversDir)

	// Inicializar motor Smart Mix
	smService := smartmix.NewSmartMixService(database)

	// Inicializar servicio de letras LRCLIB
	lyrService := lyrics.NewService()

	srv := &Server{
		database:        database,
		eventsHub:       hub,
		player:          player,
		scanner:         libScanner,
		smartmix:        smService,
		lyrics:          lyrService,
		certManager:     certMgr,
		streamClient:    newAudioStreamClient(),
		localDir:        musicLocal,
		nasDir:          musicNas,
		usbDir:          musicUsb,
		coversDir:       coversDir,
		httpsPort:       httpsPort,
		httpsPublicPort: httpsPublicPort,
		forceHTTPS:      forceHTTPS,
		startTime:       time.Now(),
	}

	// Ejecutar escaneo incremental inicial (la reparación de duraciones corre tras indexar)
	libScanner.ScanLibraryAll()

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

	// 2. Caché de carátulas locales con cabeceras de persistencia inmutables (30 días)
	mux.Handle("/api/covers/", coversCacheMiddleware(http.StripPrefix("/api/covers/", http.FileServer(http.Dir(s.coversDir)))))

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
	mux.HandleFunc("/api/player/outputs/refresh", requireMethod(s.handleRefreshOutputs, http.MethodPost))
	mux.HandleFunc("/api/player/outputs/diagnostics", requireMethod(s.handleAudioDiagnostics, http.MethodGet))
	mux.HandleFunc("/api/player/config", requireMethod(s.handleAudioConfig, http.MethodPost))
	mux.HandleFunc("/api/player/sleeptimer", requireMethod(s.handleSleepTimer, http.MethodPost))

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
	mux.HandleFunc("/api/library/artists", requireMethod(s.handleLibraryArtists, http.MethodGet))
	mux.HandleFunc("/api/library/artist/tracks", requireMethod(s.handleLibraryArtistTracks, http.MethodGet))
	mux.HandleFunc("/api/library/folders", requireMethod(s.handleLibraryFolders, http.MethodGet))
	mux.HandleFunc("/api/library/scan", requireMethod(s.handleLibraryScan, http.MethodPost))
	mux.HandleFunc("/api/library/favorite", requireMethod(s.handleFavoriteToggle, http.MethodPost))
	mux.HandleFunc("/api/library/favorites", requireMethod(s.handleGetFavorites, http.MethodGet))
	mux.HandleFunc("/api/library/favorites/play", requireMethod(s.handlePlayFavorites, http.MethodPost))
	mux.HandleFunc("/api/search", requireMethod(s.handleSearch, http.MethodGet))

	// 7. API de Smart Mix
	mux.HandleFunc("/api/smartmix", requireMethod(s.handleSmartMix, http.MethodPost))

	// 8. API de Letras LRCLIB
	mux.HandleFunc("/api/lyrics", requireMethod(s.handleLyrics, http.MethodGet))

	// 9. Emisoras de Radio, Stream, Seguridad TLS y Ajustes
	mux.HandleFunc("/api/stream", requireMethod(s.handleAudioStream, http.MethodGet, http.MethodHead))
	mux.HandleFunc("/api/radios", requireMethod(s.handleRadios, http.MethodGet, http.MethodPost, http.MethodDelete))
	mux.HandleFunc("/api/radios/play", requireMethod(s.handleRadioPlay, http.MethodPost))
	mux.HandleFunc("/api/settings", requireMethod(s.handleSettings, http.MethodGet, http.MethodPost))
	mux.HandleFunc("/api/settings/nas", requireMethod(s.handleNASConfig, http.MethodGet, http.MethodPost))
	mux.HandleFunc("/api/security/status", requireMethod(s.handleSecurityStatus, http.MethodGet))
	mux.HandleFunc("/api/security/ca.crt", requireMethod(s.handleDownloadCACert, http.MethodGet, http.MethodHead))
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
		w.Header().Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' https: http: data:; connect-src 'self'; media-src 'self'; object-src 'none'; frame-ancestors 'self'; base-uri 'self'")

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

// coversCacheMiddleware aplica cabeceras de caché persistente (30 días) para carátulas.
func coversCacheMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "public, max-age=2592000, immutable")
		next.ServeHTTP(w, r)
	})
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

// handleLibraryRecent devuelve las canciones más recientemente indexadas.
func (s *Server) handleLibraryRecent(w http.ResponseWriter, r *http.Request) {
	limitStr := r.URL.Query().Get("limit")
	limit, _ := strconv.Atoi(limitStr)
	if limit <= 0 {
		limit = 30
	} else if limit > 200 {
		limit = 200
	}
	tracks, err := s.database.GetRecentTracks(limit)
	if err != nil {
		log.Printf("[DB] Error listando recientes: %v", err)
		http.Error(w, "error al consultar pistas recientes", http.StatusInternalServerError)
		return
	}
	jsonResponse(w, http.StatusOK, tracks)
}

// handleLibraryAlbums devuelve todos los álbumes.
func (s *Server) handleLibraryAlbums(w http.ResponseWriter, r *http.Request) {
	albums, err := s.database.GetAlbums()
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
		log.Printf("[DB] Error en búsqueda: %v", err)
		http.Error(w, "error al ejecutar búsqueda", http.StatusInternalServerError)
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
	if err := decodeJSONBody(w, r, &payload); err != nil {
		http.Error(w, "json inválido", http.StatusBadRequest)
		return
	}

	if payload.Artist == "" {
		http.Error(w, "campo artist es obligatorio", http.StatusBadRequest)
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

// handleRadioPlay valida/resuelve el flujo web de radio y comienza su reproducción inmediata.
func (s *Server) handleRadioPlay(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "método no permitido", http.StatusMethodNotAllowed)
		return
	}

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
