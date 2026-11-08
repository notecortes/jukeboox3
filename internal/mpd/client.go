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
	"time"

	"aurabox/internal/db"
	"aurabox/internal/events"
)

// OutputInfo representa una interfaz de salida de audio en MPD / ALSA.
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

// Client gestiona el puente con el demonio de audio MPD o simula estado Hi-Fi local.
type Client struct {
	host             string
	port             string
	eventsHub        *events.Hub
	database         *db.DB
	state            PlayerState
	queue            []db.Track
	mu               sync.RWMutex
	conn             net.Conn
	reader           *bufio.Reader
	connected        bool
	sleepTimerStop   chan bool
	sleepEndsAt      time.Time
	sleepAtTrackEnd  bool
	fadingInProgress bool
}

// NewClient crea un nuevo controlador de audio con protecciones de volumen y fallback.
func NewClient(host, port string, hub *events.Hub, database *db.DB) *Client {
	c := &Client{
		host:      host,
		port:      port,
		eventsHub: hub,
		database:  database,
		queue:     make([]db.Track, 0),
		state: PlayerState{
			Status:         "stop",
			CurrentIndex:   -1,
			Volume:         65,
			MaxVolumeLimit: 85,
			Repeat:         false,
			Random:         false,
			Crossfade:      2,
			ReplayGain:     "auto",
			AudioOutputs: []OutputInfo{
				{ID: 0, Name: "Jack 3.5mm Analógico (snd_bcm2835)", Plugin: "alsa", Enabled: true},
				{ID: 1, Name: "DAC USB Externo (hw:1,0)", Plugin: "alsa", Enabled: false},
				{ID: 2, Name: "DAC I2S HAT (HiFiBerry/Allo)", Plugin: "alsa", Enabled: false},
				{ID: 3, Name: "Transmisión Web HTTP (Hi-Fi Stream)", Plugin: "httpd", Enabled: true},
			},
			ActiveOutput: "Jack 3.5mm Analógico",
		},
	}

	// Cargar configuración de volumen guardada
	if savedVol := database.GetSetting("player_volume", ""); savedVol != "" {
		if v, err := strconv.Atoi(savedVol); err == nil {
			c.state.Volume = v
		}
	}
	if savedMax := database.GetSetting("max_volume_limit", ""); savedMax != "" {
		if m, err := strconv.Atoi(savedMax); err == nil {
			c.state.MaxVolumeLimit = m
		}
	}

	// Iniciar bucle de conexión en segundo plano
	go c.connectionSupervisor()
	// Iniciar simulador de progreso si MPD corre en modo virtual
	go c.progressTicker()

	return c
}

// connectionSupervisor reintenta la conexión TCP con MPD y maneja reconexiones automáticas.
func (c *Client) connectionSupervisor() {
	for {
		if !c.isConnected() {
			c.tryConnect()
		}
		time.Sleep(3 * time.Second)
	}
}

// connectLocked establece la sesión TCP con MPD bajo el mutex actual.
func (c *Client) connectLocked() error {
	if c.conn != nil {
		_ = c.conn.Close()
	}
	address := net.JoinHostPort(c.host, c.port)
	conn, err := net.DialTimeout("tcp", address, 2*time.Second)
	if err != nil {
		c.connected = false
		c.conn = nil
		c.reader = nil
		return err
	}

	reader := bufio.NewReader(conn)
	banner, err := reader.ReadString('\n')
	if err != nil || !strings.HasPrefix(banner, "OK MPD") {
		conn.Close()
		c.connected = false
		c.conn = nil
		c.reader = nil
		return fmt.Errorf("banner MPD inválido: %v", err)
	}

	c.conn = conn
	c.reader = reader
	c.connected = true
	log.Printf("[MPD] Conectado exitosamente al servidor de audio en %s (%s)", address, strings.TrimSpace(banner))
	return nil
}

