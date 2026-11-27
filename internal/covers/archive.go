package covers

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"aurabox/internal/mpd"
)

const (
	defaultMusicBrainzURL = "https://musicbrainz.org"
	defaultCoverArtURL    = "https://coverartarchive.org"
	defaultITunesURL      = "https://itunes.apple.com"
	defaultDeezerURL      = "https://api.deezer.com"
	maxOnlineCoverBytes   = 3 << 20 // 3 MB límite máximo de carátula
	auraboxUserAgent      = "AuraBox/1.0 (https://github.com/aurabox)"
	negativeCacheTTL      = 10 * time.Minute
)

// reNoiseTags limpia etiquetas de compresión, tasas de bits, años y metadatos superfluos.
var reNoiseTags = regexp.MustCompile(`(?i)\((?:(?:128|192|256|320)\s*(?:kbps|kbit|kbit_aac|aac|mp3)[^)]*|[^)]*remaster[^)]*|bonus track|official|audio|video|clip|lyrics|live|en vivo|radio edit)\)|\[(?:(?:128|192|256|320)\s*(?:kbps|kbit|kbit_aac|aac|mp3)[^\]]*|FLAC|PMEDIA|Lossless|Hunter|FRG|320kbps|24Bit[^\]]*|[^\]]*)\]|\((?:19|20)\d{2}[^)]*\)`)

// mbSearchResponse modela la respuesta JSON de búsqueda de release-groups en MusicBrainz.
type mbSearchResponse struct {
	ReleaseGroups []struct {
		ID    string `json:"id"`
		Title string `json:"title"`
		Score int    `json:"score"`
	} `json:"release-groups"`
}

// itunesSearchResponse modela los resultados de iTunes Search API.
type itunesSearchResponse struct {
	ResultCount int `json:"resultCount"`
	Results     []struct {
		WrapperType   string `json:"wrapperType"`
		Kind          string `json:"kind"`
		ArtistName    string `json:"artistName"`
		CollectionName string `json:"collectionName"`
		TrackName     string `json:"trackName"`
		ArtworkURL100 string `json:"artworkUrl100"`
		ArtworkURL600 string `json:"artworkUrl600"`
	} `json:"results"`
}

// deezerSearchResponse modela los resultados devueltos por el motor de búsqueda de Deezer.
type deezerSearchResponse struct {
	Total int `json:"total"`
	Data  []struct {
		Title  string `json:"title"`
		Artist struct {
			Name string `json:"name"`
		} `json:"artist"`
		Album struct {
			Title       string `json:"title"`
			CoverBig    string `json:"cover_big"`
			CoverXL     string `json:"cover_xl"`
			CoverMedium string `json:"cover_medium"`
		} `json:"album"`
	} `json:"data"`
}

// Service gestiona la búsqueda y descarga segura de carátulas desde múltiples proveedores (Cover Art Archive, iTunes, Deezer).
type Service struct {
	coversDir    string
	mbBaseURL    string
	caaBaseURL   string
	itunesBaseURL string
	deezerBaseURL string
	httpClient   *http.Client
	failedCache  sync.Map // cache de búsquedas fallidas recientes para evitar sobrecargar redes
}

// NewService crea un servicio de descarga de carátulas configurado con cliente HTTP anti-SSRF/DNS-rebinding.
func NewService(coversDir string) *Service {
	client := mpd.NewSafeExternalHTTPClient(10 * time.Second)
	return &Service{
		coversDir:     coversDir,
		mbBaseURL:     defaultMusicBrainzURL,
		caaBaseURL:    defaultCoverArtURL,
		itunesBaseURL: defaultITunesURL,
		deezerBaseURL: defaultDeezerURL,
		httpClient:    client,
	}
}

