package smartmix

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"aurabox/internal/db"
)

const maxSmartMixHTTPBytes = 2 << 20

// SmartMixService implementa el motor de recomendaciones musicales y mezcla inteligente.
type SmartMixService struct {
	database   *db.DB
	httpClient *http.Client
}

// NewSmartMixService crea una nueva instancia del servicio Smart Mix.
func NewSmartMixService(database *db.DB) *SmartMixService {
	return &SmartMixService{
		database: database,
		httpClient: &http.Client{
			Timeout: 4 * time.Second,
		},
	}
}

// MusicBrainzArtistResponse modelo de respuesta para MusicBrainz.
type MusicBrainzArtistResponse struct {
	Artists []struct {
		Name string `json:"name"`
	} `json:"artists"`
}

// LastFMResponse modelo de respuesta para Last.fm similar artists.
type LastFMResponse struct {
	SimilarArtists struct {
		Artist []struct {
			Name string `json:"name"`
		} `json:"artist"`
	} `json:"similarartists"`
}

// GenerateMix localiza 10 artistas similares al elegido, extrae hasta 10 canciones de cada uno y los remezcla armónicamente.
func (s *SmartMixService) GenerateMix(seedArtist string, targetPerArtist int) ([]db.Track, []string, error) {
	seedArtist = strings.TrimSpace(seedArtist)
	if seedArtist == "" {
		return nil, nil, fmt.Errorf("debes especificar un artista o grupo")
	}

	if targetPerArtist <= 0 {
		targetPerArtist = 10
	}

	// 1. Obtener candidatos a artistas similares (Online + Offline local)
	candidateSimilar := s.fetchSimilarArtists(seedArtist)
	log.Printf("[SmartMix] Candidatos para '%s': %v", seedArtist, candidateSimilar)

	// 2. Comprobar pistas disponibles del artista semilla en biblioteca local
	artistTracksMap := make(map[string][]db.Track)
	var activeArtists []string

	seedTracks, err := s.database.GetTracksByArtist(seedArtist)
	if err == nil && len(seedTracks) > 0 {
		artistTracksMap[seedArtist] = selectTopTracks(seedTracks, targetPerArtist)
		activeArtists = append(activeArtists, seedArtist)
	}

	// 3. Filtrar y garantizar hasta 10 artistas similares disponibles en la biblioteca local/NAS/USB
	for _, art := range candidateSimilar {
		if strings.EqualFold(art, seedArtist) {
			continue
		}
		if _, exists := artistTracksMap[art]; exists {
			continue
		}

		tracks, err := s.database.GetTracksByArtist(art)
		if err == nil && len(tracks) > 0 {
			artistTracksMap[art] = selectTopTracks(tracks, targetPerArtist)
			activeArtists = append(activeArtists, art)
			if len(activeArtists) >= 11 { // Semilla + 10 similares
				break
			}
		}
	}

	// 4. Si aún no alcanzamos 10 similares disponibles en el catálogo local, completar con catálogo offline
	if len(activeArtists) < 11 {
		offlineExtra, err := s.database.GetSimilarArtistsOffline(seedArtist)
		if err == nil {
			for _, art := range offlineExtra {
				if strings.EqualFold(art, seedArtist) {
					continue
				}
				if _, exists := artistTracksMap[art]; exists {
					continue
				}
				tracks, err := s.database.GetTracksByArtist(art)
				if err == nil && len(tracks) > 0 {
					artistTracksMap[art] = selectTopTracks(tracks, targetPerArtist)
					activeArtists = append(activeArtists, art)
					if len(activeArtists) >= 11 {
						break
					}
				}
			}
		}
	}

	if len(activeArtists) == 0 {
		return nil, nil, fmt.Errorf("no se encontraron canciones en tu biblioteca para '%s' ni artistas similares", seedArtist)
	}

	// 5. Remezclar armónicamente (Interleave anti-repetición y anti-clustering)
	remixedTracks := antiRepetitionDistribute(artistTracksMap, activeArtists)

	similarOnly := make([]string, 0)
	for _, a := range activeArtists {
		if !strings.EqualFold(a, seedArtist) {
			similarOnly = append(similarOnly, a)
		}
	}

	return remixedTracks, similarOnly, nil
}

// fetchSimilarArtists consulta fuentes online (Last.fm, MusicBrainz) y el catálogo SQLite local.
func (s *SmartMixService) fetchSimilarArtists(seedArtist string) []string {
	var candidates []string
	seen := make(map[string]bool)
	seen[strings.ToLower(seedArtist)] = true

	// Intentar consulta a Last.fm
	lastfmArtists, err := s.queryLastFM(seedArtist)
	if err == nil && len(lastfmArtists) > 0 {
		for _, a := range lastfmArtists {
			k := strings.ToLower(a)
			if !seen[k] {
				seen[k] = true
				candidates = append(candidates, a)
			}
		}
	}

	// Intentar consulta a MusicBrainz si se necesitan más
	if len(candidates) < 10 {
		mbArtists, err := s.queryMusicBrainz(seedArtist)
		if err == nil && len(mbArtists) > 0 {
			for _, a := range mbArtists {
				k := strings.ToLower(a)
				if !seen[k] {
					seen[k] = true
					candidates = append(candidates, a)
				}
			}
		}
	}

	// Completar con motor de similitud local SQLite (género, época, etiquetas)
	offlineSimilar, err := s.database.GetSimilarArtistsOffline(seedArtist)
	if err == nil && len(offlineSimilar) > 0 {
		for _, a := range offlineSimilar {
			k := strings.ToLower(a)
			if !seen[k] {
				seen[k] = true
				candidates = append(candidates, a)
			}
		}
	}

	return candidates
}