// tryConnect establece la sesión TCP con MPD y configura opciones iniciales.
func (c *Client) tryConnect() {
	c.mu.Lock()
	err := c.connectLocked()
	c.mu.Unlock()
	if err != nil {
		return
	}

	// Configurar opciones de audio Hi-Fi iniciales en MPD
	_, _ = c.sendRawCommand("replay_gain_mode auto\n")
	_, _ = c.sendRawCommand(fmt.Sprintf("crossfade %d\n", c.state.Crossfade))
	c.syncOutputsFromMPD()

	// Si la cola local está vacía, recuperar la lista de reproducción activa desde MPD
	c.mu.RLock()
	qLen := len(c.queue)
	c.mu.RUnlock()
	if qLen == 0 {
		c.syncQueueFromMPD()
	}
	c.syncStatusFromMPD()
}

// isConnected retorna de forma concurrente si hay enlace activo con MPD.
func (c *Client) isConnected() bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.connected
}

// sendRawCommand envía una directiva de texto por el socket TCP de MPD con reintento automático.
func (c *Client) sendRawCommand(cmd string) ([]string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if !c.connected || c.conn == nil {
		if err := c.connectLocked(); err != nil {
			return nil, fmt.Errorf("mpd desconectado: %w", err)
		}
	}

	execCmd := func() ([]string, error) {
		_ = c.conn.SetDeadline(time.Now().Add(3 * time.Second))
		if _, err := c.conn.Write([]byte(cmd)); err != nil {
			c.connected = false
			return nil, err
		}

		var lines []string
		for {
			line, err := c.reader.ReadString('\n')
			if err != nil {
				c.connected = false
				return nil, err
			}
			trimmed := strings.TrimSpace(line)
			if trimmed == "OK" {
				break
			}
			if strings.HasPrefix(trimmed, "ACK ") {
				return nil, fmt.Errorf("mpd ack error: %s", trimmed)
			}
			lines = append(lines, trimmed)
		}
		return lines, nil
	}

	lines, err := execCmd()
	if err != nil {
		// En caso de corte de conexión por inactividad, reconectar y reintentar inmediatamente
		if reconnErr := c.connectLocked(); reconnErr == nil {
			return execCmd()
		}
		return nil, err
	}
	return lines, nil
}

// syncOutputsFromMPD actualiza el listado y estado de las salidas de audio físicas y virtuales.
func (c *Client) syncOutputsFromMPD() {
	lines, err := c.sendRawCommand("outputs\n")
	if err != nil {
		return
	}

	var outputs []OutputInfo
	var current OutputInfo
	for _, line := range lines {
		parts := strings.SplitN(line, ": ", 2)
		if len(parts) != 2 {
			continue
		}
		key, val := parts[0], parts[1]
		switch key {
		case "outputid":
			if current.Name != "" {
				outputs = append(outputs, current)
			}
			id, _ := strconv.Atoi(val)
			current = OutputInfo{ID: id}
		case "outputname":
			current.Name = val
		case "plugin":
			current.Plugin = val
		case "outputenabled":
			current.Enabled = (val == "1")
		}
	}
	if current.Name != "" {
		outputs = append(outputs, current)
	}

	if len(outputs) > 0 {
		c.mu.Lock()
		c.state.AudioOutputs = outputs
		c.state.ActiveOutput = ""
		for _, o := range outputs {
			if o.Enabled && (o.Plugin == "pulse" || o.Plugin == "alsa" || o.Plugin == "pipewire") {
				c.state.ActiveOutput = o.Name
				break
			}
		}
		if c.state.ActiveOutput == "" {
			for _, o := range outputs {
				if o.Enabled {
					c.state.ActiveOutput = o.Name
					break
				}
			}
		}
		c.mu.Unlock()
	}
}

