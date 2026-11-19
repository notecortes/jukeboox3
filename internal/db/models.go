package db

import "time"

// Track representa una pista de audio en la biblioteca local o en red.
type Track struct {
	ID          int64     `json:"id"`
	Path        string    `json:"path"`
	Filename    string    `json:"filename"`
	Title       string    `json:"title"`
	Artist      string    `json:"artist"`
	Album       string    `json:"album"`
	AlbumArtist string    `json:"album_artist"`
	Genre       string    `json:"genre"`
	Year        int       `json:"year"`
	Duration    float64   `json:"duration"` // en segundos
	TrackNumber int       `json:"track_number"`
	Bitrate     int       `json:"bitrate"`     // en kbps
	SampleRate  int       `json:"sample_rate"` // en Hz (ej. 44100, 96000)
	Format      string    `json:"format"`      // FLAC, MP3, etc.
	Source      string    `json:"source"`      // local, nas, usb
	CoverURL    string    `json:"cover_url"`
	Favorite    bool      `json:"favorite"`
	PlayCount   int       `json:"play_count"`
	LastPlayed  time.Time `json:"last_played"`
	BPM         float64   `json:"bpm"`
	CreatedAt   time.Time `json:"created_at"`
}

// Album representa una colección discográfica agrupada por título y artista.
type Album struct {
	ID            int64   `json:"id"`
	Title         string  `json:"title"`
	Artist        string  `json:"artist"`
	Year          int     `json:"year"`
	Genre         string  `json:"genre"`
	CoverURL      string  `json:"cover_url"`
	TrackCount    int     `json:"track_count"`
	TotalDuration float64 `json:"total_duration"`
	Source        string  `json:"source"`
	AddedSeq      int64   `json:"added_seq,omitempty"`
}

// ThemeOption representa un género o década disponible para generar mixes temáticos offline.
type ThemeOption struct {
	Type       string `json:"type"`
	Value      string `json:"value"`
	Label      string `json:"label"`
	TrackCount int    `json:"track_count"`
}

// CategorySummary resume un género musical o década navegable en Biblioteca con conteo de discos y carátula.
type CategorySummary struct {
	Type       string `json:"type"`
	Value      string `json:"value"`
	Label      string `json:"label"`
	AlbumCount int    `json:"album_count"`
	TrackCount int    `json:"track_count"`
	CoverURL   string `json:"cover_url"`
}

// Artist representa a un músico o banda en la biblioteca de AuraBox.
type Artist struct {
	ID          int64    `json:"id"`
	Name        string   `json:"name"`
	Bio         string   `json:"bio"`
	ImageURL    string   `json:"image_url"`
	Genre       string   `json:"genre"`
	TrackCount  int      `json:"track_count"`
	AlbumCount  int      `json:"album_count"`
	SimilarList []string `json:"similar_list,omitempty"`
}

// RadioStation representa una emisora de radio por internet configurable.
type RadioStation struct {
	ID          int64  `json:"id"`
	Name        string `json:"name"`
	StreamURL   string `json:"stream_url"`
	LogoURL     string `json:"logo_url"`
	Genre       string `json:"genre"`
	Broadcaster string `json:"broadcaster"`
	Country     string `json:"country"`
	Bitrate     string `json:"bitrate"`
	Format      string `json:"format"`
	IsDefault   bool   `json:"is_default"`
	Favorite    bool   `json:"favorite"`
}

// AppSetting almacena pares clave-valor de configuración persistente.
type AppSetting struct {
	Key       string    `json:"key"`
	Value     string    `json:"value"`
	UpdatedAt time.Time `json:"updated_at"`
}

// PlaybackHistory registra una reproducción para estadísticas y favoritos.
type PlaybackHistory struct {
	ID        int64     `json:"id"`
	TrackID   int64     `json:"track_id"`
	PlayedAt  time.Time `json:"played_at"`
	Completed bool      `json:"completed"`
}

// Podcast representa un programa o feed RSS de podcast suscrito en AuraBox.
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
	Favorite      bool      `json:"favorite"`
	CreatedAt     time.Time `json:"created_at"`
}

// PodcastEpisode representa un capítulo de audio descargable o reproducible de un podcast.
type PodcastEpisode struct {
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
	Favorite      bool      `json:"favorite"`
	CreatedAt     time.Time `json:"created_at"`
}

// Playlist representa una lista de reproducción personalizada creada por el usuario.
type Playlist struct {
	ID          int64     `json:"id"`
	Name        string    `json:"name"`
	Description string    `json:"description"`
	TrackCount  int       `json:"track_count"`
	Duration    float64   `json:"duration"` // Duración total acumulada en segundos
	CoverURL    string    `json:"cover_url,omitempty"` // Carátula de la primera pista o fallback
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

// PlaylistDetail engloba los metadatos de la lista junto con todas sus pistas ordenadas por posición.
type PlaylistDetail struct {
	Playlist
	Tracks []Track `json:"tracks"`
}



