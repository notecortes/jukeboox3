package lyrics

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

// LRCLIBResponse representa la respuesta de la API pública LRCLIB.
type LRCLIBResponse struct {
	ID           int     `json:"id"`
	TrackName    string  `json:"trackName"`
	ArtistName   string  `json:"artistName"`
	AlbumName    string  `json:"albumName"`
	Duration     float64 `json:"duration"`
	Instrumental bool    `json:"instrumental"`
	PlainLyrics  string  `json:"plainLyrics"`
	SyncedLyrics string  `json:"syncedLyrics"`
}

// LyricLine representa una línea de letra musical con marca de tiempo precisa en segundos.
type LyricLine struct {
	Time float64 `json:"time"`
	Text string  `json:"text"`
}

// LyricsResult representa el resultado procesado para el frontend.
type LyricsResult struct {
	Found        bool        `json:"found"`
	Instrumental bool        `json:"instrumental"`
	Plain        string      `json:"plain"`
	Synced       []LyricLine `json:"synced"`
	Source       string      `json:"source"`
}

// Service gestiona las consultas y la caché de letras musicales sincronizadas.
type Service struct {
	httpClient *http.Client
	cache      map[string]*LyricsResult
	mu         sync.RWMutex
}

// NewService crea un nuevo servicio de consulta de letras vía LRCLIB y archivos locales .lrc.
func NewService() *Service {
	return &Service{
		httpClient: &http.Client{
			Timeout: 5 * time.Second,
		},
		cache: make(map[string]*LyricsResult),
	}
}