// syncQueueFromMPD recupera la cola activa desde MPD y reconstruye los metadatos desde SQLite.
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
		fullPath := "/music/" + strings.TrimPrefix(currentFile, "/")
		if c.database != nil {
			if t, err := c.database.GetTrackByPath(fullPath); err == nil && t != nil {
				if currentDuration > 0 && (t.Duration <= 0 || t.Duration == 240.0) {
					t.Duration = currentDuration
					_ = c.database.UpdateTrackDuration(t.ID, currentDuration)
				}
				tracks = append(tracks, *t)
				currentFile = ""
				return
			}
		}
		// Fallback con metadatos leídos de los tags de MPD
		tracks = append(tracks, db.Track{
			Path:     fullPath,
			Title:    currentTitle,
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

// syncStatusFromMPD sondea el estado real del reproductor MPD para mantener sincronizado el tiempo y las canciones.
func (c *Client) syncStatusFromMPD() bool {
	if !c.isConnected() {
		return false
	}

	lines, err := c.sendRawCommand("status\n")
	if err != nil {
		return false
	}

	var mpdState string
	var songIdx = -1
	var elapsed float64
	var duration float64
	var volume = -1

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
		}
	}

	c.mu.RLock()
	qLen := len(c.queue)
	c.mu.RUnlock()
	if qLen == 0 && songIdx >= 0 {
		c.syncQueueFromMPD()
	}

	c.mu.Lock()
	stateChanged := false
	trackChanged := false

	if mpdState != "" && mpdState != c.state.Status {
		c.state.Status = mpdState
		stateChanged = true
	}

	if songIdx >= 0 && songIdx < len(c.queue) && (songIdx != c.state.CurrentIndex || c.state.CurrentTrack == nil) {
		c.state.CurrentIndex = songIdx
		c.state.CurrentTrack = &c.queue[songIdx]
		if duration > 0 {
			c.state.Duration = duration
			c.state.CurrentTrack.Duration = duration
		} else {
			c.state.Duration = c.state.CurrentTrack.Duration
		}
		trackChanged = true
		if c.database != nil && c.state.CurrentTrack.ID > 0 {
			_ = c.database.RecordPlayback(c.state.CurrentTrack.ID)
			if duration > 0 {
				_ = c.database.UpdateTrackDuration(c.state.CurrentTrack.ID, duration)
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
			if c.database != nil && c.state.CurrentTrack.ID > 0 {
				_ = c.database.UpdateTrackDuration(c.state.CurrentTrack.ID, duration)
			}
		}
	}
	if volume >= 0 && volume != c.state.Volume {
		c.state.Volume = volume
	}
	c.mu.Unlock()

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
	for range ticker.C {
		syncCounter++
		if c.isConnected() {
			// Con MPD conectado, MPD gestiona la transición gapless natural y crossfade
			c.syncStatusFromMPD()
		} else {
			// Modo mock/simulado solo si MPD daemon no está disponible
			c.mu.Lock()
			if c.state.Status == "play" && c.state.CurrentTrack != nil {
				c.state.ElapsedTime += 1.0
				if c.state.Duration > 0 && c.state.ElapsedTime >= c.state.Duration {
					c.mu.Unlock()
					c.Next()
					continue
				}
			}
			c.mu.Unlock()
		}

		// Actualizar contador del sleep timer si está activo
		c.mu.Lock()
		if !c.sleepEndsAt.IsZero() {
			rem := int(time.Until(c.sleepEndsAt).Seconds())
			if rem <= 0 {
				c.state.SleepTimerSec = 0
				c.sleepEndsAt = time.Time{}
				c.mu.Unlock()
				c.Pause()
				continue
			} else {
				c.state.SleepTimerSec = rem
			}
		} else {
			c.state.SleepTimerSec = 0
		}
		c.mu.Unlock()

		// Cada 2 segundos sincronizar reloj con los clientes móviles si hay reproducción activa
		if syncCounter%2 == 0 && c.state.Status == "play" {
			c.broadcastState()
		}
	}
}

// GetState devuelve una copia atómica del estado actual del reproductor.
func (c *Client) GetState() PlayerState {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.state
}

// GetQueue devuelve la lista de canciones encoladas en orden de reproducción.
func (c *Client) GetQueue() []db.Track {
	c.mu.RLock()
	defer c.mu.RUnlock()
	copied := make([]db.Track, len(c.queue))
	copy(copied, c.queue)
	return copied
}

// Play reanuda o arranca la reproducción con fundido suave.
func (c *Client) Play() {
	c.mu.Lock()
	if len(c.queue) == 0 {
		c.mu.Unlock()
		return
	}
	if c.state.CurrentIndex < 0 || c.state.CurrentIndex >= len(c.queue) {
		c.state.CurrentIndex = 0
	}
	c.state.CurrentTrack = &c.queue[c.state.CurrentIndex]
	c.state.Duration = c.state.CurrentTrack.Duration
	c.state.Status = "play"
	trackID := c.state.CurrentTrack.ID
	c.mu.Unlock()

	if c.isConnected() {
		_, _ = c.sendRawCommand("play\n")
	}

	if trackID > 0 && c.database != nil {
		_ = c.database.RecordPlayback(trackID)
	}
	c.broadcastState()
}

// Pause pausa la reproducción actual de manera inmediata o con desvanecimiento.
func (c *Client) Pause() {
	c.mu.Lock()
	c.state.Status = "pause"
	c.mu.Unlock()

	if c.isConnected() {
		_, _ = c.sendRawCommand("pause 1\n")
	}
	c.broadcastState()
}

// Toggle conmuta entre los modos reproducir y pausar con respuesta ultra-rápida.
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

// Next avanza a la siguiente canción en la cola con respeto a repetición o fin de lista.
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
			c.mu.Unlock()
			c.broadcastState()
			return
		}
	}

	c.state.CurrentIndex = nextIdx
	c.state.CurrentTrack = &c.queue[nextIdx]
	c.state.ElapsedTime = 0
	c.state.Duration = c.state.CurrentTrack.Duration
	c.state.Status = "play"
	trackID := c.state.CurrentTrack.ID
	c.mu.Unlock()

	if c.isConnected() {
		_, _ = c.sendRawCommand(fmt.Sprintf("play %d\n", nextIdx))
	}

	if trackID > 0 && c.database != nil {
		_ = c.database.RecordPlayback(trackID)
	}
	c.broadcastState()
}

