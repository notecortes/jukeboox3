package mpd

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"aurabox/internal/db"
)

// startMockMPDServer levanta un servidor MPD mínimo sobre Unix socket para probar protocolo sin bloquear.
func startMockMPDServer(t *testing.T, sockPath string) (*sync.Mutex, *[]string) {
	t.Helper()
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("Listen unix falló: %v", err)
	}
	t.Cleanup(func() {
		_ = ln.Close()
	})

	var cmdMu sync.Mutex
	var cmds []string

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = c.Write([]byte("OK MPD 0.23.15\n"))
				r := bufio.NewReader(c)
				repeatVal := "0"
				volVal := "65"
				stateVal := "stop"
				songVal := "-1"
				out1Enabled := "1"

				for {
					line, err := r.ReadString('\n')
					if err != nil {
						return
					}
					trimmed := strings.TrimSpace(line)
					cmdMu.Lock()
					cmds = append(cmds, trimmed)
					cmdMu.Unlock()

					switch {
					case trimmed == "outputs":
						_, _ = c.Write([]byte(fmt.Sprintf(
							"outputid: 0\noutputname: DAC USB Test\nplugin: alsa\noutputenabled: %s\n"+
								"outputid: 1\noutputname: Transmisión Web HTTP\nplugin: httpd\noutputenabled: 1\nOK\n",
							out1Enabled,
						)))
					case trimmed == "status":
						_, _ = c.Write([]byte(fmt.Sprintf(
							"volume: %s\nrepeat: %s\nstate: %s\nsong: %s\nelapsed: 12.5\nduration: 200.0\nOK\n",
							volVal, repeatVal, stateVal, songVal,
						)))
					case trimmed == "playlistinfo":
						_, _ = c.Write([]byte("OK\n"))
					case strings.HasPrefix(trimmed, "add \""):
						// Simular que la ruta relativa del catálogo aún no fue indexada, pero file:///rawmusic/... sí funciona
						if !strings.Contains(trimmed, "file:///rawmusic/") && !strings.Contains(trimmed, "http") {
							_, _ = c.Write([]byte("ACK [50@0] {add} No such directory\n"))
						} else {
							_, _ = c.Write([]byte("OK\n"))
						}
					case strings.HasPrefix(trimmed, "play"):
						stateVal = "play"
						parts := strings.Fields(trimmed)
						if len(parts) > 1 {
							songVal = parts[1]
						} else if songVal == "-1" {
							songVal = "0"
						}
						_, _ = c.Write([]byte("OK\n"))
					case strings.HasPrefix(trimmed, "repeat "):
						repeatVal = strings.TrimPrefix(trimmed, "repeat ")
						_, _ = c.Write([]byte("OK\n"))
					case strings.HasPrefix(trimmed, "setvol "):
						volVal = strings.TrimPrefix(trimmed, "setvol ")
						_, _ = c.Write([]byte("OK\n"))
					case strings.HasPrefix(trimmed, "disableoutput 0"):
						out1Enabled = "0"
						_, _ = c.Write([]byte("OK\n"))
					case strings.HasPrefix(trimmed, "enableoutput 0"):
						out1Enabled = "1"
						_, _ = c.Write([]byte("OK\n"))
					default:
						_, _ = c.Write([]byte("OK\n"))
					}
				}
			}(conn)
		}
	}()

	return &cmdMu, &cmds
}

// TestNormalizeMPDFilePath valida la conversión inversa de URIs file:///rawmusic/... y rutas de MPD a SQLite.
func TestNormalizeMPDFilePath(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"file:///rawmusic/nas/01.rock/Queen/01.flac", "/music/nas/01.rock/Queen/01.flac"},
		{"file:///music/local/track.mp3", "/music/local/track.mp3"},
		{"local/The Cure/Boys.mp3", "/music/local/The Cure/Boys.mp3"},
		{"https://stream.radioparadise.com/flacm", "https://stream.radioparadise.com/flacm"},
	}
	for _, tc := range cases {
		if got := normalizeMPDFilePath(tc.in); got != tc.want {
			t.Fatalf("normalizeMPDFilePath(%q) = %q, esperado %q", tc.in, got, tc.want)
		}
	}
}

// TestMPDQuote valida el escape seguro de comillas y barras invertidas manteniendo UTF-8 intacto.
func TestMPDQuote(t *testing.T) {
	got := mpdQuote(`nas/Ólafur\Arnalds "Live"` + "\n")
	want := `"nas/Ólafur\\Arnalds \"Live\""`
	if got != want {
		t.Fatalf("mpdQuote = %q, esperado %q", got, want)
	}
}

// TestAntiRepetitionSort comprueba que las pistas no repiten artista consecutivamente tras reordenar.
func TestAntiRepetitionSort(t *testing.T) {
	tracks := []db.Track{
		{ID: 1, Artist: "IZAL", Title: "I1"},
		{ID: 2, Artist: "IZAL", Title: "I2"},
		{ID: 3, Artist: "IZAL", Title: "I3"},
		{ID: 4, Artist: "Vetusta Morla", Title: "V1"},
		{ID: 5, Artist: "Vetusta Morla", Title: "V2"},
		{ID: 6, Artist: "Vetusta Morla", Title: "V3"},
		{ID: 7, Artist: "Queen", Title: "Q1"},
		{ID: 8, Artist: "Queen", Title: "Q2"},
		{ID: 9, Artist: "The Cure", Title: "C1"},
		{ID: 10, Artist: "The Cure", Title: "C2"},
	}

	cur := &db.Track{ID: 99, Artist: "IZAL"}
	sorted := antiRepetitionSort(tracks, cur)

	if len(sorted) != len(tracks) {
		t.Fatalf("se perdieron pistas al barajar: got=%d want=%d", len(sorted), len(tracks))
	}
	if strings.EqualFold(sorted[0].Artist, cur.Artist) {
		t.Fatalf("la primera pista no debe repetir al artista actual (%s)", sorted[0].Artist)
	}
	for i := 1; i < len(sorted); i++ {
		if strings.EqualFold(sorted[i].Artist, sorted[i-1].Artist) {
			t.Fatalf("repetición consecutiva detectada en índice %d (%s)", i, sorted[i].Artist)
		}
	}
}

