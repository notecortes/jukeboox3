package scanner

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
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

// TestScannerEmbeddedAndFallbackCovers verifica fallback por imagen arbitraria en carpeta y extracción desde etiquetas embebidas.
func TestScannerEmbeddedAndFallbackCovers(t *testing.T) {
	tmpDir := t.TempDir()
	coversDir := filepath.Join(tmpDir, "covers")
	if err := os.MkdirAll(coversDir, 0755); err != nil {
		t.Fatalf("MkdirAll covers falló: %v", err)
	}
	sc := NewLibraryScanner(nil, nil, tmpDir, "", "", coversDir)

	fakeJPEG := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0x01, 0xFF, 0xD9}

	// 1. Carpeta con archivo de imagen con nombre no estándar ("sleeve_scan.jpg")
	dirFallback := filepath.Join(tmpDir, "ArtistA", "AlbumA")
	if err := os.MkdirAll(dirFallback, 0755); err != nil {
		t.Fatalf("MkdirAll fallback falló: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dirFallback, "sleeve_scan.jpg"), fakeJPEG, 0644); err != nil {
		t.Fatalf("WriteFile sleeve_scan.jpg falló: %v", err)
	}
	trackPathFallback := filepath.Join(dirFallback, "01 - Uno.flac")
	if err := os.WriteFile(trackPathFallback, []byte("fLaC"), 0644); err != nil {
		t.Fatalf("WriteFile track falló: %v", err)
	}
	info1, _ := os.Stat(trackPathFallback)
	tr1 := sc.parseTrackMetadata(trackPathFallback, tmpDir, "local", info1)
	if !strings.HasPrefix(tr1.CoverURL, "/api/covers/") || !strings.HasSuffix(tr1.CoverURL, ".jpg") {
		t.Fatalf("se esperaba carátula fallback desde sleeve_scan.jpg, obtuvo %q", tr1.CoverURL)
	}

	// 2. Carpeta sin archivos de imagen, pero FLAC con bloque PICTURE embebido
	dirEmbedded := filepath.Join(tmpDir, "ArtistB", "AlbumB")
	if err := os.MkdirAll(dirEmbedded, 0755); err != nil {
		t.Fatalf("MkdirAll embedded falló: %v", err)
	}
	fakePNG := []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A, 0x00, 0x00, 0x00, 0x0D}
	picBlock := buildFLACPictureBlock(3, "image/png", fakePNG)
	var flacBuf bytes.Buffer
	flacBuf.WriteString("fLaC")
	pLen := len(picBlock)
	flacBuf.Write([]byte{0x86, byte(pLen >> 16), byte(pLen >> 8), byte(pLen)})
	flacBuf.Write(picBlock)

	trackPathEmb := filepath.Join(dirEmbedded, "01 - Dos.flac")
	if err := os.WriteFile(trackPathEmb, flacBuf.Bytes(), 0644); err != nil {
		t.Fatalf("WriteFile flac emb falló: %v", err)
	}
	info2, _ := os.Stat(trackPathEmb)
	tr2 := sc.parseTrackMetadata(trackPathEmb, tmpDir, "local", info2)
	if !strings.HasPrefix(tr2.CoverURL, "/api/covers/") || !strings.HasSuffix(tr2.CoverURL, ".png") {
		t.Fatalf("se esperaba carátula extraída embebida PNG, obtuvo %q", tr2.CoverURL)
	}
	cachedFile := filepath.Join(coversDir, filepath.Base(tr2.CoverURL))
	if st, err := os.Stat(cachedFile); err != nil || st.Size() != int64(len(fakePNG)) {
		t.Fatalf("archivo de carátula embebida cacheado inválido (%s): err=%v", cachedFile, err)
	}
}