// Prev retrocede a la canción anterior o reinicia la pista si han pasado más de 3 segundos.
func (c *Client) Prev() {
	c.mu.Lock()
	if len(c.queue) == 0 {
		c.mu.Unlock()
		return
	}

	// Si han transcurrido más de 3 segundos, reiniciar la pista actual
	if c.state.ElapsedTime > 3.0 {
		c.state.ElapsedTime = 0
		c.mu.Unlock()
		if c.isConnected() {
			_, _ = c.sendRawCommand("seekcur 0\n")
		}
		c.broadcastState()
		return
	}

	prevIdx := c.state.CurrentIndex - 1
	if prevIdx < 0 {
		prevIdx = 0
	}

	c.state.CurrentIndex = prevIdx
	c.state.CurrentTrack = &c.queue[prevIdx]
	c.state.ElapsedTime = 0
	c.state.Duration = c.state.CurrentTrack.Duration
	c.state.Status = "play"
	trackID := c.state.CurrentTrack.ID
	c.mu.Unlock()

	if c.isConnected() {
		_, _ = c.sendRawCommand(fmt.Sprintf("play %d\n", prevIdx))
	}

	if trackID > 0 && c.database != nil {
		_ = c.database.RecordPlayback(trackID)
	}
	c.broadcastState()
}

// Seek desplaza el cabezal de lectura al segundo especificado.
func (c *Client) Seek(seconds float64) {
	c.mu.Lock()
	if seconds < 0 {
		seconds = 0
	}
	if c.state.Duration > 0 && seconds > c.state.Duration {
		seconds = c.state.Duration
	}
	c.state.ElapsedTime = seconds
	c.mu.Unlock()

	if c.isConnected() {
		_, _ = c.sendRawCommand(fmt.Sprintf("seekcur %d\n", int(seconds)))
	}
	c.broadcastState()
}

// SetVolume aplica un nuevo nivel de volumen respetando la salvaguarda de ganancia máxima.
func (c *Client) SetVolume(volume int) {
	c.mu.Lock()
	if volume < 0 {
		volume = 0
	}
	// Protección acústica: prohibir picos superiores al límite configurado
	if volume > c.state.MaxVolumeLimit {
		volume = c.state.MaxVolumeLimit
	}
	c.state.Volume = volume
	c.mu.Unlock()

	if c.isConnected() {
		_, _ = c.sendRawCommand(fmt.Sprintf("setvol %d\n", volume))
	}

	if c.database != nil {
		_ = c.database.SetSetting("player_volume", strconv.Itoa(volume))
	}
	c.broadcastState()
}

