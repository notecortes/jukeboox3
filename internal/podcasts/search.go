package podcasts

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

const itunesPodcastSearchURL = "https://itunes.apple.com/search"

// itunesPodcastSearchResponse modela la respuesta JSON de iTunes Search API al buscar podcasts.
type itunesPodcastSearchResponse struct {
	ResultCount int `json:"resultCount"`
	Results     []struct {
		CollectionName string   `json:"collectionName"`
		ArtistName     string   `json:"artistName"`
		FeedURL        string   `json:"feedUrl"`
		ArtworkURL100  string   `json:"artworkUrl100"`
		ArtworkURL600  string   `json:"artworkUrl600"`
		TrackCount     int      `json:"trackCount"`
		Genres         []string `json:"genres"`
	} `json:"results"`
}

// SearchOnline busca programas de podcast en el directorio abierto de iTunes/Apple Podcasts.
func SearchOnline(ctx context.Context, client *http.Client, query string, limit int) ([]SearchResult, error) {
	cleanQuery := strings.TrimSpace(query)
	if cleanQuery == "" {
		return []SearchResult{}, nil
	}

	if limit <= 0 || limit > 50 {
		limit = 25
	}

	if client == nil {
		client = &http.Client{Timeout: 10 * time.Second}
	}

	reqURL := fmt.Sprintf("%s?media=podcast&term=%s&limit=%d", itunesPodcastSearchURL, url.QueryEscape(cleanQuery), limit)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, reqURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "AuraBox/1.0 PodcastClient (https://github.com/aurabox)")
	req.Header.Set("Accept", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("error al conectar con el directorio de podcasts: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("el directorio de podcasts respondió con código HTTP %d", resp.StatusCode)
	}

	var parsed itunesPodcastSearchResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&parsed); err != nil {
		return nil, fmt.Errorf("error al descodificar resultados de podcast: %w", err)
	}

	results := make([]SearchResult, 0, len(parsed.Results))
	for _, item := range parsed.Results {
		feed := strings.TrimSpace(item.FeedURL)
		if feed == "" {
			continue
		}

		cover := strings.TrimSpace(item.ArtworkURL600)
		if cover == "" && item.ArtworkURL100 != "" {
			cover = strings.Replace(item.ArtworkURL100, "100x100bb.jpg", "600x600bb.jpg", 1)
		}

		results = append(results, SearchResult{
			Title:      strings.TrimSpace(item.CollectionName),
			Author:     strings.TrimSpace(item.ArtistName),
			FeedURL:    feed,
			CoverURL:   cover,
			TrackCount: item.TrackCount,
			Genres:     item.Genres,
		})
	}

	return results, nil
}