// TestUnixSocketFallbackAndControls verifica fallback file:///rawmusic/..., PlayIndex, SetRepeat, tope de volumen y estado thread-safe.
func TestUnixSocketFallbackAndControls(t *testing.T) {
	tmpDir := t.TempDir()
	sockPath := filepath.Join(tmpDir, "mpd.sock")
	cmdMu, cmds := startMockMPDServer(t, sockPath)

	c := NewClient(sockPath, "127.0.0.1", "0", nil, nil)
	t.Cleanup(c.Close)
	c.tryConnect()

	st := c.GetState()
	if !c.isConnected() || !c.usingUnixSocket.Load() {
		t.Fatalf("se esperaba conexión activa vía Unix socket")
	}
	if len(st.AudioOutputs) != 2 || st.ActiveOutput != "DAC USB Test" {
		t.Fatalf("salidas de audio inesperadas: %+v (active=%q)", st.AudioOutputs, st.ActiveOutput)
	}

	// Probar salvaguarda de volumen máxima
	c.SetVolume(99)
	if gotVol := c.GetState().Volume; gotVol != 85 {
		t.Fatalf("volumen debería quedar limitado a 85, obtenido %d", gotVol)
	}
	c.SetMaxVolumeLimit(70)
	if gotVol := c.GetState().Volume; gotVol != 70 {
		t.Fatalf("al bajar límite a 70 el volumen debe ajustarse a 70, obtenido %d", gotVol)
	}

	// Probar ReplaceQueue con ruta aún no indexada -> debe caer a file:///rawmusic/... sin error
	tracks := []db.Track{
		{ID: 10, Path: "/music/nas/01.rock/Queen/01 - Bohemian Rhapsody.flac", Title: "Bohemian Rhapsody", Artist: "Queen", Duration: 354},
		{ID: 11, Path: "/music/nas/01.rock/Queen/02 - Killer Queen.flac", Title: "Killer Queen", Artist: "Queen", Duration: 180},
	}
	c.ReplaceQueue(tracks, true)

	// Probar PlayIndex y SetRepeat concurrentemente bajo detector -race
	var wg sync.WaitGroup
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			_ = c.GetState()
			_ = c.GetQueue()
			c.SetRepeat(idx%2 == 0)
			c.PlayIndex(idx % len(tracks))
		}(i)
	}
	wg.Wait()

	// Esperar hasta 1 segundo a que pushQueueToMPD envíe los comandos asíncronos
	deadline := time.Now().Add(1 * time.Second)
	foundFallback := false
	var joined string
	for time.Now().Before(deadline) {
		cmdMu.Lock()
		joined = strings.Join(*cmds, "\n")
		cmdMu.Unlock()
		if strings.Contains(joined, `add "file:///rawmusic/nas/01.rock/Queen/01 - Bohemian Rhapsody.flac"`) {
			foundFallback = true
			break
		}
		time.Sleep(15 * time.Millisecond)
	}
	if !foundFallback {
		t.Fatalf("no se ejecutó el fallback file:///rawmusic/... sobre Unix socket; comandos recibidos:\n%s", joined)
	}
}

