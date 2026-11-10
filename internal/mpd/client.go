package mpd

import (
	"bufio"
	"fmt"
	"log"
	"math/rand"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"aurabox/internal/db"
	"aurabox/internal/events"
)

// OutputInfo representa una interfaz de salida de audio en MPD / ALSA / PulseAudio.
type OutputInfo struct {
	ID      int    `json:"id"`
	Name    string `json:"name"`
	Plugin  string `json:"plugin"`
	Enabled bool   `json:"enabled"`
}

// AudioDiagnostics expone las salidas de MPD junto con la lectura cruda de aplay -l en el contenedor.
type AudioDiagnostics struct {
	Outputs     []OutputInfo `json:"outputs"`
	Active      string       `json:"active_output"`
	AlsaRaw     string       `json:"alsa_raw"`
	HasDAC      bool         `json:"has_dac"`
	HasHardware bool         `json:"has_hardware"`
}

// PlayerState representa el estado completo de reproducción sincronizado hacia el frontend.
type PlayerState struct {
	Status         string       `json:"status"` // "play", "pause", "stop"
	CurrentTrack   *db.Track    `json:"current_track"`
	CurrentIndex   int          `json:"current_index"`
	QueueLength    int          `json:"queue_length"`
	ElapsedTime    float64      `json:"elapsed_time"`
	Duration       float64      `json:"duration"`
	Volume         int          `json:"volume"`
	MaxVolumeLimit int          `json:"max_volume_limit"`
	Repeat         bool         `json:"repeat"`
	Random         bool         `json:"random"`
	Crossfade      int          `json:"crossfade"`
	ReplayGain     string       `json:"replay_gain"` // "off", "track", "album", "auto"
	AudioOutputs   []OutputInfo `json:"audio_outputs"`
	ActiveOutput   string       `json:"active_output"`
	SleepTimerSec  int          `json:"sleep_timer_sec"`
	StreamTitle    string       `json:"stream_title,omitempty"`
	LastError      string       `json:"last_error,omitempty"`
}

// Client gestiona la comunicación por socket Unix/TCP con MPD y mantiene el estado en memoria sin bloqueos.
type Client struct {
	socketPath        string
	host              string
	port              string
	eventsHub         *events.Hub
	database          *db.DB
	state             PlayerState
	queue             []db.Track
	mu                sync.RWMutex // Exclusivo para memoria (nunca retener en llamadas de red o SQLite)
	connMu            sync.Mutex   // Serializa el protocolo petición/respuesta en el socket de MPD
	syncMu            sync.Mutex   // Serializa secuencias compuestas de actualización de cola en MPD
	dialMu            sync.Mutex   // Evita intentos paralelos de conexión inicial hacia MPD
	conn              net.Conn
	reader            *bufio.Reader
	lastDialFail      time.Time
	connected         atomic.Bool
	usingUnixSocket   atomic.Bool
	syncingQueue      atomic.Bool
	mpdQueueSynced    atomic.Bool
	pendingMPDSync    atomic.Bool
	queueVersion      atomic.Uint64
	syncedVersion     atomic.Uint64
	lastControlNano   atomic.Int64
	radioRetryCount   int
	lastRadioRetryAt  time.Time
	trackSkipCount    int
	lastTrackSkipAt   time.Time
	lastEmptyResyncAt time.Time
	sleepEndsAt       time.Time
	reconnectCh       chan struct{}
	persistCh         chan func()
	stopCh            chan struct{}
	persistWg         sync.WaitGroup
	closeOnce         sync.Once
	closed            atomic.Bool
}

// NewClient crea un nuevo controlador de audio con soporte Unix socket / TCP, salvaguardas de volumen y recuperación de ajustes.
func NewClient(socketPath, host, port string, hub *events.Hub, database *db.DB) *Client {
	c := &Client{
		socketPath:  socketPath,
		host:        host,
		port:        port,
		eventsHub:   hub,
		database:    database,
		queue:       make([]db.Track, 0),
		reconnectCh: make(chan struct{}, 1),
		persistCh:   make(chan func(), 256),
		stopCh:      make(chan struct{}),
		state: PlayerState{
			Status:         "stop",
			CurrentIndex:   -1,
			Volume:         65,
			MaxVolumeLimit: 85,
			Repeat:         false,
			Random:         false,
			Crossfade:      2,
			ReplayGain:     "auto",
			AudioOutputs:   make([]OutputInfo, 0),
			ActiveOutput:   "Detectando audio...",
		},
	}

	if database != nil {
		if savedMax := database.GetSetting("max_volume_limit", ""); savedMax != "" {
			if m, err := strconv.Atoi(savedMax); err == nil && m >= 30 && m <= 100 {
				c.state.MaxVolumeLimit = m
			}
		}
		if savedVol := database.GetSetting("player_volume", ""); savedVol != "" {
			if v, err := strconv.Atoi(savedVol); err == nil && v >= 0 {
				if v > c.state.MaxVolumeLimit {
					v = c.state.MaxVolumeLimit
				}
				c.state.Volume = v
			}
		}
		if savedRG := database.GetSetting("replay_gain", ""); savedRG != "" {
			c.state.ReplayGain = savedRG
		}
		if savedCF := database.GetSetting("crossfade", ""); savedCF != "" {
			if cf, err := strconv.Atoi(savedCF); err == nil && cf >= 0 {
				c.state.Crossfade = cf
			}
		}
		c.persistWg.Add(1)
		go c.persistWorker()
	}

	go c.connectionSupervisor()
	go c.progressTicker()

	return c
}

// Close detiene los workers de segundo plano, vacía escrituras SQLite pendientes y cierra la conexión MPD.
func (c *Client) Close() {
	c.closeOnce.Do(func() {
		c.closed.Store(true)
		close(c.stopCh)
		c.persistWg.Wait()

		c.connMu.Lock()
		if c.conn != nil {
			_ = c.conn.Close()
			c.conn = nil
			c.reader = nil
		}
		c.connected.Store(false)
		c.connMu.Unlock()
	})
}

// persistWorker procesa escrituras diferidas hacia SQLite en orden FIFO sin bloquear rutas críticas de audio o HTTP.
func (c *Client) persistWorker() {
	defer c.persistWg.Done()
	for {
		select {
		case fn := <-c.persistCh:
			if fn != nil {
				fn()
			}
		case <-c.stopCh:
			for {
				select {
				case fn := <-c.persistCh:
					if fn != nil {
						fn()
					}
				default:
					return
				}
			}
		}
	}
}

// enqueuePersist programa una escritura en SQLite en segundo plano sin retener locks de memoria ni peticiones HTTP.
func (c *Client) enqueuePersist(fn func()) {
	if fn == nil || c.database == nil || c.closed.Load() {
		return
	}
	select {
	case c.persistCh <- fn:
	default:
		// Respaldo no bloqueante si el canal se satura temporalmente durante operaciones masivas
		go fn()
	}
}

// markControlUpdate registra el instante de un control local para descartar snapshots antiguos de status MPD.
func (c *Client) markControlUpdate() {
	c.lastControlNano.Store(time.Now().UnixNano())
}

// triggerReconnect despierta al supervisor de conexión MPD sin bloquear el hilo llamante.
func (c *Client) triggerReconnect() {
	if c.closed.Load() || c.reconnectCh == nil {
		return
	}
	select {
	case c.reconnectCh <- struct{}{}:
	default:
	}
}

// connectionSupervisor reintenta periódicamente el enlace con MPD si el servidor se reinicia o aún está arrancando.
func (c *Client) connectionSupervisor() {
	for {
		if c.closed.Load() {
			return
		}
		if !c.isConnected() {
			c.tryConnect()
		}
		timer := time.NewTimer(3 * time.Second)
		select {
		case <-c.stopCh:
			timer.Stop()
			return
		case <-c.reconnectCh:
			timer.Stop()
		case <-timer.C:
		}
	}
}

// connectLocked establece el socket Unix (preferido) o TCP con MPD bajo connMu.
func (c *Client) connectLocked() error {
	if !c.lastDialFail.IsZero() && time.Since(c.lastDialFail) < time.Second {
		return fmt.Errorf("mpd reintento reciente fallido")
	}

	if c.conn != nil {
		_ = c.conn.Close()
		c.conn = nil
		c.reader = nil
	}

	var conn net.Conn
	var err error
	var usedUnix bool
	endpoint := ""

	if c.socketPath != "" {
		conn, err = net.DialTimeout("unix", c.socketPath, 1500*time.Millisecond)
		if err == nil {
			usedUnix = true
			endpoint = c.socketPath
		}
	}

	if conn == nil {
		endpoint = net.JoinHostPort(c.host, c.port)
		conn, err = net.DialTimeout("tcp", endpoint, 2*time.Second)
		usedUnix = false
	}

	if err != nil {
		c.lastDialFail = time.Now()
		c.connected.Store(false)
		c.usingUnixSocket.Store(false)
		return err
	}

	// Asegurar timeout en el handshake inicial para evitar bloqueos indefinidos
	_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
	reader := bufio.NewReader(conn)
	banner, err := reader.ReadString('\n')
	if err != nil || !strings.HasPrefix(banner, "OK MPD") {
		_ = conn.Close()
		c.lastDialFail = time.Now()
		c.connected.Store(false)
		c.usingUnixSocket.Store(false)
		return fmt.Errorf("banner MPD inválido (%s): %v", endpoint, err)
	}
	_ = conn.SetDeadline(time.Time{})

	c.conn = conn
	c.reader = reader
	c.lastDialFail = time.Time{}
	c.usingUnixSocket.Store(usedUnix)
	c.connected.Store(true)
	log.Printf("[MPD] Conectado exitosamente al servidor de audio en %s (%s)", endpoint, strings.TrimSpace(banner))
	return nil
}

// parseMPDPlaylistLength extrae la longitud actual de la lista de MPD desde las líneas de status.
func parseMPDPlaylistLength(lines []string) int {
	for _, ln := range lines {
		if strings.HasPrefix(ln, "playlistlength: ") {
			val, err := strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(ln, "playlistlength: ")))
			if err == nil {
				return val
			}
		}
	}
	return -1
}

// snapshotQueueState obtiene una copia consistente en memoria de la cola, índice activo, estado de reproducción y versión.
func (c *Client) snapshotQueueState() ([]db.Track, int, bool, uint64) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	copied := make([]db.Track, len(c.queue))
	copy(copied, c.queue)
	return copied, c.state.CurrentIndex, c.state.Status == "play", c.queueVersion.Load()
}

// tryConnect establece sesión con MPD, aplica opciones Hi-Fi y sincroniza la cola tras reconexiones o reinicios.
func (c *Client) tryConnect() {
	c.dialMu.Lock()
	defer c.dialMu.Unlock()

	if c.closed.Load() || c.isConnected() {
		return
	}

	c.connMu.Lock()
	err := c.connectLocked()
	c.connMu.Unlock()
	if err != nil {
		return
	}

	c.mu.RLock()
	rg := c.state.ReplayGain
	cf := c.state.Crossfade
	vol := c.state.Volume
	maxVol := c.state.MaxVolumeLimit
	c.mu.RUnlock()

	if vol > maxVol {
		vol = maxVol
	}

	_, _ = c.sendRawCommand(fmt.Sprintf("replay_gain_mode %s\n", rg))
	_, _ = c.sendRawCommand(fmt.Sprintf("crossfade %d\n", cf))
	_, _ = c.sendRawCommand(fmt.Sprintf("setvol %d\n", vol))

	c.syncOutputsFromMPD()

	// Restaurar salida física preferida por nombre si existe en el hardware actual
	if c.database != nil {
		savedName := c.database.GetSetting("active_audio_output_name", "")
		if savedName != "" {
			st := c.GetState()
			for _, o := range st.AudioOutputs {
				if o.Name == savedName && !o.Enabled {
					_ = c.SelectAudioOutput(o.ID)
					break
				}
			}
		}
	}

	statusLines, statusErr := c.sendRawCommand("status\n")
	mpdPlaylistLen := -1
	if statusErr == nil {
		mpdPlaylistLen = parseMPDPlaylistLength(statusLines)
	}

	qCopy, currIdx, wasPlaying, ver := c.snapshotQueueState()
	pendingSync := c.pendingMPDSync.Load()

	if len(qCopy) == 0 {
		if pendingSync {
			_, _ = c.sendRawCommand("clear\n")
			c.syncedVersion.Store(ver)
			c.pendingMPDSync.Store(false)
			c.mpdQueueSynced.Store(true)
		} else {
			c.syncQueueFromMPD()
		}
	} else if pendingSync || mpdPlaylistLen == 0 || (mpdPlaylistLen > 0 && mpdPlaylistLen != len(qCopy)) {
		c.mpdQueueSynced.Store(false)
		go c.pushQueueToMPD(ver, qCopy, currIdx, wasPlaying)
	} else if mpdPlaylistLen > 0 {
		c.syncedVersion.Store(ver)
		c.mpdQueueSynced.Store(true)
	}

	c.syncStatusFromMPD()
}

