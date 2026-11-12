package scanner

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"

	"aurabox/internal/db"
)

// TestPruneMissingSourceTracksOnRescan comprueba que el reescaneo elimina pistas borradas de disco sin tocar orígenes desmontados.
func TestPruneMissingSourceTracksOnRescan(t *testing.T) {
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

	database, err := db.InitDB(filepath.Join(tmpDir, "scanner_prune.db"))
	if err != nil {
		t.Fatalf("InitDB falló: %v", err)
	}
	defer database.Close()

	keepFile := filepath.Join(localDir, "01 - Keep.flac")
	dropFile := filepath.Join(localDir, "02 - Missing.flac")
	if err := os.WriteFile(keepFile, []byte("fLaC"), 0644); err != nil {
		t.Fatalf("WriteFile keepFile falló: %v", err)
	}
	if err := os.WriteFile(dropFile, []byte("fLaC"), 0644); err != nil {
		t.Fatalf("WriteFile dropFile falló: %v", err)
	}

	// Sembrar también una pista NAS en SQLite mientras nasDir está vacío (simulando NAS temporalmente desmontado)
	if err := database.UpsertTrack(&db.Track{
		Path:     filepath.Join(nasDir, "Artist", "Unmounted.flac"),
		Filename: "Unmounted.flac",
		Title:    "Unmounted",
		Artist:   "NAS Band",
		Album:    "NAS Album",
		Duration: 180.0,
		Format:   "FLAC",
		Source:   "nas",
	}); err != nil {
		t.Fatalf("UpsertTrack NAS falló: %v", err)
	}

	sc := NewLibraryScanner(database, nil, localDir, nasDir, usbDir, coversDir)
	defer sc.WaitForIdle()

	var hookCalls atomic.Int32
	sc.SetOnScanComplete(func() {
		hookCalls.Add(1)
	})

	sc.ScanLibrary(false)
	sc.WaitForIdle()

	if hookCalls.Load() != 1 {
		t.Fatalf("se esperaba 1 llamada a onScanComplete, obtenidas %d", hookCalls.Load())
	}

	// Borrar el archivo local del disco y volver a escanear
	if err := os.Remove(dropFile); err != nil {
		t.Fatalf("Remove(dropFile) falló: %v", err)
	}
	sc.ScanLibrary(false)
	sc.WaitForIdle()

	all, err := database.GetAllTracks()
	if err != nil {
		t.Fatalf("GetAllTracks falló: %v", err)
	}

	foundKeep := false
	foundDrop := false
	foundNAS := false
	for _, tr := range all {
		switch tr.Path {
		case keepFile:
			foundKeep = true
		case dropFile:
			foundDrop = true
		default:
			if tr.Source == "nas" {
				foundNAS = true
			}
		}
	}
	if !foundKeep || foundDrop {
		t.Fatalf("poda local incorrecta (foundKeep=%v foundDrop=%v all=%+v)", foundKeep, foundDrop, all)
	}
	if !foundNAS {
		t.Fatalf("la pista NAS no debe borrarse cuando /music/nas está vacío/desmontado")
	}
}