// TestNonBlockingControlUnderConcurrentDBWrites verifica que PlayIndex, volumen y ajustes responden sin bloquear aunque SQLite esté escribiendo lotes masivos.
func TestNonBlockingControlUnderConcurrentDBWrites(t *testing.T) {
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "concurrent.db")
	database, err := db.InitDB(dbPath)
	if err != nil {
		t.Fatalf("InitDB falló: %v", err)
	}
	defer database.Close()

	seed := &db.Track{
		Path:     "/music/nas/01.rock/Queen/01 - Bohemian Rhapsody.flac",
		Filename: "01 - Bohemian Rhapsody.flac",
		Title:    "Bohemian Rhapsody",
		Artist:   "Queen",
		Album:    "A Night at the Opera",
		Duration: 354,
		Format:   "FLAC",
		Source:   "nas",
	}
	if err := database.UpsertTrack(seed); err != nil {
		t.Fatalf("UpsertTrack semilla falló: %v", err)
	}

	sockPath := filepath.Join(tmpDir, "mpd.sock")
	startMockMPDServer(t, sockPath)

	c := NewClient(sockPath, "127.0.0.1", "0", nil, database)
	c.tryConnect()
	c.ReplaceQueue([]db.Track{*seed}, false)

	// Lanzar escrituras pesadas concurrentes simulando un escaneo largo del NAS
	var scanWg sync.WaitGroup
	scanWg.Add(1)
	go func() {
		defer scanWg.Done()
		for b := 0; b < 5; b++ {
			batch := make([]*db.Track, 0, 100)
			for i := 0; i < 100; i++ {
				batch = append(batch, &db.Track{
					Path:     fmt.Sprintf("/music/nas/band_%d/album_%d/t_%02d.flac", i%5, b, i),
					Filename: fmt.Sprintf("t_%02d.flac", i),
					Title:    fmt.Sprintf("Track %d-%d", b, i),
					Artist:   fmt.Sprintf("Band %d", i%5),
					Album:    fmt.Sprintf("Album %d", b),
					Duration: 210,
					Format:   "FLAC",
					Source:   "nas",
				})
			}
			_ = database.BatchUpsertTracks(batch)
		}
	}()

	startCtrl := time.Now()
	c.PlayIndex(0)
	c.SetVolume(68)
	c.SetAudioConfig("track", 4)
	_ = c.SelectAudioOutput(0)
	ctrlElapsed := time.Since(startCtrl)
	if ctrlElapsed > 1500*time.Millisecond {
		t.Fatalf("los controles de reproducción tardaron demasiado (%v) durante escrituras SQLite", ctrlElapsed)
	}

	scanWg.Wait()
	// Close debe drenar persistCh de forma ordenada antes de validar en SQLite
	c.Close()

	if gotVol := database.GetSetting("player_volume", ""); gotVol != "68" {
		t.Fatalf("player_volume esperado '68', obtenido %q", gotVol)
	}
	if gotCF := database.GetSetting("crossfade", ""); gotCF != "4" {
		t.Fatalf("crossfade esperado '4', obtenido %q", gotCF)
	}
	saved, err := database.GetTrackByPath(seed.Path)
	if err != nil || saved == nil || saved.PlayCount < 1 {
		t.Fatalf("play_count debería haberse incrementado en segundo plano (err=%v, track=%+v)", err, saved)
	}
}

// TestAudioDiagnosticsAndRefresh verifica la lectura de alsa_cards.txt y el disparo de reload_outputs.
func TestAudioDiagnosticsAndRefresh(t *testing.T) {
	tmpDir := t.TempDir()
	sockPath := filepath.Join(tmpDir, "mpd.sock")
	startMockMPDServer(t, sockPath)

	diagFile := filepath.Join(tmpDir, "alsa_cards.txt")
	sampleAlsa := "**** List of PLAYBACK Hardware Devices ****\ncard 0: Headphones [bcm2835 Headphones], device 0: bcm2835 Headphones [bcm2835 Headphones]\ncard 2: USB Audio CODEC [USB Audio CODEC], device 0: USB Audio [USB Audio]\n"
	if err := os.WriteFile(diagFile, []byte(sampleAlsa), 0644); err != nil {
		t.Fatalf("WriteFile alsa_cards.txt falló: %v", err)
	}

	c := NewClient(sockPath, "127.0.0.1", "0", nil, nil)
	t.Cleanup(c.Close)
	c.tryConnect()

	// Simular el bucle del entrypoint consumiendo reload_outputs en paralelo
	doneWatch := make(chan struct{})
	go func() {
		defer close(doneWatch)
		triggerPath := filepath.Join(tmpDir, "reload_outputs")
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			if _, err := os.Stat(triggerPath); err == nil {
				_ = os.Remove(triggerPath)
				return
			}
			time.Sleep(20 * time.Millisecond)
		}
	}()

	diag, err := c.RefreshAudioOutputs()
	if err != nil {
		t.Fatalf("RefreshAudioOutputs devolvió error: %v", err)
	}
	<-doneWatch

	if !diag.HasDAC || !diag.HasHardware {
		t.Fatalf("se esperaba HasDAC=true y HasHardware=true, obtenido: %+v", diag)
	}
	if !strings.Contains(diag.AlsaRaw, "USB Audio CODEC") {
		t.Fatalf("AlsaRaw no contiene la tarjeta esperada: %q", diag.AlsaRaw)
	}
}

