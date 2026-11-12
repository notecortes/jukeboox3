package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"aurabox/internal/db"
	"aurabox/internal/events"
	"aurabox/internal/mpd"
	"aurabox/internal/scanner"
	"aurabox/internal/tlsutil"
)

// TestHTTPSRedirectAndSecurityEndpoints verifica la redirección de IP sin puerto hacia HTTPS y descarga de CA local.
func TestHTTPSRedirectAndSecurityEndpoints(t *testing.T) {
	cm, err := tlsutil.InitCertManager(t.TempDir(), nil, "", "")
	if err != nil {
		t.Fatalf("InitCertManager falló: %v", err)
	}

	srv := &Server{
		certManager:     cm,
		httpsPort:       "4443",
		httpsPublicPort: "443",
		forceHTTPS:      false,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/api/stream", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	mux.HandleFunc("/api/security/status", srv.handleSecurityStatus)
	mux.HandleFunc("/api/security/ca.crt", srv.handleDownloadCACert)

	handler := srv.httpsRedirectMiddleware(mux)

	// 1. Petición por puerto 80 (sin puerto en Host -> ej. teclear solo la IP) debe redirigir a https://192.168.1.113/
	reqBare := httptest.NewRequest(http.MethodGet, "http://192.168.1.113/?tab=settings", nil)
	reqBare.Host = "192.168.1.113"
	recBare := httptest.NewRecorder()
	handler.ServeHTTP(recBare, reqBare)
	if recBare.Code != http.StatusTemporaryRedirect {
		t.Fatalf("código esperado 307 para Host sin puerto, obtenido %d", recBare.Code)
	}
	if loc := recBare.Header().Get("Location"); loc != "https://192.168.1.113/?tab=settings" {
		t.Fatalf("Location esperada https://192.168.1.113/?tab=settings, obtenida %q", loc)
	}

	// Verificar que EnsureHost incorporó dinámicamente 192.168.1.113 en el certificado SAN
	hasIP := false
	for _, h := range cm.SANHosts() {
		if h == "192.168.1.113" {
			hasIP = true
			break
		}
	}
	if !hasIP {
		t.Fatalf("se esperaba 192.168.1.113 en SANHosts, obtenido %v", cm.SANHosts())
	}

	// 2. Con FORCE_HTTPS=false y puerto explícito :4080, HTTP debe seguir sirviendo normalmente
	reqPort := httptest.NewRequest(http.MethodGet, "http://192.168.1.113:4080/", nil)
	reqPort.Host = "192.168.1.113:4080"
	recPort := httptest.NewRecorder()
	handler.ServeHTTP(recPort, reqPort)
	if recPort.Code != http.StatusOK {
		t.Fatalf("con FORCE_HTTPS=false y :4080 se esperaba 200, obtenido %d", recPort.Code)
	}

	// 3. Con FORCE_HTTPS=true y httpsPublicPort=4443, :4080 redirige a :4443
	srv.forceHTTPS = true
	srv.httpsPublicPort = "4443"
	recForced := httptest.NewRecorder()
	handler.ServeHTTP(recForced, reqPort)
	if recForced.Code != http.StatusTemporaryRedirect {
		t.Fatalf("con FORCE_HTTPS=true se esperaba 307, obtenido %d", recForced.Code)
	}
	if loc := recForced.Header().Get("Location"); loc != "https://192.168.1.113:4443/" {
		t.Fatalf("Location esperada https://192.168.1.113:4443/, obtenida %q", loc)
	}

	// 4. Descarga de CA y stream HTTP no deben redirigir incluso con FORCE_HTTPS=true
	reqCA := httptest.NewRequest(http.MethodGet, "http://192.168.1.113:4080/api/security/ca.crt", nil)
	reqCA.Host = "192.168.1.113:4080"
	recCA := httptest.NewRecorder()
	handler.ServeHTTP(recCA, reqCA)
	if recCA.Code != http.StatusOK || !strings.Contains(recCA.Body.String(), "BEGIN CERTIFICATE") {
		t.Fatalf("descarga de CA falló (code=%d)", recCA.Code)
	}
}

// TestRadiosEndpointsCRUDAndPlayValidation verifica listado sembrado, alta/borrado de radios personalizadas y resolución .pls en /api/radios/play.
func TestRadiosEndpointsCRUDAndPlayValidation(t *testing.T) {
	tmpDir := t.TempDir()
	database, err := db.InitDB(filepath.Join(tmpDir, "radios.db"))
	if err != nil {
		t.Fatalf("InitDB falló: %v", err)
	}
	defer database.Close()

	hub := events.NewHub()
	player := mpd.NewClient(filepath.Join(tmpDir, "missing.sock"), "127.0.0.1", "0", hub, database)
	t.Cleanup(player.Close)

	srv := &Server{
		database:  database,
		eventsHub: hub,
		player:    player,
	}

	// 1. GET /api/radios debe devolver el catálogo completo (>= 280 emisoras incluyendo España)
	reqGet := httptest.NewRequest(http.MethodGet, "/api/radios", nil)
	recGet := httptest.NewRecorder()
	srv.handleRadios(recGet, reqGet)
	if recGet.Code != http.StatusOK {
		t.Fatalf("GET /api/radios devolvió %d: %s", recGet.Code, recGet.Body.String())
	}
	var list []db.RadioStation
	if err := json.Unmarshal(recGet.Body.Bytes(), &list); err != nil {
		t.Fatalf("JSON inválido en GET /api/radios: %v", err)
	}
	if len(list) < 280 {
		t.Fatalf("se esperaban al menos 280 radios, obtenidas %d", len(list))
	}
	var defaultID int64
	hasSpain := false
	for _, r := range list {
		if strings.EqualFold(r.Country, "Spain") {
			hasSpain = true
		}
		if r.IsDefault && defaultID == 0 {
			defaultID = r.ID
		}
	}
	if !hasSpain {
		t.Fatalf("GET /api/radios no incluye emisoras de Spain")
	}

	// 2. POST /api/radios debe rechazar URLs no http/https, destinos SSRF y logo javascript:
	for _, badBody := range []string{
		`{"name":"Mala","stream_url":"ftp://bad.local/stream"}`,
		`{"name":"SSRF1","stream_url":"http://127.0.0.1:8080/internal"}`,
		`{"name":"SSRF2","stream_url":"http://169.254.169.254/latest/meta-data/"}`,
		`{"name":"XSSLogo","stream_url":"https://example.org/radio.mp3","logo_url":"javascript:alert(1)"}`,
	} {
		reqBad := httptest.NewRequest(http.MethodPost, "/api/radios", bytes.NewBufferString(badBody))
		recBad := httptest.NewRecorder()
		srv.handleRadios(recBad, reqBad)
		if recBad.Code != http.StatusBadRequest {
			t.Fatalf("POST /api/radios inválido (%s) debía devolver 400, obtuvo %d", badBody, recBad.Code)
		}
	}

	// 3. POST /api/radios válido + DELETE posterior (y bloqueo de borrado en default)
	reqAdd := httptest.NewRequest(http.MethodPost, "/api/radios", bytes.NewBufferString(`{"name":"Mi Emisora PLS","stream_url":"https://example.org/radio.pls","genre":"Rock","country":"Spain"}`))
	recAdd := httptest.NewRecorder()
	srv.handleRadios(recAdd, reqAdd)
	if recAdd.Code != http.StatusCreated {
		t.Fatalf("POST /api/radios válido debía devolver 201, obtuvo %d: %s", recAdd.Code, recAdd.Body.String())
	}
	var created db.RadioStation
	if err := json.Unmarshal(recAdd.Body.Bytes(), &created); err != nil || created.ID <= 0 {
		t.Fatalf("respuesta de creación inválida: err=%v body=%s", err, recAdd.Body.String())
	}

	reqDelDefault := httptest.NewRequest(http.MethodDelete, "/api/radios?id="+strconv.FormatInt(defaultID, 10), nil)
	recDelDefault := httptest.NewRecorder()
	srv.handleRadios(recDelDefault, reqDelDefault)
	if recDelDefault.Code != http.StatusNotFound {
		t.Fatalf("DELETE de emisora default debía devolver 404, obtuvo %d", recDelDefault.Code)
	}

	reqDelCustom := httptest.NewRequest(http.MethodDelete, "/api/radios?id="+strconv.FormatInt(created.ID, 10), nil)
	recDelCustom := httptest.NewRecorder()
	srv.handleRadios(recDelCustom, reqDelCustom)
	if recDelCustom.Code != http.StatusOK {
		t.Fatalf("DELETE de emisora personalizada debía devolver 200, obtuvo %d: %s", recDelCustom.Code, recDelCustom.Body.String())
	}

	// 4. POST /api/radios/play con archivo .pls real servido localmente debe resolver la URL destino
	plsSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/x-scpls")
		_, _ = w.Write([]byte("[playlist]\nFile1=http://radio.example.local:8000/stream.mp3\nNumberOfEntries=1\n"))
	}))
	defer plsSrv.Close()

	restoreRadioClient := mpd.SetRadioHTTPClientForTest(plsSrv.Client())
	playBody, _ := json.Marshal(map[string]string{
		"name":       "Test PLS España",
		"stream_url": plsSrv.URL + "/listen.pls",
		"country":    "Spain",
		"genre":      "Pop",
	})
	reqPlay := httptest.NewRequest(http.MethodPost, "/api/radios/play", bytes.NewReader(playBody))
	recPlay := httptest.NewRecorder()
	srv.handleRadioPlay(recPlay, reqPlay)
	restoreRadioClient()

	if recPlay.Code != http.StatusOK {
		t.Fatalf("POST /api/radios/play válido debía devolver 200, obtuvo %d: %s", recPlay.Code, recPlay.Body.String())
	}
	st := player.GetState()
	if st.CurrentTrack == nil || st.CurrentTrack.Source != "radio" || st.CurrentTrack.Path != "http://radio.example.local:8000/stream.mp3" {
		t.Fatalf("estado CurrentTrack inesperado tras playRadio .pls: %+v", st.CurrentTrack)
	}

	// 5. POST /api/radios/play con URL inválida o SSRF devuelve 400 sin alterar la reproducción
	for _, badPlayBody := range []string{
		`{"name":"Mala","stream_url":"file:///etc/passwd"}`,
		`{"name":"LocalSSRF","stream_url":"http://127.0.0.1:4081/metrics"}`,
		`{"name":"BadLogo","stream_url":"https://example.org/stream.mp3","logo_url":"data:text/html,<script>"}`,
	} {
		reqBadPlay := httptest.NewRequest(http.MethodPost, "/api/radios/play", bytes.NewBufferString(badPlayBody))
		recBadPlay := httptest.NewRecorder()
		srv.handleRadioPlay(recBadPlay, reqBadPlay)
		if recBadPlay.Code != http.StatusBadRequest {
			t.Fatalf("POST /api/radios/play (%s) debía devolver 400, obtuvo %d", badPlayBody, recBadPlay.Code)
		}
	}
}

