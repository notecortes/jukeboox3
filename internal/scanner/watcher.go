package scanner

import (
	"bytes"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
	"unsafe"
)

const (
	defaultWatcherDebounce = 2500 * time.Millisecond
	defaultUSBPollInterval = 3 * time.Second
)

// autoWatcher observa cambios locales con inotify y sondea cambios en submontajes USB para reescanear en segundos.
type autoWatcher struct {
	scanner      *LibraryScanner
	fd           int
	file         *os.File
	debounce     time.Duration
	pollInterval time.Duration
	watchedDirs  map[int]string
	pathToWD     map[string]int
	mu           sync.Mutex
	stopCh       chan struct{}
	triggerCh    chan struct{}
	wg           sync.WaitGroup
	stopOnce     sync.Once
}

// StartAutoWatcher inicia la vigilancia automática en localDir (inotify) y usbDir (inotify + sondeo ligero de subdirectorios).
func (ls *LibraryScanner) StartAutoWatcher(debounce time.Duration) error {
	return ls.StartAutoWatcherWithPoll(debounce, defaultUSBPollInterval)
}

// StartAutoWatcherWithPoll inicia el observador con intervalos configurables para debounce y sondeo de puntos USB.
func (ls *LibraryScanner) StartAutoWatcherWithPoll(debounce, pollInterval time.Duration) error {
	if debounce <= 0 {
		debounce = defaultWatcherDebounce
	}
	if pollInterval <= 0 {
		pollInterval = defaultUSBPollInterval
	}

	ls.watcherMu.Lock()
	defer ls.watcherMu.Unlock()

	if ls.autoWatcher != nil {
		return nil
	}

	fd, err := syscall.InotifyInit1(syscall.IN_NONBLOCK | syscall.IN_CLOEXEC)
	if err != nil {
		return fmt.Errorf("inotify_init1: %w", err)
	}

	aw := &autoWatcher{
		scanner:      ls,
		fd:           fd,
		file:         os.NewFile(uintptr(fd), "aurabox-inotify"),
		debounce:     debounce,
		pollInterval: pollInterval,
		watchedDirs:  make(map[int]string),
		pathToWD:     make(map[string]int),
		stopCh:       make(chan struct{}),
		triggerCh:    make(chan struct{}, 1),
	}

	if ls.localDir != "" {
		aw.addTree(ls.localDir)
	}
	if ls.usbDir != "" {
		aw.addTree(ls.usbDir)
	}

	aw.wg.Add(3)
	go aw.readLoop()
	go aw.usbPollLoop()
	go aw.debounceLoop()

	ls.autoWatcher = aw
	log.Printf("[Watcher] Detección automática activa (local=%q, usb=%q, debounce=%v)", ls.localDir, ls.usbDir, debounce)
	return nil
}

// StopAutoWatcher detiene limpiamente la vigilancia automática del sistema de archivos y espera a sus goroutines.
func (ls *LibraryScanner) StopAutoWatcher() {
	ls.watcherMu.Lock()
	aw := ls.autoWatcher
	ls.autoWatcher = nil
	ls.watcherMu.Unlock()

	if aw != nil {
		aw.close()
	}
}

// close señaliza cierre, cierra el descriptor inotify y espera la salida de los loops en segundo plano.
func (aw *autoWatcher) close() {
	aw.stopOnce.Do(func() {
		close(aw.stopCh)
		if aw.file != nil {
			_ = aw.file.Close()
		}
		aw.wg.Wait()
	})
}

// addTree registra de forma recursiva los subdirectorios existentes en inotify ignorando enlaces o errores transitorios.
func (aw *autoWatcher) addTree(root string) {
	if root == "" {
		return
	}
	if _, err := os.Stat(root); err != nil {
		return
	}
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info == nil || !info.IsDir() {
			return nil
		}
		aw.addWatch(path)
		return nil
	})
}

