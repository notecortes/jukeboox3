package scanner

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"aurabox/internal/db"
)

// TestAutoWatcherLocalFileDetection comprueba que crear discos nuevos en localDir dispara escaneo automático sin pulsar Reescanear.
func TestAutoWatcherLocalFileDetection(t *testing.T) {
	tmpDir := t.TempDir()
	localDir := filepath.Join(tmpDir, "local")
	nasDir := filepath.Join(tmpDir, "nas")
	usbDir := filepath.Join(tmpDir, "usb")
	coversDir := filepath.Join(tmpDir, "covers")
	for _, d := range []string{localDir, nasDir, usbDir, coversDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatalf("MkdirAll(%s) falló: %v", d, err)
		}
	}

	database, err := db.InitDB(filepath.Join(tmpDir, "watcher_local.db"))
	if err != nil {
		t.Fatalf("InitDB falló: %v", err)
	}
	defer database.Close()

	sc := NewLibraryScanner(database, nil, localDir, nasDir, usbDir, coversDir)
	sc.ScanLibrary(false)
	sc.WaitForIdle()

	if err := sc.StartAutoWatcherWithPoll(90*time.Millisecond, 100*time.Millisecond); err != nil {
		t.Fatalf("StartAutoWatcherWithPoll falló: %v", err)
	}
	t.Cleanup(func() {
		sc.StopAutoWatcher()
		sc.WaitForIdle()
	})

	// Escribir un subdirectorio nuevo con pista de audio
	albumDir := filepath.Join(localDir, "Depeche Mode", "Songs of Faith")
	if err := os.MkdirAll(albumDir, 0755); err != nil {
		t.Fatalf("MkdirAll albumDir falló: %v", err)
	}
	trackPath := filepath.Join(albumDir, "01 - I Feel You.flac")
	if err := os.WriteFile(trackPath, []byte("fLaC"), 0644); err != nil {
		t.Fatalf("WriteFile trackPath falló: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		tr, _ := database.GetTrackByPath(trackPath)
		if tr != nil && tr.Source == "local" {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}

	t.Fatalf("el watcher automático no indexó %s a tiempo", trackPath)
}

// TestAutoWatcherUSBAddRemoveAndRescan verifica indexación automática al conectar un USB y poda al desconectarlo.
func TestAutoWatcherUSBAddRemoveAndRescan(t *testing.T) {
	tmpDir := t.TempDir()
	localDir := filepath.Join(tmpDir, "local")
	nasDir := filepath.Join(tmpDir, "nas")
	usbDir := filepath.Join(tmpDir, "usb")
	coversDir := filepath.Join(tmpDir, "covers")
	for _, d := range []string{localDir, nasDir, usbDir, coversDir} {
		if err := os.MkdirAll(d, 0755); err != nil {
			t.Fatalf("MkdirAll(%s) falló: %v", d, err)
		}
	}

	database, err := db.InitDB(filepath.Join(tmpDir, "watcher_usb.db"))
	if err != nil {
		t.Fatalf("InitDB falló: %v", err)
	}
	defer database.Close()

	sc := NewLibraryScanner(database, nil, localDir, nasDir, usbDir, coversDir)
	sc.ScanLibrary(false)
	sc.WaitForIdle()

	if err := sc.StartAutoWatcherWithPoll(80*time.Millisecond, 80*time.Millisecond); err != nil {
		t.Fatalf("StartAutoWatcherWithPoll falló: %v", err)
	}
	t.Cleanup(func() {
		sc.StopAutoWatcher()
		sc.WaitForIdle()
	})

	// Simular conexión de pendrive USB en /music/usb/PENDRIVE
	usbMount := filepath.Join(usbDir, "PENDRIVE", "New Order")
	if err := os.MkdirAll(usbMount, 0755); err != nil {
		t.Fatalf("MkdirAll usbMount falló: %v", err)
	}
	usbTrack := filepath.Join(usbMount, "01 - Blue Monday.mp3")
	if err := os.WriteFile(usbTrack, []byte("ID3\x03\x00"), 0644); err != nil {
		t.Fatalf("WriteFile usbTrack falló: %v", err)
	}

	deadlineAdd := time.Now().Add(3 * time.Second)
	indexed := false
	for time.Now().Before(deadlineAdd) {
		tr, _ := database.GetTrackByPath(usbTrack)
		if tr != nil && tr.Source == "usb" {
			indexed = true
			break
		}
		time.Sleep(30 * time.Millisecond)
	}
	if !indexed {
		t.Fatalf("el watcher no indexó automáticamente la pista USB %s", usbTrack)
	}

	// Simular desconexión/desmontaje USB retirando la carpeta de montaje
	if err := os.RemoveAll(filepath.Join(usbDir, "PENDRIVE")); err != nil {
		t.Fatalf("RemoveAll PENDRIVE falló: %v", err)
	}

	deadlineRemove := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadlineRemove) {
		all, _ := database.GetAllTracks()
		if len(all) == 0 {
			return
		}
		time.Sleep(30 * time.Millisecond)
	}

	all, _ := database.GetAllTracks()
	t.Fatalf("tras desconectar el pendrive USB debían podarse las pistas obsoletas; quedaron %d: %+v", len(all), all)
}