// isConnected retorna si existe conexión activa actual con el demonio MPD.
func (c *Client) isConnected() bool {
	return c.connected.Load()
}

// mpdCommandDeadline asigna margen temporal amplio a comandos que abren rutas NAS o flujos HTTP en MPD.
func mpdCommandDeadline(cmd string) time.Duration {
	lower := strings.ToLower(strings.TrimSpace(cmd))
	switch {
	case strings.HasPrefix(lower, "add "),
		strings.HasPrefix(lower, "addid "),
		strings.HasPrefix(lower, "play"),
		strings.HasPrefix(lower, "seek"),
		strings.HasPrefix(lower, "playlist"),
		strings.HasPrefix(lower, "delete "):
		return 10 * time.Second
	default:
		return 4 * time.Second
	}
}

// sendRawCommand envía una directiva de texto a MPD con reconexión transparente ante cortes por inactividad.
func (c *Client) sendRawCommand(cmd string) ([]string, error) {
	c.connMu.Lock()
	defer c.connMu.Unlock()

	if !c.connected.Load() || c.conn == nil {
		if err := c.connectLocked(); err != nil {
			return nil, fmt.Errorf("mpd desconectado: %w", err)
		}
	}

	deadlineDur := mpdCommandDeadline(cmd)
	execCmd := func() ([]string, error) {
		_ = c.conn.SetDeadline(time.Now().Add(deadlineDur))
		if _, err := c.conn.Write([]byte(cmd)); err != nil {
			c.connected.Store(false)
			return nil, err
		}

		var lines []string
		for {
			_ = c.conn.SetDeadline(time.Now().Add(deadlineDur))
			line, err := c.reader.ReadString('\n')
			if err != nil {
				c.connected.Store(false)
				return nil, err
			}
			trimmed := strings.TrimSpace(line)
			if trimmed == "OK" {
				break
			}
			if strings.HasPrefix(trimmed, "ACK ") {
				_ = c.conn.SetDeadline(time.Time{})
				return nil, fmt.Errorf("mpd ack error: %s", trimmed)
			}
			lines = append(lines, trimmed)
		}
		_ = c.conn.SetDeadline(time.Time{})
		return lines, nil
	}

	lines, err := execCmd()
	if err != nil && !strings.HasPrefix(err.Error(), "mpd ack error:") {
		// Reconectar de inmediato si hubo error de red/socket
		c.lastDialFail = time.Time{}
		if reconnErr := c.connectLocked(); reconnErr == nil {
			return execCmd()
		}
	}
	return lines, err
}

// mpdQuote entrecomilla argumentos para MPD preservando UTF-8 real y escapando comillas o barras invertidas.
func mpdQuote(s string) string {
	s = strings.ReplaceAll(s, "\r", "")
	s = strings.ReplaceAll(s, "\n", "")
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `"`, `\"`)
	return `"` + s + `"`
}

// localMPDURIs construye la ruta relativa en music_directory y la URI file:// directa para socket Unix.
func localMPDURIs(trackPath string) (string, string) {
	relPath := strings.TrimPrefix(trackPath, "/music/")
	relPath = strings.TrimPrefix(relPath, "/")
	if relPath == "" {
		return "", ""
	}
	rawURI := "file:///rawmusic/" + relPath
	if !strings.HasPrefix(trackPath, "/music/") && strings.HasPrefix(trackPath, "/") {
		rawURI = "file://" + trackPath
	}
	return relPath, rawURI
}

// isMPDAckError identifica errores lógicos reportados explícitamente por MPD (archivo faltante, formato, índice).
func isMPDAckError(err error) bool {
	return err != nil && strings.HasPrefix(err.Error(), "mpd ack error:")
}

// addTrackToMPD añade una pista priorizando file:///rawmusic/... en socket Unix para evitar latencia de base MPD.
func (c *Client) addTrackToMPD(t db.Track) error {
	if strings.HasPrefix(t.Path, "http://") || strings.HasPrefix(t.Path, "https://") {
		resolvedURLs, err := ResolveStreamURLs(t.Path)
		if err != nil {
			return err
		}
		var lastErr error
		for _, streamURL := range resolvedURLs {
			_, addErr := c.sendRawCommand(fmt.Sprintf("add %s\n", mpdQuote(streamURL)))
			if addErr == nil {
				return nil
			}
			lastErr = addErr
		}
		return lastErr
	}

	relPath, rawURI := localMPDURIs(t.Path)
	if relPath == "" {
		return fmt.Errorf("ruta de pista vacía")
	}

	if c.usingUnixSocket.Load() && rawURI != "" {
		_, rawErr := c.sendRawCommand(fmt.Sprintf("add %s\n", mpdQuote(rawURI)))
		if rawErr == nil {
			return nil
		}
		if !isMPDAckError(rawErr) {
			return rawErr
		}
	}

	_, err := c.sendRawCommand(fmt.Sprintf("add %s\n", mpdQuote(relPath)))
	return err
}

// addTrackToMPDAt inserta una pista en una posición concreta usando addid para saltos instantáneos en play_now.
func (c *Client) addTrackToMPDAt(t db.Track, pos int) error {
	posSuffix := ""
	if pos >= 0 {
		posSuffix = fmt.Sprintf(" %d", pos)
	}

	if strings.HasPrefix(t.Path, "http://") || strings.HasPrefix(t.Path, "https://") {
		resolvedURLs, err := ResolveStreamURLs(t.Path)
		if err != nil {
			return err
		}
		var lastErr error
		for _, streamURL := range resolvedURLs {
			_, addErr := c.sendRawCommand(fmt.Sprintf("addid %s%s\n", mpdQuote(streamURL), posSuffix))
			if addErr == nil {
				return nil
			}
			lastErr = addErr
		}
		return lastErr
	}

	relPath, rawURI := localMPDURIs(t.Path)
	if relPath == "" {
		return fmt.Errorf("ruta de pista vacía")
	}

	if c.usingUnixSocket.Load() && rawURI != "" {
		_, rawErr := c.sendRawCommand(fmt.Sprintf("addid %s%s\n", mpdQuote(rawURI), posSuffix))
		if rawErr == nil {
			return nil
		}
		if !isMPDAckError(rawErr) {
			return rawErr
		}
	}

	_, err := c.sendRawCommand(fmt.Sprintf("addid %s%s\n", mpdQuote(relPath), posSuffix))
	return err
}

// normalizeMPDFilePath normaliza rutas devueltas por MPD (incluidas file:///rawmusic/...) a rutas /music/... de SQLite.
func normalizeMPDFilePath(file string) string {
	if strings.HasPrefix(file, "http://") || strings.HasPrefix(file, "https://") {
		return file
	}
	if strings.HasPrefix(file, "file:///rawmusic/") {
		return "/music/" + strings.TrimPrefix(file, "file:///rawmusic/")
	}
	if strings.HasPrefix(file, "file:///music/") {
		return "/music/" + strings.TrimPrefix(file, "file:///music/")
	}
	if strings.HasPrefix(file, "file:///") {
		return "/" + strings.TrimPrefix(file, "file:///")
	}
	return "/music/" + strings.TrimPrefix(file, "/")
}

// syncOutputsFromMPD actualiza el listado real y estado de las salidas físicas y virtuales de MPD.
func (c *Client) syncOutputsFromMPD() {
	lines, err := c.sendRawCommand("outputs\n")
	if err != nil {
		return
	}

	var outputs []OutputInfo
	var current OutputInfo
	hasCurrent := false
	for _, line := range lines {
		parts := strings.SplitN(line, ": ", 2)
		if len(parts) != 2 {
			continue
		}
		key, val := parts[0], parts[1]
		switch key {
		case "outputid":
			if hasCurrent && current.Name != "" {
				outputs = append(outputs, current)
			}
			id, _ := strconv.Atoi(val)
			current = OutputInfo{ID: id}
			hasCurrent = true
		case "outputname":
			current.Name = val
		case "plugin":
			current.Plugin = val
		case "outputenabled":
			current.Enabled = (val == "1")
		}
	}
	if hasCurrent && current.Name != "" {
		outputs = append(outputs, current)
	}

	if len(outputs) > 0 {
		activeName := ""
		for _, o := range outputs {
			if o.Enabled && o.Plugin != "httpd" {
				activeName = o.Name
				break
			}
		}
		if activeName == "" {
			for _, o := range outputs {
				if o.Enabled {
					activeName = o.Name
					break
				}
			}
		}

		c.mu.Lock()
		c.state.AudioOutputs = outputs
		c.state.ActiveOutput = activeName
		c.mu.Unlock()
	}
}

// syncQueueFromMPD recupera la cola activa desde MPD y reconstruye sus metadatos desde SQLite.
func (c *Client) syncQueueFromMPD() {
	lines, err := c.sendRawCommand("playlistinfo\n")
	if err != nil {
		return
	}
	if len(lines) == 0 {
		c.syncedVersion.Store(c.queueVersion.Load())
		c.mpdQueueSynced.Store(true)
		c.pendingMPDSync.Store(false)
		return
	}

	var tracks []db.Track
	var currentFile string
	var currentTitle string
	var currentArtist string
	var currentAlbum string
	var currentName string
	var currentDuration float64

	addCurrent := func() {
		if currentFile == "" {
			return
		}
		fullPath := normalizeMPDFilePath(currentFile)
		isHTTP := strings.HasPrefix(fullPath, "http://") || strings.HasPrefix(fullPath, "https://")
		if c.database != nil {
			if isHTTP {
				if radio, rErr := c.database.GetRadioByStreamURL(fullPath); rErr == nil && radio != nil {
					bitrateVal := 128
					if b, err := strconv.Atoi(radio.Bitrate); err == nil && b > 0 {
						bitrateVal = b
					}
					formatVal := radio.Format
					if formatVal == "" {
						formatVal = "STREAM"
					}
					coverVal := radio.LogoURL
					if coverVal == "" {
						coverVal = "/assets/default-cover.svg"
					}
					tracks = append(tracks, db.Track{
						ID:       -1,
						Path:     fullPath,
						Filename: radio.Name,
						Title:    radio.Name,
						Artist:   FormatRadioArtist(radio.Broadcaster, radio.Country),
						Album:    radio.Genre,
						Genre:    radio.Genre,
						Bitrate:  bitrateVal,
						Format:   formatVal,
						CoverURL: coverVal,
						Source:   "radio",
						Duration: 0,
					})
					currentFile = ""
					return
				}
			} else if t, err := c.database.GetTrackByPath(fullPath); err == nil && t != nil {
				if currentDuration > 0 && (t.Duration <= 0 || t.Duration == 240.0) {
					t.Duration = currentDuration
					trackID := t.ID
					dur := currentDuration
					c.enqueuePersist(func() {
						_ = c.database.UpdateTrackDuration(trackID, dur)
					})
				}
				tracks = append(tracks, *t)
				currentFile = ""
				return
			}
		}

		// Fallback con metadatos de etiquetas o ruta si aún no estaba en SQLite
		if isHTTP {
			fallbackTitle := currentName
			if fallbackTitle == "" {
				fallbackTitle = currentTitle
			}
			if fallbackTitle == "" {
				parts := strings.Split(fullPath, "/")
				fallbackTitle = parts[len(parts)-1]
			}
			if fallbackTitle == "" {
				fallbackTitle = "Radio Web"
			}
			tracks = append(tracks, db.Track{
				ID:       -1,
				Path:     fullPath,
				Filename: fallbackTitle,
				Title:    fallbackTitle,
				Artist:   "Radio en Directo",
				Album:    "Radio Web",
				Format:   "STREAM",
				CoverURL: "/assets/default-cover.svg",
				Source:   "radio",
				Duration: 0,
			})
			currentFile = ""
			return
		}

		fallbackTitle := currentTitle
		if fallbackTitle == "" {
			parts := strings.Split(fullPath, "/")
			fallbackTitle = parts[len(parts)-1]
		}
		tracks = append(tracks, db.Track{
			Path:     fullPath,
			Filename: fallbackTitle,
			Title:    fallbackTitle,
			Artist:   currentArtist,
			Album:    currentAlbum,
			Duration: currentDuration,
			Format:   "AUDIO",
			Source:   "local",
		})
		currentFile = ""
	}

	for _, line := range lines {
		parts := strings.SplitN(line, ": ", 2)
		if len(parts) != 2 {
			continue
		}
		key, val := parts[0], parts[1]
		switch key {
		case "file":
			addCurrent()
			currentFile = val
			currentTitle = ""
			currentArtist = ""
			currentAlbum = ""
			currentName = ""
			currentDuration = 0
		case "Title":
			currentTitle = val
		case "Artist":
			currentArtist = val
		case "Album":
			currentAlbum = val
		case "Name":
			currentName = val
		case "duration":
			if d, err := strconv.ParseFloat(val, 64); err == nil {
				currentDuration = d
			}
		case "Time":
			if currentDuration == 0 {
				if d, err := strconv.ParseFloat(val, 64); err == nil {
					currentDuration = d
				}
			}
		}
	}
	addCurrent()

	if len(tracks) > 0 {
		c.mu.Lock()
		c.queue = tracks
		c.state.QueueLength = len(tracks)
		ver := c.queueVersion.Load()
		c.mu.Unlock()
		c.syncedVersion.Store(ver)
		c.mpdQueueSynced.Store(true)
		c.pendingMPDSync.Store(false)
		c.broadcastQueue()
	}
}

