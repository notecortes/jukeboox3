package mpd

import (
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"
)

const maxPlaylistBodyBytes = 64 * 1024

var radioPlaylistHTTPClient = &http.Client{
	Timeout: 4 * time.Second,
}

// ValidateHTTPStreamURL valida que una URL de radio use http:// o https:// y tenga un host válido.
func ValidateHTTPStreamURL(rawURL string) (string, error) {
	clean := strings.TrimSpace(rawURL)
	if clean == "" || strings.ContainsAny(clean, "\r\n") {
		return "", fmt.Errorf("URL de radio inválida")
	}
	parsed, err := url.ParseRequestURI(clean)
	if err != nil {
		return "", fmt.Errorf("URL de radio malformada: %w", err)
	}
	scheme := strings.ToLower(parsed.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("la URL de radio debe usar protocolo http:// o https://")
	}
	if parsed.Host == "" {
		return "", fmt.Errorf("la URL de radio debe incluir un servidor válido")
	}
	return clean, nil
}

// ResolveStreamURLs resuelve listas .pls/.m3u en URLs directas reproducibles por MPD manteniendo intactos los streams y .m3u8.
func ResolveStreamURLs(rawURL string) ([]string, error) {
	clean, err := ValidateHTTPStreamURL(rawURL)
	if err != nil {
		return nil, err
	}

	parsed, err := url.Parse(clean)
	if err != nil {
		return nil, err
	}
	if !isPlaylistCandidateURL(parsed) {
		return []string{clean}, nil
	}

	req, err := http.NewRequest(http.MethodGet, clean, nil)
	if err != nil {
		return nil, fmt.Errorf("error preparando petición de lista de radio: %w", err)
	}
	req.Header.Set("User-Agent", "AuraBoxJukebox/1.0")
	req.Header.Set("Accept", "audio/x-scpls, audio/x-mpegurl, application/x-mpegURL, text/plain, */*")

	resp, err := radioPlaylistHTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("no se pudo descargar la lista de radio: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= http.StatusBadRequest {
		return nil, fmt.Errorf("la lista de radio devolvió HTTP %d", resp.StatusCode)
	}

	contentType := strings.ToLower(strings.TrimSpace(strings.Split(resp.Header.Get("Content-Type"), ";")[0]))
	if isDirectMediaContentType(contentType) {
		return []string{clean}, nil
	}

	rawBody, err := io.ReadAll(io.LimitReader(resp.Body, maxPlaylistBodyBytes))
	if err != nil {
		return nil, fmt.Errorf("error leyendo contenido de la lista de radio: %w", err)
	}
	body := string(rawBody)

	// Si la respuesta es un manifiesto HLS (.m3u8 o #EXT-X-*), pasarlo directo al plugin ffmpeg de MPD
	if strings.Contains(strings.ToUpper(body), "#EXT-X-") {
		return []string{clean}, nil
	}

	extHint := strings.ToLower(path.Ext(parsed.Path))
	resolved := parsePlaylistBody(body, parsed, extHint)
	if len(resolved) == 0 {
		return nil, fmt.Errorf("la lista %s no contiene URLs de streaming válidas", extHint)
	}
	return resolved, nil
}

// isDirectMediaContentType indica si un Content-Type HTTP corresponde a audio/vídeo directo y no a una lista .pls/.m3u.
func isDirectMediaContentType(contentType string) bool {
	switch contentType {
	case "audio/x-scpls", "audio/scpls", "audio/x-mpegurl", "audio/mpegurl", "audio/m3u", "audio/x-m3u", "audio/x-pn-realaudio":
		return false
	}
	return strings.HasPrefix(contentType, "audio/") ||
		strings.HasPrefix(contentType, "video/") ||
		contentType == "application/ogg"
}

// isPlaylistCandidateURL indica si una URL apunta a una lista .pls o .m3u clásica (excluyendo HLS .m3u8).
func isPlaylistCandidateURL(u *url.URL) bool {
	if u == nil {
		return false
	}
	ext := strings.ToLower(path.Ext(strings.TrimSpace(u.Path)))
	return ext == ".pls" || ext == ".m3u"
}

