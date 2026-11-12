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
	"time"
)

const (
	defaultMusicBrainzURL = "https://musicbrainz.org"
	defaultCoverArtURL    = "https://coverartarchive.org"
	maxOnlineCoverBytes   = 2 << 20 // 2 MB límite máximo de carátula
	auraboxUserAgent      = "AuraBox/1.0 (https://github.com/aurabox)"
)

var reAlbumTagNoise = regexp.MustCompile(`(?i)\[(?:FLAC|PMEDIA|Lossless|Hunter|FRG|320kbps|24Bit[^\]]*|[^\]]*)\]|\((?:19|20)\d{2}[^)]*\)`)

// mbSearchResponse modela la respuesta JSON de búsqueda de release-groups en MusicBrainz.
type mbSearchResponse struct {
	ReleaseGroups []struct {
		ID    string `json:"id"`
		Title string `json:"title"`
		Score int    `json:"score"`
	} `json:"release-groups"`
}

// Service gestiona la búsqueda y descarga segura de carátulas desde MusicBrainz y Cover Art Archive.
type Service struct {
	coversDir  string
	mbBaseURL  string
	caaBaseURL string
	httpClient *http.Client
}

// NewService crea un servicio de descarga de carátulas apuntando a MusicBrainz y Cover Art Archive.
func NewService(coversDir string) *Service {
	client := &http.Client{
		Timeout: 10 * time.Second,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			if len(via) >= 5 {
				return fmt.Errorf("demasiadas redirecciones al descargar carátula")
			}
			scheme := strings.ToLower(req.URL.Scheme)
			if scheme != "http" && scheme != "https" {
				return fmt.Errorf("esquema de redirección no permitido: %s", scheme)
			}
			return nil
		},
	}
	return &Service{
		coversDir:  coversDir,
		mbBaseURL:  defaultMusicBrainzURL,
		caaBaseURL: defaultCoverArtURL,
		httpClient: client,
	}
}

// NewServiceWithEndpoints crea una instancia configurable para pruebas unitarias aisladas sin acceso a internet.
func NewServiceWithEndpoints(coversDir, mbBaseURL, caaBaseURL string, client *http.Client) *Service {
	if client == nil {
		client = NewService(coversDir).httpClient
	}
	return &Service{
		coversDir:  coversDir,
		mbBaseURL:  strings.TrimRight(mbBaseURL, "/"),
		caaBaseURL: strings.TrimRight(caaBaseURL, "/"),
		httpClient: client,
	}
}

// cleanAlbumQuery elimina etiquetas de ripeo o año entre paréntesis para mejorar la coincidencia en MusicBrainz.
func cleanAlbumQuery(raw string) string {
	cleaned := reAlbumTagNoise.ReplaceAllString(raw, " ")
	cleaned = strings.ReplaceAll(cleaned, "⭐️", " ")
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
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, imgURL, nil)
		if err != nil {
			continue
		}
		req.Header.Set("User-Agent", auraboxUserAgent)
		resp, err := s.httpClient.Do(req)
		if err != nil {
			continue
		}

		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			continue
		}

		data, readErr := io.ReadAll(io.LimitReader(resp.Body, maxOnlineCoverBytes+1))
		_ = resp.Body.Close()
		if readErr != nil || len(data) == 0 || len(data) > maxOnlineCoverBytes {
			continue
		}

		ext, valid := detectOnlineImageExt(data)
		if valid {
			return data, ext, nil
		}
	}

	return nil, "", fmt.Errorf("sin carátula disponible en Cover Art Archive para %s", mbid)
}

// FetchAndCacheAlbumCover busca la portada del álbum en MusicBrainz/Cover Art Archive y la guarda en caché local.
func (s *Service) FetchAndCacheAlbumCover(ctx context.Context, albumTitle, artistName string) (string, error) {
	if s.coversDir == "" {
		return "", fmt.Errorf("directorio de carátulas no configurado")
	}

	cleanAlbum := sanitizeLuceneTerm(cleanAlbumQuery(albumTitle))
	if cleanAlbum == "" {
		cleanAlbum = sanitizeLuceneTerm(albumTitle)
	}
	if cleanAlbum == "" {
		return "", fmt.Errorf("título de álbum vacío")
	}

	cleanArtist := sanitizeLuceneTerm(artistName)
	skipArtist := cleanArtist == "" ||
		strings.EqualFold(cleanArtist, "Varios Artistas") ||
		strings.EqualFold(cleanArtist, "Various Artists") ||
		strings.EqualFold(cleanArtist, "Artista Desconocido")

	var mbids []string
	if !skipArtist {
		query := fmt.Sprintf(`releasegroup:"%s" AND artist:"%s"`, cleanAlbum, cleanArtist)
		if ids, err := s.searchReleaseGroupIDs(ctx, query); err == nil {
			mbids = ids
		}
	}
	if len(mbids) == 0 {
		query := fmt.Sprintf(`releasegroup:"%s"`, cleanAlbum)
		ids, err := s.searchReleaseGroupIDs(ctx, query)
		if err != nil {
			return "", fmt.Errorf("error consultando MusicBrainz: %w", err)
		}
		mbids = ids
	}

	if len(mbids) == 0 {
		return "", fmt.Errorf("no se encontraron coincidencias en MusicBrainz para %q", albumTitle)
	}

	var imgBytes []byte
	var imgExt string
	for _, mbid := range mbids {
		data, ext, dlErr := s.downloadFrontImage(ctx, mbid)
		if dlErr == nil && len(data) > 0 {
			imgBytes = data
			imgExt = ext
			break
		}
	}
	if len(imgBytes) == 0 || imgExt == "" {
		return "", fmt.Errorf("Cover Art Archive no devolvió portada válida para %q", albumTitle)
	}

	if err := os.MkdirAll(s.coversDir, 0755); err != nil {
		return "", err
	}

	cacheKey := strings.ToLower(cleanAlbum) + "\x00" + strings.ToLower(cleanArtist)
	hash := md5.Sum([]byte(cacheKey))
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