// isHTTPStreamTrack indica si una pista corresponde a un flujo HTTP/HTTPS de radio por internet.
func isHTTPStreamTrack(t *db.Track) bool {
	if t == nil {
		return false
	}
	if t.Source == "radio" {
		return true
	}
	return strings.HasPrefix(t.Path, "http://") || strings.HasPrefix(t.Path, "https://")
}

// parseMPDAudioSampleRate extrae la frecuencia de muestreo en Hz desde la línea "audio: rate:bits:ch" de MPD.
func parseMPDAudioSampleRate(raw string) int {
	parts := strings.Split(strings.TrimSpace(raw), ":")
	if len(parts) == 0 {
		return 0
	}
	rate, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || rate <= 0 {
		return 0
	}
	return rate
}

// fetchCurrentStreamTitle consulta currentsong en MPD para obtener el título ICY actual de una radio en directo.
func (c *Client) fetchCurrentStreamTitle(stationTitle string) (string, bool) {
	lines, err := c.sendRawCommand("currentsong\n")
	if err != nil {
		return "", false
	}

	var icyTitle string
	var icyArtist string
	var icyName string
	for _, line := range lines {
		parts := strings.SplitN(line, ": ", 2)
		if len(parts) != 2 {
			continue
		}
		key, val := parts[0], parts[1]
		switch key {
		case "Title":
			icyTitle = val
		case "Artist":
			icyArtist = val
		case "Name":
			icyName = val
		}
	}

	refName := stationTitle
	if refName == "" {
		refName = icyName
	}
	return cleanStreamTitle(icyTitle, icyArtist, refName), true
}