// addWatch registra un único directorio en inotify evitando duplicados si ya estaba observado.
func (aw *autoWatcher) addWatch(dir string) {
	aw.mu.Lock()
	if _, exists := aw.pathToWD[dir]; exists {
		aw.mu.Unlock()
		return
	}
	aw.mu.Unlock()

	const mask = syscall.IN_CLOSE_WRITE | syscall.IN_CREATE | syscall.IN_DELETE |
		syscall.IN_MOVED_TO | syscall.IN_MOVED_FROM | syscall.IN_DELETE_SELF | syscall.IN_MOVE_SELF
	wd, err := syscall.InotifyAddWatch(aw.fd, dir, mask)
	if err != nil {
		return
	}

	aw.mu.Lock()
	aw.watchedDirs[wd] = dir
	aw.pathToWD[dir] = wd
	aw.mu.Unlock()
}

// isRelevantAudioFilename determina si el nombre corresponde a un formato de audio soportado por el escáner.
func isRelevantAudioFilename(name string) bool {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".mp3", ".flac", ".ogg", ".m4a", ".wav", ".opus", ".aac", ".alac", ".wma":
		return true
	default:
		return false
	}
}

// signalChange solicita un reescaneo debounced sin bloquear al productor de eventos.
func (aw *autoWatcher) signalChange() {
	select {
	case aw.triggerCh <- struct{}{}:
	default:
	}
}

// readLoop consume eventos inotify usando deadline breve para responder rápido a StopAutoWatcher().
func (aw *autoWatcher) readLoop() {
	defer aw.wg.Done()
	buf := make([]byte, 16*1024)

	for {
		select {
		case <-aw.stopCh:
			return
		default:
		}

		if aw.file != nil {
			_ = aw.file.SetReadDeadline(time.Now().Add(250 * time.Millisecond))
		}
		n, err := aw.file.Read(buf)
		if err != nil {
			select {
			case <-aw.stopCh:
				return
			default:
			}
			if os.IsTimeout(err) {
				continue
			}
			if strings.Contains(err.Error(), "closed") {
				return
			}
			time.Sleep(50 * time.Millisecond)
			continue
		}

		aw.processInotifyBuffer(buf[:n])
	}
}

// removeWatchByPath elimina de los mapas internos el watch de un directorio borrado o desmontado para permitir re-registro posterior.
func (aw *autoWatcher) removeWatchByPath(dir string) {
	aw.mu.Lock()
	defer aw.mu.Unlock()
	prefix := dir + string(filepath.Separator)
	for path, wd := range aw.pathToWD {
		if path == dir || strings.HasPrefix(path, prefix) {
			delete(aw.pathToWD, path)
			delete(aw.watchedDirs, wd)
		}
	}
}

// removeWatchByWD limpia los mapas internos al invalidarse un descriptor de vigilancia (IN_IGNORED / IN_DELETE_SELF).
func (aw *autoWatcher) removeWatchByWD(wd int) {
	aw.mu.Lock()
	if dir, ok := aw.watchedDirs[wd]; ok {
		delete(aw.watchedDirs, wd)
		delete(aw.pathToWD, dir)
	}
	aw.mu.Unlock()
}