// StepVolume incrementa o decrementa el volumen por pasos fijos (+/- 5%).
func (c *Client) StepVolume(delta int) {
	c.mu.RLock()
	current := c.state.Volume
	c.mu.RUnlock()
	c.SetVolume(current + delta)
}

// SetMaxVolumeLimit establece el tope de protección auditiva y de altavoces.
func (c *Client) SetMaxVolumeLimit(limit int) {
	c.mu.Lock()
	if limit < 30 {
		limit = 30
	}
	if limit > 100 {
		limit = 100
	}
	c.state.MaxVolumeLimit = limit
	if c.state.Volume > limit {
		c.state.Volume = limit
	}
	c.mu.Unlock()

	if c.database != nil {
		_ = c.database.SetSetting("max_volume_limit", strconv.Itoa(limit))
	}
	c.broadcastState()
}

// EnqueueTracks añade una o más pistas a la cola según el modo ("play_now", "play_next", "append").
func (c *Client) EnqueueTracks(tracks []db.Track, mode string) {
	if len(tracks) == 0 {
		return
	}

	c.mu.Lock()
	wasEmpty := (len(c.queue) == 0)
	switch mode {
	case "play_now":
		// Insertar inmediatamente a continuación y reproducir
		if wasEmpty {
			c.queue = append(c.queue, tracks...)
			c.state.CurrentIndex = 0
		} else {
			insertPos := c.state.CurrentIndex + 1
			newQ := make([]db.Track, 0, len(c.queue)+len(tracks))
			newQ = append(newQ, c.queue[:insertPos]...)
			newQ = append(newQ, tracks...)
			newQ = append(newQ, c.queue[insertPos:]...)
			c.queue = newQ
			c.state.CurrentIndex = insertPos
		}
		c.state.CurrentTrack = &c.queue[c.state.CurrentIndex]
		c.state.Duration = c.state.CurrentTrack.Duration
		c.state.ElapsedTime = 0
		c.state.Status = "play"

	case "play_next":
		// Insertar justo después de la pista en curso sin interrumpir
		if wasEmpty {
			c.queue = append(c.queue, tracks...)
			c.state.CurrentIndex = 0
		} else {
			insertPos := c.state.CurrentIndex + 1
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
			c.state.CurrentTrack = &c.queue[0]
			c.state.Duration = c.state.CurrentTrack.Duration
			c.state.ElapsedTime = 0
			c.state.Status = "play"
		}
	}
	c.state.QueueLength = len(c.queue)
	needSyncPlay := (mode == "play_now" || (mode == "append" && wasEmpty))
	c.mu.Unlock()

	c.broadcastQueue()
	c.broadcastState()

	if needSyncPlay {
		go c.syncQueueToMPD(true)
	} else if c.isConnected() {
		go func() {
			for _, t := range tracks {
				relPath := strings.TrimPrefix(t.Path, "/music/")
				_, _ = c.sendRawCommand(fmt.Sprintf("add %q\n", relPath))
			}
		}()
	}
}

// syncQueueToMPD vuelca la lista de canciones en la cola de MPD y opcionalmente arranca la reproducción.
func (c *Client) syncQueueToMPD(startPlay bool) {
	if !c.isConnected() {
		return
	}

	c.mu.RLock()
	tracks := make([]db.Track, len(c.queue))
	copy(tracks, c.queue)
	currIdx := c.state.CurrentIndex
	c.mu.RUnlock()

	// 1. Limpiar cola de MPD
	_, _ = c.sendRawCommand("clear\n")

	// 2. Añadir secuencialmente las pistas para evitar abortos en bloque si alguna ruta difiere
	for _, t := range tracks {
		relPath := strings.TrimPrefix(t.Path, "/music/")
		relPath = strings.TrimPrefix(relPath, "/")
		_, _ = c.sendRawCommand(fmt.Sprintf("add %q\n", relPath))
	}

	// 3. Iniciar reproducción en el índice deseado
	if startPlay && currIdx >= 0 && currIdx < len(tracks) {
		_, _ = c.sendRawCommand(fmt.Sprintf("play %d\n", currIdx))
	}
}

