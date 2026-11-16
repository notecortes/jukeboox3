package podcasts

import "time"

// Podcast modela una suscripción a un canal o feed RSS de podcast.
type Podcast struct {
	ID            int64     `json:"id"`
	Title         string    `json:"title"`
	Author        string    `json:"author"`
	FeedURL       string    `json:"feed_url"`
	Description   string    `json:"description"`
	CoverURL      string    `json:"cover_url"`
	Link          string    `json:"link"`
	LastBuildDate string    `json:"last_build_date"`
	EpisodeCount  int       `json:"episode_count"`
	CreatedAt     time.Time `json:"created_at"`
}

// Episode modela un capítulo o emisión individual con su archivo multimedia asociado.
type Episode struct {
	ID            int64     `json:"id"`
	PodcastID     int64     `json:"podcast_id"`
	PodcastTitle  string    `json:"podcast_title,omitempty"`
	PodcastCover  string    `json:"podcast_cover,omitempty"`
	GUID          string    `json:"guid"`
	Title         string    `json:"title"`
	Description   string    `json:"description"`
	EnclosureURL  string    `json:"enclosure_url"`
	EnclosureType string    `json:"enclosure_type"`
	Duration      float64   `json:"duration"` // Duración en segundos
	PubDate       string    `json:"pub_date"`
	Played        bool      `json:"played"`
	Progress      float64   `json:"progress"` // Segundos reproducidos
	CreatedAt     time.Time `json:"created_at"`
}

// SearchResult modela un podcast descubierto en directorios abiertos de internet.
type SearchResult struct {
	Title       string   `json:"title"`
	Author      string   `json:"author"`
	FeedURL     string   `json:"feed_url"`
	CoverURL    string   `json:"cover_url"`
	Description string   `json:"description"`
	TrackCount  int      `json:"track_count"`
	Genres      []string `json:"genres,omitempty"`
}

// OPMLOutline modela una entrada individual dentro de la jerarquía de un archivo OPML.
type OPMLOutline struct {
	Title   string `json:"title"`
	Text    string `json:"text"`
	Type    string `json:"type"`
	XMLURL  string `json:"xml_url"`
	HTMLURL string `json:"html_url"`
}