// processInotifyBuffer decodifica los registros binarios de inotify y dispara rescan ante cambios musicales o carpetas.
func (aw *autoWatcher) processInotifyBuffer(buf []byte) {
	offset := 0
	for offset+syscall.SizeofInotifyEvent <= len(buf) {
		ev := (*syscall.InotifyEvent)(unsafe.Pointer(&buf[offset]))
		offset += syscall.SizeofInotifyEvent

		name := ""
		nameLen := int(ev.Len)
		if nameLen > 0 && offset+nameLen <= len(buf) {
			rawName := buf[offset : offset+nameLen]
			name = string(bytes.TrimRight(rawName, "\x00"))
			offset += nameLen
		}

		aw.mu.Lock()
		dirPath := aw.watchedDirs[int(ev.Wd)]
		aw.mu.Unlock()

		isDir := (ev.Mask & syscall.IN_ISDIR) != 0
		if (ev.Mask & syscall.IN_IGNORED) != 0 {
			aw.removeWatchByWD(int(ev.Wd))
			continue
		}
		if (ev.Mask&(syscall.IN_DELETE_SELF|syscall.IN_MOVE_SELF)) != 0 && dirPath != "" {
			aw.removeWatchByWD(int(ev.Wd))
			aw.signalChange()
			continue
		}

		if name == "" || strings.HasPrefix(name, ".") {
			continue
		}

		fullPath := filepath.Join(dirPath, name)
		if isDir {
			if (ev.Mask & (syscall.IN_CREATE | syscall.IN_MOVED_TO)) != 0 {
				aw.addTree(fullPath)
			} else if (ev.Mask & (syscall.IN_DELETE | syscall.IN_MOVED_FROM)) != 0 {
				aw.removeWatchByPath(fullPath)
			}
			if (ev.Mask & (syscall.IN_CREATE | syscall.IN_DELETE | syscall.IN_MOVED_TO | syscall.IN_MOVED_FROM)) != 0 {
				aw.signalChange()
			}
			continue
		}

		if isRelevantAudioFilename(name) &&
			(ev.Mask&(syscall.IN_CLOSE_WRITE|syscall.IN_MOVED_TO|syscall.IN_MOVED_FROM|syscall.IN_DELETE)) != 0 {
			aw.signalChange()
		}
	}
}

// snapshotUSBSignature obtiene una firma ligera de puntos de montaje USB activos sin recorrer todo el árbol de archivos.
func snapshotUSBSignature(usbDir string) string {
	if usbDir == "" {
		return ""
	}
	entries, err := os.ReadDir(usbDir)
	if err != nil {
		return "err"
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			continue
		}
		info, iErr := entry.Info()
		mod := int64(0)
		if iErr == nil && info != nil {
			mod = info.ModTime().UnixNano()
		}
		childCount := -1
		if entry.IsDir() {
			if sub, sErr := os.ReadDir(filepath.Join(usbDir, entry.Name())); sErr == nil {
				childCount = len(sub)
			}
		}
		names = append(names, fmt.Sprintf("%s:%d:%d", entry.Name(), mod, childCount))
	}
	sort.Strings(names)
	return strings.Join(names, "|")
}

// usbPollLoop detecta montajes/desmontajes USB desde el host incluso cuando Docker bind-mount no reenvía eventos inotify.
func (aw *autoWatcher) usbPollLoop() {
	defer aw.wg.Done()
	if aw.scanner.usbDir == "" {
		return
	}

	lastSig := snapshotUSBSignature(aw.scanner.usbDir)
	ticker := time.NewTicker(aw.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-aw.stopCh:
			return
		case <-ticker.C:
		}

		curSig := snapshotUSBSignature(aw.scanner.usbDir)
		if curSig != lastSig {
			lastSig = curSig
			aw.addTree(aw.scanner.usbDir)
			aw.signalChange()
		}
	}
}

// debounceLoop agrupa ráfagas de copias o desmontajes y lanza ScanLibraryAll() cuando cesa la actividad.
func (aw *autoWatcher) debounceLoop() {
	defer aw.wg.Done()

	var timer *time.Timer
	var timerCh <-chan time.Time

	resetTimer := func(d time.Duration) {
		if timer == nil {
			timer = time.NewTimer(d)
			timerCh = timer.C
			return
		}
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		timer.Reset(d)
	}

	for {
		select {
		case <-aw.stopCh:
			if timer != nil {
				timer.Stop()
			}
			return
		case <-aw.triggerCh:
			resetTimer(aw.debounce)
		case <-timerCh:
			aw.scanner.mu.Lock()
			busy := aw.scanner.scanning
			aw.scanner.mu.Unlock()
			if busy {
				// Reprogramar si aún había un escaneo previo en curso para no perder archivos recién copiados
				resetTimer(500 * time.Millisecond)
				continue
			}
			log.Println("[Watcher] Cambios detectados en música local/USB; lanzando reescaneo incremental...")
			aw.scanner.ScanLibraryAll()
		}
	}
}