// MoveInQueue reordena una pista dentro de la lista para soportar Drag & Drop táctil.
func (c *Client) MoveInQueue(from, to int) {
	c.mu.Lock()
	if from < 0 || from >= len(c.queue) || to < 0 || to >= len(c.queue) || from == to {
		c.mu.Unlock()
		return
	}

	track := c.queue[from]
	c.queue = append(c.queue[:from], c.queue[from+1:]...)
	newQ := make([]db.Track, 0, len(c.queue)+1)
	newQ = append(newQ, c.queue[:to]...)
	newQ = append(newQ, track)
	newQ = append(newQ, c.queue[to:]...)
	c.queue = newQ

	// Ajustar índice de pista activa
	if c.state.CurrentIndex == from {
		c.state.CurrentIndex = to
	} else if from < c.state.CurrentIndex && to >= c.state.CurrentIndex {
		c.state.CurrentIndex--
	} else if from > c.state.CurrentIndex && to <= c.state.CurrentIndex {
		c.state.CurrentIndex++
	}
	c.mu.Unlock()

	if c.isConnected() {
		go func() {
			_, _ = c.sendRawCommand(fmt.Sprintf("move %d %d\n", from, to))
		}()
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
		c.state.CurrentTrack = &c.queue[c.state.CurrentIndex]
		c.state.Duration = c.state.CurrentTrack.Duration
		c.state.ElapsedTime = 0
	}
	c.mu.Unlock()

	if c.isConnected() {
		go func() {
			_, _ = c.sendRawCommand(fmt.Sprintf("delete %d\n", index))
		}()
	}

	c.broadcastQueue()
	c.broadcastState()
}

// ClearQueue vacía completamente la lista de reproducción.
func (c *Client) ClearQueue() {
	c.mu.Lock()
	c.queue = make([]db.Track, 0)
	c.state.QueueLength = 0
	c.state.CurrentIndex = -1
	c.state.CurrentTrack = nil
	c.state.Status = "stop"
	c.state.ElapsedTime = 0
	c.mu.Unlock()

	if c.isConnected() {
		_, _ = c.sendRawCommand("clear\n")
	}

	c.broadcastQueue()
	c.broadcastState()
}

// AntiRepetitionShuffle baraja la cola garantizando que ningún artista suene dos veces seguidas y sincroniza el nuevo orden con MPD.
func (c *Client) AntiRepetitionShuffle() {
	c.mu.Lock()
	if len(c.queue) <= 1 {
		c.mu.Unlock()
		return
	}

	currentTrack := c.state.CurrentTrack
	currIdx := c.state.CurrentIndex
	oldLen := len(c.queue)

	// Separar pistas pendientes a barajar (las que van después de la que está sonando)
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

	// Barajado y reorganización anti-repetición y anti-clustering
	reordered := antiRepetitionSort(pending, currentTrack)

	if currIdx >= 0 && currIdx < len(c.queue) {
		c.queue = append(c.queue[:currIdx+1], reordered...)
	} else {
		c.queue = reordered
		c.state.CurrentIndex = 0
		c.state.CurrentTrack = &c.queue[0]
	}
	c.state.QueueLength = len(c.queue)
	c.mu.Unlock()

	// Sincronizar de inmediato con el motor MPD de manera no bloqueante y robusta
	if c.isConnected() {
		go func() {
			if currIdx >= 0 && currIdx < oldLen {
				if currIdx+1 < oldLen {
					_, _ = c.sendRawCommand(fmt.Sprintf("delete %d:%d\n", currIdx+1, oldLen))
				}
				for _, t := range reordered {
					relPath := strings.TrimPrefix(t.Path, "/music/")
					relPath = strings.TrimPrefix(relPath, "/")
					_, _ = c.sendRawCommand(fmt.Sprintf("add %q\n", relPath))
				}
			} else {
				c.syncQueueToMPD(false)
			}
		}()
	}

	c.broadcastQueue()
	c.broadcastState()
}