// parsePlaylistBody extrae URLs de streaming desde texto PLS o M3U conservando el orden original.
func parsePlaylistBody(body string, base *url.URL, extHint string) []string {
	upper := strings.ToUpper(body)
	if extHint == ".pls" || strings.Contains(upper, "[PLAYLIST]") {
		if urls := parsePLSBody(body, base); len(urls) > 0 {
			return urls
		}
	}
	return parseM3UBody(body, base)
}

// parsePLSBody analiza secciones FileN=URL propias de archivos de lista .pls.
func parsePLSBody(body string, base *url.URL) []string {
	lines := strings.Split(body, "\n")
	seen := make(map[string]struct{})
	urls := make([]string, 0, 4)

	for _, line := range lines {
		trimmed := strings.TrimSpace(strings.TrimPrefix(line, "\ufeff"))
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
			continue
		}
		eqIdx := strings.IndexByte(trimmed, '=')
		if eqIdx <= 0 {
			continue
		}
		key := strings.ToLower(strings.TrimSpace(trimmed[:eqIdx]))
		if !strings.HasPrefix(key, "file") {
			continue
		}
		candidate := resolvePlaylistEntryURL(trimmed[eqIdx+1:], base)
		if candidate == "" {
			continue
		}
		if _, exists := seen[candidate]; !exists {
			seen[candidate] = struct{}{}
			urls = append(urls, candidate)
		}
	}
	return urls
}

// parseM3UBody analiza líneas de URLs de streaming dentro de un archivo .m3u tradicional.
func parseM3UBody(body string, base *url.URL) []string {
	lines := strings.Split(body, "\n")
	seen := make(map[string]struct{})
	urls := make([]string, 0, 4)

	for _, line := range lines {
		trimmed := strings.TrimSpace(strings.TrimPrefix(line, "\ufeff"))
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		candidate := resolvePlaylistEntryURL(trimmed, base)
		if candidate == "" {
			continue
		}
		if _, exists := seen[candidate]; !exists {
			seen[candidate] = struct{}{}
			urls = append(urls, candidate)
		}
	}
	return urls
}

// resolvePlaylistEntryURL normaliza una entrada de lista respecto a su URL base permitiendo solo http/https.
func resolvePlaylistEntryURL(rawEntry string, base *url.URL) string {
	trimmed := strings.TrimSpace(rawEntry)
	if trimmed == "" {
		return ""
	}
	ref, err := url.Parse(trimmed)
	if err != nil {
		return ""
	}
	if !ref.IsAbs() && base != nil {
		ref = base.ResolveReference(ref)
	}
	scheme := strings.ToLower(ref.Scheme)
	if (scheme != "http" && scheme != "https") || ref.Host == "" {
		return ""
	}
	return ref.String()
}

// cleanStreamTitle filtra etiquetas genéricas ICY y formatea "Artista - Canción" si ambos metadatos existen.
func cleanStreamTitle(icyTitle, icyArtist, stationTitle string) string {
	title := strings.TrimSpace(icyTitle)
	artist := strings.TrimSpace(icyArtist)

	if artist != "" && title != "" && !strings.Contains(strings.ToLower(title), strings.ToLower(artist)) {
		title = artist + " - " + title
	} else if title == "" && artist != "" {
		title = artist
	}

	title = strings.TrimSpace(strings.Trim(title, "-• "))
	if title == "" {
		return ""
	}

	lower := strings.ToLower(title)
	if lower == "no name" || lower == "unknown" || strings.HasPrefix(lower, "http://") || strings.HasPrefix(lower, "https://") {
		return ""
	}
	if stationTitle != "" && strings.EqualFold(title, strings.TrimSpace(stationTitle)) {
		return ""
	}
	return title
}

// FormatRadioArtist construye una línea legible de emisora y país para mostrar como subtítulo de radio.
func FormatRadioArtist(broadcaster, country string) string {
	b := strings.TrimSpace(broadcaster)
	c := strings.TrimSpace(country)
	if b != "" && c != "" {
		return b + " (" + c + ")"
	}
	if b != "" {
		return b
	}
	if c != "" {
		return c
	}
	return "Radio en Directo"
}

