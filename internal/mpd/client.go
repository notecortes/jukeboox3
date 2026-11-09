package mpd

import (
	"bufio"
	"fmt"
	"log"
	"math/rand"
	"net"
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
}

// Client gestiona la comunicación por socket Unix/TCP con MPD y mantiene el estado en memoria sin bloqueos.
type Client struct {
	socketPath      string
	host            string
	port            string
	eventsHub       *events.Hub
	database        *db.DB
	state           PlayerState
	queue           []db.Track
	mu              sync.RWMutex // Exclusivo para memoria (nunca retener en llamadas de red o SQLite)
	connMu          sync.Mutex   // Serializa el protocolo petición/respuesta en el socket de MPD
	syncMu          sync.Mutex   // Serializa secuencias compuestas de actualización de cola en MPD
	dialMu          sync.Mutex   // Evita intentos paralelos de conexión inicial hacia MPD
	conn            net.Conn
	reader          *bufio.Reader
	lastDialFail    time.Time
	connected       atomic.Bool
	usingUnixSocket atomic.Bool
	syncingQueue    atomic.Bool
	queueVersion    atomic.Uint64
	lastControlNano atomic.Int64
	sleepEndsAt     time.Time
	persistCh       chan func()
	stopCh          chan struct{}
	persistWg       sync.WaitGroup
	closeOnce       sync.Once
	closed          atomic.Bool
}