// TestResolveStreamURLsAndPlaylistParsing verifica resolución de .pls/.m3u, paso directo de HLS .m3u8 y bloqueo de URLs inválidas.
func TestResolveStreamURLsAndPlaylistParsing(t *testing.T) {
	if _, err := ValidateHTTPStreamURL("ftp://example.org/radio.mp3"); err == nil {
		t.Fatalf("ValidateHTTPStreamURL debía rechazar protocolo ftp://")
	}
	if _, err := ValidateHTTPStreamURL("http://127.0.0.1:8080/internal.mp3"); err == nil {
		t.Fatalf("ValidateHTTPStreamURL debía rechazar IP loopback SSRF")
	}
	if _, err := ValidateHTTPStreamURL("http://169.254.169.254/latest/meta-data/"); err == nil {
		t.Fatalf("ValidateHTTPStreamURL debía rechazar IP link-local/metadata SSRF")
	}
	if _, err := ValidateHTTPStreamURL(""); err == nil {
		t.Fatalf("ValidateHTTPStreamURL debía rechazar URL vacía")
	}

	var tsURL string
	mux := http.NewServeMux()
	mux.HandleFunc("/station.pls", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/x-scpls")
		_, _ = w.Write([]byte("[playlist]\nFile1=https://stream.example.org/a.mp3\nFile2=/mirror/b.mp3\nNumberOfEntries=2\n"))
	})
	mux.HandleFunc("/station.m3u", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/x-mpegurl")
		_, _ = w.Write([]byte("\ufeff#EXTM3U\n#EXTINF:-1,Radio Prueba\nhttps://cdn.example.org/live.aac\n"))
	})
	mux.HandleFunc("/hls/live.m3u8", func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("no se debe descargar una URL .m3u8 antes de pasarla a MPD")
	})
	mux.HandleFunc("/hls.m3u", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
		_, _ = w.Write([]byte("#EXTM3U\n#EXT-X-VERSION:3\n#EXT-X-STREAM-INF:BANDWIDTH=128000\nchunklist.m3u8\n"))
	})
	mux.HandleFunc("/audio.pls", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "audio/mpeg; charset=utf-8")
		_, _ = w.Write([]byte("MP3_STREAM_BYTES"))
	})
	mux.HandleFunc("/missing.pls", func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	})

	ts := httptest.NewServer(mux)
	defer ts.Close()
	defer SetRadioHTTPClientForTest(ts.Client())()
	tsURL = ts.URL

	plsURLs, err := ResolveStreamURLs(tsURL + "/station.pls")
	if err != nil {
		t.Fatalf("ResolveStreamURLs(.pls) falló: %v", err)
	}
	if len(plsURLs) != 2 || plsURLs[0] != "https://stream.example.org/a.mp3" || plsURLs[1] != tsURL+"/mirror/b.mp3" {
		t.Fatalf("URLs resueltas de .pls inesperadas: %v", plsURLs)
	}

	m3uURLs, err := ResolveStreamURLs(tsURL + "/station.m3u")
	if err != nil {
		t.Fatalf("ResolveStreamURLs(.m3u) falló: %v", err)
	}
	if len(m3uURLs) != 1 || m3uURLs[0] != "https://cdn.example.org/live.aac" {
		t.Fatalf("URL resuelta de .m3u inesperada: %v", m3uURLs)
	}

	m3u8Direct, err := ResolveStreamURLs(tsURL + "/hls/live.m3u8")
	if err != nil || len(m3u8Direct) != 1 || m3u8Direct[0] != tsURL+"/hls/live.m3u8" {
		t.Fatalf(".m3u8 debe pasar directo sin descarga previa: urls=%v err=%v", m3u8Direct, err)
	}

	hlsViaM3U, err := ResolveStreamURLs(tsURL + "/hls.m3u")
	if err != nil || len(hlsViaM3U) != 1 || hlsViaM3U[0] != tsURL+"/hls.m3u" {
		t.Fatalf("manifiesto #EXT-X- debe pasar directo a ffmpeg de MPD: urls=%v err=%v", hlsViaM3U, err)
	}

	audioDirect, err := ResolveStreamURLs(tsURL + "/audio.pls")
	if err != nil || len(audioDirect) != 1 || audioDirect[0] != tsURL+"/audio.pls" {
		t.Fatalf("Content-Type audio/ debe pasar directo sin consumir stream: urls=%v err=%v", audioDirect, err)
	}

	if _, err := ResolveStreamURLs(tsURL + "/missing.pls"); err == nil {
		t.Fatalf("lista HTTP 404 debía devolver error")
	}
}

// TestRadioStatusErrorAndStreamTitle verifica sincronización de metadatos ICY, specs audio/bitrate y propagación/limpieza de errores MPD.
func TestRadioStatusErrorAndStreamTitle(t *testing.T) {
	tmpDir := t.TempDir()
	sockPath := filepath.Join(tmpDir, "radio_mpd.sock")
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("Listen unix falló: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	var hasMPDErr atomic.Bool
	var cmdMu sync.Mutex
	var cmds []string

	go func() {
		for {
			conn, aErr := ln.Accept()
			if aErr != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = c.Write([]byte("OK MPD 0.24.18\n"))
				r := bufio.NewReader(c)
				for {
					line, rErr := r.ReadString('\n')
					if rErr != nil {
						return
					}
					trimmed := strings.TrimSpace(line)
					cmdMu.Lock()
					cmds = append(cmds, trimmed)
					cmdMu.Unlock()

					switch trimmed {
					case "outputs":
						_, _ = c.Write([]byte("outputid: 0\noutputname: DAC\nplugin: alsa\noutputenabled: 1\nOK\n"))
					case "status":
						if hasMPDErr.Load() {
							_, _ = c.Write([]byte("volume: 65\nstate: stop\nerror: Failed to decode stream\nOK\n"))
						} else {
							_, _ = c.Write([]byte("volume: 65\nstate: play\nsong: 0\nelapsed: 5.0\naudio: 48000:24:2\nbitrate: 192\nOK\n"))
						}
					case "currentsong":
						_, _ = c.Write([]byte("file: https://stream.example.org/live\nName: RNE Radio 3\nTitle: Vetusta Morla - Copenhague\nOK\n"))
					case "clearerror":
						hasMPDErr.Store(false)
						_, _ = c.Write([]byte("OK\n"))
					default:
						_, _ = c.Write([]byte("OK\n"))
					}
				}
			}(conn)
		}
	}()

	client := NewClient(sockPath, "127.0.0.1", "0", nil, nil)
	t.Cleanup(client.Close)
	client.tryConnect()

	client.ReplaceQueue([]db.Track{{
		ID:     -1,
		Path:   "https://stream.example.org/live",
		Title:  "RNE Radio 3",
		Artist: "RNE (Spain)",
		Source: "radio",
	}}, true)

	// Esperar a que pushQueueToMPD termine antes de sondear status
	deadline := time.Now().Add(1 * time.Second)
	for client.syncingQueue.Load() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	// Avanzar reloj mínimo para evitar lastControlNano > startedAt en el test síncrono
	time.Sleep(5 * time.Millisecond)
	client.syncStatusFromMPD()

	st := client.GetState()
	if st.StreamTitle != "Vetusta Morla - Copenhague" {
		t.Fatalf("StreamTitle esperado 'Vetusta Morla - Copenhague', obtenido %q", st.StreamTitle)
	}
	if st.CurrentTrack == nil || st.CurrentTrack.SampleRate != 48000 || st.CurrentTrack.Bitrate != 192 {
		t.Fatalf("specs de radio inesperados: %+v", st.CurrentTrack)
	}

	hasMPDErr.Store(true)
	time.Sleep(5 * time.Millisecond)
	client.syncStatusFromMPD()

	errState := client.GetState()
	if errState.LastError != "Failed to decode stream" {
		t.Fatalf("LastError esperado 'Failed to decode stream', obtenido %q", errState.LastError)
	}
	cmdMu.Lock()
	joined := strings.Join(cmds, "\n")
	cmdMu.Unlock()
	if !strings.Contains(joined, "clearerror") {
		t.Fatalf("se esperaba envío de comando clearerror tras capturar error MPD; comandos:\n%s", joined)
	}
}

