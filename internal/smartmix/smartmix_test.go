package smartmix

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"aurabox/internal/db"
)

// newTestSmartMix crea una instancia SmartMixService respaldada por una SQLite temporal aislada.
func newTestSmartMix(t *testing.T) (*SmartMixService, *db.DB) {
	t.Helper()
	d, err := db.InitDB(filepath.Join(t.TempDir(), "smartmix_test.db"))
	if err != nil {
		t.Fatalf("InitDB falló: %v", err)
	}
	t.Cleanup(func() { _ = d.Close() })
	return NewSmartMixService(d), d
}

// TestGenerateThemeMixGenreAndDecade verifica mixes offline por género y década sin artistas consecutivos.
func TestGenerateThemeMixGenreAndDecade(t *testing.T) {
	svc, d := newTestSmartMix(t)

	artists := []string{"Depeche Mode", "New Order", "Pet Shop Boys", "The Cure"}
	var batch []*db.Track
	for aIdx, art := range artists {
		for i := 1; i <= 4; i++ {
			batch = append(batch, &db.Track{
				Path:        fmt.Sprintf("/music/local/%d/%02d.flac", aIdx, i),
				Filename:    fmt.Sprintf("%02d.flac", i),
				Title:       fmt.Sprintf("%s Track %d", art, i),
				Artist:      art,
				Album:       fmt.Sprintf("%s Best Of", art),
				Genre:       "Synthpop",
				Year:        1982 + i,
				Duration:    210,
				TrackNumber: i,
				Format:      "FLAC",
				Source:      "local",
			})
		}
	}
	batch = append(batch,
		&db.Track{Path: "/music/local/90s/1.flac", Filename: "1.flac", Title: "Oasis 1", Artist: "Oasis", Album: "Morning Glory", Genre: "Britpop", Year: 1995, Duration: 240, TrackNumber: 1, Format: "FLAC", Source: "local"},
		&db.Track{Path: "/music/local/90s/2.flac", Filename: "2.flac", Title: "Blur 1", Artist: "Blur", Album: "Parklife", Genre: "Britpop", Year: 1994, Duration: 200, TrackNumber: 1, Format: "FLAC", Source: "local"},
	)
	if err := d.BatchUpsertTracks(batch); err != nil {
		t.Fatalf("BatchUpsertTracks falló: %v", err)
	}

	// Mix por género
	genreTracks, label, err := svc.GenerateThemeMix("genre", "Synthpop", 12)
	if err != nil {
		t.Fatalf("GenerateThemeMix genre falló: %v", err)
	}
	if label != "Synthpop" || len(genreTracks) != 12 {
		t.Fatalf("esperado label=Synthpop y 12 pistas, obtuvo label=%q len=%d", label, len(genreTracks))
	}
	for i := 1; i < len(genreTracks); i++ {
		if strings.EqualFold(genreTracks[i].Artist, genreTracks[i-1].Artist) {
			t.Fatalf("artista repetido consecutivamente en índice %d: %q", i, genreTracks[i].Artist)
		}
	}

	// Mix por década aceptando formato corto "90s" y año completo "1980"
	decTracks, decLabel, err := svc.GenerateThemeMix("decade", "90s", 10)
	if err != nil {
		t.Fatalf("GenerateThemeMix decade 90s falló: %v", err)
	}
	if decLabel != "90s" || len(decTracks) != 2 {
		t.Fatalf("esperado label=90s y 2 pistas, obtuvo label=%q len=%d", decLabel, len(decTracks))
	}
	for _, tr := range decTracks {
		if tr.Year < 1990 || tr.Year >= 2000 {
			t.Fatalf("pista fuera de los 90s: %+v", tr)
		}
	}
}