// TestAudioStreamProxyHeadersAndChunks verifica que /api/stream no añade Connection: close y retransmite chunks sin buffering.
func TestAudioStreamProxyHeadersAndChunks(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg")
		w.WriteHeader(http.StatusOK)
		fl, _ := w.(http.Flusher)
		_, _ = w.Write([]byte("ID3_FRAME_1"))
		if fl != nil {
			fl.Flush()
		}
		_, _ = w.Write([]byte("MP3_AUDIO_CHUNK_2"))
		if fl != nil {
			fl.Flush()
		}
	}))
	defer upstream.Close()

	srv := &Server{
		streamClient:      newAudioStreamClient(),
		streamUpstreamURL: upstream.URL,
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/api/stream", srv.handleAudioStream)
	proxySrv := httptest.NewServer(compressionAndSecurityMiddleware(mux))
	defer proxySrv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, proxySrv.URL+"/api/stream", nil)
	if err != nil {
		t.Fatalf("NewRequest falló: %v", err)
	}
	resp, err := proxySrv.Client().Do(req)
	if err != nil {
		t.Fatalf("GET /api/stream falló: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status esperado 200 en /api/stream, obtenido %d", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); ct != "audio/mpeg" {
		t.Fatalf("Content-Type esperado audio/mpeg, obtenido %q", ct)
	}
	if xb := resp.Header.Get("X-Accel-Buffering"); xb != "no" {
		t.Fatalf("X-Accel-Buffering esperado no, obtenido %q", xb)
	}
	if strings.EqualFold(resp.Header.Get("Connection"), "close") {
		t.Fatalf("la respuesta de /api/stream no debe forzar Connection: close")
	}

	buf := make([]byte, 128)
	n, _ := resp.Body.Read(buf)
	bodyText := string(buf[:n])
	if !strings.Contains(bodyText, "ID3_FRAME_1") {
		t.Fatalf("no se recibió chunk de audio esperado en /api/stream, obtenido %q", bodyText)
	}
}