type mockMPDBackend struct {
	mu          sync.Mutex
	playlist    []string
	state       string
	song        int
	errMsg      string
	addDelay    time.Duration
	failAddPath string
	cmds        []string
}

// startStatefulMockMPDServer levanta un servidor MPD en memoria con soporte completo de playlistlength, move, delete y errores.
func startStatefulMockMPDServer(t *testing.T, sockPath string) *mockMPDBackend {
	t.Helper()
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("Listen unix falló: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	srv := &mockMPDBackend{
		state: "stop",
		song:  -1,
	}

	unquote := func(s string) string {
		s = strings.TrimSpace(s)
		if len(s) >= 2 && strings.HasPrefix(s, `"`) && strings.HasSuffix(s, `"`) {
			s = s[1 : len(s)-1]
			s = strings.ReplaceAll(s, `\"`, `"`)
			s = strings.ReplaceAll(s, `\\`, `\`)
		}
		return s
	}

	go func() {
		for {
			conn, aErr := ln.Accept()
			if aErr != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = c.Write([]byte("OK MPD 0.24.18\n"))
				r := bufio.NewReader(c)
				for {
					line, rErr := r.ReadString('\n')
					if rErr != nil {
						return
					}
					trimmed := strings.TrimSpace(line)
					srv.mu.Lock()
					srv.cmds = append(srv.cmds, trimmed)
					addDelay := srv.addDelay
					failPath := srv.failAddPath
					srv.mu.Unlock()

					switch {
					case trimmed == "outputs":
						_, _ = c.Write([]byte("outputid: 0\noutputname: DAC USB Test\nplugin: alsa\noutputenabled: 1\nOK\n"))
					case trimmed == "status":
						srv.mu.Lock()
						var sb strings.Builder
						sb.WriteString(fmt.Sprintf("volume: 65\nrepeat: 0\nstate: %s\nplaylistlength: %d\n", srv.state, len(srv.playlist)))
						if srv.song >= 0 && srv.song < len(srv.playlist) {
							sb.WriteString(fmt.Sprintf("song: %d\nelapsed: 5.0\nduration: 200.0\n", srv.song))
						}
						if srv.errMsg != "" {
							sb.WriteString(fmt.Sprintf("error: %s\n", srv.errMsg))
						}
						sb.WriteString("OK\n")
						srv.mu.Unlock()
						_, _ = c.Write([]byte(sb.String()))
					case trimmed == "playlistinfo":
						srv.mu.Lock()
						var sb strings.Builder
						for idx, p := range srv.playlist {
							sb.WriteString(fmt.Sprintf("file: %s\nPos: %d\n", p, idx))
						}
						sb.WriteString("OK\n")
						srv.mu.Unlock()
						_, _ = c.Write([]byte(sb.String()))
					case trimmed == "clear":
						srv.mu.Lock()
						srv.playlist = nil
						srv.state = "stop"
						srv.song = -1
						srv.mu.Unlock()
						_, _ = c.Write([]byte("OK\n"))
					case strings.HasPrefix(trimmed, "add "):
						if addDelay > 0 {
							time.Sleep(addDelay)
						}
						arg := unquote(strings.TrimPrefix(trimmed, "add "))
						if failPath != "" && strings.Contains(arg, failPath) {
							_, _ = c.Write([]byte("ACK [50@0] {add} No such directory\n"))
							continue
						}
						srv.mu.Lock()
						srv.playlist = append(srv.playlist, arg)
						srv.mu.Unlock()
						_, _ = c.Write([]byte("OK\n"))
					case strings.HasPrefix(trimmed, "addid "):
						rest := strings.TrimPrefix(trimmed, "addid ")
						lastQuote := strings.LastIndex(rest, `"`)
						arg := unquote(rest)
						pos := -1
						if lastQuote >= 0 && lastQuote+1 < len(rest) {
							if parsedPos, pErr := strconv.Atoi(strings.TrimSpace(rest[lastQuote+1:])); pErr == nil {
								pos = parsedPos
							}
							arg = unquote(rest[:lastQuote+1])
						}
						srv.mu.Lock()
						if pos < 0 || pos > len(srv.playlist) {
							srv.playlist = append(srv.playlist, arg)
						} else {
							srv.playlist = append(srv.playlist, "")
							copy(srv.playlist[pos+1:], srv.playlist[pos:])
							srv.playlist[pos] = arg
						}
						srv.mu.Unlock()
						_, _ = c.Write([]byte("Id: 1\nOK\n"))
					case strings.HasPrefix(trimmed, "delete "):
						spec := strings.TrimSpace(strings.TrimPrefix(trimmed, "delete "))
						srv.mu.Lock()
						if strings.Contains(spec, ":") {
							parts := strings.SplitN(spec, ":", 2)
							start, _ := strconv.Atoi(parts[0])
							end, _ := strconv.Atoi(parts[1])
							if start >= 0 && start <= end && end <= len(srv.playlist) {
								srv.playlist = append(srv.playlist[:start], srv.playlist[end:]...)
							}
						} else if idx, err := strconv.Atoi(spec); err == nil && idx >= 0 && idx < len(srv.playlist) {
							srv.playlist = append(srv.playlist[:idx], srv.playlist[idx+1:]...)
						}
						srv.mu.Unlock()
						_, _ = c.Write([]byte("OK\n"))
					case strings.HasPrefix(trimmed, "move "):
						fields := strings.Fields(strings.TrimPrefix(trimmed, "move "))
						srv.mu.Lock()
						if len(fields) == 2 {
							from, _ := strconv.Atoi(fields[0])
							to, _ := strconv.Atoi(fields[1])
							if from >= 0 && from < len(srv.playlist) && to >= 0 && to < len(srv.playlist) {
								item := srv.playlist[from]
								srv.playlist = append(srv.playlist[:from], srv.playlist[from+1:]...)
								newPL := make([]string, 0, len(srv.playlist)+1)
								newPL = append(newPL, srv.playlist[:to]...)
								newPL = append(newPL, item)
								newPL = append(newPL, srv.playlist[to:]...)
								srv.playlist = newPL
							}
						}
						srv.mu.Unlock()
						_, _ = c.Write([]byte("OK\n"))
					case strings.HasPrefix(trimmed, "play"):
						parts := strings.Fields(trimmed)
						idx := 0
						if len(parts) > 1 {
							idx, _ = strconv.Atoi(parts[1])
						}
						srv.mu.Lock()
						if idx < 0 || idx >= len(srv.playlist) {
							srv.mu.Unlock()
							_, _ = c.Write([]byte("ACK [2@0] {play} Bad song index\n"))
							continue
						}
						srv.song = idx
						srv.state = "play"
						srv.mu.Unlock()
						_, _ = c.Write([]byte("OK\n"))
					case trimmed == "pause 0":
						srv.mu.Lock()
						if srv.state == "pause" {
							srv.state = "play"
						}
						srv.mu.Unlock()
						_, _ = c.Write([]byte("OK\n"))
					case trimmed == "pause 1":
						srv.mu.Lock()
						srv.state = "pause"
						srv.mu.Unlock()
						_, _ = c.Write([]byte("OK\n"))
					case trimmed == "stop":
						srv.mu.Lock()
						srv.state = "stop"
						srv.mu.Unlock()
						_, _ = c.Write([]byte("OK\n"))
					case trimmed == "clearerror":
						srv.mu.Lock()
						srv.errMsg = ""
						srv.mu.Unlock()
						_, _ = c.Write([]byte("OK\n"))
					default:
						_, _ = c.Write([]byte("OK\n"))
					}
				}
			}(conn)
		}
	}()

	return srv
}

