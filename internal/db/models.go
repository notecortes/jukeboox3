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