// TestHTTPHardeningMethodsCORSCompressionAndPathSafety valida métodos HTTP, CORS same-origin, compresión segura, symlinks y ajustes.
func TestHTTPHardeningMethodsCORSCompressionAndPathSafety(t *testing.T) {
	tmpDir := t.TempDir()
	localDir := filepath.Join(tmpDir, "local")
	nasDir := filepath.Join(tmpDir, "nas")
	usbDir := filepath.Join(tmpDir, "usb")
	coversDir := filepath.Join(tmpDir, "covers")
	outsideDir := filepath.Join(tmpDir, "outside")
	for _, d := range []string{localDir, nasDir, usbDir, coversDir, outsideDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatalf("MkdirAll(%s) falló: %v", d, err)
		}
	}

	safeTrack := filepath.Join(localDir, "track.flac")
	if err := os.WriteFile(safeTrack, []byte("fLaC"), 0644); err != nil {
		t.Fatalf("WriteFile safeTrack falló: %v", err)
	}
	outsideSecret := filepath.Join(outsideDir, "secret.txt")
	if err := os.WriteFile(outsideSecret, []byte("secret"), 0644); err != nil {
		t.Fatalf("WriteFile outsideSecret falló: %v", err)
	}
	escapeLink := filepath.Join(localDir, "escape")
	if err := os.Symlink(outsideDir, escapeLink); err != nil {
		t.Fatalf("Symlink falló: %v", err)
	}

	database, err := db.InitDB(filepath.Join(tmpDir, "hardening.db"))
	if err != nil {
		t.Fatalf("InitDB falló: %v", err)
	}
	defer database.Close()

	hub := events.NewHub()
	player := mpd.NewClient(filepath.Join(tmpDir, "missing.sock"), "127.0.0.1", "0", hub, database)
	t.Cleanup(player.Close)
	sc := scanner.NewLibraryScanner(database, hub, localDir, nasDir, usbDir, coversDir)
	defer sc.WaitForIdle()

	srv := &Server{
		database:  database,
		eventsHub: hub,
		player:    player,
		scanner:   sc,
		localDir:  localDir,
		nasDir:    nasDir,
		usbDir:    usbDir,
		coversDir: coversDir,
	}

	// 1. Validación de escape por symlink en isSafeMusicPath
	if !srv.isSafeMusicPath(safeTrack) {
		t.Fatalf("ruta válida dentro de localDir debía permitirse: %s", safeTrack)
	}
	if srv.isSafeMusicPath(filepath.Join(escapeLink, "secret.txt")) {
		t.Fatalf("isSafeMusicPath debía bloquear acceso vía symlink fuera de raíz")
	}
	if srv.isSafeMusicPath(filepath.Join(escapeLink, "new_nonexistent.flac")) {
		t.Fatalf("isSafeMusicPath debía bloquear ruta inexistente bajo symlink externo")
	}
	if srv.isSafeMusicPath("https://evil.example/a.mp3") {
		t.Fatalf("isSafeMusicPath debía rechazar URLs remotas")
	}

	// 2. Restricción de métodos HTTP en registerRoutes
	mux := http.NewServeMux()
	srv.registerRoutes(mux)
	handler := corsMiddleware(compressionAndSecurityMiddleware(mux))

	reqWrongMethod := httptest.NewRequest(http.MethodGet, "http://192.168.1.50:4080/api/player/play", nil)
	recWrongMethod := httptest.NewRecorder()
	handler.ServeHTTP(recWrongMethod, reqWrongMethod)
	if recWrongMethod.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET /api/player/play debía devolver 405, obtuvo %d", recWrongMethod.Code)
	}
	if allow := recWrongMethod.Header().Get("Allow"); !strings.Contains(allow, http.MethodPost) {
		t.Fatalf("cabecera Allow esperada con POST, obtenida %q", allow)
	}

	// 3. CORS mismo origen sin wildcard global
	reqEvilCORS := httptest.NewRequest(http.MethodOptions, "http://192.168.1.50:4080/api/status", nil)
	reqEvilCORS.Host = "192.168.1.50:4080"
	reqEvilCORS.Header.Set("Origin", "https://evil.example")
	recEvilCORS := httptest.NewRecorder()
	handler.ServeHTTP(recEvilCORS, reqEvilCORS)
	if recEvilCORS.Code != http.StatusForbidden {
		t.Fatalf("preflight CORS de origen externo debía devolver 403, obtuvo %d", recEvilCORS.Code)
	}
	if recEvilCORS.Header().Get("Access-Control-Allow-Origin") != "" {
		t.Fatalf("no debe exponer Access-Control-Allow-Origin para origen externo")
	}

	reqSameCORS := httptest.NewRequest(http.MethodOptions, "http://192.168.1.50:4080/api/status", nil)
	reqSameCORS.Host = "192.168.1.50:4080"
	reqSameCORS.Header.Set("Origin", "http://192.168.1.50:4080")
	recSameCORS := httptest.NewRecorder()
	handler.ServeHTTP(recSameCORS, reqSameCORS)
	if recSameCORS.Code != http.StatusNoContent || recSameCORS.Header().Get("Access-Control-Allow-Origin") != "http://192.168.1.50:4080" {
		t.Fatalf("preflight mismo origen inesperado: code=%d acao=%q", recSameCORS.Code, recSameCORS.Header().Get("Access-Control-Allow-Origin"))
	}

	// 4. Cabeceras de seguridad y compresión diferida correcta según estado y MIME
	secTestMux := http.NewServeMux()
	secTestMux.HandleFunc("/notmod", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotModified)
	})
	secTestMux.HandleFunc("/png", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("PNGDATA"))
	})
	secTestMux.HandleFunc("/json", func(w http.ResponseWriter, r *http.Request) {
		jsonResponse(w, http.StatusOK, map[string]string{"ok": "yes"})
	})
	secWrapped := compressionAndSecurityMiddleware(secTestMux)

	req304 := httptest.NewRequest(http.MethodGet, "/notmod", nil)
	req304.Header.Set("Accept-Encoding", "gzip")
	rec304 := httptest.NewRecorder()
	secWrapped.ServeHTTP(rec304, req304)
	if rec304.Code != http.StatusNotModified || rec304.Header().Get("Content-Encoding") != "" {
		t.Fatalf("respuesta 304 no debía llevar Content-Encoding gzip (%q)", rec304.Header().Get("Content-Encoding"))
	}
	if csp := rec304.Header().Get("Content-Security-Policy"); !strings.Contains(csp, "object-src 'none'") {
		t.Fatalf("CSP esperada en respuesta HTTP, obtenida %q", csp)
	}
	if rec304.Header().Get("X-XSS-Protection") != "" {
		t.Fatalf("cabecera obsoleta X-XSS-Protection no debía enviarse")
	}

	reqPNG := httptest.NewRequest(http.MethodGet, "/png", nil)
	reqPNG.Header.Set("Accept-Encoding", "gzip")
	recPNG := httptest.NewRecorder()
	secWrapped.ServeHTTP(recPNG, reqPNG)
	if recPNG.Header().Get("Content-Encoding") != "" || recPNG.Body.String() != "PNGDATA" {
		t.Fatalf("image/png no debía comprimirse con gzip (enc=%q, body=%q)", recPNG.Header().Get("Content-Encoding"), recPNG.Body.String())
	}

	reqJSON := httptest.NewRequest(http.MethodGet, "/json", nil)
	reqJSON.Header.Set("Accept-Encoding", "gzip")
	recJSON := httptest.NewRecorder()
	secWrapped.ServeHTTP(recJSON, reqJSON)
	if recJSON.Header().Get("Content-Encoding") != "gzip" {
		t.Fatalf("JSON textual con Accept-Encoding: gzip debía comprimirse")
	}
	gzReader, err := gzip.NewReader(recJSON.Body)
	if err != nil {
		t.Fatalf("cuerpo gzip inválido: %v", err)
	}
	defer gzReader.Close()
	rawJSON, _ := io.ReadAll(gzReader)
	if !strings.Contains(string(rawJSON), `"ok":"yes"`) {
		t.Fatalf("contenido JSON descomprimido inesperado: %s", string(rawJSON))
	}

	// 5. Validación de /api/player/config, /api/settings (allowlist) y /api/settings/nas (sin nas_pass en claro)
	reqBadRG := httptest.NewRequest(http.MethodPost, "/api/player/config?replay_gain=track%0Asetvol%20100&crossfade=2", nil)
	recBadRG := httptest.NewRecorder()
	srv.handleAudioConfig(recBadRG, reqBadRG)
	if recBadRG.Code != http.StatusBadRequest {
		t.Fatalf("replay_gain con salto de línea debía devolver 400, obtuvo %d", recBadRG.Code)
	}

	reqBadSet := httptest.NewRequest(http.MethodPost, "/api/settings", bytes.NewBufferString(`{"evil_key":"value"}`))
	recBadSet := httptest.NewRecorder()
	srv.handleSettings(recBadSet, reqBadSet)
	if recBadSet.Code != http.StatusBadRequest {
		t.Fatalf("clave arbitraria en /api/settings debía devolver 400, obtuvo %d", recBadSet.Code)
	}

	reqGoodSet := httptest.NewRequest(http.MethodPost, "/api/settings", bytes.NewBufferString(`{"max_volume_limit":"75","replay_gain":"album","crossfade":"3"}`))
	recGoodSet := httptest.NewRecorder()
	srv.handleSettings(recGoodSet, reqGoodSet)
	if recGoodSet.Code != http.StatusOK {
		t.Fatalf("/api/settings válido debía devolver 200, obtuvo %d: %s", recGoodSet.Code, recGoodSet.Body.String())
	}

	reqNASBad := httptest.NewRequest(http.MethodPost, "/api/settings/nas", bytes.NewBufferString(`{"host":"192.168.1.10\nbad"}`))
	recNASBad := httptest.NewRecorder()
	srv.handleNASConfig(recNASBad, reqNASBad)
	if recNASBad.Code != http.StatusBadRequest {
		t.Fatalf("/api/settings/nas con saltos de línea debía devolver 400, obtuvo %d", recNASBad.Code)
	}

	reqNASOK := httptest.NewRequest(http.MethodPost, "/api/settings/nas", bytes.NewBufferString(`{"host":"192.168.1.10","share":"music","username":"admin","password":"supersecretpassword"}`))
	recNASOK := httptest.NewRecorder()
	srv.handleNASConfig(recNASOK, reqNASOK)
	if recNASOK.Code != http.StatusOK {
		t.Fatalf("/api/settings/nas válido debía devolver 200, obtuvo %d: %s", recNASOK.Code, recNASOK.Body.String())
	}
	if storedPass := database.GetSetting("nas_pass", ""); storedPass != "" {
		t.Fatalf("nas_pass no debe almacenarse en texto plano en SQLite, obtuvo %q", storedPass)
	}
}