// TestQueueRestoreOnReconnectAndOfflineClear verifica restauración de cola tras reconexión y clear pendiente offline.
func TestQueueRestoreOnReconnectAndOfflineClear(t *testing.T) {
	tmpDir := t.TempDir()
	sockPath := filepath.Join(tmpDir, "restore_mpd.sock")

	c := NewClient(sockPath, "127.0.0.1", "0", nil, nil)
	t.Cleanup(c.Close)

	// Encolar mientras MPD aún no ha arrancado -> debe marcar pendingMPDSync
	tracks := []db.Track{
		{ID: 1, Path: "/music/local/A.flac", Title: "A", Artist: "Artist 1", Duration: 180},
		{ID: 2, Path: "/music/local/B.flac", Title: "B", Artist: "Artist 2", Duration: 200},
	}
	c.ReplaceQueue(tracks, true)
	if !c.pendingMPDSync.Load() {
		t.Fatalf("se esperaba pendingMPDSync=true tras encolar desconectado")
	}

	// Levantar MPD vacío (playlistlength: 0) y reconectar
	srv := startStatefulMockMPDServer(t, sockPath)
	c.tryConnect()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		srv.mu.Lock()
		plLen := len(srv.playlist)
		st := srv.state
		srv.mu.Unlock()
		if plLen == 2 && st == "play" && c.mpdQueueSynced.Load() {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	srv.mu.Lock()
	plLen := len(srv.playlist)
	st := srv.state
	srv.mu.Unlock()
	if plLen != 2 || st != "play" {
		t.Fatalf("tryConnect no restauró cola/reproducción pendiente en MPD (len=%d, state=%q)", plLen, st)
	}

	// Desconectar cliente, ejecutar ClearQueue offline y validar que reconectar vacía MPD en lugar de resucitar tracks
	c.connected.Store(false)
	c.ClearQueue()
	if !c.pendingMPDSync.Load() {
		t.Fatalf("ClearQueue offline debe dejar pendingMPDSync=true")
	}
	c.tryConnect()

	srv.mu.Lock()
	afterClearLen := len(srv.playlist)
	srv.mu.Unlock()
	if afterClearLen != 0 || !c.mpdQueueSynced.Load() {
		t.Fatalf("tryConnect debía vaciar MPD tras ClearQueue offline (len=%d, synced=%v)", afterClearLen, c.mpdQueueSynced.Load())
	}
}

// TestPreemptedPushAndDeltaReconciliation verifica que deltas concurrentes sobre un push parcial reconcilian sin perder colas.
func TestPreemptedPushAndDeltaReconciliation(t *testing.T) {
	tmpDir := t.TempDir()
	sockPath := filepath.Join(tmpDir, "reconcile_mpd.sock")
	srv := startStatefulMockMPDServer(t, sockPath)
	srv.mu.Lock()
	srv.addDelay = 45 * time.Millisecond
	srv.mu.Unlock()

	c := NewClient(sockPath, "127.0.0.1", "0", nil, nil)
	t.Cleanup(c.Close)
	c.tryConnect()

	t0 := db.Track{ID: 1, Path: "/music/local/01.flac", Title: "T0", Artist: "A0"}
	t1 := db.Track{ID: 2, Path: "/music/local/02.flac", Title: "T1", Artist: "A1"}
	t2 := db.Track{ID: 3, Path: "/music/local/03.flac", Title: "T2", Artist: "A2"}
	t3 := db.Track{ID: 4, Path: "/music/local/04.flac", Title: "T3", Artist: "A3"}

	c.ReplaceQueue([]db.Track{t0, t1, t2}, true)
	time.Sleep(55 * time.Millisecond) // Permitir que al menos t0 entre antes de interrumpir el resto
	c.EnqueueTracks([]db.Track{t3}, "append")
	c.MoveInQueue(2, 1)

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if c.mpdQueueSynced.Load() && c.syncedVersion.Load() == c.queueVersion.Load() {
			break
		}
		time.Sleep(25 * time.Millisecond)
	}

	if !c.mpdQueueSynced.Load() || c.syncedVersion.Load() != c.queueVersion.Load() {
		t.Fatalf("no se completó la reconciliación (synced=%v, syncedVer=%d, queueVer=%d)",
			c.mpdQueueSynced.Load(), c.syncedVersion.Load(), c.queueVersion.Load())
	}

	srv.mu.Lock()
	gotPL := append([]string{}, srv.playlist...)
	gotState := srv.state
	srv.mu.Unlock()

	wantOrder := []string{
		"file:///rawmusic/local/01.flac",
		"file:///rawmusic/local/03.flac",
		"file:///rawmusic/local/02.flac",
		"file:///rawmusic/local/04.flac",
	}
	if len(gotPL) != len(wantOrder) {
		t.Fatalf("longitud de playlist MPD inesperada: got=%v want=%v", gotPL, wantOrder)
	}
	for i := range wantOrder {
		if gotPL[i] != wantOrder[i] {
			t.Fatalf("posición %d de MPD desalineada: got=%v want=%v", i, gotPL, wantOrder)
		}
	}
	if gotState != "play" {
		t.Fatalf("se esperaba MPD en play sin corte tras reconciliación, obtenido %q", gotState)
	}
}