// isAudioOutputHardwareError detecta errores locales de dispositivo ALSA o hardware para no saltar toda la cola en vano.
func isAudioOutputHardwareError(errMsg string) bool {
	lower := strings.ToLower(strings.TrimSpace(errMsg))
	if lower == "" {
		return false
	}
	for _, kw := range []string{"audio output", "alsa", "device or resource busy", "no such device", "failed to open"} {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// scheduleRadioReconnect espera el backoff indicado y relanza la emisora si no medió intervención del usuario.
func (c *Client) scheduleRadioReconnect(ver uint64, ctrlNano int64, targetIdx int, stationTitle string, attempt int, delay time.Duration) {
	timer := time.NewTimer(delay)
	defer timer.Stop()

	select {
	case <-c.stopCh:
		return
	case <-timer.C:
	}

	if c.closed.Load() || !c.isConnected() || c.queueVersion.Load() != ver || c.lastControlNano.Load() != ctrlNano {
		return
	}

	c.mu.Lock()
	if c.queueVersion.Load() != ver || targetIdx < 0 || targetIdx >= len(c.queue) || c.state.Status == "pause" {
		c.mu.Unlock()
		return
	}
	trackCopy := c.queue[targetIdx]
	if !isHTTPStreamTrack(&trackCopy) {
		c.mu.Unlock()
		return
	}
	c.state.CurrentIndex = targetIdx
	c.state.CurrentTrack = &trackCopy
	c.state.Status = "play"
	c.state.LastError = ""
	c.mu.Unlock()

	log.Printf("[MPD] Corte detectado en radio en directo (%s); reintento automático %d/3...", stationTitle, attempt)
	_, _ = c.sendRawCommand("clearerror\n")
	c.playIndexOrRetry(ver, targetIdx, false)
	c.broadcastState()
}

// scheduleTrackErrorSkip salta a la siguiente pista de la cola tras un fallo puntual de archivo o códec en MPD.
func (c *Client) scheduleTrackErrorSkip(ver uint64, ctrlNano int64, nextIdx int, mpdErr string) {
	timer := time.NewTimer(1500 * time.Millisecond)
	defer timer.Stop()

	select {
	case <-c.stopCh:
		return
	case <-timer.C:
	}

	if c.closed.Load() || !c.isConnected() || c.queueVersion.Load() != ver || c.lastControlNano.Load() != ctrlNano {
		return
	}

	c.mu.RLock()
	valid := (c.queueVersion.Load() == ver && nextIdx >= 0 && nextIdx < len(c.queue) && c.state.Status != "pause")
	c.mu.RUnlock()
	if !valid {
		return
	}

	log.Printf("[MPD] Saltando pista tras error puntual de lectura/decodificación (%s) -> índice %d", mpdErr, nextIdx)
	c.PlayIndex(nextIdx)
}

// syncStatusFromMPD sondea el estado real de MPD para sincronizar tiempo, pista actual, errores, metadata ICY y recuperación automática.
func (c *Client) syncStatusFromMPD() bool {
	if !c.isConnected() || c.syncingQueue.Load() {
		return false
	}

	startedAt := time.Now().UnixNano()
	lines, err := c.sendRawCommand("status\n")
	if err != nil {
		return false
	}

	if c.syncingQueue.Load() {
		return false
	}

	var mpdState string
	var songIdx = -1
	var mpdPlaylistLen = -1
	var elapsed float64 = -1
	var duration float64
	var volume = -1
	var hasRepeat bool
	var mpdRepeat bool
	var mpdErr string
	var mpdSampleRate int
	var mpdBitrate int

	for _, line := range lines {
		parts := strings.SplitN(line, ": ", 2)
		if len(parts) != 2 {
			continue
		}
		key, val := parts[0], parts[1]
		switch key {
		case "state":
			mpdState = val // "play", "pause", "stop"
		case "song":
			if s, err := strconv.Atoi(val); err == nil {
				songIdx = s
			}
		case "playlistlength":
			if pl, err := strconv.Atoi(strings.TrimSpace(val)); err == nil {
				mpdPlaylistLen = pl
			}
		case "elapsed":
			if el, err := strconv.ParseFloat(val, 64); err == nil {
				elapsed = el
			}
		case "duration":
			if d, err := strconv.ParseFloat(val, 64); err == nil {
				duration = d
			}
		case "volume":
			if v, err := strconv.Atoi(val); err == nil && v >= 0 {
				volume = v
			}
		case "repeat":
			hasRepeat = true
			mpdRepeat = (val == "1")
		case "error":
			mpdErr = strings.TrimSpace(val)
		case "audio":
			mpdSampleRate = parseMPDAudioSampleRate(val)
		case "bitrate":
			if b, err := strconv.Atoi(strings.TrimSpace(val)); err == nil && b > 0 {
				mpdBitrate = b
			}
		}
	}

	c.mu.RLock()
	qLen := len(c.queue)
	c.mu.RUnlock()
	if qLen == 0 && songIdx >= 0 {
		c.syncQueueFromMPD()
	}

	var isLiveStream bool
	var stationTitle string
	c.mu.RLock()
	var targetTrack *db.Track
	if songIdx >= 0 && songIdx < len(c.queue) {
		t := c.queue[songIdx]
		targetTrack = &t
	} else if c.state.CurrentTrack != nil {
		t := *c.state.CurrentTrack
		targetTrack = &t
	}
	if targetTrack != nil && isHTTPStreamTrack(targetTrack) {
		isLiveStream = true
		stationTitle = targetTrack.Title
	}
	c.mu.RUnlock()

	var fetchedStreamTitle string
	var hasStreamTitle bool
	if isLiveStream && mpdState == "play" {
		fetchedStreamTitle, hasStreamTitle = c.fetchCurrentStreamTitle(stationTitle)
	}

	var recordTrackID int64
	var durUpdateTrackID int64
	var durUpdateVal float64
	clampVolTarget := -1

	needEmptyResync := false
	var resyncQueue []db.Track
	var resyncIdx int
	var resyncPlay bool
	var resyncVer uint64

	needRadioReconnect := false
	var radioAttempt int
	var radioDelay time.Duration
	var radioVer uint64
	var radioCtrlNano int64
	var radioTargetIdx int

	needTrackSkip := false
	var skipVer uint64
	var skipCtrlNano int64
	var skipNextIdx int
	var prevLastErr string

	c.mu.Lock()
	if c.lastControlNano.Load() > startedAt {
		c.mu.Unlock()
		return false
	}
	stateChanged := false
	trackChanged := false

	prevStatus := c.state.Status
	prevLastErr = c.state.LastError
	currQLen := len(c.queue)
	wasLiveStream := isLiveStream || (c.state.CurrentTrack != nil && isHTTPStreamTrack(c.state.CurrentTrack))

	if mpdState != "" && mpdState != c.state.Status {
		c.state.Status = mpdState
		stateChanged = true
	}

	if hasRepeat && mpdRepeat != c.state.Repeat {
		c.state.Repeat = mpdRepeat
		stateChanged = true
	}

	if songIdx >= 0 && songIdx < len(c.queue) && (songIdx != c.state.CurrentIndex || c.state.CurrentTrack == nil) {
		trackCopy := c.queue[songIdx]
		c.state.CurrentIndex = songIdx
		c.state.CurrentTrack = &trackCopy
		c.state.StreamTitle = ""
		if duration > 0 {
			c.state.Duration = duration
			c.state.CurrentTrack.Duration = duration
			durUpdateVal = duration
		} else {
			c.state.Duration = c.state.CurrentTrack.Duration
		}
		trackChanged = true
		if c.state.CurrentTrack.ID > 0 {
			recordTrackID = c.state.CurrentTrack.ID
			if duration > 0 {
				durUpdateTrackID = c.state.CurrentTrack.ID
			}
		}
	}

	if mpdErr != "" {
		if mpdErr != c.state.LastError {
			c.state.LastError = mpdErr
			stateChanged = true
		}
	} else if mpdState == "play" && c.state.LastError != "" {
		c.state.LastError = ""
		stateChanged = true
	}

	if isLiveStream && mpdState == "play" {
		if hasStreamTitle && fetchedStreamTitle != c.state.StreamTitle {
			c.state.StreamTitle = fetchedStreamTitle
			stateChanged = true
		}
	} else if c.state.StreamTitle != "" {
		c.state.StreamTitle = ""
		stateChanged = true
	}

	if c.state.CurrentTrack != nil && isHTTPStreamTrack(c.state.CurrentTrack) {
		if mpdSampleRate > 0 && c.state.CurrentTrack.SampleRate != mpdSampleRate {
			c.state.CurrentTrack.SampleRate = mpdSampleRate
			if songIdx >= 0 && songIdx < len(c.queue) && isHTTPStreamTrack(&c.queue[songIdx]) {
				c.queue[songIdx].SampleRate = mpdSampleRate
			}
			stateChanged = true
		}
		if mpdBitrate > 0 && c.state.CurrentTrack.Bitrate != mpdBitrate {
			c.state.CurrentTrack.Bitrate = mpdBitrate
			if songIdx >= 0 && songIdx < len(c.queue) && isHTTPStreamTrack(&c.queue[songIdx]) {
				c.queue[songIdx].Bitrate = mpdBitrate
			}
			stateChanged = true
		}
	}

	if elapsed >= 0 {
		c.state.ElapsedTime = elapsed
	}
	if duration > 0 {
		c.state.Duration = duration
		if c.state.CurrentTrack != nil && (c.state.CurrentTrack.Duration <= 0 || c.state.CurrentTrack.Duration == 240.0) {
			c.state.CurrentTrack.Duration = duration
			if c.state.CurrentTrack.ID > 0 {
				durUpdateTrackID = c.state.CurrentTrack.ID
				durUpdateVal = duration
			}
		}
	}

	if volume >= 0 {
		if volume > c.state.MaxVolumeLimit {
			clampVolTarget = c.state.MaxVolumeLimit
			if c.state.Volume != clampVolTarget {
				c.state.Volume = clampVolTarget
				stateChanged = true
			}
		} else if volume != c.state.Volume {
			c.state.Volume = volume
			stateChanged = true
		}
	}

	if mpdState == "play" && elapsed > 12.0 {
		c.radioRetryCount = 0
		c.trackSkipCount = 0
	}

	lastCtrlTime := time.Unix(0, c.lastControlNano.Load())
	if mpdPlaylistLen == 0 && currQLen > 0 && !c.syncingQueue.Load() &&
		prevLastErr == "" && (prevStatus == "play" || prevStatus == "pause" || c.pendingMPDSync.Load()) &&
		time.Since(lastCtrlTime) > 2*time.Second && time.Since(c.lastEmptyResyncAt) > 5*time.Second {
		c.lastEmptyResyncAt = time.Now()
		c.mpdQueueSynced.Store(false)
		needEmptyResync = true
		resyncQueue = make([]db.Track, currQLen)
		copy(resyncQueue, c.queue)
		resyncIdx = c.state.CurrentIndex
		resyncPlay = (prevStatus == "play")
		resyncVer = c.queueVersion.Load()
	}

	if !needEmptyResync && prevStatus == "play" && mpdState == "stop" && wasLiveStream && !isAudioOutputHardwareError(mpdErr) {
		if c.radioRetryCount < 3 && time.Since(c.lastRadioRetryAt) > 2*time.Second {
			idx := c.state.CurrentIndex
			if idx >= 0 && idx < currQLen {
				c.radioRetryCount++
				c.lastRadioRetryAt = time.Now()
				radioAttempt = c.radioRetryCount
				delays := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second}
				radioDelay = delays[radioAttempt-1]
				radioVer = c.queueVersion.Load()
				radioCtrlNano = c.lastControlNano.Load()
				radioTargetIdx = idx
				needRadioReconnect = true
			}
		}
	}

	if !needEmptyResync && !needRadioReconnect && mpdErr != "" && mpdState == "stop" &&
		!wasLiveStream && !isAudioOutputHardwareError(mpdErr) && currQLen > 1 &&
		c.trackSkipCount < 2 && time.Since(c.lastTrackSkipAt) > 2*time.Second {
		idx := c.state.CurrentIndex
		nextIdx := -1
		if idx >= 0 && idx+1 < currQLen {
			nextIdx = idx + 1
		} else if idx >= 0 && c.state.Repeat {
			nextIdx = 0
		}
		if nextIdx >= 0 {
			c.trackSkipCount++
			c.lastTrackSkipAt = time.Now()
			skipVer = c.queueVersion.Load()
			skipCtrlNano = c.lastControlNano.Load()
			skipNextIdx = nextIdx
			needTrackSkip = true
		}
	}
	c.mu.Unlock()

	if mpdErr != "" && mpdErr != prevLastErr {
		log.Printf("[MPD] Error reportado por MPD: %s", mpdErr)
	}

	if c.database != nil && (recordTrackID > 0 || (durUpdateTrackID > 0 && durUpdateVal > 0)) {
		recID := recordTrackID
		durID := durUpdateTrackID
		durVal := durUpdateVal
		c.enqueuePersist(func() {
			if recID > 0 {
				_ = c.database.RecordPlayback(recID)
			}
			if durID > 0 && durVal > 0 {
				_ = c.database.UpdateTrackDuration(durID, durVal)
			}
		})
	}

	if mpdErr != "" {
		_, _ = c.sendRawCommand("clearerror\n")
	}

	if clampVolTarget >= 0 {
		_, _ = c.sendRawCommand(fmt.Sprintf("setvol %d\n", clampVolTarget))
	}

	if trackChanged || stateChanged {
		c.broadcastState()
	}

	if needEmptyResync {
		log.Printf("[MPD] Cola vacía inesperadamente en MPD (%d pistas en memoria), restaurando...", len(resyncQueue))
		go c.pushQueueToMPD(resyncVer, resyncQueue, resyncIdx, resyncPlay)
	} else if needRadioReconnect {
		go c.scheduleRadioReconnect(radioVer, radioCtrlNano, radioTargetIdx, stationTitle, radioAttempt, radioDelay)
	} else if needTrackSkip {
		go c.scheduleTrackErrorSkip(skipVer, skipCtrlNano, skipNextIdx, mpdErr)
	}

	return true
}

// progressTicker avanza el reloj de reproducción, consulta el hardware MPD y sincroniza con los clientes móviles.
func (c *Client) progressTicker() {
	ticker := time.NewTicker(1000 * time.Millisecond)
	defer ticker.Stop()

	syncCounter := 0
	for {
		select {
		case <-c.stopCh:
			return
		case <-ticker.C:
		}

		syncCounter++
		if c.isConnected() {
			c.syncStatusFromMPD()
		} else {
			c.mu.Lock()
			advanceNext := false
			if c.state.Status == "play" && c.state.CurrentTrack != nil {
				c.state.ElapsedTime += 1.0
				if c.state.Duration > 0 && c.state.ElapsedTime >= c.state.Duration {
					advanceNext = true
				}
			}
			c.mu.Unlock()
			if advanceNext {
				c.Next()
				continue
			}
		}

		// Actualizar contador del sleep timer si está activo
		c.mu.Lock()
		triggerSleepPause := false
		if !c.sleepEndsAt.IsZero() {
			rem := int(time.Until(c.sleepEndsAt).Seconds())
			if rem <= 0 {
				c.state.SleepTimerSec = 0
				c.sleepEndsAt = time.Time{}
				triggerSleepPause = true
			} else {
				c.state.SleepTimerSec = rem
			}
		} else {
			c.state.SleepTimerSec = 0
		}
		isPlaying := (c.state.Status == "play")
		c.mu.Unlock()

		if triggerSleepPause {
			c.Pause()
			continue
		}

		if syncCounter%2 == 0 && isPlaying {
			c.broadcastState()
		}
	}
}

// GetState devuelve una copia profunda e inmutable del estado actual del reproductor.
func (c *Client) GetState() PlayerState {
	c.mu.RLock()
	defer c.mu.RUnlock()

	st := c.state
	if c.state.CurrentTrack != nil {
		trackCopy := *c.state.CurrentTrack
		st.CurrentTrack = &trackCopy
	}
	if len(c.state.AudioOutputs) > 0 {
		outputsCopy := make([]OutputInfo, len(c.state.AudioOutputs))
		copy(outputsCopy, c.state.AudioOutputs)
		st.AudioOutputs = outputsCopy
	} else {
		st.AudioOutputs = make([]OutputInfo, 0)
	}
	return st
}

// GetQueue devuelve una copia segura de las canciones encoladas en orden de reproducción.
func (c *Client) GetQueue() []db.Track {
	c.mu.RLock()
	defer c.mu.RUnlock()
	copied := make([]db.Track, len(c.queue))
	copy(copied, c.queue)
	return copied
}

// Play reanuda o arranca la reproducción en MPD preservando la posición si estaba en pausa.
func (c *Client) Play() {
	c.mu.Lock()
	if len(c.queue) == 0 {
		c.mu.Unlock()
		return
	}
	prevStatus := c.state.Status
	if c.state.CurrentIndex < 0 || c.state.CurrentIndex >= len(c.queue) {
		c.state.CurrentIndex = 0
		prevStatus = "stop"
	}
	trackCopy := c.queue[c.state.CurrentIndex]
	c.state.CurrentTrack = &trackCopy
	c.state.Duration = trackCopy.Duration
	c.state.Status = "play"
	c.state.LastError = ""
	c.radioRetryCount = 0
	c.trackSkipCount = 0
	if prevStatus != "pause" && !isHTTPStreamTrack(&trackCopy) {
		c.state.StreamTitle = ""
	}
	idx := c.state.CurrentIndex
	trackID := trackCopy.ID
	ver := c.queueVersion.Load()
	c.markControlUpdate()
	c.mu.Unlock()

	c.playIndexOrRetry(ver, idx, prevStatus == "pause")

	if prevStatus == "stop" && trackID > 0 {
		id := trackID
		c.enqueuePersist(func() {
			_ = c.database.RecordPlayback(id)
		})
	}
	c.broadcastState()
}

// Pause pausa la reproducción actual manteniendo el punto exacto de la pista.
func (c *Client) Pause() {
	c.mu.Lock()
	c.state.Status = "pause"
	c.state.LastError = ""
	c.radioRetryCount = 0
	c.trackSkipCount = 0
	c.markControlUpdate()
	c.mu.Unlock()

	if c.isConnected() {
		_, _ = c.sendRawCommand("pause 1\n")
		c.markControlUpdate()
	}
	c.broadcastState()
}

// Toggle conmuta entre reproducir y pausar con actualización optimista.
func (c *Client) Toggle() {
	c.mu.RLock()
	status := c.state.Status
	c.mu.RUnlock()

	if status == "play" {
		c.Pause()
	} else {
		c.Play()
	}
}

// PlayIndex salta directamente a reproducir una posición concreta de la cola activa.
func (c *Client) PlayIndex(index int) {
	c.mu.Lock()
	if index < 0 || index >= len(c.queue) {
		c.mu.Unlock()
		return
	}
	trackCopy := c.queue[index]
	ver := c.queueVersion.Load()
	c.state.CurrentIndex = index
	c.state.CurrentTrack = &trackCopy
	c.state.ElapsedTime = 0
	c.state.Duration = trackCopy.Duration
	c.state.Status = "play"
	c.state.LastError = ""
	c.state.StreamTitle = ""
	c.radioRetryCount = 0
	c.trackSkipCount = 0
	trackID := trackCopy.ID
	c.markControlUpdate()
	c.mu.Unlock()

	c.playIndexOrRetry(ver, index, false)

	if trackID > 0 {
		id := trackID
		c.enqueuePersist(func() {
			_ = c.database.RecordPlayback(id)
		})
	}
	c.broadcastState()
}

// playIndexOrRetry reanuda o reproduce el índice objetivo y reconcilia la cola con MPD si existe desincronización.
func (c *Client) playIndexOrRetry(ver uint64, index int, resumePause bool) {
	if !c.isConnected() {
		c.pendingMPDSync.Store(true)
		c.triggerReconnect()
		return
	}

	if !c.mpdQueueSynced.Load() || c.pendingMPDSync.Load() || c.syncingQueue.Load() || c.syncedVersion.Load() != ver {
		go c.retryPlayIndexAfterSync(ver, index)
		return
	}

	if resumePause {
		if statusLines, stErr := c.sendRawCommand("status\n"); stErr == nil {
			mpdState := ""
			for _, ln := range statusLines {
				if strings.HasPrefix(ln, "state: ") {
					mpdState = strings.TrimSpace(strings.TrimPrefix(ln, "state: "))
					break
				}
			}
			if parseMPDPlaylistLength(statusLines) == 0 {
				c.mpdQueueSynced.Store(false)
				go c.retryPlayIndexAfterSync(ver, index)
				return
			}
			if mpdState == "pause" || mpdState == "play" {
				if _, err := c.sendRawCommand("pause 0\n"); err == nil {
					c.markControlUpdate()
					return
				}
			}
		}
	}

	if _, err := c.sendRawCommand(fmt.Sprintf("play %d\n", index)); err != nil {
		if !c.isConnected() {
			c.pendingMPDSync.Store(true)
			c.triggerReconnect()
		} else if c.syncingQueue.Load() || !c.mpdQueueSynced.Load() || isMPDAckError(err) {
			go c.retryPlayIndexAfterSync(ver, index)
		}
	}
	c.markControlUpdate()
}

// retryPlayIndexAfterSync espera al bloqueo de cola y reconcilia o reproduce el índice objetivo tras completarse la sincronización.
func (c *Client) retryPlayIndexAfterSync(ver uint64, index int) {
	c.syncMu.Lock()
	defer c.syncMu.Unlock()

	c.syncingQueue.Store(true)
	defer c.syncingQueue.Store(false)

	if c.closed.Load() || c.queueVersion.Load() != ver {
		return
	}
	if !c.isConnected() {
		c.pendingMPDSync.Store(true)
		c.triggerReconnect()
		return
	}

	c.mu.RLock()
	stillTarget := (c.state.CurrentIndex == index && c.state.Status == "play")
	c.mu.RUnlock()
	if !stillTarget {
		return
	}

	if !c.mpdQueueSynced.Load() || c.pendingMPDSync.Load() || c.syncedVersion.Load() != ver {
		if c.reconcileQueueWithMPDLocked(ver, true) {
			return
		}
	}

	if _, err := c.sendRawCommand(fmt.Sprintf("play %d\n", index)); err != nil && isMPDAckError(err) {
		c.mpdQueueSynced.Store(false)
		c.reconcileQueueWithMPDLocked(ver, true)
	}
	c.markControlUpdate()
}

// Next avanza a la siguiente canción en la cola respetando el modo de repetición.
func (c *Client) Next() {
	c.mu.Lock()
	if len(c.queue) == 0 {
		c.mu.Unlock()
		return
	}

	nextIdx := c.state.CurrentIndex + 1
	if nextIdx >= len(c.queue) {
		if c.state.Repeat {
			nextIdx = 0
		} else {
			c.state.Status = "stop"
			c.state.ElapsedTime = 0
			c.state.StreamTitle = ""
			c.state.LastError = ""
			c.radioRetryCount = 0
			c.trackSkipCount = 0
			c.markControlUpdate()
			c.mu.Unlock()
			if c.isConnected() {
				_, _ = c.sendRawCommand("stop\n")
				c.markControlUpdate()
			}
			c.broadcastState()
			return
		}
	}

	trackCopy := c.queue[nextIdx]
	ver := c.queueVersion.Load()
	c.state.CurrentIndex = nextIdx
	c.state.CurrentTrack = &trackCopy
	c.state.ElapsedTime = 0
	c.state.Duration = trackCopy.Duration
	c.state.Status = "play"
	c.state.LastError = ""
	c.state.StreamTitle = ""
	c.radioRetryCount = 0
	c.trackSkipCount = 0
	trackID := trackCopy.ID
	c.markControlUpdate()
	c.mu.Unlock()

	c.playIndexOrRetry(ver, nextIdx, false)

	if trackID > 0 {
		id := trackID
		c.enqueuePersist(func() {
			_ = c.database.RecordPlayback(id)
		})
	}
	c.broadcastState()
}

// Prev retrocede a la canción anterior o reinicia la pista actual si lleva más de 3 segundos sonando.
func (c *Client) Prev() {
	c.mu.Lock()
	if len(c.queue) == 0 {
		c.mu.Unlock()
		return
	}

	if c.state.ElapsedTime > 3.0 {
		ver := c.queueVersion.Load()
		currIdx := c.state.CurrentIndex
		c.state.ElapsedTime = 0
		c.markControlUpdate()
		c.mu.Unlock()
		if c.isConnected() {
			if _, err := c.sendRawCommand("seekcur 0\n"); err != nil && (!c.mpdQueueSynced.Load() || isMPDAckError(err)) {
				go c.retryPlayIndexAfterSync(ver, currIdx)
			}
			c.markControlUpdate()
		} else {
			c.pendingMPDSync.Store(true)
			c.triggerReconnect()
		}
		c.broadcastState()
		return
	}

	prevIdx := c.state.CurrentIndex - 1
	if prevIdx < 0 {
		prevIdx = 0
	}

	trackCopy := c.queue[prevIdx]
	ver := c.queueVersion.Load()
	c.state.CurrentIndex = prevIdx
	c.state.CurrentTrack = &trackCopy
	c.state.ElapsedTime = 0
	c.state.Duration = trackCopy.Duration
	c.state.Status = "play"
	c.state.LastError = ""
	c.state.StreamTitle = ""
	c.radioRetryCount = 0
	c.trackSkipCount = 0
	trackID := trackCopy.ID
	c.markControlUpdate()
	c.mu.Unlock()

	c.playIndexOrRetry(ver, prevIdx, false)

	if trackID > 0 {
		id := trackID
		c.enqueuePersist(func() {
			_ = c.database.RecordPlayback(id)
		})
	}
	c.broadcastState()
}

// Seek desplaza la reproducción actual al segundo especificado.
func (c *Client) Seek(seconds float64) {
	c.mu.Lock()
	if seconds < 0 {
		seconds = 0
	}
	if c.state.Duration > 0 && seconds > c.state.Duration {
		seconds = c.state.Duration
	}
	c.state.ElapsedTime = seconds
	c.markControlUpdate()
	c.mu.Unlock()

	if c.isConnected() {
		_, _ = c.sendRawCommand(fmt.Sprintf("seekcur %d\n", int(seconds)))
		c.markControlUpdate()
	}
	c.broadcastState()
}

// SetRepeat activa o desactiva la repetición de lista tanto en memoria como en MPD.
func (c *Client) SetRepeat(enabled bool) {
	c.mu.Lock()
	c.state.Repeat = enabled
	c.markControlUpdate()
	c.mu.Unlock()

	if c.isConnected() {
		val := 0
		if enabled {
			val = 1
		}
		_, _ = c.sendRawCommand(fmt.Sprintf("repeat %d\n", val))
		c.markControlUpdate()
	}
	c.broadcastState()
}

// SetVolume ajusta el volumen general respetando el límite máximo de protección acústica.
func (c *Client) SetVolume(volume int) {
	c.mu.Lock()
	if volume < 0 {
		volume = 0
	}
	if volume > c.state.MaxVolumeLimit {
		volume = c.state.MaxVolumeLimit
	}
	c.state.Volume = volume
	c.markControlUpdate()
	c.mu.Unlock()

	if c.isConnected() {
		_, _ = c.sendRawCommand(fmt.Sprintf("setvol %d\n", volume))
		c.markControlUpdate()
	}

	volStr := strconv.Itoa(volume)
	c.enqueuePersist(func() {
		_ = c.database.SetSetting("player_volume", volStr)
	})
	c.broadcastState()
}

// StepVolume incrementa o decrementa el volumen en pasos fijos (+/- delta).
func (c *Client) StepVolume(delta int) {
	c.mu.RLock()
	current := c.state.Volume
	c.mu.RUnlock()
	c.SetVolume(current + delta)
}

// SetMaxVolumeLimit fija el tope de salvaguarda de volumen y ajusta el nivel activo si lo supera.
func (c *Client) SetMaxVolumeLimit(limit int) {
	c.mu.Lock()
	if limit < 30 {
		limit = 30
	}
	if limit > 100 {
		limit = 100
	}
	c.state.MaxVolumeLimit = limit
	volChanged := false
	newVol := c.state.Volume
	if c.state.Volume > limit {
		c.state.Volume = limit
		newVol = limit
		volChanged = true
	}
	c.markControlUpdate()
	c.mu.Unlock()

	if volChanged && c.isConnected() {
		_, _ = c.sendRawCommand(fmt.Sprintf("setvol %d\n", newVol))
		c.markControlUpdate()
	}

	limitStr := strconv.Itoa(limit)
	newVolStr := strconv.Itoa(newVol)
	shouldSaveVol := volChanged
	c.enqueuePersist(func() {
		_ = c.database.SetSetting("max_volume_limit", limitStr)
		if shouldSaveVol {
			_ = c.database.SetSetting("player_volume", newVolStr)
		}
	})
	c.broadcastState()
}

// ReplaceQueue sustituye atómicamente toda la cola actual y arranca el primer tema sin bloquear el hilo HTTP.
func (c *Client) ReplaceQueue(tracks []db.Track, autoPlay bool) {
	if len(tracks) == 0 {
		c.ClearQueue()
		return
	}

	c.mu.Lock()
	ver := c.queueVersion.Add(1)
	c.mpdQueueSynced.Store(false)
	c.queue = make([]db.Track, len(tracks))
	copy(c.queue, tracks)
	c.state.QueueLength = len(c.queue)

	first := c.queue[0]
	c.state.CurrentIndex = 0
	c.state.CurrentTrack = &first
	c.state.Duration = first.Duration
	c.state.ElapsedTime = 0
	c.state.LastError = ""
	c.state.StreamTitle = ""
	c.radioRetryCount = 0
	c.trackSkipCount = 0
	if autoPlay {
		c.state.Status = "play"
	} else {
		c.state.Status = "stop"
	}
	firstID := first.ID
	tracksCopy := make([]db.Track, len(c.queue))
	copy(tracksCopy, c.queue)
	c.markControlUpdate()
	c.mu.Unlock()

	c.broadcastQueue()
	c.broadcastState()

	if autoPlay && firstID > 0 {
		id := firstID
		c.enqueuePersist(func() {
			_ = c.database.RecordPlayback(id)
		})
	}

	if c.isConnected() {
		go c.pushQueueToMPD(ver, tracksCopy, 0, autoPlay)
	} else {
		c.pendingMPDSync.Store(true)
		c.triggerReconnect()
	}
}

// EnqueueTracks añade pistas a la cola según el modo ("play_now", "play_next", "append", "replace").
func (c *Client) EnqueueTracks(tracks []db.Track, mode string) {
	if len(tracks) == 0 {
		return
	}

	if mode == "replace" {
		c.ReplaceQueue(tracks, true)
		return
	}

	c.mu.Lock()
	ver := c.queueVersion.Add(1)
	wasEmpty := (len(c.queue) == 0)
	insertPos := c.state.CurrentIndex + 1
	if insertPos < 0 || insertPos > len(c.queue) {
		insertPos = len(c.queue)
	}

	switch mode {
	case "play_now":
		if wasEmpty {
			c.queue = append(c.queue, tracks...)
			c.state.CurrentIndex = 0
		} else {
			newQ := make([]db.Track, 0, len(c.queue)+len(tracks))
			newQ = append(newQ, c.queue[:insertPos]...)
			newQ = append(newQ, tracks...)
			newQ = append(newQ, c.queue[insertPos:]...)
			c.queue = newQ
			c.state.CurrentIndex = insertPos
		}
		first := c.queue[c.state.CurrentIndex]
		c.state.CurrentTrack = &first
		c.state.Duration = first.Duration
		c.state.ElapsedTime = 0
		c.state.Status = "play"

	case "play_next":
		if wasEmpty {
			c.queue = append(c.queue, tracks...)
			c.state.CurrentIndex = 0
		} else {
			newQ := make([]db.Track, 0, len(c.queue)+len(tracks))
			newQ = append(newQ, c.queue[:insertPos]...)
			newQ = append(newQ, tracks...)
			newQ = append(newQ, c.queue[insertPos:]...)
			c.queue = newQ
		}

	default: // "append"
		c.queue = append(c.queue, tracks...)
		if wasEmpty {
			c.state.CurrentIndex = 0
			first := c.queue[0]
			c.state.CurrentTrack = &first
			c.state.Duration = first.Duration
			c.state.ElapsedTime = 0
			c.state.Status = "play"
		}
	}

	c.state.QueueLength = len(c.queue)
	currIdx := c.state.CurrentIndex
	needPlay := (mode == "play_now" || (mode == "append" && wasEmpty))
	if needPlay {
		c.state.LastError = ""
		c.state.StreamTitle = ""
		c.radioRetryCount = 0
		c.trackSkipCount = 0
	}
	if wasEmpty {
		c.mpdQueueSynced.Store(false)
	}
	queueSnapshot := make([]db.Track, len(c.queue))
	copy(queueSnapshot, c.queue)
	var firstPlayID int64
	if needPlay && currIdx >= 0 && currIdx < len(c.queue) {
		firstPlayID = c.queue[currIdx].ID
	}
	c.markControlUpdate()
	c.mu.Unlock()

	c.broadcastQueue()
	c.broadcastState()

	if firstPlayID > 0 {
		id := firstPlayID
		c.enqueuePersist(func() {
			_ = c.database.RecordPlayback(id)
		})
	}

	if !c.isConnected() {
		c.mpdQueueSynced.Store(false)
		c.pendingMPDSync.Store(true)
		c.triggerReconnect()
		return
	}

	if wasEmpty {
		go c.pushQueueToMPD(ver, queueSnapshot, currIdx, needPlay)
		return
	}

	tracksCopy := make([]db.Track, len(tracks))
	copy(tracksCopy, tracks)
	go c.applyQueueDeltaToMPD(ver, tracksCopy, mode, insertPos, currIdx, needPlay)
}

// setPlaybackErrorIfVersion registra un error de reproducción en el estado cuando falla la pista activa de la versión vigente.
func (c *Client) setPlaybackErrorIfVersion(ver uint64, t db.Track, err error) {
	if err == nil {
		return
	}
	c.mu.Lock()
	if c.queueVersion.Load() != ver {
		c.mu.Unlock()
		return
	}
	label := strings.TrimSpace(t.Title)
	if label == "" {
		label = t.Path
	}
	c.state.LastError = fmt.Sprintf("No se pudo reproducir %s: %v", label, err)
	c.state.Status = "stop"
	c.state.StreamTitle = ""
	c.markControlUpdate()
	c.mu.Unlock()
	c.broadcastState()
}

// parseMPDPlaylistFiles extrae y normaliza las rutas de la cola devueltas por playlistinfo en MPD.
func parseMPDPlaylistFiles(lines []string) []string {
	var files []string
	for _, line := range lines {
		if strings.HasPrefix(line, "file: ") {
			raw := strings.TrimSpace(strings.TrimPrefix(line, "file: "))
			files = append(files, normalizeMPDFilePath(raw))
		}
	}
	return files
}

// mpdPlaylistMatchesPrefix verifica si los primeros elementos en MPD coinciden con la cola esperada hasta uptoIdx.
func mpdPlaylistMatchesPrefix(mpdFiles []string, queue []db.Track, uptoIdx int) bool {
	if uptoIdx < 0 || uptoIdx >= len(queue) || len(mpdFiles) <= uptoIdx {
		return false
	}
	for i := 0; i <= uptoIdx; i++ {
		if mpdFiles[i] == queue[i].Path {
			continue
		}
		if isHTTPStreamTrack(&queue[i]) && (strings.HasPrefix(mpdFiles[i], "http://") || strings.HasPrefix(mpdFiles[i], "https://")) && uptoIdx == 0 {
			continue
		}
		return false
	}
	return true
}

// pushQueueToMPD reemplaza la cola de MPD y empieza reproduciendo startIdx antes de añadir el resto.
func (c *Client) pushQueueToMPD(ver uint64, tracks []db.Track, startIdx int, autoPlay bool) {
	if len(tracks) == 0 {
		return
	}

	c.syncMu.Lock()
	defer c.syncMu.Unlock()

	c.syncingQueue.Store(true)
	defer c.syncingQueue.Store(false)

	if c.closed.Load() || c.queueVersion.Load() != ver || (c.mpdQueueSynced.Load() && c.syncedVersion.Load() == ver) {
		return
	}
	if !c.isConnected() {
		c.mpdQueueSynced.Store(false)
		c.pendingMPDSync.Store(true)
		c.triggerReconnect()
		return
	}

	c.pushQueueToMPDLocked(ver, tracks, startIdx, autoPlay)
}

// pushQueueToMPDLocked reemplaza la cola de MPD bajo syncMu, poda pistas inválidas y marca syncedVersion al completar.
func (c *Client) pushQueueToMPDLocked(ver uint64, tracks []db.Track, startIdx int, autoPlay bool) bool {
	if len(tracks) == 0 {
		if _, err := c.sendRawCommand("clear\n"); err != nil {
			c.mpdQueueSynced.Store(false)
			c.pendingMPDSync.Store(true)
			c.triggerReconnect()
			return false
		}
		c.syncedVersion.Store(ver)
		c.mpdQueueSynced.Store(true)
		c.pendingMPDSync.Store(false)
		return true
	}

	if c.queueVersion.Load() != ver || !c.isConnected() {
		if !c.isConnected() {
			c.mpdQueueSynced.Store(false)
			c.pendingMPDSync.Store(true)
			c.triggerReconnect()
		}
		return false
	}

	if _, err := c.sendRawCommand("clear\n"); err != nil {
		c.mpdQueueSynced.Store(false)
		c.pendingMPDSync.Store(true)
		c.triggerReconnect()
		return false
	}

	if startIdx < 0 || startIdx >= len(tracks) {
		startIdx = 0
	}

	validTracks := make([]db.Track, 0, len(tracks))
	droppedAny := false
	mpdPlayIdx := 0

	// Añadir rápidamente hasta startIdx para comenzar a sonar en <50ms
	for i := 0; i <= startIdx; i++ {
		if c.queueVersion.Load() != ver {
			return false
		}
		if err := c.addTrackToMPD(tracks[i]); err != nil {
			log.Printf("[MPD] Advertencia al encolar pista inicial %s: %v", tracks[i].Path, err)
			if !isMPDAckError(err) {
				c.mpdQueueSynced.Store(false)
				c.pendingMPDSync.Store(true)
				c.triggerReconnect()
				return false
			}
			if i == startIdx {
				if autoPlay {
					c.setPlaybackErrorIfVersion(ver, tracks[i], err)
				}
				return false
			}
			droppedAny = true
		} else {
			validTracks = append(validTracks, tracks[i])
			if i < startIdx {
				mpdPlayIdx++
			}
		}
	}

	c.mu.RLock()
	stillPlay := (c.queueVersion.Load() == ver && c.state.Status == "play")
	c.mu.RUnlock()

	if autoPlay && stillPlay && c.queueVersion.Load() == ver {
		if _, err := c.sendRawCommand(fmt.Sprintf("play %d\n", mpdPlayIdx)); err != nil {
			if !isMPDAckError(err) {
				c.mpdQueueSynced.Store(false)
				c.pendingMPDSync.Store(true)
				c.triggerReconnect()
				return false
			}
			c.setPlaybackErrorIfVersion(ver, tracks[startIdx], err)
			return false
		}
		c.markControlUpdate()
	}

	// Encolar el resto de pistas en segundo plano cancelando si el usuario lanzó otra petición
	for i := startIdx + 1; i < len(tracks); i++ {
		if c.queueVersion.Load() != ver {
			return false
		}
		if err := c.addTrackToMPD(tracks[i]); err != nil {
			log.Printf("[MPD] Advertencia al añadir pista restante %s: %v", tracks[i].Path, err)
			if !isMPDAckError(err) {
				c.mpdQueueSynced.Store(false)
				c.pendingMPDSync.Store(true)
				c.triggerReconnect()
				return false
			}
			droppedAny = true
		} else {
			validTracks = append(validTracks, tracks[i])
		}
	}

	if c.queueVersion.Load() != ver {
		return false
	}

	if droppedAny && len(validTracks) > 0 {
		c.mu.Lock()
		if c.queueVersion.Load() == ver {
			c.queue = validTracks
			c.state.QueueLength = len(validTracks)
			if mpdPlayIdx >= 0 && mpdPlayIdx < len(validTracks) {
				c.state.CurrentIndex = mpdPlayIdx
				trackCopy := validTracks[mpdPlayIdx]
				c.state.CurrentTrack = &trackCopy
			}
		}
		c.mu.Unlock()
		c.broadcastQueue()
		c.broadcastState()
	}

	c.syncedVersion.Store(ver)
	c.mpdQueueSynced.Store(true)
	c.pendingMPDSync.Store(false)
	return true
}

// reconcileQueueWithMPDLocked repara la cola en MPD cuando un delta previo se perdió o fue interrumpido.
func (c *Client) reconcileQueueWithMPDLocked(ver uint64, forcePlay bool) bool {
	qCopy, currIdx, wantPlay, snapVer := c.snapshotQueueState()
	if c.closed.Load() || snapVer != ver {
		return false
	}
	if !c.isConnected() {
		c.mpdQueueSynced.Store(false)
		c.pendingMPDSync.Store(true)
		c.triggerReconnect()
		return false
	}
	if len(qCopy) == 0 {
		return c.pushQueueToMPDLocked(ver, qCopy, 0, false)
	}

	if !forcePlay && currIdx >= 0 && currIdx < len(qCopy) {
		statusLines, stErr := c.sendRawCommand("status\n")
		plLines, plErr := c.sendRawCommand("playlistinfo\n")
		if stErr == nil && plErr == nil {
			mpdState := ""
			mpdSong := -1
			for _, ln := range statusLines {
				if strings.HasPrefix(ln, "state: ") {
					mpdState = strings.TrimSpace(strings.TrimPrefix(ln, "state: "))
				} else if strings.HasPrefix(ln, "song: ") {
					mpdSong, _ = strconv.Atoi(strings.TrimSpace(strings.TrimPrefix(ln, "song: ")))
				}
			}
			mpdFiles := parseMPDPlaylistFiles(plLines)
			if (mpdState == "play" || mpdState == "pause") && mpdSong == currIdx && mpdPlaylistMatchesPrefix(mpdFiles, qCopy, currIdx) {
				if len(mpdFiles) > currIdx+1 {
					if _, delErr := c.sendRawCommand(fmt.Sprintf("delete %d:%d\n", currIdx+1, len(mpdFiles))); delErr != nil {
						return c.pushQueueToMPDLocked(ver, qCopy, currIdx, wantPlay)
					}
				}
				validQueue := make([]db.Track, 0, len(qCopy))
				validQueue = append(validQueue, qCopy[:currIdx+1]...)
				droppedAny := false
				for i := currIdx + 1; i < len(qCopy); i++ {
					if c.queueVersion.Load() != ver {
						return false
					}
					if err := c.addTrackToMPD(qCopy[i]); err != nil {
						log.Printf("[MPD] Advertencia al reconciliar cola (%s): %v", qCopy[i].Path, err)
						if !isMPDAckError(err) {
							c.mpdQueueSynced.Store(false)
							c.pendingMPDSync.Store(true)
							c.triggerReconnect()
							return false
						}
						droppedAny = true
					} else {
						validQueue = append(validQueue, qCopy[i])
					}
				}
				if c.queueVersion.Load() != ver {
					return false
				}
				if droppedAny && len(validQueue) > 0 {
					c.mu.Lock()
					if c.queueVersion.Load() == ver {
						c.queue = validQueue
						c.state.QueueLength = len(validQueue)
					}
					c.mu.Unlock()
					c.broadcastQueue()
					c.broadcastState()
				}
				c.syncedVersion.Store(ver)
				c.mpdQueueSynced.Store(true)
				c.pendingMPDSync.Store(false)
				return true
			}
		}
	}

	return c.pushQueueToMPDLocked(ver, qCopy, currIdx, wantPlay)
}

// applyQueueDeltaToMPD inserta o anexa pistas en una cola activa de MPD, reconciliando si faltaban cambios previos.
func (c *Client) applyQueueDeltaToMPD(ver uint64, tracks []db.Track, mode string, insertPos, playIdx int, autoPlay bool) {
	c.syncMu.Lock()
	defer c.syncMu.Unlock()

	c.syncingQueue.Store(true)
	defer c.syncingQueue.Store(false)

	if c.closed.Load() || c.queueVersion.Load() != ver {
		return
	}
	if !c.isConnected() {
		c.mpdQueueSynced.Store(false)
		c.pendingMPDSync.Store(true)
		c.triggerReconnect()
		return
	}
	if !c.mpdQueueSynced.Load() || c.pendingMPDSync.Load() || c.syncedVersion.Load() != ver-1 {
		c.reconcileQueueWithMPDLocked(ver, mode == "play_now")
		return
	}

	if mode == "play_now" || mode == "play_next" {
		for offset, t := range tracks {
			if c.queueVersion.Load() != ver {
				return
			}
			targetPos := insertPos + offset
			if err := c.addTrackToMPDAt(t, targetPos); err != nil {
				log.Printf("[MPD] Advertencia al insertar pista en posición %d (%s): %v", targetPos, t.Path, err)
				if !isMPDAckError(err) {
					c.mpdQueueSynced.Store(false)
					c.pendingMPDSync.Store(true)
					c.triggerReconnect()
					return
				}
				if offset == 0 && autoPlay {
					c.setPlaybackErrorIfVersion(ver, t, err)
					return
				}
				c.reconcileQueueWithMPDLocked(ver, false)
				return
			} else if offset == 0 && autoPlay {
				if _, err := c.sendRawCommand(fmt.Sprintf("play %d\n", playIdx)); err != nil {
					if !isMPDAckError(err) {
						c.mpdQueueSynced.Store(false)
						c.pendingMPDSync.Store(true)
						c.triggerReconnect()
						return
					}
					c.reconcileQueueWithMPDLocked(ver, true)
					return
				}
				c.markControlUpdate()
			}
		}
		if c.queueVersion.Load() == ver {
			c.syncedVersion.Store(ver)
			c.mpdQueueSynced.Store(true)
			c.pendingMPDSync.Store(false)
		}
		return
	}

	for _, t := range tracks {
		if c.queueVersion.Load() != ver {
			return
		}
		if err := c.addTrackToMPD(t); err != nil {
			log.Printf("[MPD] Advertencia al añadir pista a cola (%s): %v", t.Path, err)
			if !isMPDAckError(err) {
				c.mpdQueueSynced.Store(false)
				c.pendingMPDSync.Store(true)
				c.triggerReconnect()
				return
			}
			c.reconcileQueueWithMPDLocked(ver, false)
			return
		}
	}

	if c.queueVersion.Load() == ver {
		c.syncedVersion.Store(ver)
		c.mpdQueueSynced.Store(true)
		c.pendingMPDSync.Store(false)
	}
}

// MoveInQueue reordena una pista dentro de la lista para soportar Drag & Drop táctil.
func (c *Client) MoveInQueue(from, to int) {
	c.mu.Lock()
	if from < 0 || from >= len(c.queue) || to < 0 || to >= len(c.queue) || from == to {
		c.mu.Unlock()
		return
	}

	ver := c.queueVersion.Add(1)
	track := c.queue[from]
	c.queue = append(c.queue[:from], c.queue[from+1:]...)
	newQ := make([]db.Track, 0, len(c.queue)+1)
	newQ = append(newQ, c.queue[:to]...)
	newQ = append(newQ, track)
	newQ = append(newQ, c.queue[to:]...)
	c.queue = newQ

	if c.state.CurrentIndex == from {
		c.state.CurrentIndex = to
	} else if from < c.state.CurrentIndex && to >= c.state.CurrentIndex {
		c.state.CurrentIndex--
	} else if from > c.state.CurrentIndex && to <= c.state.CurrentIndex {
		c.state.CurrentIndex++
	}
	c.markControlUpdate()
	c.mu.Unlock()

	if c.isConnected() {
		go func(v uint64, f, t int) {
			c.syncMu.Lock()
			defer c.syncMu.Unlock()
			c.syncingQueue.Store(true)
			defer c.syncingQueue.Store(false)

			if c.closed.Load() || c.queueVersion.Load() != v {
				return
			}
			if !c.mpdQueueSynced.Load() || c.pendingMPDSync.Load() || c.syncedVersion.Load() != v-1 {
				c.reconcileQueueWithMPDLocked(v, false)
				return
			}
			if _, err := c.sendRawCommand(fmt.Sprintf("move %d %d\n", f, t)); err != nil {
				c.mpdQueueSynced.Store(false)
				if !c.isConnected() || !isMPDAckError(err) {
					c.pendingMPDSync.Store(true)
					c.triggerReconnect()
				} else {
					c.reconcileQueueWithMPDLocked(v, false)
				}
				return
			}
			if c.queueVersion.Load() == v {
				c.syncedVersion.Store(v)
				c.mpdQueueSynced.Store(true)
				c.pendingMPDSync.Store(false)
			}
		}(ver, from, to)
	} else {
		c.mpdQueueSynced.Store(false)
		c.pendingMPDSync.Store(true)
		c.triggerReconnect()
	}

	c.broadcastQueue()
	c.broadcastState()
}

// RemoveFromQueue elimina una pista por su índice (soporte de deslizamiento lateral táctil).
func (c *Client) RemoveFromQueue(index int) {
	c.mu.Lock()
	if index < 0 || index >= len(c.queue) {
		c.mu.Unlock()
		return
	}

	ver := c.queueVersion.Add(1)
	c.queue = append(c.queue[:index], c.queue[index+1:]...)
	c.state.QueueLength = len(c.queue)

	if len(c.queue) == 0 {
		c.state.CurrentIndex = -1
		c.state.CurrentTrack = nil
		c.state.Status = "stop"
		c.state.ElapsedTime = 0
		c.state.Duration = 0
		c.state.StreamTitle = ""
		c.state.LastError = ""
		c.radioRetryCount = 0
		c.trackSkipCount = 0
	} else if index < c.state.CurrentIndex {
		c.state.CurrentIndex--
	} else if index == c.state.CurrentIndex {
		if c.state.CurrentIndex >= len(c.queue) {
			c.state.CurrentIndex = len(c.queue) - 1
		}
		trackCopy := c.queue[c.state.CurrentIndex]
		c.state.CurrentTrack = &trackCopy
		c.state.Duration = trackCopy.Duration
		c.state.ElapsedTime = 0
		c.state.StreamTitle = ""
		c.state.LastError = ""
	}
	c.markControlUpdate()
	c.mu.Unlock()

	if c.isConnected() {
		go func(v uint64, idx int) {
			c.syncMu.Lock()
			defer c.syncMu.Unlock()
			c.syncingQueue.Store(true)
			defer c.syncingQueue.Store(false)

			if c.closed.Load() || c.queueVersion.Load() != v {
				return
			}
			if !c.mpdQueueSynced.Load() || c.pendingMPDSync.Load() || c.syncedVersion.Load() != v-1 {
				c.reconcileQueueWithMPDLocked(v, false)
				return
			}
			if _, err := c.sendRawCommand(fmt.Sprintf("delete %d\n", idx)); err != nil {
				c.mpdQueueSynced.Store(false)
				if !c.isConnected() || !isMPDAckError(err) {
					c.pendingMPDSync.Store(true)
					c.triggerReconnect()
				} else {
					c.reconcileQueueWithMPDLocked(v, false)
				}
				return
			}
			if c.queueVersion.Load() == v {
				c.syncedVersion.Store(v)
				c.mpdQueueSynced.Store(true)
				c.pendingMPDSync.Store(false)
			}
		}(ver, index)
	} else {
		c.mpdQueueSynced.Store(false)
		c.pendingMPDSync.Store(true)
		c.triggerReconnect()
	}

	c.broadcastQueue()
	c.broadcastState()
}

// ClearQueue vacía completamente la lista de reproducción.
func (c *Client) ClearQueue() {
	c.mu.Lock()
	ver := c.queueVersion.Add(1)
	c.queue = make([]db.Track, 0)
	c.state.QueueLength = 0
	c.state.CurrentIndex = -1
	c.state.CurrentTrack = nil
	c.state.Status = "stop"
	c.state.ElapsedTime = 0
	c.state.Duration = 0
	c.state.StreamTitle = ""
	c.state.LastError = ""
	c.radioRetryCount = 0
	c.trackSkipCount = 0
	c.mpdQueueSynced.Store(false)
	c.markControlUpdate()
	c.mu.Unlock()

	if c.isConnected() {
		c.syncMu.Lock()
		if c.queueVersion.Load() == ver {
			if _, err := c.sendRawCommand("clear\n"); err == nil {
				c.syncedVersion.Store(ver)
				c.mpdQueueSynced.Store(true)
				c.pendingMPDSync.Store(false)
			} else {
				c.pendingMPDSync.Store(true)
				c.triggerReconnect()
			}
		}
		c.syncMu.Unlock()
		c.markControlUpdate()
	} else {
		c.pendingMPDSync.Store(true)
		c.triggerReconnect()
	}

	c.broadcastQueue()
	c.broadcastState()
}

// AntiRepetitionShuffle baraja la cola evitando artistas seguidos consecutivamente y sincroniza MPD sin bloqueos.
func (c *Client) AntiRepetitionShuffle() {
	c.mu.Lock()
	if len(c.queue) <= 1 {
		c.mu.Unlock()
		return
	}

	ver := c.queueVersion.Add(1)
	currentTrack := c.state.CurrentTrack
	currIdx := c.state.CurrentIndex
	oldLen := len(c.queue)

	var pending []db.Track
	if currIdx >= 0 && currIdx < len(c.queue) {
		pending = append([]db.Track{}, c.queue[currIdx+1:]...)
	} else {
		pending = append([]db.Track{}, c.queue...)
	}

	if len(pending) <= 1 {
		c.mu.Unlock()
		return
	}

	reordered := antiRepetitionSort(pending, currentTrack)

	if currIdx >= 0 && currIdx < len(c.queue) {
		c.queue = append(c.queue[:currIdx+1], reordered...)
	} else {
		c.queue = reordered
		c.state.CurrentIndex = 0
		first := c.queue[0]
		c.state.CurrentTrack = &first
	}
	c.state.QueueLength = len(c.queue)
	queueSnapshot := make([]db.Track, len(c.queue))
	copy(queueSnapshot, c.queue)
	c.markControlUpdate()
	c.mu.Unlock()

	if c.isConnected() {
		reorderedCopy := make([]db.Track, len(reordered))
		copy(reorderedCopy, reordered)
		go func() {
			if currIdx >= 0 && currIdx < oldLen {
				c.syncMu.Lock()
				defer c.syncMu.Unlock()
				c.syncingQueue.Store(true)
				defer c.syncingQueue.Store(false)

				if c.closed.Load() || c.queueVersion.Load() != ver {
					return
				}
				if !c.mpdQueueSynced.Load() || c.pendingMPDSync.Load() || c.syncedVersion.Load() != ver-1 {
					c.reconcileQueueWithMPDLocked(ver, false)
					return
				}
				if currIdx+1 < oldLen {
					if _, err := c.sendRawCommand(fmt.Sprintf("delete %d:%d\n", currIdx+1, oldLen)); err != nil {
						c.mpdQueueSynced.Store(false)
						if !c.isConnected() || !isMPDAckError(err) {
							c.pendingMPDSync.Store(true)
							c.triggerReconnect()
						} else {
							c.reconcileQueueWithMPDLocked(ver, false)
						}
						return
					}
				}
				for _, t := range reorderedCopy {
					if c.queueVersion.Load() != ver {
						return
					}
					if err := c.addTrackToMPD(t); err != nil {
						if !isMPDAckError(err) {
							c.mpdQueueSynced.Store(false)
							c.pendingMPDSync.Store(true)
							c.triggerReconnect()
							return
						}
						c.reconcileQueueWithMPDLocked(ver, false)
						return
					}
				}
				if c.queueVersion.Load() == ver {
					c.syncedVersion.Store(ver)
					c.mpdQueueSynced.Store(true)
					c.pendingMPDSync.Store(false)
				}
			} else {
				c.pushQueueToMPD(ver, queueSnapshot, 0, false)
			}
		}()
	} else {
		c.mpdQueueSynced.Store(false)
		c.pendingMPDSync.Store(true)
		c.triggerReconnect()
	}

	c.broadcastQueue()
	c.broadcastState()
}

// antiRepetitionSort organiza una lista de canciones evitando repetir artista en posiciones consecutivas.
func antiRepetitionSort(tracks []db.Track, currentTrack *db.Track) []db.Track {
	if len(tracks) <= 1 {
		return tracks
	}

	byArtist := make(map[string][]db.Track)
	for _, t := range tracks {
		k := strings.ToLower(strings.TrimSpace(t.Artist))
		byArtist[k] = append(byArtist[k], t)
	}

	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	for k := range byArtist {
		r.Shuffle(len(byArtist[k]), func(i, j int) {
			byArtist[k][i], byArtist[k][j] = byArtist[k][j], byArtist[k][i]
		})
	}

	result := make([]db.Track, 0, len(tracks))
	lastArtist := ""
	if currentTrack != nil {
		lastArtist = strings.ToLower(strings.TrimSpace(currentTrack.Artist))
	}

	for {
		var candidates []string
		for k, list := range byArtist {
			if len(list) > 0 && k != lastArtist {
				candidates = append(candidates, k)
			}
		}

		if len(candidates) == 0 {
			var remainingArtist string
			for k, list := range byArtist {
				if len(list) > 0 {
					remainingArtist = k
					break
				}
			}
			if remainingArtist == "" {
				break
			}

			remList := byArtist[remainingArtist]
			byArtist[remainingArtist] = nil

			for _, t := range remList {
				placed := false
				for idx := 0; idx < len(result); idx++ {
					prevA := ""
					if idx > 0 {
						prevA = strings.ToLower(strings.TrimSpace(result[idx-1].Artist))
					}
					currA := strings.ToLower(strings.TrimSpace(result[idx].Artist))
					if prevA != remainingArtist && currA != remainingArtist {
						newRes := make([]db.Track, 0, len(result)+1)
						newRes = append(newRes, result[:idx]...)
						newRes = append(newRes, t)
						newRes = append(newRes, result[idx:]...)
						result = newRes
						placed = true
						break
					}
				}
				if !placed {
					result = append(result, t)
				}
			}
			break
		}

		maxCount := 0
		for _, c := range candidates {
			if len(byArtist[c]) > maxCount {
				maxCount = len(byArtist[c])
			}
		}

		var best []string
		for _, c := range candidates {
			if len(byArtist[c]) == maxCount {
				best = append(best, c)
			}
		}

		chosenKey := best[r.Intn(len(best))]
		chosenList := byArtist[chosenKey]
		chosenTrack := chosenList[len(chosenList)-1]
		byArtist[chosenKey] = chosenList[:len(chosenList)-1]

		result = append(result, chosenTrack)
		lastArtist = chosenKey
	}

	return result
}

// SelectAudioOutput activa la salida seleccionada en MPD, manteniendo activo el stream HTTP.
func (c *Client) SelectAudioOutput(outputID int) error {
	c.mu.Lock()
	var selectedName string
	var selectedPlugin string
	outsCopy := make([]OutputInfo, len(c.state.AudioOutputs))
	copy(outsCopy, c.state.AudioOutputs)

	for i := range c.state.AudioOutputs {
		if c.state.AudioOutputs[i].ID == outputID {
			selectedName = c.state.AudioOutputs[i].Name
			selectedPlugin = c.state.AudioOutputs[i].Plugin
		}
	}

	if selectedName == "" {
		c.mu.Unlock()
		return fmt.Errorf("salida de audio %d no encontrada", outputID)
	}

	for i := range c.state.AudioOutputs {
		if c.state.AudioOutputs[i].ID == outputID {
			c.state.AudioOutputs[i].Enabled = true
		} else if selectedPlugin != "httpd" && c.state.AudioOutputs[i].Plugin != "httpd" {
			c.state.AudioOutputs[i].Enabled = false
		}
	}
	if selectedPlugin != "httpd" {
		c.state.ActiveOutput = selectedName
	}
	c.mu.Unlock()

	if c.isConnected() {
		var enableErr error
		for _, o := range outsCopy {
			if o.ID == outputID {
				_, enableErr = c.sendRawCommand(fmt.Sprintf("enableoutput %d\n", o.ID))
			} else if selectedPlugin != "httpd" && o.Plugin != "httpd" {
				_, _ = c.sendRawCommand(fmt.Sprintf("disableoutput %d\n", o.ID))
			}
		}
		c.syncOutputsFromMPD()
		if enableErr != nil {
			c.broadcastState()
			return fmt.Errorf("no se pudo activar %s en MPD: %w", selectedName, enableErr)
		}
	}

	outIDStr := strconv.Itoa(outputID)
	outName := selectedName
	outPlugin := selectedPlugin
	c.enqueuePersist(func() {
		_ = c.database.SetSetting("active_audio_output", outIDStr)
		if outPlugin != "httpd" {
			_ = c.database.SetSetting("active_audio_output_name", outName)
		}
	})
	c.broadcastState()
	return nil
}

// isDACOutputName identifica si el nombre de una salida corresponde a un DAC USB o tarjeta I2S HAT externa.
func isDACOutputName(name string) bool {
	lower := strings.ToLower(name)
	for _, kw := range []string{"dac", "usb", "i2s", "hifiberry", "allo", "iqaudio", "justboom", "pisound", "topping", "fiio", "smsl", "codec", "uac", "xmos"} {
		if strings.Contains(lower, kw) {
			return true
		}
	}
	return false
}

// GetAudioDiagnostics devuelve las salidas actuales en MPD y la lectura hardware de aplay -l en el contenedor.
func (c *Client) GetAudioDiagnostics() AudioDiagnostics {
	if c.isConnected() {
		c.syncOutputsFromMPD()
	}
	st := c.GetState()
	hasDAC := false
	hasHW := false
	for _, o := range st.AudioOutputs {
		if o.Plugin == "alsa" || o.Plugin == "pulse" {
			hasHW = true
		}
		if isDACOutputName(o.Name) {
			hasDAC = true
		}
	}

	alsaRaw := ""
	if c.socketPath != "" {
		diagPath := filepath.Join(filepath.Dir(c.socketPath), "alsa_cards.txt")
		if raw, err := os.ReadFile(diagPath); err == nil {
			alsaRaw = strings.TrimSpace(string(raw))
		}
	}

	return AudioDiagnostics{
		Outputs:     st.AudioOutputs,
		Active:      st.ActiveOutput,
		AlsaRaw:     alsaRaw,
		HasDAC:      hasDAC,
		HasHardware: hasHW,
	}
}

// RefreshAudioOutputs solicita a MPD regenerar las tarjetas ALSA detectadas, reconecta y sincroniza la cola si es necesario.
func (c *Client) RefreshAudioOutputs() (AudioDiagnostics, error) {
	if c.socketPath != "" {
		dir := filepath.Dir(c.socketPath)
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			triggerPath := filepath.Join(dir, "reload_outputs")
			_ = os.WriteFile(triggerPath, []byte(fmt.Sprintf("%d\n", time.Now().UnixNano())), 0644)

			// Esperar brevemente a que entrypoint.sh recoja el trigger y reinicie MPD
			deadline := time.Now().Add(3500 * time.Millisecond)
			for time.Now().Before(deadline) {
				if _, statErr := os.Stat(triggerPath); os.IsNotExist(statErr) {
					break
				}
				time.Sleep(150 * time.Millisecond)
			}
			_ = os.Remove(triggerPath)
		}
	}

	// Esperar a que MPD vuelva a responder en el socket/puerto tras regenerar mpd.runtime.conf
	reconnDeadline := time.Now().Add(4 * time.Second)
	for time.Now().Before(reconnDeadline) {
		c.connMu.Lock()
		c.lastDialFail = time.Time{}
		c.connMu.Unlock()
		if _, err := c.sendRawCommand("outputs\n"); err == nil {
			break
		}
		time.Sleep(200 * time.Millisecond)
	}

	c.mu.RLock()
	rg := c.state.ReplayGain
	cf := c.state.Crossfade
	vol := c.state.Volume
	maxVol := c.state.MaxVolumeLimit
	c.mu.RUnlock()
	if vol > maxVol {
		vol = maxVol
	}
	_, _ = c.sendRawCommand(fmt.Sprintf("replay_gain_mode %s\n", rg))
	_, _ = c.sendRawCommand(fmt.Sprintf("crossfade %d\n", cf))
	_, _ = c.sendRawCommand(fmt.Sprintf("setvol %d\n", vol))

	c.syncOutputsFromMPD()

	// Comprobar si MPD perdió la cola durante el reinicio rápido y restaurarla desde memoria
	if lines, err := c.sendRawCommand("status\n"); err == nil {
		mpdPlaylistLen := parseMPDPlaylistLength(lines)
		qCopy, currIdx, wasPlaying, ver := c.snapshotQueueState()
		if len(qCopy) > 0 && (mpdPlaylistLen == 0 || !c.mpdQueueSynced.Load() || c.pendingMPDSync.Load() || (mpdPlaylistLen >= 0 && mpdPlaylistLen != len(qCopy))) {
			c.mpdQueueSynced.Store(false)
			go c.pushQueueToMPD(ver, qCopy, currIdx, wasPlaying)
		}
	}

	c.broadcastState()
	return c.GetAudioDiagnostics(), nil
}