// NewServiceWithEndpoints crea una instancia configurable para pruebas unitarias aisladas sin acceso a internet.
func NewServiceWithEndpoints(coversDir, mbBaseURL, caaBaseURL string, client *http.Client) *Service {
	if client == nil {
		client = NewService(coversDir).httpClient
	}
	return &Service{
		coversDir:     coversDir,
		mbBaseURL:     strings.TrimRight(mbBaseURL, "/"),
		caaBaseURL:    strings.TrimRight(caaBaseURL, "/"),
		itunesBaseURL: "", // en tests unitarios de endpoint no se consultan APIs externas salvo que se configuren
		deezerBaseURL: "",
		httpClient:    client,
	}
}

// CleanQuery limpia etiquetas de ripeo, tasas de bits, años o artefactos de texto para mejorar la coincidencia en APIs.
func CleanQuery(raw string) string {
	cleaned := reNoiseTags.ReplaceAllString(raw, " ")
	cleaned = strings.ReplaceAll(cleaned, "⭐️", " ")
	cleaned = strings.ReplaceAll(cleaned, "_", " ")
	return strings.Join(strings.Fields(cleaned), " ")
}

// detectOnlineImageExt valida los bytes mágicos reales de la imagen descargada y devuelve su extensión.
func detectOnlineImageExt(data []byte) (string, bool) {
	if len(data) >= 3 && data[0] == 0xFF && data[1] == 0xD8 && data[2] == 0xFF {
		return ".jpg", true
	}
	if len(data) >= 8 && bytes.Equal(data[:8], []byte{0x89, 0x50, 0x4E, 0x47, 0x0D, 0x0A, 0x1A, 0x0A}) {
		return ".png", true
	}
	if len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")) {
		return ".webp", true
	}
	if len(data) >= 4 && bytes.Equal(data[:4], []byte("GIF8")) {
		return ".gif", true
	}
	return "", false
}