// TestBadSongIndexTriggersSyncRetry verifica que Next/PlayIndex recuperan y reproducen cuando MPD responde Bad song index.
func TestBadSongIndexTriggersSyncRetry(t *testing.T) {
	tmpDir := t.TempDir()
	sockPath := filepath.Join(tmpDir, "badsong_mpd.sock")
	srv := startStatefulMockMPDServer(t, sockPath)

	c := NewClient(sockPath, "127.0.0.1", "0", nil, nil)
	t.Cleanup(c.Close)
	c.tryConnect()

	tracks := []db.Track{
		{ID: 1, Path: "/music/local/One.flac", Title: "One", Artist: "Art"},
		{ID: 2, Path: "/music/local/Two.flac", Title: "Two", Artist: "Art"},
	}
	c.ReplaceQueue(tracks, true)

	deadline := time.Now().Add(2 * time.Second)
	for !c.mpdQueueSynced.Load() && time.Now().Before(deadline) {
		time.Sleep(15 * time.Millisecond)
	}

	// Vaciar MPD en secreto dejando mpdQueueSynced aún optimista para provocar "Bad song index" en Next()
	srv.mu.Lock()
	srv.playlist = nil
	srv.song = -1
	srv.state = "stop"
	srv.mu.Unlock()

	c.Next()

	deadline2 := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline2) {
		srv.mu.Lock()
		song := srv.song
		st := srv.state
		plLen := len(srv.playlist)
		srv.mu.Unlock()
		if plLen == 2 && song == 1 && st == "play" {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}

	srv.mu.Lock()
	t.Fatalf("Next() tras Bad song index debía reconciliar y sonar índice 1 (song=%d, state=%q, pl=%v)", srv.song, srv.state, srv.playlist)
}