// SetAudioConfig configura opciones audiófilas de ReplayGain y Crossfade en MPD y SQLite.
func (c *Client) SetAudioConfig(replayGain string, crossfade int) {
	c.mu.Lock()
	c.state.ReplayGain = replayGain
	c.state.Crossfade = crossfade
	c.mu.Unlock()

	if c.isConnected() {
		_, _ = c.sendRawCommand(fmt.Sprintf("replay_gain_mode %s\n", replayGain))
		_, _ = c.sendRawCommand(fmt.Sprintf("crossfade %d\n", crossfade))
	}

	rg := replayGain
	cfStr := strconv.Itoa(crossfade)
	c.enqueuePersist(func() {
		_ = c.database.SetSetting("replay_gain", rg)
		_ = c.database.SetSetting("crossfade", cfStr)
	})
	c.broadcastState()
}

// StartSleepTimer programa el apagado gradual de la música transcurridos los minutos indicados.
func (c *Client) StartSleepTimer(minutes int) {
	c.mu.Lock()
	if minutes <= 0 {
		c.sleepEndsAt = time.Time{}
		c.state.SleepTimerSec = 0
		c.mu.Unlock()
		c.broadcastState()
		return
	}

	c.sleepEndsAt = time.Now().Add(time.Duration(minutes) * time.Minute)
	c.state.SleepTimerSec = minutes * 60
	c.mu.Unlock()

	c.broadcastState()
}

// CancelSleepTimer desactiva el temporizador de apagado.
func (c *Client) CancelSleepTimer() {
	c.StartSleepTimer(0)
}

// broadcastState emite el estado del reproductor vía SSE con baja latencia sin bloquear los getters.
func (c *Client) broadcastState() {
	if c.eventsHub != nil {
		c.eventsHub.Broadcast("player_state", c.GetState())
	}
}

// broadcastQueue emite la cola de reproducción actualizada hacia los clientes web.
func (c *Client) broadcastQueue() {
	if c.eventsHub != nil {
		q := c.GetQueue()
		c.eventsHub.Broadcast("queue_update", map[string]interface{}{
			"queue":  q,
			"length": len(q),
		})
	}
}