// queryLastFM consulta la API de Last.fm para artistas similares.
func (s *SmartMixService) queryLastFM(seedArtist string) ([]string, error) {
	apiKey := strings.TrimSpace(os.Getenv("LASTFM_API_KEY"))
	if apiKey == "" {
		apiKey = "b25b959554ed76058ac220b7b2e0a026"
	}
	apiURL := fmt.Sprintf("https://ws.audioscrobbler.com/2.0/?method=artist.getsimilar&artist=%s&api_key=%s&format=json&limit=15",
		url.QueryEscape(seedArtist),
		url.QueryEscape(apiKey),
	)
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "AuraBoxJukebox/1.0")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status code %d", resp.StatusCode)
	}

	var lfm LastFMResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxSmartMixHTTPBytes)).Decode(&lfm); err != nil {
		return nil, err
	}

	var results []string
	for _, a := range lfm.SimilarArtists.Artist {
		if a.Name != "" {
			results = append(results, a.Name)
		}
	}
	return results, nil
}

// queryMusicBrainz consulta MusicBrainz para obtener artistas relacionados.
func (s *SmartMixService) queryMusicBrainz(seedArtist string) ([]string, error) {
	apiURL := fmt.Sprintf("https://musicbrainz.org/ws/2/artist/?query=artist:%s&fmt=json&limit=6", url.QueryEscape(seedArtist))
	req, err := http.NewRequest("GET", apiURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "AuraBoxJukebox/1.0 (aurabox@local.pi)")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status code %d", resp.StatusCode)
	}

	var mbResp MusicBrainzArtistResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxSmartMixHTTPBytes)).Decode(&mbResp); err != nil {
		return nil, err
	}

	var results []string
	for _, a := range mbResp.Artists {
		if !strings.EqualFold(a.Name, seedArtist) && a.Name != "" {
			results = append(results, a.Name)
		}
	}
	return results, nil
}

// selectTopTracks escoge hasta N canciones de un artista barajando de forma aleatoria controlada.
func selectTopTracks(tracks []db.Track, count int) []db.Track {
	if count >= len(tracks) {
		copied := make([]db.Track, len(tracks))
		copy(copied, tracks)
		return copied
	}

	r := rand.New(rand.NewSource(time.Now().UnixNano()))
	indices := r.Perm(len(tracks))
	selected := make([]db.Track, 0, count)
	for i := 0; i < count; i++ {
		selected = append(selected, tracks[indices[i]])
	}
	return selected
}

// antiRepetitionDistribute reparte las canciones de forma que ningún grupo o artista suene de forma consecutiva.
func antiRepetitionDistribute(artistTracks map[string][]db.Track, artists []string) []db.Track {
	// Clonar y barajar internamente las canciones de cada artista
	pools := make(map[string][]db.Track)
	totalTracks := 0
	r := rand.New(rand.NewSource(time.Now().UnixNano()))

	for _, art := range artists {
		tList := artistTracks[art]
		if len(tList) == 0 {
			continue
		}
		shuffled := make([]db.Track, len(tList))
		perm := r.Perm(len(tList))
		for idx, p := range perm {
			shuffled[idx] = tList[p]
		}
		k := strings.ToLower(strings.TrimSpace(art))
		pools[k] = shuffled
		totalTracks += len(tList)
	}

	result := make([]db.Track, 0, totalTracks)
	lastArtist := ""

	for len(result) < totalTracks {
		// Candidatos: artistas con canciones disponibles distintos de lastArtist
		var candidates []string
		for k, list := range pools {
			if len(list) > 0 && k != lastArtist {
				candidates = append(candidates, k)
			}
		}

		if len(candidates) == 0 {
			// Solo queda el último artista con canciones restantes
			var remainingArtist string
			for k, list := range pools {
				if len(list) > 0 {
					remainingArtist = k
					break
				}
			}
			if remainingArtist == "" {
				break
			}

			// Intercalar las canciones restantes del mismo artista en huecos válidos del resultado ya generado
			remList := pools[remainingArtist]
			pools[remainingArtist] = nil

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
			if len(pools[c]) > maxCount {
				maxCount = len(pools[c])
			}
		}

		// Filtrar los mejores candidatos con maxCount
		var best []string
		for _, c := range candidates {
			if len(pools[c]) == maxCount {
				best = append(best, c)
			}
		}

		// Escoger uno aleatoriamente entre los mejores para máxima variedad
		chosenKey := best[r.Intn(len(best))]
		chosenList := pools[chosenKey]
		chosenTrack := chosenList[len(chosenList)-1]
		pools[chosenKey] = chosenList[:len(chosenList)-1]

		result = append(result, chosenTrack)
		lastArtist = chosenKey
	}

	return result
}
