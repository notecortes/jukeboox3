package lyrics

import (
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"

	"aurabox/internal/db"
)

// TestSQLiteLyricsCacheHitAndPersistence valida que las letras se persisten en SQLite y evitan consultas externas posteriores.
func TestSQLiteLyricsCacheHitAndPersistence(t *testing.T) {
	tmpDir := t.TempDir()
	database, err := db.InitDB(filepath.Join(tmpDir, "lyrics.db"))
	if err != nil {
		t.Fatalf("InitDB falló: %v", err)
	}
	defer database.Close()

	// Sembrar entrada en SQLite directamente como si hubiera sido descargada en una sesión previa
	err = database.SaveLyricsCache(
		"bohemian rhapsody|queen",
		true,
		false,
		"Is this the real life?",
		`[{"time":1.5,"text":"Is this the real life?"}]`,
		"lrclib.net/get",
	)
	if err != nil {
		t.Fatalf("SaveLyricsCache falló: %v", err)
	}

	var networkHits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		networkHits.Add(1)
		http.NotFound(w, r)
	}))
	defer srv.Close()

	svc := NewService()
	svc.httpClient = srv.Client()
	svc.SetStore(database)

	res, err := svc.GetLyrics("01 - Bohemian Rhapsody (2011 Remaster)", "Queen", "A Night at the Opera", 354, "")
	if err != nil {
		t.Fatalf("GetLyrics falló: %v", err)
	}
	if !res.Found || len(res.Synced) != 1 || res.Synced[0].Text != "Is this the real life?" {
		t.Fatalf("resultado inesperado desde caché SQLite: %+v", res)
	}
	if networkHits.Load() != 0 {
		t.Fatalf("no debían hacerse peticiones HTTP al existir caché SQLite, hits=%d", networkHits.Load())
	}
}
