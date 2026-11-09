package mpd

import (
	"bufio"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