// NewClient crea un nuevo controlador de audio con soporte Unix socket / TCP, salvaguardas de volumen y recuperación de ajustes.
func NewClient(socketPath, host, port string, hub *events.Hub, database *db.DB) *Client {
	c := &Client{
		socketPath: socketPath,
		host:       host,
		port:       port,
		eventsHub:  hub,
		database:   database,
		queue:      make([]db.Track, 0),
		persistCh:  make(chan func(), 256),
		stopCh:     make(chan struct{}),
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

// tryConnect establece sesión con MPD y aplica opciones iniciales (ReplayGain, Crossfade, volumen seguro y salidas).
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
	qLen := len(c.queue)
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

	if qLen == 0 {
		c.syncQueueFromMPD()
	}
	c.syncStatusFromMPD()
}

// isConnected retorna si existe conexión activa actual con el demonio MPD.
func (c *Client) isConnected() bool {
	return c.connected.Load()
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

	execCmd := func() ([]string, error) {
		_ = c.conn.SetDeadline(time.Now().Add(4 * time.Second))
		if _, err := c.conn.Write([]byte(cmd)); err != nil {
			c.connected.Store(false)
			return nil, err
		}

		var lines []string
		for {
			_ = c.conn.SetDeadline(time.Now().Add(4 * time.Second))
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

// addTrackToMPD adds a track using catalog relative path and falls back to file:///rawmusic/... over Unix socket.
func (c *Client) addTrackToMPD(t db.Track) error {
	if strings.HasPrefix(t.Path, "http://") || strings.HasPrefix(t.Path, "https://") {
		_, err := c.sendRawCommand(fmt.Sprintf("add %s\n", mpdQuote(t.Path)))
		return err
	}

	relPath := strings.TrimPrefix(t.Path, "/music/")
	relPath = strings.TrimPrefix(relPath, "/")
	if relPath != "" {
		_, err := c.sendRawCommand(fmt.Sprintf("add %s\n", mpdQuote(relPath)))
		if err == nil {
			return nil
		}
		if c.usingUnixSocket.Load() {
			rawURI := "file:///rawmusic/" + relPath
			if !strings.HasPrefix(t.Path, "/music/") && strings.HasPrefix(t.Path, "/") {
				rawURI = "file://" + t.Path
			}
			_, rawErr := c.sendRawCommand(fmt.Sprintf("add %s\n", mpdQuote(rawURI)))
			return rawErr
		}
		return err
	}
	return fmt.Errorf("ruta de pista vacía")
}

// addTrackToMPDAt inserta una pista en una posición concreta usando addid para saltos instantáneos en play_now.
func (c *Client) addTrackToMPDAt(t db.Track, pos int) error {
	posSuffix := ""
	if pos >= 0 {
		posSuffix = fmt.Sprintf(" %d", pos)
	}

	if strings.HasPrefix(t.Path, "http://") || strings.HasPrefix(t.Path, "https://") {
		_, err := c.sendRawCommand(fmt.Sprintf("addid %s%s\n", mpdQuote(t.Path), posSuffix))
		return err
	}

	relPath := strings.TrimPrefix(t.Path, "/music/")
	relPath = strings.TrimPrefix(relPath, "/")
	if relPath == "" {
		return fmt.Errorf("ruta de pista vacía")
	}

	_, err := c.sendRawCommand(fmt.Sprintf("addid %s%s\n", mpdQuote(relPath), posSuffix))
	if err == nil {
		return nil
	}

	if c.usingUnixSocket.Load() {
		rawURI := "file:///rawmusic/" + relPath
		if !strings.HasPrefix(t.Path, "/music/") && strings.HasPrefix(t.Path, "/") {
			rawURI = "file://" + t.Path
		}
		_, rawErr := c.sendRawCommand(fmt.Sprintf("addid %s%s\n", mpdQuote(rawURI), posSuffix))
		return rawErr
	}
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
	if err != nil || len(lines) == 0 {
		return
	}

	var tracks []db.Track
	var currentFile string
	var currentTitle string
	var currentArtist string
	var currentAlbum string
	var currentDuration float64

	addCurrent := func() {
		if currentFile == "" {
			return
		}
		fullPath := normalizeMPDFilePath(currentFile)
		if c.database != nil {
			if t, err := c.database.GetTrackByPath(fullPath); err == nil && t != nil {
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
			currentDuration = 0
		case "Title":
			currentTitle = val
		case "Artist":
			currentArtist = val
		case "Album":
			currentAlbum = val
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
		c.mu.Unlock()
		c.broadcastQueue()
	}
}

// syncStatusFromMPD sondea el estado real de MPD para sincronizar tiempo, pista actual, repetición y volumen.
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
	var elapsed float64 = -1
	var duration float64
	var volume = -1
	var hasRepeat bool
	var mpdRepeat bool

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
		}
	}

	c.mu.RLock()
	qLen := len(c.queue)
	c.mu.RUnlock()
	if qLen == 0 && songIdx >= 0 {
		c.syncQueueFromMPD()
	}

	var recordTrackID int64
	var durUpdateTrackID int64
	var durUpdateVal float64
	clampVolTarget := -1

	c.mu.Lock()
	if c.lastControlNano.Load() > startedAt {
		c.mu.Unlock()
		return false
	}
	stateChanged := false
	trackChanged := false

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
	c.mu.Unlock()

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

	if clampVolTarget >= 0 {
		_, _ = c.sendRawCommand(fmt.Sprintf("setvol %d\n", clampVolTarget))
	}

	if trackChanged || stateChanged {
		c.broadcastState()
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
	idx := c.state.CurrentIndex
	trackID := trackCopy.ID
	c.markControlUpdate()
	c.mu.Unlock()

	if c.isConnected() {
		if prevStatus == "pause" {
			_, _ = c.sendRawCommand("pause 0\n")
		} else {
			_, _ = c.sendRawCommand(fmt.Sprintf("play %d\n", idx))
		}
		c.markControlUpdate()
	}

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
	trackID := trackCopy.ID
	c.markControlUpdate()
	c.mu.Unlock()

	if c.isConnected() {
		if _, err := c.sendRawCommand(fmt.Sprintf("play %d\n", index)); err != nil &&
			strings.Contains(err.Error(), "Bad song index") && c.syncingQueue.Load() {
			go c.retryPlayIndexAfterSync(ver, index)
		}
		c.markControlUpdate()
	}

	if trackID > 0 {
		id := trackID
		c.enqueuePersist(func() {
			_ = c.database.RecordPlayback(id)
		})
	}
	c.broadcastState()
}

// retryPlayIndexAfterSync reintenta activar un índice en MPD tras completarse una población de cola en segundo plano.
func (c *Client) retryPlayIndexAfterSync(ver uint64, index int) {
	c.syncMu.Lock()
	defer c.syncMu.Unlock()

	if c.closed.Load() || c.queueVersion.Load() != ver || !c.isConnected() {
		return
	}
	c.mu.RLock()
	stillTarget := (c.state.CurrentIndex == index && c.state.Status == "play")
	c.mu.RUnlock()
	if !stillTarget {
		return
	}
	_, _ = c.sendRawCommand(fmt.Sprintf("play %d\n", index))
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
	c.state.CurrentIndex = nextIdx
	c.state.CurrentTrack = &trackCopy
	c.state.ElapsedTime = 0
	c.state.Duration = trackCopy.Duration
	c.state.Status = "play"
	trackID := trackCopy.ID
	c.markControlUpdate()
	c.mu.Unlock()

	if c.isConnected() {
		_, _ = c.sendRawCommand(fmt.Sprintf("play %d\n", nextIdx))
		c.markControlUpdate()
	}

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
		c.state.ElapsedTime = 0
		c.markControlUpdate()
		c.mu.Unlock()
		if c.isConnected() {
			_, _ = c.sendRawCommand("seekcur 0\n")
			c.markControlUpdate()
		}
		c.broadcastState()
		return
	}

	prevIdx := c.state.CurrentIndex - 1
	if prevIdx < 0 {
		prevIdx = 0
	}

	trackCopy := c.queue[prevIdx]
	c.state.CurrentIndex = prevIdx
	c.state.CurrentTrack = &trackCopy
	c.state.ElapsedTime = 0
	c.state.Duration = trackCopy.Duration
	c.state.Status = "play"
	trackID := trackCopy.ID
	c.markControlUpdate()
	c.mu.Unlock()

	if c.isConnected() {
		_, _ = c.sendRawCommand(fmt.Sprintf("play %d\n", prevIdx))
		c.markControlUpdate()
	}

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
	c.queue = make([]db.Track, len(tracks))
	copy(c.queue, tracks)
	c.state.QueueLength = len(c.queue)

	first := c.queue[0]
	c.state.CurrentIndex = 0
	c.state.CurrentTrack = &first
	c.state.Duration = first.Duration
	c.state.ElapsedTime = 0
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

// pushQueueToMPD reemplaza la cola de MPD y empieza reproduciendo startIdx antes de añadir el resto.
func (c *Client) pushQueueToMPD(ver uint64, tracks []db.Track, startIdx int, autoPlay bool) {
	if len(tracks) == 0 {
		return
	}

	c.syncMu.Lock()
	defer c.syncMu.Unlock()

	c.syncingQueue.Store(true)
	defer c.syncingQueue.Store(false)

	if c.queueVersion.Load() != ver || !c.isConnected() {
		return
	}

	_, _ = c.sendRawCommand("clear\n")

	if startIdx < 0 || startIdx >= len(tracks) {
		startIdx = 0
	}

	// Añadir rápidamente hasta startIdx para comenzar a sonar en <50ms
	for i := 0; i <= startIdx; i++ {
		if c.queueVersion.Load() != ver {
			return
		}
		if err := c.addTrackToMPD(tracks[i]); err != nil {
			log.Printf("[MPD] Advertencia al encolar pista inicial %s: %v", tracks[i].Path, err)
		}
	}

	if autoPlay && c.queueVersion.Load() == ver {
		_, _ = c.sendRawCommand(fmt.Sprintf("play %d\n", startIdx))
	}

	// Encolar el resto de pistas en segundo plano cancelando si el usuario lanzó otra petición
	for i := startIdx + 1; i < len(tracks); i++ {
		if c.queueVersion.Load() != ver {
			return
		}
		if err := c.addTrackToMPD(tracks[i]); err != nil {
			log.Printf("[MPD] Advertencia al añadir pista restante %s: %v", tracks[i].Path, err)
		}
	}
}

// applyQueueDeltaToMPD inserta o anexa pistas en una cola activa de MPD sin interrumpir ni limpiar la cola.
func (c *Client) applyQueueDeltaToMPD(ver uint64, tracks []db.Track, mode string, insertPos, playIdx int, autoPlay bool) {
	c.syncMu.Lock()
	defer c.syncMu.Unlock()

	c.syncingQueue.Store(true)
	defer c.syncingQueue.Store(false)

	if c.queueVersion.Load() != ver || !c.isConnected() {
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
			} else if offset == 0 && autoPlay {
				_, _ = c.sendRawCommand(fmt.Sprintf("play %d\n", playIdx))
			}
		}
		return
	}

	for _, t := range tracks {
		if c.queueVersion.Load() != ver {
			return
		}
		if err := c.addTrackToMPD(t); err != nil {
			log.Printf("[MPD] Advertencia al añadir pista a cola (%s): %v", t.Path, err)
		}
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
	c.mu.Unlock()

	if c.isConnected() {
		go func(v uint64, f, t int) {
			c.syncMu.Lock()
			defer c.syncMu.Unlock()
			if c.queueVersion.Load() != v {
				return
			}
			_, _ = c.sendRawCommand(fmt.Sprintf("move %d %d\n", f, t))
		}(ver, from, to)
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
	}
	c.mu.Unlock()

	if c.isConnected() {
		go func(v uint64, idx int) {
			c.syncMu.Lock()
			defer c.syncMu.Unlock()
			if c.queueVersion.Load() != v {
				return
			}
			_, _ = c.sendRawCommand(fmt.Sprintf("delete %d\n", idx))
		}(ver, index)
	}

	c.broadcastQueue()
	c.broadcastState()
}

// ClearQueue vacía completamente la lista de reproducción.
func (c *Client) ClearQueue() {
	c.mu.Lock()
	_ = c.queueVersion.Add(1)
	c.queue = make([]db.Track, 0)
	c.state.QueueLength = 0
	c.state.CurrentIndex = -1
	c.state.CurrentTrack = nil
	c.state.Status = "stop"
	c.state.ElapsedTime = 0
	c.markControlUpdate()
	c.mu.Unlock()

	if c.isConnected() {
		c.syncMu.Lock()
		_, _ = c.sendRawCommand("clear\n")
		c.syncMu.Unlock()
		c.markControlUpdate()
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

				if c.queueVersion.Load() != ver {
					return
				}
				if currIdx+1 < oldLen {
					_, _ = c.sendRawCommand(fmt.Sprintf("delete %d:%d\n", currIdx+1, oldLen))
				}
				for _, t := range reorderedCopy {
					if c.queueVersion.Load() != ver {
						return
					}
					_ = c.addTrackToMPD(t)
				}
			} else {
				c.pushQueueToMPD(ver, queueSnapshot, 0, false)
			}
		}()
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
		for _, o := range outsCopy {
			if o.ID == outputID {
				_, _ = c.sendRawCommand(fmt.Sprintf("enableoutput %d\n", o.ID))
			} else if selectedPlugin != "httpd" && o.Plugin != "httpd" {
				_, _ = c.sendRawCommand(fmt.Sprintf("disableoutput %d\n", o.ID))
			}
		}
		c.syncOutputsFromMPD()
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