// sanitizeLuceneTerm elimina caracteres especiales Lucene de MusicBrainz preservando texto legible.
func sanitizeLuceneTerm(s string) string {
	replacer := strings.NewReplacer(`"`, " ", `\`, " ", `:`, " ", `(`, " ", `)`, " ", `[`, " ", `]`, " ", `*`, " ", `?`, " ")
	return strings.Join(strings.Fields(replacer.Replace(s)), " ")
}

// searchReleaseGroupIDs consulta MusicBrainz buscando identificadores MBID candidatos del álbum.
func (s *Service) searchReleaseGroupIDs(ctx context.Context, luceneQuery string) ([]string, error) {
	reqURL := fmt.Sprintf("%s/ws/2/release-group/?query=%s&fmt=json&limit=5", s.mbBaseURL, url.QueryEscape(luceneQuery))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", auraboxUserAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("musicbrainz respondió HTTP %d", resp.StatusCode)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 512*1024))
	if err != nil {
		return nil, err
	}

	var parsed mbSearchResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, err
	}

	ids := make([]string, 0, len(parsed.ReleaseGroups))
	for _, rg := range parsed.ReleaseGroups {
		id := strings.TrimSpace(rg.ID)
		if id != "" {
			ids = append(ids, id)
		}
	}
	return ids, nil
}

// downloadFrontImage descarga la portada frontal desde Cover Art Archive validando tamaño y bytes mágicos.
func (s *Service) downloadFrontImage(ctx context.Context, mbid string) ([]byte, string, error) {
	candidates := []string{
		fmt.Sprintf("%s/release-group/%s/front-500", s.caaBaseURL, url.PathEscape(mbid)),
		fmt.Sprintf("%s/release-group/%s/front", s.caaBaseURL, url.PathEscape(mbid)),
	}

	for _, imgURL := range candidates {
		data, ext, err := s.downloadImageFromURL(ctx, imgURL)
		if err == nil && len(data) > 0 {
			return data, ext, nil
		}
	}

	return nil, "", fmt.Errorf("sin carátula disponible en Cover Art Archive para %s", mbid)
}

// downloadImageFromURL descarga una imagen HTTP/HTTPS validando que no exceda el límite y posea cabeceras de imagen válidas.
func (s *Service) downloadImageFromURL(ctx context.Context, imgURL string) ([]byte, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, imgURL, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", auraboxUserAgent)

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("servidor devolvió código HTTP %d para imagen", resp.StatusCode)
	}

	data, err := io.ReadAll(io.LimitReader(resp.Body, maxOnlineCoverBytes+1))
	if err != nil {
		return nil, "", err
	}
	if len(data) == 0 || len(data) > maxOnlineCoverBytes {
		return nil, "", fmt.Errorf("tamaño de imagen fuera de rango (leídos %d bytes)", len(data))
	}

	ext, valid := detectOnlineImageExt(data)
	if !valid {
		return nil, "", fmt.Errorf("formato o cabecera mágica de imagen no reconocida")
	}

	return data, ext, nil
}

// isGenericTitle determina si el nombre de álbum o artista es un marcador genérico que no conviene buscar tal cual.
func isGenericTitle(s string) bool {
	clean := strings.ToLower(strings.TrimSpace(s))
	return clean == "" ||
		clean == "sencillos" ||
		clean == "singles" ||
		clean == "desconocido" ||
		clean == "unknown" ||
		clean == "various artists" ||
		clean == "varios artistas" ||
		clean == "radio web" ||
		clean == "podcast"
}

// searchITunesCover busca una carátula mediante iTunes Search API y devuelve los bytes y extensión si la encuentra.
func (s *Service) searchITunesCover(ctx context.Context, query string, isSong bool) ([]byte, string, error) {
	if s.itunesBaseURL == "" || query == "" {
		return nil, "", fmt.Errorf("consulta vacía o servicio no configurado")
	}

	entity := "album"
	if isSong {
		entity = "song"
	}

	reqURL := fmt.Sprintf("%s/search?term=%s&entity=%s&limit=3", s.itunesBaseURL, url.QueryEscape(query), entity)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", auraboxUserAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("iTunes API respondió HTTP %d", resp.StatusCode)
	}

	var parsed itunesSearchResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 512*1024)).Decode(&parsed); err != nil {
		return nil, "", err
	}

	for _, item := range parsed.Results {
		imgURL := item.ArtworkURL600
		if imgURL == "" && item.ArtworkURL100 != "" {
			// Elevar resolución a 600x600 sustituyendo el sufijo de miniatura
			imgURL = strings.Replace(item.ArtworkURL100, "100x100bb.jpg", "600x600bb.jpg", 1)
		}
		if imgURL != "" {
			data, ext, err := s.downloadImageFromURL(ctx, imgURL)
			if err == nil && len(data) > 0 {
				return data, ext, nil
			}
		}
	}

	return nil, "", fmt.Errorf("no se encontraron carátulas válidas en iTunes para %q", query)
}

// searchDeezerCover busca una carátula en la API abierta de Deezer y devuelve sus bytes y extensión.
func (s *Service) searchDeezerCover(ctx context.Context, query string) ([]byte, string, error) {
	if s.deezerBaseURL == "" || query == "" {
		return nil, "", fmt.Errorf("consulta vacía o Deezer no configurado")
	}

	reqURL := fmt.Sprintf("%s/search?q=%s&limit=3", s.deezerBaseURL, url.QueryEscape(query))
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("User-Agent", auraboxUserAgent)
	req.Header.Set("Accept", "application/json")

	resp, err := s.httpClient.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, "", fmt.Errorf("Deezer API respondió HTTP %d", resp.StatusCode)
	}

	var parsed deezerSearchResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 512*1024)).Decode(&parsed); err != nil {
		return nil, "", err
	}

	for _, item := range parsed.Data {
		imgURL := item.Album.CoverXL
		if imgURL == "" {
			imgURL = item.Album.CoverBig
		}
		if imgURL == "" {
			imgURL = item.Album.CoverMedium
		}
		if imgURL != "" {
			data, ext, err := s.downloadImageFromURL(ctx, imgURL)
			if err == nil && len(data) > 0 {
				return data, ext, nil
			}
		}
	}

	return nil, "", fmt.Errorf("no se encontraron carátulas en Deezer para %q", query)
}

// FindCachedCoverURL busca si ya existe en disco una carátula descargada para la clave dada.
func (s *Service) FindCachedCoverURL(key string) string {
	if s.coversDir == "" || key == "" {
		return ""
	}
	hash := md5.Sum([]byte(strings.ToLower(strings.TrimSpace(key))))
	prefix := "online_" + hex.EncodeToString(hash[:])
	for _, ext := range []string{".jpg", ".png", ".webp", ".gif"} {
		path := filepath.Join(s.coversDir, prefix+ext)
		if fi, err := os.Stat(path); err == nil && fi.Size() > 0 {
			return fmt.Sprintf("/api/covers/%s%s", prefix, ext)
		}
	}
	return ""
}

// saveCoverFile guarda los bytes de imagen en disco con nombre hasheado determinista y devuelve la URL relativa.
func (s *Service) saveCoverFile(imgBytes []byte, imgExt, cacheKey string) (string, error) {
	if s.coversDir == "" {
		return "", fmt.Errorf("directorio de carátulas no configurado")
	}
	if err := os.MkdirAll(s.coversDir, 0755); err != nil {
		return "", err
	}

	hash := md5.Sum([]byte(strings.ToLower(strings.TrimSpace(cacheKey))))
	fileName := "online_" + hex.EncodeToString(hash[:]) + imgExt
	destPath := filepath.Join(s.coversDir, fileName)
	tmpPath := destPath + ".tmp"

	if err := os.WriteFile(tmpPath, imgBytes, 0644); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}
	if err := os.Rename(tmpPath, destPath); err != nil {
		_ = os.Remove(tmpPath)
		return "", err
	}

	return fmt.Sprintf("/api/covers/%s", fileName), nil
}

// isRecentlyFailed comprueba si la búsqueda falló hace poco tiempo para evitar saturar las APIs.
func (s *Service) isRecentlyFailed(key string) bool {
	if val, ok := s.failedCache.Load(key); ok {
		if t, okTime := val.(time.Time); okTime && time.Since(t) < negativeCacheTTL {
			return true
		}
	}
	return false
}

// markFailed registra temporalmente una búsqueda infructuosa en la caché negativa.
func (s *Service) markFailed(key string) {
	s.failedCache.Store(key, time.Now())
}

// FetchAndCacheAlbumCover busca la portada del álbum en Cover Art Archive, iTunes o Deezer y la guarda en caché local.
func (s *Service) FetchAndCacheAlbumCover(ctx context.Context, albumTitle, artistName string) (string, error) {
	cleanAlbum := CleanQuery(albumTitle)
	cleanArtist := CleanQuery(artistName)
	if cleanAlbum == "" {
		return "", fmt.Errorf("título de álbum vacío")
	}

	cacheKey := cleanAlbum + "\x00" + cleanArtist
	if existing := s.FindCachedCoverURL(cacheKey); existing != "" {
		return existing, nil
	}
	if s.isRecentlyFailed(cacheKey) {
		return "", fmt.Errorf("búsqueda previa de álbum sin resultados recientes")
	}

	// 1. Intentar MusicBrainz + Cover Art Archive
	var mbids []string
	luceneAlbum := sanitizeLuceneTerm(cleanAlbum)
	luceneArtist := sanitizeLuceneTerm(cleanArtist)

	if !isGenericTitle(cleanArtist) {
		query := fmt.Sprintf(`releasegroup:"%s" AND artist:"%s"`, luceneAlbum, luceneArtist)
		if ids, err := s.searchReleaseGroupIDs(ctx, query); err == nil {
			mbids = ids
		}
	}
	if len(mbids) == 0 {
		query := fmt.Sprintf(`releasegroup:"%s"`, luceneAlbum)
		if ids, err := s.searchReleaseGroupIDs(ctx, query); err == nil {
			mbids = ids
		}
	}

	if len(mbids) > 0 {
		for _, mbid := range mbids {
			data, ext, dlErr := s.downloadFrontImage(ctx, mbid)
			if dlErr == nil && len(data) > 0 {
				return s.saveCoverFile(data, ext, cacheKey)
			}
		}
	}

	// 2. Fallback a iTunes para el álbum
	queryTerm := cleanAlbum
	if !isGenericTitle(cleanArtist) {
		queryTerm = cleanArtist + " " + cleanAlbum
	}
	if data, ext, err := s.searchITunesCover(ctx, queryTerm, false); err == nil && len(data) > 0 {
		return s.saveCoverFile(data, ext, cacheKey)
	}

	// 3. Fallback a Deezer para el álbum
	if data, ext, err := s.searchDeezerCover(ctx, queryTerm); err == nil && len(data) > 0 {
		return s.saveCoverFile(data, ext, cacheKey)
	}

	s.markFailed(cacheKey)
	return "", fmt.Errorf("no se encontró carátula para el álbum %q", albumTitle)
}

// FetchAndCacheTrackCover busca portada online para una canción o sencillo reproduciéndose actualmente.
func (s *Service) FetchAndCacheTrackCover(ctx context.Context, title, artist, album string) (string, error) {
	cleanTitle := CleanQuery(title)
	cleanArtist := CleanQuery(artist)
	cleanAlbum := CleanQuery(album)

	if cleanTitle == "" {
		return "", fmt.Errorf("título de canción vacío")
	}

	// Clave de búsqueda única para la pista
	cacheKey := cleanTitle + "\x00" + cleanArtist + "\x00" + cleanAlbum
	if existing := s.FindCachedCoverURL(cacheKey); existing != "" {
		return existing, nil
	}

	// Si tiene álbum real asociado y no es sencillo genérico, comprobar si el álbum ya está cacheado
	if !isGenericTitle(cleanAlbum) {
		albumKey := cleanAlbum + "\x00" + cleanArtist
		if existing := s.FindCachedCoverURL(albumKey); existing != "" {
			return existing, nil
		}
	}

	if s.isRecentlyFailed(cacheKey) {
		return "", fmt.Errorf("búsqueda previa de pista sin resultados recientes")
	}

	// 1. Si pertenece a un álbum específico, probar primero búsqueda por álbum
	if !isGenericTitle(cleanAlbum) && !isGenericTitle(cleanArtist) {
		if coverURL, err := s.FetchAndCacheAlbumCover(ctx, cleanAlbum, cleanArtist); err == nil && coverURL != "" {
			return coverURL, nil
		}
	}

	// 2. Búsqueda directa por canción en iTunes (artista + título)
	searchQuery := cleanTitle
	if !isGenericTitle(cleanArtist) {
		searchQuery = cleanArtist + " " + cleanTitle
	}

	if data, ext, err := s.searchITunesCover(ctx, searchQuery, true); err == nil && len(data) > 0 {
		return s.saveCoverFile(data, ext, cacheKey)
	}

	// 3. Búsqueda en Deezer (artista + título)
	if data, ext, err := s.searchDeezerCover(ctx, searchQuery); err == nil && len(data) > 0 {
		return s.saveCoverFile(data, ext, cacheKey)
	}

	// 4. Si el título tenía detalles adicionales, probar con los dos primeros términos clave
	words := strings.Fields(cleanTitle)
	if len(words) > 3 && !isGenericTitle(cleanArtist) {
		shortQuery := cleanArtist + " " + strings.Join(words[:3], " ")
		if data, ext, err := s.searchITunesCover(ctx, shortQuery, true); err == nil && len(data) > 0 {
			return s.saveCoverFile(data, ext, cacheKey)
		}
	}

	s.markFailed(cacheKey)
	return "", fmt.Errorf("no se encontró carátula para la canción %q de %q", title, artist)
}