// TestHardwareOutputErrorVsDecodeAndRadioRecovery valida errores de DAC vs errores de pista y reconexión automática de radio.
func TestHardwareOutputErrorVsDecodeAndRadioRecovery(t *testing.T) {
	if !isAudioOutputHardwareError(`Failed to open "DAC USB" (alsa); device busy`) {
		t.Fatalf("se esperaba clasificar error de dispositivo ALSA como hardware output error")
	}
	if isAudioOutputHardwareError("Failed to decode /music/nas/corrupt.flac") {
		t.Fatalf("error de decodificación de archivo no debe tratarse como fallo hardware ALSA")
	}

	tmpDir := t.TempDir()
	sockPath := filepath.Join(tmpDir, "recovery_mpd.sock")
	srv := startStatefulMockMPDServer(t, sockPath)

	c := NewClient(sockPath, "127.0.0.1", "0", nil, nil)
	t.Cleanup(c.Close)
	c.tryConnect()

	// 1. Con fallo de salida ALSA hardware, syncStatusFromMPD NO debe avanzar trackSkipCount
	c.ReplaceQueue([]db.Track{
		{ID: 1, Path: "/music/local/A.flac", Title: "A", Artist: "X"},
		{ID: 2, Path: "/music/local/B.flac", Title: "B", Artist: "Y"},
	}, true)
	deadline := time.Now().Add(2 * time.Second)
	for !c.mpdQueueSynced.Load() && time.Now().Before(deadline) {
		time.Sleep(15 * time.Millisecond)
	}

	srv.mu.Lock()
	srv.state = "stop"
	srv.errMsg = `Failed to open "DAC USB" (alsa); No such device`
	srv.mu.Unlock()
	time.Sleep(5 * time.Millisecond)
	c.syncStatusFromMPD()

	c.mu.RLock()
	skips := c.trackSkipCount
	c.mu.RUnlock()
	if skips != 0 {
		t.Fatalf("un error de hardware ALSA no debe incrementar trackSkipCount (got=%d)", skips)
	}

	// 2. Corte inesperado en emisora de radio activa -> debe reintentar automáticamente tras 2s
	c.ReplaceQueue([]db.Track{
		{ID: -1, Path: "https://stream.example.org/live.mp3", Title: "Radio Test", Artist: "Spain", Source: "radio"},
	}, true)
	deadline2 := time.Now().Add(2 * time.Second)
	for !c.mpdQueueSynced.Load() && time.Now().Before(deadline2) {
		time.Sleep(15 * time.Millisecond)
	}

	// Simular que la radio sonó brevemente y cayó de golpe (state: stop sin intervención)
	srv.mu.Lock()
	srv.state = "stop"
	srv.mu.Unlock()
	time.Sleep(5 * time.Millisecond)
	c.syncStatusFromMPD()

	// En ~2s scheduleRadioReconnect debería reactivar "play" en índice 0
	deadline3 := time.Now().Add(3500 * time.Millisecond)
	restarted := false
	for time.Now().Before(deadline3) {
		srv.mu.Lock()
		st := srv.state
		srv.mu.Unlock()
		if st == "play" {
			restarted = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !restarted {
		t.Fatalf("la radio cortada inesperadamente debía reconectarse automáticamente")
	}
}

// TestSelectAudioOutputRollbackAndValidation verifica rollback al fallar enableoutput y normalización de ReplayGain/Crossfade.
func TestSelectAudioOutputRollbackAndValidation(t *testing.T) {
	if got, ok := NormalizeReplayGainMode("Track"); !ok || got != "track" {
		t.Fatalf("NormalizeReplayGainMode('Track') inesperado: %q, %v", got, ok)
	}
	if _, ok := NormalizeReplayGainMode("track\nsetvol 100"); ok {
		t.Fatalf("NormalizeReplayGainMode debía rechazar saltos de línea")
	}
	if ClampCrossfade(-5) != 0 || ClampCrossfade(99) != 15 || ClampCrossfade(4) != 4 {
		t.Fatalf("ClampCrossfade fuera de rango esperado")
	}

	tmpDir := t.TempDir()
	sockPath := filepath.Join(tmpDir, "rollback_mpd.sock")
	_ = os.Remove(sockPath)
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("Listen unix falló: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	var mu sync.Mutex
	var cmds []string
	go func() {
		for {
			conn, aErr := ln.Accept()
			if aErr != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				_, _ = c.Write([]byte("OK MPD 0.24.18\n"))
				r := bufio.NewReader(c)
				for {
					line, rErr := r.ReadString('\n')
					if rErr != nil {
						return
					}
					trimmed := strings.TrimSpace(line)
					mu.Lock()
					cmds = append(cmds, trimmed)
					mu.Unlock()
					switch trimmed {
					case "outputs":
						_, _ = c.Write([]byte("outputid: 0\noutputname: DAC USB\nplugin: alsa\noutputenabled: 1\n" +
							"outputid: 1\noutputname: Jack 3.5mm\nplugin: alsa\noutputenabled: 0\nOK\n"))
					case "enableoutput 1":
						_, _ = c.Write([]byte("ACK [50@0] {enableoutput} Device or resource busy\n"))
					default:
						_, _ = c.Write([]byte("OK\n"))
					}
				}
			}(conn)
		}
	}()

	client := NewClient(sockPath, "127.0.0.1", "0", nil, nil)
	t.Cleanup(client.Close)
	client.tryConnect()

	if err := client.SelectAudioOutput(1); err == nil {
		t.Fatalf("SelectAudioOutput(1) debía devolver error cuando enableoutput falla")
	}

	mu.Lock()
	joined := strings.Join(cmds, "\n")
	mu.Unlock()
	if strings.Contains(joined, "disableoutput 0") {
		t.Fatalf("no debía deshabilitar la salida previa 0 si falló enableoutput 1; comandos:\n%s", joined)
	}

	st := client.GetState()
	if st.ActiveOutput != "DAC USB" || len(st.AudioOutputs) < 2 || !st.AudioOutputs[0].Enabled || st.AudioOutputs[1].Enabled {
		t.Fatalf("rollback de salidas inconsistente: active=%q outputs=%+v", st.ActiveOutput, st.AudioOutputs)
	}
}