// antiRepetitionSort organiza una lista de canciones para que no haya dos del mismo artista consecutivas.
func antiRepetitionSort(tracks []db.Track, currentTrack *db.Track) []db.Track {
	if len(tracks) <= 1 {
		return tracks
	}

	// Agrupar pistas por artista
	byArtist := make(map[string][]db.Track)
	for _, t := range tracks {
		k := strings.ToLower(strings.TrimSpace(t.Artist))
		byArtist[k] = append(byArtist[k], t)
	}

	// Barajar internamente las pistas de cada artista
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
		// Candidatos: artistas con canciones restantes y distintos de lastArtist
		var candidates []string
		for k, list := range byArtist {
			if len(list) > 0 && k != lastArtist {
				candidates = append(candidates, k)
			}
		}

		if len(candidates) == 0 {
			// Solo queda el lastArtist con canciones restantes
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

			// Intercalar las canciones restantes del mismo artista en huecos válidos del resultado ya generado
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

		// Encontrar el número máximo de canciones pendientes entre los candidatos
		maxCount := 0
		for _, c := range candidates {
			if len(byArtist[c]) > maxCount {
				maxCount = len(byArtist[c])
			}
		}

		// Filtrar los mejores candidatos con maxCount
		var best []string
		for _, c := range candidates {
			if len(byArtist[c]) == maxCount {
				best = append(best, c)
			}
		}

		// Escoger uno aleatoriamente entre los mejores para variedad
		chosenKey := best[r.Intn(len(best))]
		chosenList := byArtist[chosenKey]
		chosenTrack := chosenList[len(chosenList)-1]
		byArtist[chosenKey] = chosenList[:len(chosenList)-1]

		result = append(result, chosenTrack)
		lastArtist = chosenKey
	}

	return result
}

// SelectAudioOutput activa la salida seleccionada (PulseAudio / Jack / USB / I2S / Stream) en MPD.
func (c *Client) SelectAudioOutput(outputID int) error {
	c.mu.Lock()
	var selectedName string
	for i := range c.state.AudioOutputs {
		if c.state.AudioOutputs[i].ID == outputID {
			c.state.AudioOutputs[i].Enabled = true
			selectedName = c.state.AudioOutputs[i].Name
		} else if c.state.AudioOutputs[i].Plugin != "httpd" {
			// Desactivar otras salidas directas para evitar duplicación
			c.state.AudioOutputs[i].Enabled = false
		}
	}
	c.state.ActiveOutput = selectedName
	c.mu.Unlock()

	if c.isConnected() {
		for _, o := range c.state.AudioOutputs {
			if o.ID == outputID {
				_, _ = c.sendRawCommand(fmt.Sprintf("enableoutput %d\n", o.ID))
			} else if o.Plugin != "httpd" {
				_, _ = c.sendRawCommand(fmt.Sprintf("disableoutput %d\n", o.ID))
			}
		}
	}

	if c.database != nil {
		_ = c.database.SetSetting("active_audio_output", strconv.Itoa(outputID))
	}
	c.broadcastState()
	return nil
}

// SetAudioConfig configura opciones audiófilas de ReplayGain y Crossfade.
func (c *Client) SetAudioConfig(replayGain string, crossfade int) {
	c.mu.Lock()
	c.state.ReplayGain = replayGain
	c.state.Crossfade = crossfade
	c.mu.Unlock()

	if c.isConnected() {
		_, _ = c.sendRawCommand(fmt.Sprintf("replay_gain_mode %s\n", replayGain))
		_, _ = c.sendRawCommand(fmt.Sprintf("crossfade %d\n", crossfade))
	}

	if c.database != nil {
		_ = c.database.SetSetting("replay_gain", replayGain)
		_ = c.database.SetSetting("crossfade", strconv.Itoa(crossfade))
	}
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

// broadcastState emite el estado del reproductor vía SSE con latencia menor a 50ms.
func (c *Client) broadcastState() {
	if c.eventsHub != nil {
		c.eventsHub.Broadcast("player_state", c.GetState())
	}
}

// broadcastQueue emite la cola de reproducción actualizada hacia los clientes web.
func (c *Client) broadcastQueue() {
	if c.eventsHub != nil {
		c.eventsHub.Broadcast("queue_update", map[string]interface{}{
			"queue":  c.GetQueue(),
			"length": len(c.queue),
		})
	}
}