// cleanSongTitle limpia metadatos ruidosos como numeración de pista y sufijos de remasterización.
func cleanSongTitle(raw string) string {
	s := strings.TrimSpace(raw)

	// Eliminar extensiones de archivo si el título vino del nombre de archivo
	ext := filepath.Ext(s)
	if ext != "" && len(ext) <= 5 {
		s = strings.TrimSuffix(s, ext)
	}

	// Eliminar prefijos de número de pista como "01 - ", "01. ", "1. ", "01 "
	reTrackNum := regexp.MustCompile(`^\s*\d+[\s\.\-_]+\s*`)
	s = reTrackNum.ReplaceAllString(s, "")

	// Eliminar corchetes o paréntesis con etiquetas comunes como [Remastered], (2011 Remaster), [FLAC]
	reNoise := regexp.MustCompile(`(?i)[\(\[]\s*(remaster(?:ed)?(?:\s*\d{4})?|live|acoustic|mono|stereo|bonus\s*track|\d{4}\s*remaster|radio\s*edit|deluxe(?:\s*edition)?|flac|official\s*audio|explicit)[\)\]]`)
	s = reNoise.ReplaceAllString(s, "")

	// Limpiar espacios múltiples y bordes
	s = regexp.MustCompile(`\s+`).ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// cleanArtistName limpia colaboradores adicionales como feat. o ft. para maximizar coincidencia.
func cleanArtistName(raw string) string {
	s := strings.TrimSpace(raw)
	reFeat := regexp.MustCompile(`(?i)\s*[\(\[]?(?:feat\.|featuring|ft\.)[^\)\]]+[\)\]]?`)
	s = reFeat.ReplaceAllString(s, "")
	s = regexp.MustCompile(`\s+`).ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

// checkLocalLRC comprueba si existe un archivo .lrc en disco junto a la pista de audio.
func checkLocalLRC(audioPath string) *LyricsResult {
	if audioPath == "" {
		return nil
	}
	base := strings.TrimSuffix(audioPath, filepath.Ext(audioPath))
	lrcPath := base + ".lrc"
	content, err := os.ReadFile(lrcPath)
	if err != nil {
		// Probar con minúscula .LRC
		lrcPath = base + ".LRC"
		content, err = os.ReadFile(lrcPath)
		if err != nil {
			return nil
		}
	}

	synced := parseLRC(string(content))
	if len(synced) > 0 {
		return &LyricsResult{
			Found:        true,
			Instrumental: false,
			Plain:        string(content),
			Synced:       synced,
			Source:       "local_lrc",
		}
	}
	return nil
}

// GetLyrics obtiene letras sincronizadas o texto plano consultando la API LRCLIB con caché en memoria.
func (s *Service) GetLyrics(trackName, artistName, albumName string, duration float64, audioPath string) (*LyricsResult, error) {
	cleanTitle := cleanSongTitle(trackName)
	cleanArtist := cleanArtistName(artistName)

	cacheKey := fmt.Sprintf("%s|%s", strings.ToLower(cleanTitle), strings.ToLower(cleanArtist))

	s.mu.RLock()
	if cached, ok := s.cache[cacheKey]; ok {
		s.mu.RUnlock()
		return cached, nil
	}
	s.mu.RUnlock()

	// 1. Prioridad: Archivo .lrc local en disco
	if localRes := checkLocalLRC(audioPath); localRes != nil {
		s.mu.Lock()
		s.cache[cacheKey] = localRes
		s.mu.Unlock()
		return localRes, nil
	}

	// 2. Consulta a LRCLIB con estrategia multi-fase (get -> search track/artist -> search q general)
	res := s.fetchLRCLIB(cleanTitle, cleanArtist, albumName, duration)
	if !res.Found && (cleanTitle != trackName || cleanArtist != artistName) {
		// Fallback con nombres sin limpiar por si LRCLIB indexó el título original
		res = s.fetchLRCLIB(trackName, artistName, albumName, duration)
	}

	s.mu.Lock()
	s.cache[cacheKey] = res
	s.mu.Unlock()

	return res, nil
}

// fetchLRCLIB realiza la petición HTTP a la API LRCLIB probando endpoints progresivos.
func (s *Service) fetchLRCLIB(title, artist, album string, duration float64) *LyricsResult {
	emptyRes := &LyricsResult{
		Found:        false,
		Instrumental: false,
		Plain:        "No se encontraron letras disponibles para este tema.",
		Synced:       []LyricLine{},
		Source:       "offline",
	}

	// Fase A: /api/get con título y artista
	getURL := fmt.Sprintf("https://lrclib.net/api/get?track_name=%s&artist_name=%s",
		url.QueryEscape(title),
		url.QueryEscape(artist),
	)
	if album != "" {
		getURL += fmt.Sprintf("&album_name=%s", url.QueryEscape(album))
	}
	if duration > 0 {
		getURL += fmt.Sprintf("&duration=%d", int(duration))
	}

	if item := s.httpGetSingle(getURL); item != nil && (item.SyncedLyrics != "" || item.PlainLyrics != "" || item.Instrumental) {
		return &LyricsResult{
			Found:        true,
			Instrumental: item.Instrumental,
			Plain:        item.PlainLyrics,
			Synced:       parseLRC(item.SyncedLyrics),
			Source:       "lrclib.net/get",
		}
	}

	// Si falló con album y duración exactos, reintentar /api/get solo con track y artista
	if album != "" || duration > 0 {
		simpleGetURL := fmt.Sprintf("https://lrclib.net/api/get?track_name=%s&artist_name=%s",
			url.QueryEscape(title),
			url.QueryEscape(artist),
		)
		if item := s.httpGetSingle(simpleGetURL); item != nil && (item.SyncedLyrics != "" || item.PlainLyrics != "" || item.Instrumental) {
			return &LyricsResult{
				Found:        true,
				Instrumental: item.Instrumental,
				Plain:        item.PlainLyrics,
				Synced:       parseLRC(item.SyncedLyrics),
				Source:       "lrclib.net/get-simple",
			}
		}
	}

	// Fase B: /api/search?track_name=...&artist_name=...
	searchURL := fmt.Sprintf("https://lrclib.net/api/search?track_name=%s&artist_name=%s",
		url.QueryEscape(title),
		url.QueryEscape(artist),
	)
	if item := s.httpGetBestFromSearch(searchURL); item != nil {
		return &LyricsResult{
			Found:        true,
			Instrumental: item.Instrumental,
			Plain:        item.PlainLyrics,
			Synced:       parseLRC(item.SyncedLyrics),
			Source:       "lrclib.net/search",
		}
	}

	// Fase C: /api/search?q=... (búsqueda de texto libre)
	queryText := strings.TrimSpace(artist + " " + title)
	if queryText != "" {
		freeSearchURL := fmt.Sprintf("https://lrclib.net/api/search?q=%s", url.QueryEscape(queryText))
		if item := s.httpGetBestFromSearch(freeSearchURL); item != nil {
			return &LyricsResult{
				Found:        true,
				Instrumental: item.Instrumental,
				Plain:        item.PlainLyrics,
				Synced:       parseLRC(item.SyncedLyrics),
				Source:       "lrclib.net/search-q",
			}
		}
	}

	return emptyRes
}

// httpGetSingle ejecuta una petición GET y decodifica un único objeto LRCLIBResponse.
func (s *Service) httpGetSingle(reqURL string) *LRCLIBResponse {
	req, err := http.NewRequest("GET", reqURL, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("User-Agent", "AuraBoxJukebox/1.0 (aurabox@music.local)")

	resp, err := s.httpClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil
	}
	defer resp.Body.Close()

	var apiResp LRCLIBResponse
	if err := json.NewDecoder(resp.Body).Decode(&apiResp); err != nil {
		return nil
	}
	return &apiResp
}

// httpGetBestFromSearch consulta un endpoint de búsqueda y selecciona la mejor respuesta sincronizada.
func (s *Service) httpGetBestFromSearch(reqURL string) *LRCLIBResponse {
	req, err := http.NewRequest("GET", reqURL, nil)
	if err != nil {
		return nil
	}
	req.Header.Set("User-Agent", "AuraBoxJukebox/1.0 (aurabox@music.local)")

	resp, err := s.httpClient.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}
		return nil
	}
	defer resp.Body.Close()

	var items []LRCLIBResponse
	if err := json.NewDecoder(resp.Body).Decode(&items); err != nil || len(items) == 0 {
		return nil
	}

	// 1. Preferir primera coincidencia con letras sincronizadas
	for _, it := range items {
		if it.SyncedLyrics != "" {
			return &it
		}
	}

	// 2. Si no hay sincronizadas, preferir la primera con texto plano o instrumental
	for _, it := range items {
		if it.PlainLyrics != "" || it.Instrumental {
			return &it
		}
	}

	return nil
}

// parseLRC decodifica el formato de marcas de tiempo LRC `[mm:ss.xx]` en segundos flotantes.
func parseLRC(lrcText string) []LyricLine {
	if lrcText == "" {
		return []LyricLine{}
	}

	re := regexp.MustCompile(`\[(\d+):(\d+(?:\.\d+)?)\](.*)`)
	lines := strings.Split(lrcText, "\n")
	var result []LyricLine

	for _, line := range lines {
		line = strings.TrimSpace(line)
		matches := re.FindStringSubmatch(line)
		if len(matches) == 4 {
			min, _ := strconv.ParseFloat(matches[1], 64)
			sec, _ := strconv.ParseFloat(matches[2], 64)
			text := strings.TrimSpace(matches[3])
			totalSec := (min * 60.0) + sec

			result = append(result, LyricLine{
				Time: totalSec,
				Text: text,
			})
		}
	}
	return result
}
