package covers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestFetchAndCacheAlbumCover descarga y cachea una carátula válida simulando MusicBrainz y Cover Art Archive.
func TestFetchAndCacheAlbumCover(t *testing.T) {
	validJPEG := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F', 0x00, 0x01, 0xFF, 0xD9}

	caaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/release-group/mbid-1/front-500":
			w.WriteHeader(http.StatusNotFound)
		case "/release-group/mbid-1/front":
			w.Header().Set("Content-Type", "image/jpeg")
			_, _ = w.Write(validJPEG)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer caaSrv.Close()

	mbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if ua := r.Header.Get("User-Agent"); !strings.Contains(ua, "AuraBox") {
			t.Fatalf("User-Agent esperado con AuraBox, obtenido %q", ua)
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"release-groups": []map[string]interface{}{
				{"id": "mbid-1", "title": "Violator", "score": 100},
			},
		})
	}))
	defer mbSrv.Close()

	coversDir := t.TempDir()
	svc := NewServiceWithEndpoints(coversDir, mbSrv.URL, caaSrv.URL, caaSrv.Client())

	coverURL, err := svc.FetchAndCacheAlbumCover(context.Background(), "Violator (2006 Remaster)", "Depeche Mode")
	if err != nil {
		t.Fatalf("FetchAndCacheAlbumCover falló: %v", err)
	}
	if !strings.HasPrefix(coverURL, "/api/covers/online_") || !strings.HasSuffix(coverURL, ".jpg") {
		t.Fatalf("coverURL inesperado: %q", coverURL)
	}

	diskFile := filepath.Join(coversDir, strings.TrimPrefix(coverURL, "/api/covers/"))
	saved, err := os.ReadFile(diskFile)
	if err != nil {
		t.Fatalf("no se pudo leer archivo cacheado %s: %v", diskFile, err)
	}
	if len(saved) != len(validJPEG) {
		t.Fatalf("tamaño de imagen cacheada inesperado: %d vs %d", len(saved), len(validJPEG))
	}
}

// TestFetchAndCacheRejectsInvalidMagicBytes rechaza respuestas corruptas o HTML en lugar de imagen real.
func TestFetchAndCacheRejectsInvalidMagicBytes(t *testing.T) {
	caaSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<html>not an image</html>"))
	}))
	defer caaSrv.Close()

	mbSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]interface{}{
			"release-groups": []map[string]interface{}{{"id": "mbid-bad", "title": "Test", "score": 90}},
		})
	}))
	defer mbSrv.Close()

	svc := NewServiceWithEndpoints(t.TempDir(), mbSrv.URL, caaSrv.URL, caaSrv.Client())
	if _, err := svc.FetchAndCacheAlbumCover(context.Background(), "Test", "Artist"); err == nil {
		t.Fatalf("debía rechazar contenido HTML no correspondiente a bytes mágicos de imagen")
	}
}

