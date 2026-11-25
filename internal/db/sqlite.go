package db

import (
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

//go:embed default_radios.json
var embeddedRadiosJSON []byte

// DB gestiona las conexiones SQLite separando un escritor único serializado y un pool concurrente de lectura en WAL.
type DB struct {
	conn     *sql.DB
	readConn *sql.DB
	writeMu  sync.Mutex
	hasFTS5  bool
}

// isMemorySQLiteDSN detecta si la ruta de SQLite apunta a una base de datos efímera en memoria.
func isMemorySQLiteDSN(dbPath string) bool {
	return dbPath == ":memory:" || strings.Contains(dbPath, ":memory:") || strings.Contains(dbPath, "mode=memory")
}

// reader devuelve el pool de solo lectura (o la conexión compartida en modo :memory:) sin bloquear escrituras WAL.
func (d *DB) reader() *sql.DB {
	if d.readConn != nil {
		return d.readConn
	}
	return d.conn
}

// InitDB inicializa la conexión con SQLite, activa WAL y crea las tablas necesarias.
func InitDB(dbPath string) (*DB, error) {
	isMemory := isMemorySQLiteDSN(dbPath)
	if !isMemory {
		// Asegurar directorio padre para bases de datos en disco
		dir := filepath.Dir(dbPath)
		if err := os.MkdirAll(dir, 0755); err != nil {
			return nil, fmt.Errorf("error al crear directorio para db: %w", err)
		}
	}

	dsn := fmt.Sprintf("%s?_journal_mode=WAL&_busy_timeout=10000&_synchronous=NORMAL&_cache_size=-16000&_foreign_keys=ON", dbPath)
	if isMemory {
		dsn = dbPath
	}
	conn, err := sql.Open("sqlite3", dsn)
	if err != nil {
		return nil, fmt.Errorf("error al abrir SQLite: %w", err)
	}

	// Limitar conexión de escritura a 1 para evitar SQLITE_BUSY entre escritores
	conn.SetMaxOpenConns(1)
	conn.SetMaxIdleConns(1)
	conn.SetConnMaxLifetime(time.Hour)

	// Pragmas obligatorios para máxima velocidad, bajo consumo de RAM y resistencia a caídas
	pragmas := []string{
		"PRAGMA journal_mode = WAL;",
		"PRAGMA synchronous = NORMAL;",
		"PRAGMA busy_timeout = 10000;",
		"PRAGMA cache_size = -16000;", // 16MB de memoria caché controlada (ideal para Raspberry Pi)
		"PRAGMA mmap_size = 33554432;", // 32MB de lectura en memoria virtual sin copia de kernel
		"PRAGMA temp_store = MEMORY;",
		"PRAGMA wal_autocheckpoint = 500;",
	}
	for _, p := range pragmas {
		_, _ = conn.Exec(p)
	}

	var readConn *sql.DB
	if isMemory {
		readConn = conn
	} else {
		readDSN := fmt.Sprintf("%s?_journal_mode=WAL&_busy_timeout=10000&_synchronous=NORMAL&_cache_size=-8000&_foreign_keys=ON", dbPath)
		readConn, err = sql.Open("sqlite3", readDSN)
		if err != nil {
			conn.Close()
			return nil, fmt.Errorf("error al abrir pool de lectura SQLite: %w", err)
		}
		// Pool concurrente de lectura para que SELECT nunca espere a escaneos o checkpoints WAL
		readConn.SetMaxOpenConns(4)
		readConn.SetMaxIdleConns(4)
		readConn.SetConnMaxLifetime(time.Hour)
		for _, p := range []string{
			"PRAGMA busy_timeout = 10000;",
			"PRAGMA cache_size = -8000;",
			"PRAGMA mmap_size = 33554432;",
			"PRAGMA temp_store = MEMORY;",
		} {
			_, _ = readConn.Exec(p)
		}
	}

	db := &DB{conn: conn, readConn: readConn}
	if err := db.createSchema(); err != nil {
		db.Close()
		return nil, fmt.Errorf("error al crear esquema: %w", err)
	}

	if err := db.seedDefaultRadios(); err != nil {
		log.Printf("[DB] Advertencia al sembrar radios: %v", err)
	}

	go db.syncMissingTrackCoversFromAlbums()

	return db, nil
}

// createSchema define las tablas e índices optimizados para búsquedas rápidas.
func (d *DB) createSchema() error {
	schema := `
	CREATE TABLE IF NOT EXISTS tracks (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		path TEXT UNIQUE NOT NULL,
		filename TEXT NOT NULL,
		title TEXT NOT NULL,
		artist TEXT NOT NULL,
		album TEXT NOT NULL,
		album_artist TEXT,
		genre TEXT,
		year INTEGER DEFAULT 0,
		duration REAL DEFAULT 0,
		track_number INTEGER DEFAULT 0,
		bitrate INTEGER DEFAULT 0,
		sample_rate INTEGER DEFAULT 44100,
		format TEXT DEFAULT 'MP3',
		source TEXT DEFAULT 'local',
		cover_url TEXT DEFAULT '',
		favorite INTEGER DEFAULT 0,
		play_count INTEGER DEFAULT 0,
		last_played DATETIME,
		bpm REAL DEFAULT 120,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE INDEX IF NOT EXISTS idx_tracks_artist ON tracks(artist);
	CREATE INDEX IF NOT EXISTS idx_tracks_artist_nocase ON tracks(artist COLLATE NOCASE);
	CREATE INDEX IF NOT EXISTS idx_tracks_album ON tracks(album);
	CREATE INDEX IF NOT EXISTS idx_tracks_album_nocase ON tracks(album COLLATE NOCASE);
	CREATE INDEX IF NOT EXISTS idx_tracks_album_artist ON tracks(album, artist);
	CREATE INDEX IF NOT EXISTS idx_tracks_title ON tracks(title COLLATE NOCASE);
	CREATE INDEX IF NOT EXISTS idx_tracks_genre ON tracks(genre);
	CREATE INDEX IF NOT EXISTS idx_tracks_favorite ON tracks(favorite);
	CREATE INDEX IF NOT EXISTS idx_tracks_created ON tracks(created_at DESC);

	CREATE TABLE IF NOT EXISTS artists (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT UNIQUE NOT NULL,
		bio TEXT DEFAULT '',
		image_url TEXT DEFAULT '',
		genre TEXT DEFAULT ''
	);

	CREATE INDEX IF NOT EXISTS idx_artists_name ON artists(name COLLATE NOCASE);

	CREATE TABLE IF NOT EXISTS albums (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		title TEXT NOT NULL,
		artist TEXT NOT NULL,
		year INTEGER DEFAULT 0,
		genre TEXT DEFAULT '',
		cover_url TEXT DEFAULT '',
		source TEXT DEFAULT 'local',
		UNIQUE(title, artist)
	);

	CREATE INDEX IF NOT EXISTS idx_albums_artist ON albums(artist);
	CREATE INDEX IF NOT EXISTS idx_albums_year ON albums(year DESC);
	CREATE INDEX IF NOT EXISTS idx_albums_title ON albums(title COLLATE NOCASE);

	CREATE TABLE IF NOT EXISTS playback_history (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		track_id INTEGER NOT NULL,
		played_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		completed INTEGER DEFAULT 1,
		FOREIGN KEY(track_id) REFERENCES tracks(id) ON DELETE CASCADE
	);

	CREATE TABLE IF NOT EXISTS radio_stations (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL,
		stream_url TEXT UNIQUE NOT NULL,
		logo_url TEXT DEFAULT '',
		genre TEXT DEFAULT 'General',
		broadcaster TEXT DEFAULT '',
		country TEXT DEFAULT '',
		bitrate TEXT DEFAULT '128',
		format TEXT DEFAULT 'MP3',
		is_default INTEGER DEFAULT 0
	);

	CREATE INDEX IF NOT EXISTS idx_radio_stations_genre ON radio_stations(genre);
	CREATE INDEX IF NOT EXISTS idx_radio_stations_name ON radio_stations(name COLLATE NOCASE);

	CREATE TABLE IF NOT EXISTS app_settings (
		key TEXT PRIMARY KEY,
		value TEXT NOT NULL,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE TABLE IF NOT EXISTS podcasts (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		title TEXT NOT NULL,
		author TEXT DEFAULT '',
		feed_url TEXT UNIQUE NOT NULL,
		description TEXT DEFAULT '',
		cover_url TEXT DEFAULT '',
		link TEXT DEFAULT '',
		last_build_date TEXT DEFAULT '',
		favorite INTEGER DEFAULT 0,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE INDEX IF NOT EXISTS idx_podcasts_title ON podcasts(title COLLATE NOCASE);

	CREATE TABLE IF NOT EXISTS podcast_episodes (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		podcast_id INTEGER NOT NULL,
		guid TEXT NOT NULL,
		title TEXT NOT NULL,
		description TEXT DEFAULT '',
		enclosure_url TEXT NOT NULL,
		enclosure_type TEXT DEFAULT 'audio/mpeg',
		duration REAL DEFAULT 0,
		pub_date TEXT DEFAULT '',
		played INTEGER DEFAULT 0,
		progress REAL DEFAULT 0,
		favorite INTEGER DEFAULT 0,
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		FOREIGN KEY(podcast_id) REFERENCES podcasts(id) ON DELETE CASCADE,
		UNIQUE(podcast_id, guid)
	);

	CREATE INDEX IF NOT EXISTS idx_podcast_episodes_podcast_id ON podcast_episodes(podcast_id);
	CREATE INDEX IF NOT EXISTS idx_podcast_episodes_enclosure ON podcast_episodes(enclosure_url);

	CREATE TABLE IF NOT EXISTS playlists (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		name TEXT NOT NULL UNIQUE COLLATE NOCASE,
		description TEXT DEFAULT '',
		created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
	);

	CREATE INDEX IF NOT EXISTS idx_playlists_name ON playlists(name COLLATE NOCASE);
	CREATE INDEX IF NOT EXISTS idx_playlists_updated ON playlists(updated_at DESC);

	CREATE TABLE IF NOT EXISTS playlist_tracks (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		playlist_id INTEGER NOT NULL,
		track_id INTEGER NOT NULL,
		position INTEGER NOT NULL,
		added_at DATETIME DEFAULT CURRENT_TIMESTAMP,
		FOREIGN KEY(playlist_id) REFERENCES playlists(id) ON DELETE CASCADE,
		FOREIGN KEY(track_id) REFERENCES tracks(id) ON DELETE CASCADE,
		UNIQUE(playlist_id, position)
	);

	CREATE INDEX IF NOT EXISTS idx_playlist_tracks_pos ON playlist_tracks(playlist_id, position);
	CREATE INDEX IF NOT EXISTS idx_playlist_tracks_track ON playlist_tracks(track_id);

	CREATE TABLE IF NOT EXISTS lyrics_cache (
		cache_key TEXT PRIMARY KEY,
		found INTEGER NOT NULL DEFAULT 0,
		instrumental INTEGER NOT NULL DEFAULT 0,
		plain_lyrics TEXT DEFAULT '',
		synced_json TEXT DEFAULT '[]',
		source TEXT DEFAULT '',
		updated_at INTEGER NOT NULL DEFAULT 0
	);
	`
	if _, err := d.conn.Exec(schema); err != nil {
		return err
	}

	// Migraciones seguras para columnas agregadas dinámicamente
	_, _ = d.conn.Exec(`ALTER TABLE radio_stations ADD COLUMN broadcaster TEXT DEFAULT ''`)
	_, _ = d.conn.Exec(`ALTER TABLE radio_stations ADD COLUMN country TEXT DEFAULT ''`)
	_, _ = d.conn.Exec(`ALTER TABLE radio_stations ADD COLUMN format TEXT DEFAULT 'MP3'`)
	_, _ = d.conn.Exec(`ALTER TABLE radio_stations ADD COLUMN favorite INTEGER DEFAULT 0`)
	_, _ = d.conn.Exec(`CREATE INDEX IF NOT EXISTS idx_radio_stations_favorite ON radio_stations(favorite)`)

	_, _ = d.conn.Exec(`ALTER TABLE podcasts ADD COLUMN favorite INTEGER DEFAULT 0`)
	_, _ = d.conn.Exec(`CREATE INDEX IF NOT EXISTS idx_podcasts_favorite ON podcasts(favorite)`)
	_, _ = d.conn.Exec(`ALTER TABLE podcast_episodes ADD COLUMN favorite INTEGER DEFAULT 0`)
	_, _ = d.conn.Exec(`CREATE INDEX IF NOT EXISTS idx_podcast_episodes_favorite ON podcast_episodes(favorite)`)

	// Inicialización del motor de búsqueda SQLite FTS5 y triggers de sincronización
	ftsSchema := `
	CREATE VIRTUAL TABLE IF NOT EXISTS tracks_fts USING fts5(
		title,
		artist,
		album,
		genre,
		content='tracks',
		content_rowid='id'
	);

	CREATE TRIGGER IF NOT EXISTS tracks_ai AFTER INSERT ON tracks BEGIN
		INSERT INTO tracks_fts(rowid, title, artist, album, genre) VALUES (new.id, new.title, new.artist, new.album, new.genre);
	END;
	CREATE TRIGGER IF NOT EXISTS tracks_ad AFTER DELETE ON tracks BEGIN
		INSERT INTO tracks_fts(tracks_fts, rowid, title, artist, album, genre) VALUES('delete', old.id, old.title, old.artist, old.album, old.genre);
	END;
	CREATE TRIGGER IF NOT EXISTS tracks_au AFTER UPDATE ON tracks BEGIN
		INSERT INTO tracks_fts(tracks_fts, rowid, title, artist, album, genre) VALUES('delete', old.id, old.title, old.artist, old.album, old.genre);
		INSERT INTO tracks_fts(rowid, title, artist, album, genre) VALUES (new.id, new.title, new.artist, new.album, new.genre);
	END;
	`
	if _, ftsErr := d.conn.Exec(ftsSchema); ftsErr == nil {
		d.hasFTS5 = true
		var ftsCount int
		_ = d.conn.QueryRow("SELECT COUNT(*) FROM tracks_fts").Scan(&ftsCount)
		if ftsCount == 0 {
			_, _ = d.conn.Exec("INSERT OR IGNORE INTO tracks_fts(rowid, title, artist, album, genre) SELECT id, title, artist, album, genre FROM tracks")
		}
	} else {
		d.hasFTS5 = false
	}

	return nil
}

// seedDefaultRadios inserta o actualiza emisoras de radio web desde el catálogo embebido y archivos externos opcionales.
func (d *DB) seedDefaultRadios() error {
	seeded, err := d.upsertRadiosFromJSON(embeddedRadiosJSON, true)
	if err != nil {
		log.Printf("[DB] Error al cargar catálogo embebido de radios: %v", err)
	}

	// Incorporar emisoras extra definidas en archivos externos si existen (sin sobrescribir el catálogo embebido)
	extraCandidates := []string{
		"/app/data/custom_radios.json",
		"data/custom_radios.json",
		"/app/data/default_radios.json",
		"data/default_radios.json",
	}
	for _, p := range extraCandidates {
		if b, readErr := os.ReadFile(p); readErr == nil && len(b) > 0 {
			extra, _ := d.upsertRadiosFromJSON(b, false)
			seeded += extra
		}
	}

	if seeded > 0 {
		total := seeded
		_ = d.conn.QueryRow(`SELECT COUNT(*) FROM radio_stations`).Scan(&total)
		log.Printf("[DB] %d emisoras de radio web indexadas correctamente", total)
		return nil
	}

	// Fallback mínimo en caso extremo de catálogo vacío
	defaultRadios := []RadioStation{
		{
			Name:        "Radio Paradise - Main Mix",
			StreamURL:   "https://stream.radioparadise.com/flacm",
			Genre:       "Eclectic",
			Broadcaster: "Radio Paradise",
			Country:     "United States",
			Bitrate:     "900",
			Format:      "FLAC",
			IsDefault:   true,
		},
		{
			Name:        "RNE Radio Nacional",
			StreamURL:   "https://dispatcher.rndfnk.com/crtve/rne1/main/mp3/high",
			Genre:       "General, News, Talk",
			Broadcaster: "RNE",
			Country:     "Spain",
			Bitrate:     "128",
			Format:      "MP3",
			IsDefault:   true,
		},
	}

	for _, r := range defaultRadios {
		_, _ = d.conn.Exec(`
			INSERT OR IGNORE INTO radio_stations (name, stream_url, logo_url, genre, broadcaster, country, bitrate, format, is_default)
			VALUES (?, ?, '', ?, ?, ?, ?, ?, 1)
		`, r.Name, r.StreamURL, r.Genre, r.Broadcaster, r.Country, r.Bitrate, r.Format)
	}
	return nil
}

// upsertRadiosFromJSON decodifica un array JSON de emisoras e inserta o actualiza las entradas válidas en SQLite.
func (d *DB) upsertRadiosFromJSON(jsonBytes []byte, updateOnConflict bool) (int, error) {
	if len(jsonBytes) == 0 {
		return 0, nil
	}
	type jsonRadio struct {
		ID          int         `json:"id"`
		Name        string      `json:"name"`
		URL         string      `json:"url"`
		StreamURL   string      `json:"stream_url"`
		LogoURL     string      `json:"logo_url"`
		Genre       string      `json:"genre"`
		Broadcaster string      `json:"broadcaster"`
		Country     string      `json:"country"`
		Bitrate     interface{} `json:"bitrate"`
		Format      string      `json:"format"`
	}

	var radios []jsonRadio
	if err := json.Unmarshal(jsonBytes, &radios); err != nil {
		return 0, err
	}
	if len(radios) == 0 {
		return 0, nil
	}

	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	tx, err := d.conn.Begin()
	if err != nil {
		return 0, err
	}

	query := `
		INSERT OR IGNORE INTO radio_stations (name, stream_url, logo_url, genre, broadcaster, country, bitrate, format, is_default)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1)
	`
	if updateOnConflict {
		query = `
			INSERT INTO radio_stations (name, stream_url, logo_url, genre, broadcaster, country, bitrate, format, is_default)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, 1)
			ON CONFLICT(stream_url) DO UPDATE SET
				name=excluded.name,
				logo_url=CASE WHEN excluded.logo_url != '' THEN excluded.logo_url ELSE radio_stations.logo_url END,
				genre=excluded.genre,
				broadcaster=excluded.broadcaster,
				country=excluded.country,
				bitrate=excluded.bitrate,
				format=excluded.format
		`
	}

	stmt, err := tx.Prepare(query)
	if err != nil {
		_ = tx.Rollback()
		return 0, err
	}
	defer stmt.Close()

	count := 0
	for _, r := range radios {
		streamURL := strings.TrimSpace(r.URL)
		if streamURL == "" {
			streamURL = strings.TrimSpace(r.StreamURL)
		}
		name := strings.TrimSpace(r.Name)
		if name == "" || (!strings.HasPrefix(streamURL, "http://") && !strings.HasPrefix(streamURL, "https://")) {
			continue
		}
		if strings.Contains(strings.ToLower(name), "reserved") {
			continue
		}

		bitrateStr := fmt.Sprintf("%v", r.Bitrate)
		if bitrateStr == "" || bitrateStr == "<nil>" || bitrateStr == "0" {
			bitrateStr = "128"
		}
		formatStr := strings.TrimSpace(r.Format)
		if formatStr == "" {
			formatStr = "MP3"
		}
		genreStr := strings.TrimSpace(r.Genre)
		if genreStr == "" {
			genreStr = "General"
		}

		if _, execErr := stmt.Exec(name, streamURL, strings.TrimSpace(r.LogoURL), genreStr, strings.TrimSpace(r.Broadcaster), strings.TrimSpace(r.Country), bitrateStr, formatStr); execErr == nil {
			count++
		}
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}

// UpsertTrack inserta o actualiza una pista de audio en la base de datos.
func (d *DB) UpsertTrack(t *Track) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	query := `
	INSERT INTO tracks (
		path, filename, title, artist, album, album_artist,
		genre, year, duration, track_number, bitrate, sample_rate,
		format, source, cover_url, bpm
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(path) DO UPDATE SET
		title=excluded.title,
		artist=excluded.artist,
		album=excluded.album,
		album_artist=excluded.album_artist,
		genre=excluded.genre,
		year=excluded.year,
		duration=excluded.duration,
		track_number=excluded.track_number,
		bitrate=excluded.bitrate,
		sample_rate=excluded.sample_rate,
		format=excluded.format,
		source=excluded.source,
		cover_url=CASE WHEN excluded.cover_url != '' THEN excluded.cover_url ELSE tracks.cover_url END,
		bpm=excluded.bpm
	`
	_, err := d.conn.Exec(query,
		t.Path, t.Filename, t.Title, t.Artist, t.Album, t.AlbumArtist,
		t.Genre, t.Year, t.Duration, t.TrackNumber, t.Bitrate, t.SampleRate,
		t.Format, t.Source, t.CoverURL, t.BPM,
	)
	if err != nil {
		return err
	}

	var insertedID int64
	if err := d.conn.QueryRow(`SELECT id FROM tracks WHERE path = ?`, t.Path).Scan(&insertedID); err == nil {
		t.ID = insertedID
	}

	// Mantener catálogo de artistas y álbumes sincronizado
	_, _ = d.conn.Exec(`
		INSERT OR IGNORE INTO artists (name, genre) VALUES (?, ?)
	`, t.Artist, t.Genre)

	_, _ = d.conn.Exec(`
		INSERT OR IGNORE INTO albums (title, artist, year, genre, cover_url, source)
		VALUES (?, ?, ?, ?, ?, ?)
	`, t.Album, t.Artist, t.Year, t.Genre, t.CoverURL, t.Source)

	return nil
}

// BatchUpsertTracks inserta o actualiza un lote de pistas dentro de una transacción única atómica.
func (d *DB) BatchUpsertTracks(tracks []*Track) error {
	if len(tracks) == 0 {
		return nil
	}

	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	tx, err := d.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	trackQuery := `
	INSERT INTO tracks (
		path, filename, title, artist, album, album_artist,
		genre, year, duration, track_number, bitrate, sample_rate,
		format, source, cover_url, bpm
	) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	ON CONFLICT(path) DO UPDATE SET
		title=excluded.title,
		artist=excluded.artist,
		album=excluded.album,
		album_artist=excluded.album_artist,
		genre=excluded.genre,
		year=excluded.year,
		duration=excluded.duration,
		track_number=excluded.track_number,
		bitrate=excluded.bitrate,
		sample_rate=excluded.sample_rate,
		format=excluded.format,
		source=excluded.source,
		cover_url=CASE WHEN excluded.cover_url != '' THEN excluded.cover_url ELSE tracks.cover_url END,
		bpm=excluded.bpm
	`
	stmtTrack, err := tx.Prepare(trackQuery)
	if err != nil {
		return err
	}
	defer stmtTrack.Close()

	stmtArtist, err := tx.Prepare(`INSERT OR IGNORE INTO artists (name, genre) VALUES (?, ?)`)
	if err != nil {
		return err
	}
	defer stmtArtist.Close()

	stmtAlbum, err := tx.Prepare(`
		INSERT OR IGNORE INTO albums (title, artist, year, genre, cover_url, source)
		VALUES (?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return err
	}
	defer stmtAlbum.Close()

	seenArtists := make(map[string]struct{}, 16)
	seenAlbums := make(map[string]struct{}, 16)

	for _, t := range tracks {
		if t == nil {
			continue
		}
		_, err := stmtTrack.Exec(
			t.Path, t.Filename, t.Title, t.Artist, t.Album, t.AlbumArtist,
			t.Genre, t.Year, t.Duration, t.TrackNumber, t.Bitrate, t.SampleRate,
			t.Format, t.Source, t.CoverURL, t.BPM,
		)
		if err != nil {
			return err
		}
		if _, ok := seenArtists[t.Artist]; !ok {
			seenArtists[t.Artist] = struct{}{}
			_, _ = stmtArtist.Exec(t.Artist, t.Genre)
		}
		albumKey := t.Album + "\x00" + t.Artist
		if _, ok := seenAlbums[albumKey]; !ok {
			seenAlbums[albumKey] = struct{}{}
			_, _ = stmtAlbum.Exec(t.Album, t.Artist, t.Year, t.Genre, t.CoverURL, t.Source)
		}
	}

	return tx.Commit()
}

// GetIndexedTrackDurations devuelve un mapa ruta -> duración para acelerar escaneos incrementales.
func (d *DB) GetIndexedTrackDurations() (map[string]float64, error) {
	rows, err := d.reader().Query(`SELECT path, duration FROM tracks`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	durations := make(map[string]float64)
	for rows.Next() {
		var p string
		var dur float64
		if err := rows.Scan(&p, &dur); err == nil {
			durations[p] = dur
		}
	}
	return durations, nil
}

// GetAllTracks devuelve todas las pistas registradas ordenadas alfabéticamente.
func (d *DB) GetAllTracks() ([]Track, error) {
	rows, err := d.reader().Query(`
		SELECT id, path, filename, title, artist, album, album_artist, genre,
		       year, duration, track_number, bitrate, sample_rate, format,
		       source, cover_url, favorite, play_count, bpm, created_at
		FROM tracks
		ORDER BY artist COLLATE NOCASE ASC, album COLLATE NOCASE ASC, track_number ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	tracks := make([]Track, 0)
	for rows.Next() {
		var t Track
		var fav int
		var createdAt string
		err := rows.Scan(
			&t.ID, &t.Path, &t.Filename, &t.Title, &t.Artist, &t.Album, &t.AlbumArtist, &t.Genre,
			&t.Year, &t.Duration, &t.TrackNumber, &t.Bitrate, &t.SampleRate, &t.Format,
			&t.Source, &t.CoverURL, &fav, &t.PlayCount, &t.BPM, &createdAt,
		)
		if err != nil {
			return nil, err
		}
		t.Favorite = (fav == 1)
		tracks = append(tracks, t)
	}
	return tracks, nil
}

// GetRecentTracks devuelve las últimas pistas indexadas en la biblioteca.
func (d *DB) GetRecentTracks(limit int) ([]Track, error) {
	rows, err := d.reader().Query(`
		SELECT id, path, filename, title, artist, album, album_artist, genre,
		       year, duration, track_number, bitrate, sample_rate, format,
		       source, cover_url, favorite, play_count, bpm, created_at
		FROM tracks
		ORDER BY id DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	tracks := make([]Track, 0)
	for rows.Next() {
		var t Track
		var fav int
		var createdAt string
		err := rows.Scan(
			&t.ID, &t.Path, &t.Filename, &t.Title, &t.Artist, &t.Album, &t.AlbumArtist, &t.Genre,
			&t.Year, &t.Duration, &t.TrackNumber, &t.Bitrate, &t.SampleRate, &t.Format,
			&t.Source, &t.CoverURL, &fav, &t.PlayCount, &t.BPM, &createdAt,
		)
		if err != nil {
			return nil, err
		}
		t.Favorite = (fav == 1)
		tracks = append(tracks, t)
	}
	return tracks, nil
}

// minAlbumTracksExclusive define el umbral estricto (> 5 temas, es decir, 6 o más) para mostrar un álbum en los listados reproducibles.
const minAlbumTracksExclusive = 5

// scanTracksFromRows recorre un conjunto de filas SQL estándar de pistas y construye el slice tipado.
func scanTracksFromRows(rows *sql.Rows) ([]Track, error) {
	tracks := make([]Track, 0)
	for rows.Next() {
		var t Track
		var fav int
		var createdAt string
		err := rows.Scan(
			&t.ID, &t.Path, &t.Filename, &t.Title, &t.Artist, &t.Album, &t.AlbumArtist, &t.Genre,
			&t.Year, &t.Duration, &t.TrackNumber, &t.Bitrate, &t.SampleRate, &t.Format,
			&t.Source, &t.CoverURL, &fav, &t.PlayCount, &t.BPM, &createdAt,
		)
		if err != nil {
			return nil, err
		}
		t.Favorite = (fav == 1)
		tracks = append(tracks, t)
	}
	return tracks, nil
}

// GetRecentlyPlayedTracks devuelve las últimas pistas reproducidas realmente según el historial.
func (d *DB) GetRecentlyPlayedTracks(limit int) ([]Track, error) {
	if limit <= 0 {
		limit = 30
	}
	rows, err := d.reader().Query(`
		SELECT t.id, t.path, t.filename, t.title, t.artist, t.album, t.album_artist, t.genre,
		       t.year, t.duration, t.track_number, t.bitrate, t.sample_rate, t.format,
		       t.source, t.cover_url, t.favorite, t.play_count, t.bpm, t.created_at
		FROM tracks t
		INNER JOIN (
			SELECT track_id, MAX(id) AS latest_play_id
			FROM playback_history
			GROUP BY track_id
		) h ON h.track_id = t.id
		ORDER BY h.latest_play_id DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTracksFromRows(rows)
}

// GetMostPlayedTracks devuelve las pistas con mayor número de reproducciones registradas.
func (d *DB) GetMostPlayedTracks(limit int) ([]Track, error) {
	if limit <= 0 {
		limit = 30
	}
	rows, err := d.reader().Query(`
		SELECT id, path, filename, title, artist, album, album_artist, genre,
		       year, duration, track_number, bitrate, sample_rate, format,
		       source, cover_url, favorite, play_count, bpm, created_at
		FROM tracks
		WHERE play_count > 0
		ORDER BY play_count DESC, last_played DESC, id DESC
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTracksFromRows(rows)
}

// GetAlbums devuelve la lista consolidada de álbumes reproducibles (más de 5 canciones), agrupados por título sin duplicados.
func (d *DB) GetAlbums() ([]Album, error) {
	return d.GetAlbumsFiltered(minAlbumTracksExclusive)
}

// GetAlbumsFiltered devuelve álbumes consolidados filtrados por número mínimo estricto de pistas (> minTracksExclusive).
func (d *DB) GetAlbumsFiltered(minTracksExclusive int) ([]Album, error) {
	rows, err := d.reader().Query(`
		SELECT 
			MIN(t.id) as id,
			t.album as title,
			CASE 
				WHEN COUNT(DISTINCT t.artist) > 1 THEN 
					CASE 
						WHEN MAX(t.album_artist) != '' AND MAX(t.album_artist) != MAX(t.artist) THEN MAX(t.album_artist)
						ELSE 'Varios Artistas'
					END
				ELSE MAX(t.artist)
			END as artist,
			MAX(t.year) as year,
			MAX(t.genre) as genre,
			COALESCE(MAX(NULLIF(t.cover_url, '')), '') as cover_url,
			COUNT(t.id) as track_count,
			COALESCE(SUM(t.duration), 0) as total_duration,
			MAX(t.source) as source,
			MAX(t.id) as added_seq
		FROM tracks t
		WHERE t.album != ''
		GROUP BY t.album COLLATE NOCASE
		HAVING COUNT(t.id) > ?
		ORDER BY t.album COLLATE NOCASE ASC
	`, minTracksExclusive)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	albums := make([]Album, 0)
	for rows.Next() {
		var a Album
		if err := rows.Scan(&a.ID, &a.Title, &a.Artist, &a.Year, &a.Genre, &a.CoverURL, &a.TrackCount, &a.TotalDuration, &a.Source, &a.AddedSeq); err != nil {
			return nil, err
		}
		albums = append(albums, a)
	}
	return albums, nil
}

// GetAlbumsByArtist devuelve la discografía completa de un artista en la biblioteca (incluyendo EPs y sencillos).
func (d *DB) GetAlbumsByArtist(artistName string) ([]Album, error) {
	rows, err := d.reader().Query(`
		SELECT 
			MIN(t.id) as id,
			t.album as title,
			MAX(t.artist) as artist,
			MAX(t.year) as year,
			MAX(t.genre) as genre,
			COALESCE(MAX(NULLIF(t.cover_url, '')), '') as cover_url,
			COUNT(t.id) as track_count,
			COALESCE(SUM(t.duration), 0) as total_duration,
			MAX(t.source) as source,
			MAX(t.id) as added_seq
		FROM tracks t
		WHERE t.album != '' AND t.artist = ? COLLATE NOCASE
		GROUP BY t.album COLLATE NOCASE
		ORDER BY MAX(t.year) DESC, t.album COLLATE NOCASE ASC
	`, artistName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	albums := make([]Album, 0)
	for rows.Next() {
		var a Album
		if err := rows.Scan(&a.ID, &a.Title, &a.Artist, &a.Year, &a.Genre, &a.CoverURL, &a.TrackCount, &a.TotalDuration, &a.Source, &a.AddedSeq); err != nil {
			return nil, err
		}
		albums = append(albums, a)
	}
	return albums, nil
}

// GetAlbumByID devuelve un álbum y todas sus canciones agrupadas por el título del álbum.
func (d *DB) GetAlbumByID(id int64) (*Album, []Track, error) {
	rDB := d.reader()

	// 1. Obtener el título del álbum correspondiente al ID (desde tracks o de albums histórico)
	var albumTitle string
	err := rDB.QueryRow(`SELECT album FROM tracks WHERE id = ?`, id).Scan(&albumTitle)
	if err != nil || albumTitle == "" {
		_ = rDB.QueryRow(`SELECT title FROM albums WHERE id = ?`, id).Scan(&albumTitle)
	}
	if albumTitle == "" {
		return nil, nil, fmt.Errorf("álbum no encontrado")
	}

	// 2. Obtener metadatos consolidados del álbum
	var a Album
	err = rDB.QueryRow(`
		SELECT 
			MIN(t.id) as id,
			t.album as title,
			CASE 
				WHEN COUNT(DISTINCT t.artist) > 1 THEN 
					CASE 
						WHEN MAX(t.album_artist) != '' AND MAX(t.album_artist) != MAX(t.artist) THEN MAX(t.album_artist)
						ELSE 'Varios Artistas'
					END
				ELSE MAX(t.artist)
			END as artist,
			MAX(t.year) as year,
			MAX(t.genre) as genre,
			COALESCE(MAX(NULLIF(t.cover_url, '')), '') as cover_url,
			COUNT(t.id) as track_count,
			COALESCE(SUM(t.duration), 0) as total_duration,
			MAX(t.source) as source
		FROM tracks t
		WHERE t.album = ? COLLATE NOCASE
		GROUP BY t.album COLLATE NOCASE
	`, albumTitle).Scan(&a.ID, &a.Title, &a.Artist, &a.Year, &a.Genre, &a.CoverURL, &a.TrackCount, &a.TotalDuration, &a.Source)
	if err != nil {
		return nil, nil, err
	}

	if a.CoverURL == "" {
		var albCover string
		_ = rDB.QueryRow(`SELECT cover_url FROM albums WHERE title = ? COLLATE NOCASE AND cover_url != '' LIMIT 1`, albumTitle).Scan(&albCover)
		if albCover != "" {
			a.CoverURL = albCover
		}
	}

	// 3. Obtener TODAS las pistas de este álbum, ordenadas por track_number y nombre de archivo
	rows, err := rDB.Query(`
		SELECT id, path, filename, title, artist, album, album_artist, genre,
		       year, duration, track_number, bitrate, sample_rate, format,
		       source, cover_url, favorite, play_count, bpm, created_at
		FROM tracks
		WHERE album = ? COLLATE NOCASE
		ORDER BY track_number ASC, filename ASC
	`, albumTitle)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()

	var tracks []Track
	var totalDur float64
	for rows.Next() {
		var t Track
		var fav int
		var createdAt string
		err := rows.Scan(
			&t.ID, &t.Path, &t.Filename, &t.Title, &t.Artist, &t.Album, &t.AlbumArtist, &t.Genre,
			&t.Year, &t.Duration, &t.TrackNumber, &t.Bitrate, &t.SampleRate, &t.Format,
			&t.Source, &t.CoverURL, &fav, &t.PlayCount, &t.BPM, &createdAt,
		)
		if err != nil {
			return nil, nil, err
		}
		t.Favorite = (fav == 1)
		if t.CoverURL == "" && a.CoverURL != "" {
			t.CoverURL = a.CoverURL
		}
		totalDur += t.Duration
		tracks = append(tracks, t)
	}
	a.TrackCount = len(tracks)
	a.TotalDuration = totalDur

	return &a, tracks, nil
}

// GetArtists devuelve todos los artistas con conteo de canciones y álbumes.
func (d *DB) GetArtists() ([]Artist, error) {
	rows, err := d.reader().Query(`
		SELECT ar.id, ar.name, ar.bio, ar.image_url, ar.genre,
		       COUNT(t.id) as track_count,
		       COUNT(DISTINCT t.album) as album_count
		FROM artists ar
		INNER JOIN tracks t ON t.artist = ar.name
		GROUP BY ar.id, ar.name
		ORDER BY ar.name COLLATE NOCASE ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	artists := make([]Artist, 0)
	for rows.Next() {
		var a Artist
		if err := rows.Scan(&a.ID, &a.Name, &a.Bio, &a.ImageURL, &a.Genre, &a.TrackCount, &a.AlbumCount); err != nil {
			return nil, err
		}
		artists = append(artists, a)
	}
	return artists, nil
}

// GetTracksByArtist devuelve todas las pistas de un artista en específico.
func (d *DB) GetTracksByArtist(artistName string) ([]Track, error) {
	rows, err := d.reader().Query(`
		SELECT id, path, filename, title, artist, album, album_artist, genre,
		       year, duration, track_number, bitrate, sample_rate, format,
		       source,
		       COALESCE(NULLIF(cover_url, ''), (SELECT a.cover_url FROM albums a WHERE a.title = tracks.album COLLATE NOCASE AND a.cover_url != '' LIMIT 1), '') as cover_url,
		       favorite, play_count, bpm, created_at
		FROM tracks
		WHERE artist = ? COLLATE NOCASE
		ORDER BY year DESC, album ASC, track_number ASC
	`, artistName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	tracks := make([]Track, 0)
	for rows.Next() {
		var t Track
		var fav int
		var createdAt string
		err := rows.Scan(
			&t.ID, &t.Path, &t.Filename, &t.Title, &t.Artist, &t.Album, &t.AlbumArtist, &t.Genre,
			&t.Year, &t.Duration, &t.TrackNumber, &t.Bitrate, &t.SampleRate, &t.Format,
			&t.Source, &t.CoverURL, &fav, &t.PlayCount, &t.BPM, &createdAt,
		)
		if err != nil {
			return nil, err
		}
		t.Favorite = (fav == 1)
		tracks = append(tracks, t)
	}
	return tracks, nil
}

// escapeLikePattern escapa comodines SQL LIKE (%, _, \) para búsquedas literales seguras con ESCAPE '\'.
func escapeLikePattern(s string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s)
}

// GetTracksByPathPrefix busca todas las pistas dentro de una carpeta o sus subcarpetas.
func (d *DB) GetTracksByPathPrefix(folderPath string) ([]Track, error) {
	clean := strings.TrimRight(folderPath, "/")
	pattern := escapeLikePattern(clean) + "/%"

	rows, err := d.reader().Query(`
		SELECT id, path, filename, title, artist, album, album_artist, genre,
		       year, duration, track_number, bitrate, sample_rate, format,
		       source,
		       COALESCE(NULLIF(cover_url, ''), (SELECT a.cover_url FROM albums a WHERE a.title = tracks.album COLLATE NOCASE AND a.cover_url != '' LIMIT 1), '') as cover_url,
		       favorite, play_count, bpm, created_at
		FROM tracks
		WHERE path = ? OR path LIKE ? ESCAPE '\'
		ORDER BY path ASC, track_number ASC
	`, clean, pattern)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	tracks := make([]Track, 0)
	for rows.Next() {
		var t Track
		var fav int
		var createdAt string
		err := rows.Scan(
			&t.ID, &t.Path, &t.Filename, &t.Title, &t.Artist, &t.Album, &t.AlbumArtist, &t.Genre,
			&t.Year, &t.Duration, &t.TrackNumber, &t.Bitrate, &t.SampleRate, &t.Format,
			&t.Source, &t.CoverURL, &fav, &t.PlayCount, &t.BPM, &createdAt,
		)
		if err != nil {
			return nil, err
		}
		t.Favorite = (fav == 1)
		tracks = append(tracks, t)
	}
	return tracks, nil
}

// GetTrackByPath busca una canción específica por su ruta exacta en el almacenamiento.
func (d *DB) GetTrackByPath(filePath string) (*Track, error) {
	row := d.reader().QueryRow(`
		SELECT id, path, filename, title, artist, album, album_artist, genre,
		       year, duration, track_number, bitrate, sample_rate, format,
		       source,
		       COALESCE(NULLIF(cover_url, ''), (SELECT a.cover_url FROM albums a WHERE a.title = tracks.album COLLATE NOCASE AND a.cover_url != '' LIMIT 1), '') as cover_url,
		       favorite, play_count, bpm, created_at
		FROM tracks
		WHERE path = ?
		LIMIT 1
	`, filePath)

	var t Track
	var fav int
	var createdAt string
	err := row.Scan(
		&t.ID, &t.Path, &t.Filename, &t.Title, &t.Artist, &t.Album, &t.AlbumArtist, &t.Genre,
		&t.Year, &t.Duration, &t.TrackNumber, &t.Bitrate, &t.SampleRate, &t.Format,
		&t.Source, &t.CoverURL, &fav, &t.PlayCount, &t.BPM, &createdAt,
	)
	if err != nil {
		return nil, err
	}
	t.Favorite = (fav == 1)
	return &t, nil
}

// GetTrackByID recupera una canción específica por su identificador numérico primario.
func (d *DB) GetTrackByID(id int64) (*Track, error) {
	tracks, err := d.GetTracksByIDs([]int64{id})
	if err != nil {
		return nil, err
	}
	if len(tracks) == 0 {
		return nil, sql.ErrNoRows
	}
	return &tracks[0], nil
}

// GetTracksByIDs devuelve las canciones específicas por sus identificadores preservando el orden solicitado.
func (d *DB) GetTracksByIDs(ids []int64) ([]Track, error) {
	tracks := make([]Track, 0, len(ids))
	if len(ids) == 0 {
		return tracks, nil
	}

	byID := make(map[int64]Track, len(ids))
	const chunkSize = 500
	rDB := d.reader()
	for start := 0; start < len(ids); start += chunkSize {
		end := start + chunkSize
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]

		placeholders := make([]string, len(chunk))
		args := make([]interface{}, len(chunk))
		for i, id := range chunk {
			placeholders[i] = "?"
			args[i] = id
		}

		query := fmt.Sprintf(`
			SELECT id, path, filename, title, artist, album, album_artist, genre,
			       year, duration, track_number, bitrate, sample_rate, format,
			       source,
			       COALESCE(NULLIF(cover_url, ''), (SELECT a.cover_url FROM albums a WHERE a.title = tracks.album COLLATE NOCASE AND a.cover_url != '' LIMIT 1), '') as cover_url,
			       favorite, play_count, bpm, created_at
			FROM tracks
			WHERE id IN (%s)
		`, strings.Join(placeholders, ","))

		rows, err := rDB.Query(query, args...)
		if err != nil {
			return nil, err
		}

		for rows.Next() {
			var t Track
			var fav int
			var createdAt string
			if err := rows.Scan(
				&t.ID, &t.Path, &t.Filename, &t.Title, &t.Artist, &t.Album, &t.AlbumArtist, &t.Genre,
				&t.Year, &t.Duration, &t.TrackNumber, &t.Bitrate, &t.SampleRate, &t.Format,
				&t.Source, &t.CoverURL, &fav, &t.PlayCount, &t.BPM, &createdAt,
			); err == nil {
				t.Favorite = (fav == 1)
				byID[t.ID] = t
			}
		}
		_ = rows.Close()
	}

	for _, id := range ids {
		if t, ok := byID[id]; ok {
			tracks = append(tracks, t)
		}
	}
	return tracks, nil
}

// buildFTS5Query sanitiza y estructura los términos de búsqueda como prefijos FTS5.
func buildFTS5Query(raw string) string {
	terms := strings.Fields(strings.TrimSpace(raw))
	if len(terms) == 0 {
		return ""
	}
	var clean []string
	for _, term := range terms {
		t := strings.Map(func(r rune) rune {
			if r == '"' || r == '*' || r == ':' || r == '^' || r == '(' || r == ')' || r == '{' || r == '}' || r == '[' || r == ']' {
				return -1
			}
			return r
		}, term)
		t = strings.TrimSpace(t)
		if t != "" {
			clean = append(clean, `"`+t+`"*`)
		}
	}
	return strings.Join(clean, " ")
}

// Search efectúa búsquedas instantáneas en pistas, álbumes y artistas con motor FTS5 y respaldo LIKE.
func (d *DB) Search(query string) ([]Track, []Album, []Artist, error) {
	rDB := d.reader()
	var tRows *sql.Rows
	var err error

	// 1. Intento de búsqueda ultrarrápida indexada mediante FTS5 (si no contiene comodines literales % o _)
	ftsQuery := buildFTS5Query(query)
	if d.hasFTS5 && ftsQuery != "" && !strings.ContainsAny(query, "%_") {
		tRows, err = rDB.Query(`
			SELECT t.id, t.path, t.filename, t.title, t.artist, t.album, t.album_artist, t.genre,
			       t.year, t.duration, t.track_number, t.bitrate, t.sample_rate, t.format,
			       t.source, t.cover_url, t.favorite, t.play_count, t.bpm, t.created_at
			FROM tracks t
			JOIN tracks_fts f ON t.id = f.rowid
			WHERE tracks_fts MATCH ?
			ORDER BY bm25(tracks_fts), t.play_count DESC, t.title ASC
			LIMIT 25
		`, ftsQuery)
		if err != nil {
			tRows = nil
		}
	}

	// 2. Fallback a búsqueda LIKE si FTS5 no está disponible o la consulta falló
	pattern := "%" + escapeLikePattern(strings.TrimSpace(query)) + "%"
	if tRows == nil {
		tRows, err = rDB.Query(`
			SELECT id, path, filename, title, artist, album, album_artist, genre,
			       year, duration, track_number, bitrate, sample_rate, format,
			       source, cover_url, favorite, play_count, bpm, created_at
			FROM tracks
			WHERE title LIKE ? ESCAPE '\' OR artist LIKE ? ESCAPE '\' OR album LIKE ? ESCAPE '\' OR genre LIKE ? ESCAPE '\'
			ORDER BY play_count DESC, title ASC
			LIMIT 25
		`, pattern, pattern, pattern, pattern)
		if err != nil {
			return nil, nil, nil, err
		}
	}

	tracks := make([]Track, 0)
	for tRows.Next() {
		var t Track
		var fav int
		var createdAt string
		if err := tRows.Scan(
			&t.ID, &t.Path, &t.Filename, &t.Title, &t.Artist, &t.Album, &t.AlbumArtist, &t.Genre,
			&t.Year, &t.Duration, &t.TrackNumber, &t.Bitrate, &t.SampleRate, &t.Format,
			&t.Source, &t.CoverURL, &fav, &t.PlayCount, &t.BPM, &createdAt,
		); err == nil {
			t.Favorite = (fav == 1)
			tracks = append(tracks, t)
		}
	}
	_ = tRows.Close()

	// Álbumes agrupados por título (sin repetición y con más de 5 canciones)
	aRows, err := rDB.Query(`
		SELECT 
			MIN(t.id) as id,
			t.album as title,
			CASE 
				WHEN COUNT(DISTINCT t.artist) > 1 THEN 'Varios Artistas'
				ELSE MAX(t.artist)
			END as artist,
			MAX(t.year) as year,
			MAX(t.genre) as genre,
			COALESCE(MAX(NULLIF(t.cover_url, '')), '') as cover_url,
			COUNT(t.id) as track_count,
			MAX(t.source) as source
		FROM tracks t
		WHERE t.album != '' AND (t.album LIKE ? ESCAPE '\' OR t.artist LIKE ? ESCAPE '\')
		GROUP BY t.album COLLATE NOCASE
		HAVING COUNT(t.id) > ?
		LIMIT 15
	`, pattern, pattern, minAlbumTracksExclusive)
	if err != nil {
		return tracks, nil, nil, err
	}

	albums := make([]Album, 0)
	for aRows.Next() {
		var a Album
		if err := aRows.Scan(&a.ID, &a.Title, &a.Artist, &a.Year, &a.Genre, &a.CoverURL, &a.TrackCount, &a.Source); err == nil {
			albums = append(albums, a)
		}
	}
	_ = aRows.Close()

	// Artistas
	arRows, err := rDB.Query(`
		SELECT id, name, bio, image_url, genre
		FROM artists
		WHERE name LIKE ? ESCAPE '\'
		LIMIT 10
	`, pattern)
	if err != nil {
		return tracks, albums, nil, err
	}
	defer arRows.Close()

	artists := make([]Artist, 0)
	for arRows.Next() {
		var ar Artist
		if err := arRows.Scan(&ar.ID, &ar.Name, &ar.Bio, &ar.ImageURL, &ar.Genre); err == nil {
			artists = append(artists, ar)
		}
	}

	return tracks, albums, artists, nil
}

// ToggleFavorite alterna el estado de favorito de una pista dada.
func (d *DB) ToggleFavorite(trackID int64) (bool, error) {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	var currentFav int
	err := d.conn.QueryRow("SELECT favorite FROM tracks WHERE id = ?", trackID).Scan(&currentFav)
	if err != nil {
		return false, err
	}

	newFav := 1
	if currentFav == 1 {
		newFav = 0
	}

	_, err = d.conn.Exec("UPDATE tracks SET favorite = ? WHERE id = ?", newFav, trackID)
	return (newFav == 1), err
}

// GetFavorites devuelve todas las pistas marcadas como favoritas en la biblioteca.
func (d *DB) GetFavorites() ([]Track, error) {
	rows, err := d.reader().Query(`
		SELECT id, path, filename, title, artist, album, album_artist, genre,
		       year, duration, track_number, bitrate, sample_rate, format,
		       source, cover_url, favorite, play_count, bpm, created_at
		FROM tracks
		WHERE favorite = 1
		ORDER BY id DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	tracks := make([]Track, 0)
	for rows.Next() {
		var t Track
		var fav int
		var createdAt string
		err := rows.Scan(
			&t.ID, &t.Path, &t.Filename, &t.Title, &t.Artist, &t.Album, &t.AlbumArtist, &t.Genre,
			&t.Year, &t.Duration, &t.TrackNumber, &t.Bitrate, &t.SampleRate, &t.Format,
			&t.Source, &t.CoverURL, &fav, &t.PlayCount, &t.BPM, &createdAt,
		)
		if err != nil {
			return nil, err
		}
		t.Favorite = (fav == 1)
		tracks = append(tracks, t)
	}

	return tracks, nil
}

// RecordPlayback incrementa el contador de reproducción y guarda en el historial.
func (d *DB) RecordPlayback(trackID int64) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	_, err := d.conn.Exec(`
		UPDATE tracks
		SET play_count = play_count + 1, last_played = CURRENT_TIMESTAMP
		WHERE id = ?
	`, trackID)
	if err != nil {
		return err
	}

	_, err = d.conn.Exec(`
		INSERT INTO playback_history (track_id, played_at) VALUES (?, CURRENT_TIMESTAMP)
	`, trackID)
	return err
}

// UpdateTrackDuration actualiza la duración real en segundos de una pista en la base de datos.
func (d *DB) UpdateTrackDuration(trackID int64, duration float64) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	_, err := d.conn.Exec("UPDATE tracks SET duration = ? WHERE id = ?", duration, trackID)
	return err
}

// TrackPropertyUpdate contiene los campos de audio calculados para actualización en lote.
type TrackPropertyUpdate struct {
	ID         int64
	Duration   float64
	Bitrate    int
	SampleRate int
}

// TrackPathID asocia el identificador de una pista con su ruta física en disco.
type TrackPathID struct {
	ID   int64
	Path string
}

// BatchUpdateTrackProperties actualiza en bloque las duraciones y calidades en una única transacción atómica.
func (d *DB) BatchUpdateTrackProperties(updates []TrackPropertyUpdate) error {
	if len(updates) == 0 {
		return nil
	}
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	tx, err := d.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		UPDATE tracks
		SET duration = ?,
		    bitrate = CASE WHEN ? > 0 THEN ? ELSE bitrate END,
		    sample_rate = CASE WHEN ? > 0 THEN ? ELSE sample_rate END
		WHERE id = ?
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, u := range updates {
		if u.Duration > 0 {
			_, _ = stmt.Exec(u.Duration, u.Bitrate, u.Bitrate, u.SampleRate, u.SampleRate, u.ID)
		}
	}

	return tx.Commit()
}

// GetTracksNeedingDuration localiza pistas que tienen la duración por defecto (240s) o <= 0.
func (d *DB) GetTracksNeedingDuration() ([]TrackPathID, error) {
	rows, err := d.reader().Query("SELECT id, path FROM tracks WHERE duration = 240.0 OR duration <= 0")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	list := make([]TrackPathID, 0)
	for rows.Next() {
		var item TrackPathID
		if err := rows.Scan(&item.ID, &item.Path); err == nil {
			list = append(list, item)
		}
	}
	return list, nil
}

// GetTrackRefsBySource devuelve los IDs y rutas indexadas para un origen concreto (local, nas o usb).
func (d *DB) GetTrackRefsBySource(source string) ([]TrackPathID, error) {
	rows, err := d.reader().Query(`SELECT id, path FROM tracks WHERE source = ?`, source)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	list := make([]TrackPathID, 0)
	for rows.Next() {
		var item TrackPathID
		if err := rows.Scan(&item.ID, &item.Path); err == nil {
			list = append(list, item)
		}
	}
	return list, nil
}

// DeleteTracksByIDs elimina pistas obsoletas en lotes dentro de una transacción para mantener el índice sin rutas rotas.
func (d *DB) DeleteTracksByIDs(ids []int64) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	tx, err := d.conn.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	const chunkSize = 500
	var totalDeleted int64
	for start := 0; start < len(ids); start += chunkSize {
		end := start + chunkSize
		if end > len(ids) {
			end = len(ids)
		}
		chunk := ids[start:end]
		placeholders := make([]string, len(chunk))
		args := make([]interface{}, len(chunk))
		for i, id := range chunk {
			placeholders[i] = "?"
			args[i] = id
		}
		inClause := strings.Join(placeholders, ",")
		_, _ = tx.Exec(`DELETE FROM playback_history WHERE track_id IN (`+inClause+`)`, args...)
		res, err := tx.Exec(`DELETE FROM tracks WHERE id IN (`+inClause+`)`, args...)
		if err != nil {
			return totalDeleted, err
		}
		if n, err := res.RowsAffected(); err == nil {
			totalDeleted += n
		}
	}

	if err := tx.Commit(); err != nil {
		return totalDeleted, err
	}
	return totalDeleted, nil
}

// GetSetting lee un valor de configuración persistente con valor por defecto.
func (d *DB) GetSetting(key, defaultValue string) string {
	var val string
	err := d.reader().QueryRow("SELECT value FROM app_settings WHERE key = ?", key).Scan(&val)
	if err != nil {
		return defaultValue
	}
	return val
}

// SetSetting guarda un valor de configuración en la base de datos.
func (d *DB) SetSetting(key, value string) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	query := `
		INSERT INTO app_settings (key, value, updated_at)
		VALUES (?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=CURRENT_TIMESTAMP
	`
	_, err := d.conn.Exec(query, key, value)
	return err
}

// GetRadios devuelve la lista de emisoras de radio web guardadas, priorizando favoritas.
func (d *DB) GetRadios() ([]RadioStation, error) {
	rows, err := d.reader().Query("SELECT id, name, stream_url, COALESCE(logo_url, ''), COALESCE(genre, 'General'), COALESCE(broadcaster, ''), COALESCE(country, ''), COALESCE(bitrate, '128'), COALESCE(format, 'MP3'), is_default, COALESCE(favorite, 0) FROM radio_stations ORDER BY favorite DESC, name COLLATE NOCASE ASC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	list := make([]RadioStation, 0)
	for rows.Next() {
		var r RadioStation
		var def int
		var fav int
		if err := rows.Scan(&r.ID, &r.Name, &r.StreamURL, &r.LogoURL, &r.Genre, &r.Broadcaster, &r.Country, &r.Bitrate, &r.Format, &def, &fav); err == nil {
			r.IsDefault = (def == 1)
			r.Favorite = (fav == 1)
			list = append(list, r)
		}
	}
	return list, nil
}

// GetRadioByStreamURL busca una emisora registrada en la base de datos por su URL de flujo.
func (d *DB) GetRadioByStreamURL(streamURL string) (*RadioStation, error) {
	var r RadioStation
	var def int
	var fav int
	err := d.reader().QueryRow(`
		SELECT id, name, stream_url, COALESCE(logo_url, ''), COALESCE(genre, 'General'), COALESCE(broadcaster, ''), COALESCE(country, ''), COALESCE(bitrate, '128'), COALESCE(format, 'MP3'), is_default, COALESCE(favorite, 0)
		FROM radio_stations
		WHERE stream_url = ?
		LIMIT 1
	`, streamURL).Scan(&r.ID, &r.Name, &r.StreamURL, &r.LogoURL, &r.Genre, &r.Broadcaster, &r.Country, &r.Bitrate, &r.Format, &def, &fav)
	if err != nil {
		return nil, err
	}
	r.IsDefault = (def == 1)
	r.Favorite = (fav == 1)
	return &r, nil
}

// GetRadioByID busca una emisora registrada en la base de datos por su identificador numérico.
func (d *DB) GetRadioByID(id int) (*RadioStation, error) {
	var r RadioStation
	var def int
	var fav int
	err := d.reader().QueryRow(`
		SELECT id, name, stream_url, COALESCE(logo_url, ''), COALESCE(genre, 'General'), COALESCE(broadcaster, ''), COALESCE(country, ''), COALESCE(bitrate, '128'), COALESCE(format, 'MP3'), is_default, COALESCE(favorite, 0)
		FROM radio_stations
		WHERE id = ?
		LIMIT 1
	`, id).Scan(&r.ID, &r.Name, &r.StreamURL, &r.LogoURL, &r.Genre, &r.Broadcaster, &r.Country, &r.Bitrate, &r.Format, &def, &fav)
	if err != nil {
		return nil, err
	}
	r.IsDefault = (def == 1)
	r.Favorite = (fav == 1)
	return &r, nil
}

// ToggleRadioFavorite alterna el estado de favorita de una emisora en SQLite.
func (d *DB) ToggleRadioFavorite(id int64) (bool, error) {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	var currentFav int
	err := d.conn.QueryRow("SELECT COALESCE(favorite, 0) FROM radio_stations WHERE id = ?", id).Scan(&currentFav)
	if err != nil {
		return false, err
	}
	newFav := 1
	if currentFav == 1 {
		newFav = 0
	}
	_, err = d.conn.Exec("UPDATE radio_stations SET favorite = ? WHERE id = ?", newFav, id)
	if err != nil {
		return false, err
	}
	return newFav == 1, nil
}

// SetRadioFavorite asigna explícitamente el estado de favorita de una emisora en SQLite.
func (d *DB) SetRadioFavorite(id int64, fav bool) error {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	favInt := 0
	if fav {
		favInt = 1
	}
	_, err := d.conn.Exec("UPDATE radio_stations SET favorite = ? WHERE id = ?", favInt, id)
	return err
}

// SearchRadios busca emisoras cuyo nombre, género, cadena o país coincidan con la búsqueda.
func (d *DB) SearchRadios(query string) ([]RadioStation, error) {
	clean := strings.TrimSpace(query)
	if clean == "" {
		return []RadioStation{}, nil
	}
	pattern := "%" + escapeLikePattern(clean) + "%"
	rows, err := d.reader().Query(`
		SELECT id, name, stream_url, COALESCE(logo_url, ''), COALESCE(genre, 'General'), COALESCE(broadcaster, ''), COALESCE(country, ''), COALESCE(bitrate, '128'), COALESCE(format, 'MP3'), is_default, COALESCE(favorite, 0)
		FROM radio_stations
		WHERE name LIKE ? ESCAPE '\'
		   OR genre LIKE ? ESCAPE '\'
		   OR broadcaster LIKE ? ESCAPE '\'
		   OR country LIKE ? ESCAPE '\'
		ORDER BY favorite DESC, name COLLATE NOCASE ASC
		LIMIT 20
	`, pattern, pattern, pattern, pattern)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	list := make([]RadioStation, 0)
	for rows.Next() {
		var r RadioStation
		var def int
		var fav int
		if err := rows.Scan(&r.ID, &r.Name, &r.StreamURL, &r.LogoURL, &r.Genre, &r.Broadcaster, &r.Country, &r.Bitrate, &r.Format, &def, &fav); err == nil {
			r.IsDefault = (def == 1)
			r.Favorite = (fav == 1)
			list = append(list, r)
		}
	}
	return list, nil
}

// AddRadio inserta o actualiza una emisora personalizada (.mp3, .aac, .m3u, .pls o .m3u8) en SQLite.
func (d *DB) AddRadio(r *RadioStation) error {
	if r == nil {
		return fmt.Errorf("emisora vacía")
	}
	name := strings.TrimSpace(r.Name)
	streamURL := strings.TrimSpace(r.StreamURL)
	if name == "" {
		return fmt.Errorf("el nombre de la emisora es obligatorio")
	}
	if !strings.HasPrefix(streamURL, "http://") && !strings.HasPrefix(streamURL, "https://") {
		return fmt.Errorf("la URL de la emisora debe comenzar por http:// o https://")
	}

	genre := strings.TrimSpace(r.Genre)
	if genre == "" {
		genre = "General"
	}
	bitrate := strings.TrimSpace(r.Bitrate)
	if bitrate == "" {
		bitrate = "128"
	}
	format := strings.TrimSpace(r.Format)
	if format == "" {
		format = "STREAM"
	}

	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	var existingName string
	var existingDefault int
	if err := d.conn.QueryRow(`SELECT name, is_default FROM radio_stations WHERE stream_url = ? LIMIT 1`, streamURL).Scan(&existingName, &existingDefault); err == nil && existingDefault == 1 {
		return fmt.Errorf("esta URL ya existe en el catálogo integrado (%s)", existingName)
	}

	_, err := d.conn.Exec(`
		INSERT INTO radio_stations (name, stream_url, logo_url, genre, broadcaster, country, bitrate, format, is_default)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, 0)
		ON CONFLICT(stream_url) DO UPDATE SET
			name=excluded.name,
			logo_url=CASE WHEN excluded.logo_url != '' THEN excluded.logo_url ELSE radio_stations.logo_url END,
			genre=excluded.genre,
			broadcaster=excluded.broadcaster,
			country=excluded.country,
			bitrate=excluded.bitrate,
			format=excluded.format
	`, name, streamURL, strings.TrimSpace(r.LogoURL), genre, strings.TrimSpace(r.Broadcaster), strings.TrimSpace(r.Country), bitrate, format)
	if err != nil {
		return err
	}

	var def int
	if err := d.conn.QueryRow(`
		SELECT id, name, stream_url, COALESCE(logo_url, ''), COALESCE(genre, 'General'), COALESCE(broadcaster, ''), COALESCE(country, ''), COALESCE(bitrate, '128'), COALESCE(format, 'MP3'), is_default
		FROM radio_stations WHERE stream_url = ?
	`, streamURL).Scan(&r.ID, &r.Name, &r.StreamURL, &r.LogoURL, &r.Genre, &r.Broadcaster, &r.Country, &r.Bitrate, &r.Format, &def); err != nil {
		return err
	}
	r.IsDefault = (def == 1)
	return nil
}

// DeleteRadio elimina una emisora personalizada añadida por el usuario según su ID.
func (d *DB) DeleteRadio(id int64) error {
	if id <= 0 {
		return fmt.Errorf("id de emisora inválido")
	}
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	res, err := d.conn.Exec("DELETE FROM radio_stations WHERE id = ? AND is_default = 0", id)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return fmt.Errorf("emisora personalizada no encontrada")
	}
	return nil
}

// GetSimilarArtistsOffline calcula similitud de artistas usando géneros, décadas y conteo en SQLite.
func (d *DB) GetSimilarArtistsOffline(seedArtist string) ([]string, error) {
	rDB := d.reader()

	// Obtener géneros y año promedio del artista semilla
	var seedGenre string
	var avgYear float64
	_ = rDB.QueryRow(`
		SELECT COALESCE(genre, ''), COALESCE(AVG(year), 0)
		FROM tracks
		WHERE artist = ? COLLATE NOCASE AND genre != ''
		GROUP BY artist
	`, seedArtist).Scan(&seedGenre, &avgYear)

	if seedGenre == "" {
		// Fallback: 10 artistas más prolíficos de la biblioteca
		rows, err := rDB.Query(`
			SELECT artist FROM tracks
			WHERE artist != ? COLLATE NOCASE AND artist != 'Artista Desconocido' AND artist != 'Varios Artistas'
			GROUP BY artist
			ORDER BY COUNT(id) DESC
			LIMIT 10
		`, seedArtist)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		res := make([]string, 0)
		for rows.Next() {
			var art string
			if err := rows.Scan(&art); err == nil && art != "" {
				res = append(res, art)
			}
		}
		return res, nil
	}

	// Buscar hasta 10 artistas con géneros coincidentes o épocas cercanas
	rows, err := rDB.Query(`
		SELECT artist,
		       (CASE WHEN genre LIKE ? ESCAPE '\' THEN 3 ELSE 0 END +
		        CASE WHEN ABS(AVG(year) - ?) <= 10 THEN 2 ELSE 0 END) AS similarity_score
		FROM tracks
		WHERE artist != ? COLLATE NOCASE AND artist != 'Artista Desconocido' AND artist != 'Varios Artistas'
		GROUP BY artist
		ORDER BY similarity_score DESC, COUNT(id) DESC
		LIMIT 10
	`, "%"+escapeLikePattern(seedGenre)+"%", avgYear, seedArtist)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	result := make([]string, 0)
	for rows.Next() {
		var art string
		var score int
		if err := rows.Scan(&art, &score); err == nil && art != "" {
			result = append(result, art)
		}
	}
	return result, nil
}

// formatDecadeLabel formatea un año de inicio de década (ej. 1980 -> "80s", 2000 -> "2000s").
func formatDecadeLabel(decadeStart int) string {
	if decadeStart >= 1900 && decadeStart < 2000 {
		return fmt.Sprintf("%02ds", decadeStart%100)
	}
	return fmt.Sprintf("%ds", decadeStart)
}

// GetLibraryThemes extrae los principales géneros y décadas disponibles en SQLite para generar mixes temáticos offline.
func (d *DB) GetLibraryThemes() ([]ThemeOption, error) {
	rDB := d.reader()
	themes := make([]ThemeOption, 0)

	gRows, err := rDB.Query(`
		SELECT TRIM(genre) AS g, COUNT(id) AS cnt
		FROM tracks
		WHERE TRIM(COALESCE(genre, '')) != ''
		GROUP BY LOWER(TRIM(genre))
		HAVING COUNT(id) >= 2
		ORDER BY cnt DESC, g COLLATE NOCASE ASC
		LIMIT 10
	`)
	if err != nil {
		return nil, err
	}
	for gRows.Next() {
		var genre string
		var cnt int
		if scanErr := gRows.Scan(&genre, &cnt); scanErr == nil && genre != "" {
			themes = append(themes, ThemeOption{
				Type:       "genre",
				Value:      genre,
				Label:      genre,
				TrackCount: cnt,
			})
		}
	}
	_ = gRows.Close()

	dRows, err := rDB.Query(`
		SELECT (year / 10) * 10 AS decade, COUNT(id) AS cnt
		FROM tracks
		WHERE year >= 1940 AND year <= 2039
		GROUP BY decade
		HAVING COUNT(id) >= 2
		ORDER BY decade DESC
		LIMIT 8
	`)
	if err != nil {
		return themes, err
	}
	defer dRows.Close()

	for dRows.Next() {
		var decade, cnt int
		if scanErr := dRows.Scan(&decade, &cnt); scanErr == nil && decade > 0 {
			themes = append(themes, ThemeOption{
				Type:       "decade",
				Value:      fmt.Sprintf("%d", decade),
				Label:      formatDecadeLabel(decade),
				TrackCount: cnt,
			})
		}
	}
	return themes, nil
}

// GetTracksByGenre devuelve pistas aleatorias de un género musical específico (insensible a mayúsculas).
func (d *DB) GetTracksByGenre(genre string, limit int) ([]Track, error) {
	if limit <= 0 {
		limit = 60
	}
	clean := strings.TrimSpace(genre)
	if clean == "" {
		return make([]Track, 0), nil
	}
	rows, err := d.reader().Query(`
		SELECT id, path, filename, title, artist, album, album_artist, genre,
		       year, duration, track_number, bitrate, sample_rate, format,
		       source, cover_url, favorite, play_count, bpm, created_at
		FROM tracks
		WHERE TRIM(genre) = ? COLLATE NOCASE
		ORDER BY RANDOM()
		LIMIT ?
	`, clean, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTracksFromRows(rows)
}

// GetRandomTracks devuelve una selección aleatoria de canciones de la biblioteca local.
func (d *DB) GetRandomTracks(limit int) ([]Track, error) {
	if limit <= 0 {
		limit = 15
	}
	rows, err := d.reader().Query(`
		SELECT id, path, filename, title, artist, album, album_artist, genre,
		       year, duration, track_number, bitrate, sample_rate, format,
		       source, cover_url, favorite, play_count, bpm, created_at
		FROM tracks
		ORDER BY RANDOM()
		LIMIT ?
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTracksFromRows(rows)
}

// GetTracksByDecade devuelve pistas aleatorias publicadas dentro de una década concreta [decadeStart, decadeStart+9].
func (d *DB) GetTracksByDecade(decadeStart int, limit int) ([]Track, error) {
	if limit <= 0 {
		limit = 60
	}
	if decadeStart < 1900 || decadeStart > 2100 {
		return make([]Track, 0), nil
	}
	rows, err := d.reader().Query(`
		SELECT id, path, filename, title, artist, album, album_artist, genre,
		       year, duration, track_number, bitrate, sample_rate, format,
		       source, cover_url, favorite, play_count, bpm, created_at
		FROM tracks
		WHERE year >= ? AND year < ?
		ORDER BY RANDOM()
		LIMIT ?
	`, decadeStart, decadeStart+10, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTracksFromRows(rows)
}

// scanAlbumsFromRows lee filas estándar de consultas consolidadas de álbumes.
func scanAlbumsFromRows(rows *sql.Rows) ([]Album, error) {
	albums := make([]Album, 0)
	for rows.Next() {
		var a Album
		if err := rows.Scan(&a.ID, &a.Title, &a.Artist, &a.Year, &a.Genre, &a.CoverURL, &a.TrackCount, &a.TotalDuration, &a.Source, &a.AddedSeq); err != nil {
			return nil, err
		}
		albums = append(albums, a)
	}
	return albums, nil
}

// GetCategoriesOverview devuelve todos los géneros y décadas disponibles en Biblioteca con número de álbumes, pistas y carátula.
func (d *DB) GetCategoriesOverview() ([]CategorySummary, []CategorySummary, error) {
	rDB := d.reader()
	genres := make([]CategorySummary, 0)
	decades := make([]CategorySummary, 0)

	gRows, err := rDB.Query(`
		SELECT MIN(TRIM(genre)) AS g,
		       COUNT(DISTINCT CASE WHEN album != '' THEN LOWER(album) END) AS alb_cnt,
		       COUNT(id) AS tr_cnt,
		       COALESCE(MAX(NULLIF(cover_url, '')), '') AS cover
		FROM tracks
		WHERE TRIM(COALESCE(genre, '')) != ''
		GROUP BY LOWER(TRIM(genre))
		HAVING COUNT(id) >= 1
		ORDER BY tr_cnt DESC, g COLLATE NOCASE ASC
	`)
	if err != nil {
		return nil, nil, err
	}
	for gRows.Next() {
		var item CategorySummary
		if scanErr := gRows.Scan(&item.Value, &item.AlbumCount, &item.TrackCount, &item.CoverURL); scanErr == nil && item.Value != "" {
			item.Type = "genre"
			item.Label = item.Value
			genres = append(genres, item)
		}
	}
	_ = gRows.Close()

	dRows, err := rDB.Query(`
		SELECT (year / 10) * 10 AS decade,
		       COUNT(DISTINCT CASE WHEN album != '' THEN LOWER(album) END) AS alb_cnt,
		       COUNT(id) AS tr_cnt,
		       COALESCE(MAX(NULLIF(cover_url, '')), '') AS cover
		FROM tracks
		WHERE year >= 1940 AND year <= 2039
		GROUP BY decade
		HAVING COUNT(id) >= 1
		ORDER BY decade DESC
	`)
	if err != nil {
		return genres, nil, err
	}
	defer dRows.Close()

	for dRows.Next() {
		var dec int
		var item CategorySummary
		if scanErr := dRows.Scan(&dec, &item.AlbumCount, &item.TrackCount, &item.CoverURL); scanErr == nil && dec > 0 {
			item.Type = "decade"
			item.Value = fmt.Sprintf("%d", dec)
			item.Label = formatDecadeLabel(dec)
			decades = append(decades, item)
		}
	}

	return genres, decades, nil
}

// GetAlbumsByGenre devuelve los álbumes pertenecientes a un género musical concreto.
func (d *DB) GetAlbumsByGenre(genre string) ([]Album, error) {
	clean := strings.TrimSpace(genre)
	if clean == "" {
		return make([]Album, 0), nil
	}
	rows, err := d.reader().Query(`
		SELECT 
			MIN(t.id) as id,
			t.album as title,
			CASE 
				WHEN COUNT(DISTINCT t.artist) > 1 THEN 
					CASE 
						WHEN MAX(t.album_artist) != '' AND MAX(t.album_artist) != MAX(t.artist) THEN MAX(t.album_artist)
						ELSE 'Varios Artistas'
					END
				ELSE MAX(t.artist)
			END as artist,
			MAX(t.year) as year,
			MAX(t.genre) as genre,
			COALESCE(MAX(NULLIF(t.cover_url, '')), '') as cover_url,
			COUNT(t.id) as track_count,
			COALESCE(SUM(t.duration), 0) as total_duration,
			MAX(t.source) as source,
			MAX(t.id) as added_seq
		FROM tracks t
		WHERE t.album != '' AND TRIM(t.genre) = ? COLLATE NOCASE
		GROUP BY t.album COLLATE NOCASE
		ORDER BY t.album COLLATE NOCASE ASC
	`, clean)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAlbumsFromRows(rows)
}

// GetAlbumsByDecade devuelve los álbumes publicados dentro de la década [decadeStart, decadeStart+9].
func (d *DB) GetAlbumsByDecade(decadeStart int) ([]Album, error) {
	if decadeStart < 1900 || decadeStart > 2100 {
		return make([]Album, 0), nil
	}
	rows, err := d.reader().Query(`
		SELECT 
			MIN(t.id) as id,
			t.album as title,
			CASE 
				WHEN COUNT(DISTINCT t.artist) > 1 THEN 
					CASE 
						WHEN MAX(t.album_artist) != '' AND MAX(t.album_artist) != MAX(t.artist) THEN MAX(t.album_artist)
						ELSE 'Varios Artistas'
					END
				ELSE MAX(t.artist)
			END as artist,
			MAX(t.year) as year,
			MAX(t.genre) as genre,
			COALESCE(MAX(NULLIF(t.cover_url, '')), '') as cover_url,
			COUNT(t.id) as track_count,
			COALESCE(SUM(t.duration), 0) as total_duration,
			MAX(t.source) as source,
			MAX(t.id) as added_seq
		FROM tracks t
		WHERE t.album != '' AND t.year >= ? AND t.year < ?
		GROUP BY t.album COLLATE NOCASE
		ORDER BY MAX(t.year) DESC, t.album COLLATE NOCASE ASC
	`, decadeStart, decadeStart+10)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanAlbumsFromRows(rows)
}

// UpdateAlbumCover actualiza la carátula en todas las pistas del álbum indicado sin modificar archivos de disco.
func (d *DB) UpdateAlbumCover(albumTitle, coverURL string) error {
	cleanAlbum := strings.TrimSpace(albumTitle)
	cleanCover := strings.TrimSpace(coverURL)
	if cleanAlbum == "" || cleanCover == "" {
		return fmt.Errorf("álbum y coverURL requeridos")
	}

	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	if _, err := d.conn.Exec(`UPDATE tracks SET cover_url = ? WHERE album = ? COLLATE NOCASE`, cleanCover, cleanAlbum); err != nil {
		return err
	}
	_, _ = d.conn.Exec(`UPDATE albums SET cover_url = ? WHERE title = ? COLLATE NOCASE`, cleanCover, cleanAlbum)
	return nil
}

// UpdateTrackCover actualiza la URL de carátula de una pista individual identificada por su ID.
func (d *DB) UpdateTrackCover(trackID int64, coverURL string) error {
	cleanCover := strings.TrimSpace(coverURL)
	if trackID <= 0 || cleanCover == "" {
		return fmt.Errorf("trackID y coverURL válidos requeridos")
	}

	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	_, err := d.conn.Exec(`UPDATE tracks SET cover_url = ? WHERE id = ?`, cleanCover, trackID)
	return err
}

// UpdateTrackCoverByPath actualiza la URL de carátula de una pista identificada por su ruta en disco.
func (d *DB) UpdateTrackCoverByPath(path, coverURL string) error {
	cleanPath := strings.TrimSpace(path)
	cleanCover := strings.TrimSpace(coverURL)
	if cleanPath == "" || cleanCover == "" {
		return fmt.Errorf("ruta y coverURL requeridos")
	}

	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	_, err := d.conn.Exec(`UPDATE tracks SET cover_url = ? WHERE path = ?`, cleanCover, cleanPath)
	return err
}

// GetAlbumCoverByTitle devuelve la URL de carátula para un álbum consultando tanto albums como tracks.
func (d *DB) GetAlbumCoverByTitle(albumTitle string) string {
	clean := strings.TrimSpace(albumTitle)
	if clean == "" {
		return ""
	}
	var cover string
	_ = d.reader().QueryRow(`
		SELECT cover_url FROM albums WHERE title = ? COLLATE NOCASE AND cover_url != '' LIMIT 1
	`, clean).Scan(&cover)
	if cover == "" {
		_ = d.reader().QueryRow(`
			SELECT cover_url FROM tracks WHERE album = ? COLLATE NOCASE AND cover_url != '' LIMIT 1
		`, clean).Scan(&cover)
	}
	return cover
}

// syncMissingTrackCoversFromAlbums propaga en segundo plano las carátulas conocidas de álbumes a sus pistas huérfanas sin carátula.
func (d *DB) syncMissingTrackCoversFromAlbums() {
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	res, err := d.conn.Exec(`
		UPDATE tracks
		SET cover_url = (
			SELECT a.cover_url
			FROM albums a
			WHERE a.title = tracks.album COLLATE NOCASE
			  AND a.cover_url != ''
			  AND a.cover_url IS NOT NULL
			LIMIT 1
		)
		WHERE (cover_url = '' OR cover_url IS NULL)
		  AND album != ''
		  AND EXISTS (
			SELECT 1 FROM albums a
			WHERE a.title = tracks.album COLLATE NOCASE
			  AND a.cover_url != ''
			  AND a.cover_url IS NOT NULL
		  )
	`)
	if err == nil {
		if aff, _ := res.RowsAffected(); aff > 0 {
			log.Printf("[DB] Sincronizadas %d pistas huérfanas con la carátula de su álbum", aff)
		}
	}
}

// SavePodcast guarda o actualiza una suscripción de podcast en SQLite.
func (d *DB) SavePodcast(p *Podcast) (int64, error) {
	if p == nil || strings.TrimSpace(p.FeedURL) == "" {
		return 0, fmt.Errorf("feed_url requerido para guardar podcast")
	}

	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	var existingID int64
	err := d.conn.QueryRow(`SELECT id FROM podcasts WHERE feed_url = ?`, strings.TrimSpace(p.FeedURL)).Scan(&existingID)
	if err == nil && existingID > 0 {
		_, updateErr := d.conn.Exec(`
			UPDATE podcasts 
			SET title = ?, author = ?, description = ?, cover_url = ?, link = ?, last_build_date = ?
			WHERE id = ?
		`, p.Title, p.Author, p.Description, p.CoverURL, p.Link, p.LastBuildDate, existingID)
		if updateErr != nil {
			return 0, updateErr
		}
		p.ID = existingID
		return existingID, nil
	}

	res, insertErr := d.conn.Exec(`
		INSERT INTO podcasts (title, author, feed_url, description, cover_url, link, last_build_date)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`, p.Title, p.Author, p.FeedURL, p.Description, p.CoverURL, p.Link, p.LastBuildDate)
	if insertErr != nil {
		return 0, insertErr
	}

	id, err := res.LastInsertId()
	if err == nil {
		p.ID = id
	}
	return id, err
}

// GetPodcasts lista todas las suscripciones a podcasts junto con el conteo de episodios indexados.
func (d *DB) GetPodcasts() ([]Podcast, error) {
	query := `
		SELECT p.id, p.title, p.author, p.feed_url, p.description, p.cover_url, p.link, p.last_build_date, p.created_at,
		       COALESCE(p.favorite, 0) AS favorite,
		       COUNT(e.id) AS episode_count
		FROM podcasts p
		LEFT JOIN podcast_episodes e ON p.id = e.podcast_id
		GROUP BY p.id
		ORDER BY p.favorite DESC, p.title COLLATE NOCASE ASC
	`
	rows, err := d.reader().Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var podcasts []Podcast
	for rows.Next() {
		var p Podcast
		var createdAt time.Time
		var favInt int
		if err := rows.Scan(&p.ID, &p.Title, &p.Author, &p.FeedURL, &p.Description, &p.CoverURL, &p.Link, &p.LastBuildDate, &createdAt, &favInt, &p.EpisodeCount); err != nil {
			continue
		}
		p.Favorite = (favInt == 1)
		p.CreatedAt = createdAt
		podcasts = append(podcasts, p)
	}
	if podcasts == nil {
		podcasts = []Podcast{}
	}
	return podcasts, nil
}

// GetFavoritePodcasts lista únicamente los programas marcados como favoritos.
func (d *DB) GetFavoritePodcasts() ([]Podcast, error) {
	all, err := d.GetPodcasts()
	if err != nil {
		return nil, err
	}
	favs := make([]Podcast, 0)
	for _, p := range all {
		if p.Favorite {
			favs = append(favs, p)
		}
	}
	return favs, nil
}

// GetPodcastByID obtiene los detalles de un podcast y todos sus episodios asociados ordenados cronológicamente.
func (d *DB) GetPodcastByID(id int64) (*Podcast, []PodcastEpisode, error) {
	if id <= 0 {
		return nil, nil, fmt.Errorf("id de podcast inválido")
	}

	var p Podcast
	var createdAt time.Time
	var favInt int
	queryPodcast := `SELECT id, title, author, feed_url, description, cover_url, link, last_build_date, COALESCE(favorite, 0), created_at FROM podcasts WHERE id = ?`
	err := d.reader().QueryRow(queryPodcast, id).Scan(&p.ID, &p.Title, &p.Author, &p.FeedURL, &p.Description, &p.CoverURL, &p.Link, &p.LastBuildDate, &favInt, &createdAt)
	if err != nil {
		return nil, nil, err
	}
	p.Favorite = (favInt == 1)
	p.CreatedAt = createdAt

	queryEpisodes := `
		SELECT id, podcast_id, guid, title, description, enclosure_url, enclosure_type, duration, pub_date, played, progress, COALESCE(favorite, 0), created_at
		FROM podcast_episodes
		WHERE podcast_id = ?
		ORDER BY id DESC
	`
	rows, err := d.reader().Query(queryEpisodes, id)
	if err != nil {
		return &p, []PodcastEpisode{}, nil
	}
	defer rows.Close()

	var episodes []PodcastEpisode
	for rows.Next() {
		var ep PodcastEpisode
		var epCreated time.Time
		var playedInt int
		var epFavInt int
		if err := rows.Scan(&ep.ID, &ep.PodcastID, &ep.GUID, &ep.Title, &ep.Description, &ep.EnclosureURL, &ep.EnclosureType, &ep.Duration, &ep.PubDate, &playedInt, &ep.Progress, &epFavInt, &epCreated); err != nil {
			continue
		}
		ep.Played = (playedInt == 1)
		ep.Favorite = (epFavInt == 1)
		ep.CreatedAt = epCreated
		ep.PodcastTitle = p.Title
		ep.PodcastCover = p.CoverURL
		episodes = append(episodes, ep)
	}
	if episodes == nil {
		episodes = []PodcastEpisode{}
	}
	p.EpisodeCount = len(episodes)
	return &p, episodes, nil
}

// GetPodcastByFeedURL busca si un feed ya está registrado en la base de datos.
func (d *DB) GetPodcastByFeedURL(feedURL string) (*Podcast, error) {
	cleanFeed := strings.TrimSpace(feedURL)
	if cleanFeed == "" {
		return nil, fmt.Errorf("feed_url requerido")
	}
	var p Podcast
	var createdAt time.Time
	var favInt int
	err := d.reader().QueryRow(`SELECT id, title, author, feed_url, description, cover_url, link, last_build_date, COALESCE(favorite, 0), created_at FROM podcasts WHERE feed_url = ?`, cleanFeed).
		Scan(&p.ID, &p.Title, &p.Author, &p.FeedURL, &p.Description, &p.CoverURL, &p.Link, &p.LastBuildDate, &favInt, &createdAt)
	if err != nil {
		return nil, err
	}
	p.Favorite = (favInt == 1)
	p.CreatedAt = createdAt
	return &p, nil
}

// TogglePodcastFavorite conmuta el estado de favorito de un canal de podcast suscrito.
func (d *DB) TogglePodcastFavorite(id int64) (bool, error) {
	if id <= 0 {
		return false, fmt.Errorf("id de podcast inválido")
	}
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	var curFav int
	err := d.conn.QueryRow(`SELECT COALESCE(favorite, 0) FROM podcasts WHERE id = ?`, id).Scan(&curFav)
	if err != nil {
		return false, err
	}
	newFav := 1
	if curFav == 1 {
		newFav = 0
	}
	_, err = d.conn.Exec(`UPDATE podcasts SET favorite = ? WHERE id = ?`, newFav, id)
	return newFav == 1, err
}

// ToggleEpisodeFavorite conmuta el estado de favorito de un episodio específico.
func (d *DB) ToggleEpisodeFavorite(id int64) (bool, error) {
	if id <= 0 {
		return false, fmt.Errorf("id de episodio inválido")
	}
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	var curFav int
	err := d.conn.QueryRow(`SELECT COALESCE(favorite, 0) FROM podcast_episodes WHERE id = ?`, id).Scan(&curFav)
	if err != nil {
		return false, err
	}
	newFav := 1
	if curFav == 1 {
		newFav = 0
	}
	_, err = d.conn.Exec(`UPDATE podcast_episodes SET favorite = ? WHERE id = ?`, newFav, id)
	return newFav == 1, err
}

// ToggleEpisodePlayed conmuta el estado de escuchado/no escuchado de un episodio específico.
func (d *DB) ToggleEpisodePlayed(id int64) (bool, error) {
	if id <= 0 {
		return false, fmt.Errorf("id de episodio inválido")
	}
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	var curPlayed int
	err := d.conn.QueryRow(`SELECT COALESCE(played, 0) FROM podcast_episodes WHERE id = ?`, id).Scan(&curPlayed)
	if err != nil {
		return false, err
	}
	newPlayed := 1
	if curPlayed == 1 {
		newPlayed = 0
	}
	_, err = d.conn.Exec(`UPDATE podcast_episodes SET played = ? WHERE id = ?`, newPlayed, id)
	return newPlayed == 1, err
}

// DeletePodcast elimina un podcast y todos sus episodios asociados en cascada.
func (d *DB) DeletePodcast(id int64) error {
	if id <= 0 {
		return fmt.Errorf("id de podcast inválido")
	}
	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	_, _ = d.conn.Exec(`DELETE FROM podcast_episodes WHERE podcast_id = ?`, id)
	_, err := d.conn.Exec(`DELETE FROM podcasts WHERE id = ?`, id)
	return err
}

// SavePodcastEpisodes inserta o actualiza en bloque los episodios de un podcast sin duplicar registros.
func (d *DB) SavePodcastEpisodes(podcastID int64, episodes []PodcastEpisode) error {
	if podcastID <= 0 || len(episodes) == 0 {
		return nil
	}

	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	tx, err := d.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`
		INSERT INTO podcast_episodes (podcast_id, guid, title, description, enclosure_url, enclosure_type, duration, pub_date)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(podcast_id, guid) DO UPDATE SET
			title = excluded.title,
			description = excluded.description,
			enclosure_url = excluded.enclosure_url,
			enclosure_type = excluded.enclosure_type,
			duration = CASE WHEN excluded.duration > 0 THEN excluded.duration ELSE podcast_episodes.duration END,
			pub_date = excluded.pub_date
	`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	for _, ep := range episodes {
		guid := ep.GUID
		if guid == "" {
			guid = ep.EnclosureURL
		}
		if ep.EnclosureURL == "" {
			continue
		}
		_, _ = stmt.Exec(podcastID, guid, ep.Title, ep.Description, ep.EnclosureURL, ep.EnclosureType, ep.Duration, ep.PubDate)
	}

	return tx.Commit()
}

// GetPodcastEpisodeByID obtiene un episodio por su identificador primario junto con los datos de su podcast.
func (d *DB) GetPodcastEpisodeByID(id int64) (*PodcastEpisode, *Podcast, error) {
	if id <= 0 {
		return nil, nil, fmt.Errorf("id de episodio inválido")
	}

	query := `
		SELECT e.id, e.podcast_id, e.guid, e.title, e.description, e.enclosure_url, e.enclosure_type, e.duration, e.pub_date, e.played, e.progress, COALESCE(e.favorite, 0), e.created_at,
		       p.title, p.author, p.cover_url, p.feed_url
		FROM podcast_episodes e
		JOIN podcasts p ON e.podcast_id = p.id
		WHERE e.id = ?
	`
	var ep PodcastEpisode
	var p Podcast
	var epCreated time.Time
	var playedInt int
	var epFavInt int

	err := d.reader().QueryRow(query, id).Scan(
		&ep.ID, &ep.PodcastID, &ep.GUID, &ep.Title, &ep.Description, &ep.EnclosureURL, &ep.EnclosureType, &ep.Duration, &ep.PubDate, &playedInt, &ep.Progress, &epFavInt, &epCreated,
		&p.Title, &p.Author, &p.CoverURL, &p.FeedURL,
	)
	if err != nil {
		return nil, nil, err
	}

	ep.Played = (playedInt == 1)
	ep.Favorite = (epFavInt == 1)
	ep.CreatedAt = epCreated
	ep.PodcastTitle = p.Title
	ep.PodcastCover = p.CoverURL
	p.ID = ep.PodcastID

	return &ep, &p, nil
}

// GetPodcastEpisodeByEnclosureURL busca un episodio a partir de su URL de flujo multimedia.
func (d *DB) GetPodcastEpisodeByEnclosureURL(url string) (*PodcastEpisode, *Podcast, error) {
	cleanURL := strings.TrimSpace(url)
	if cleanURL == "" {
		return nil, nil, fmt.Errorf("url requerida")
	}

	query := `
		SELECT e.id, e.podcast_id, e.guid, e.title, e.description, e.enclosure_url, e.enclosure_type, e.duration, e.pub_date, e.played, e.progress, COALESCE(e.favorite, 0), e.created_at,
		       p.title, p.author, p.cover_url, p.feed_url
		FROM podcast_episodes e
		JOIN podcasts p ON e.podcast_id = p.id
		WHERE e.enclosure_url = ?
		LIMIT 1
	`
	var ep PodcastEpisode
	var p Podcast
	var epCreated time.Time
	var playedInt int
	var epFavInt int

	err := d.reader().QueryRow(query, cleanURL).Scan(
		&ep.ID, &ep.PodcastID, &ep.GUID, &ep.Title, &ep.Description, &ep.EnclosureURL, &ep.EnclosureType, &ep.Duration, &ep.PubDate, &playedInt, &ep.Progress, &epFavInt, &epCreated,
		&p.Title, &p.Author, &p.CoverURL, &p.FeedURL,
	)
	if err != nil {
		return nil, nil, err
	}

	ep.Played = (playedInt == 1)
	ep.Favorite = (epFavInt == 1)
	ep.CreatedAt = epCreated
	ep.PodcastTitle = p.Title
	ep.PodcastCover = p.CoverURL
	p.ID = ep.PodcastID

	return &ep, &p, nil
}

// MarkEpisodePlayed actualiza el estado de reproducción de un episodio.
func (d *DB) MarkEpisodePlayed(id int64, played bool) error {
	if id <= 0 {
		return fmt.Errorf("id de episodio inválido")
	}
	val := 0
	if played {
		val = 1
	}
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	_, err := d.conn.Exec(`UPDATE podcast_episodes SET played = ? WHERE id = ?`, val, id)
	return err
}

// UpdateEpisodeProgress actualiza los segundos reproducidos de un episodio para reanudar donde se dejó.
func (d *DB) UpdateEpisodeProgress(id int64, progress float64) error {
	if id <= 0 {
		return fmt.Errorf("id de episodio inválido")
	}
	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	_, err := d.conn.Exec(`UPDATE podcast_episodes SET progress = ? WHERE id = ?`, progress, id)
	return err
}

// CreatePlaylist inserta una nueva lista de reproducción personalizada en la base de datos.
func (d *DB) CreatePlaylist(name, description string) (*Playlist, error) {
	cleanName := strings.TrimSpace(name)
	if cleanName == "" {
		return nil, fmt.Errorf("el nombre de la playlist no puede estar vacío")
	}
	cleanDesc := strings.TrimSpace(description)

	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	// Comprobar existencia previa insensible a mayúsculas y acentos (UTF-8 case fold)
	rows, err := d.conn.Query(`SELECT name FROM playlists`)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var existingName string
			if err := rows.Scan(&existingName); err == nil {
				if strings.EqualFold(existingName, cleanName) {
					return nil, fmt.Errorf("ya existe una playlist con el nombre '%s'", cleanName)
				}
			}
		}
	}

	res, err := d.conn.Exec(`INSERT INTO playlists (name, description) VALUES (?, ?)`, cleanName, cleanDesc)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, fmt.Errorf("ya existe una playlist con el nombre '%s'", cleanName)
		}
		return nil, fmt.Errorf("error al crear playlist: %w", err)
	}

	newID, err := res.LastInsertId()
	if err != nil {
		return nil, err
	}

	now := time.Now()
	return &Playlist{
		ID:          newID,
		Name:        cleanName,
		Description: cleanDesc,
		TrackCount:  0,
		Duration:    0,
		CreatedAt:   now,
		UpdatedAt:   now,
	}, nil
}

// GetAllPlaylists devuelve el listado completo de playlists con conteo de temas, duración total y carátula.
func (d *DB) GetAllPlaylists() ([]Playlist, error) {
	query := `
		SELECT p.id, p.name, p.description, p.created_at, p.updated_at,
		       COUNT(pt.id) AS track_count,
		       COALESCE(SUM(t.duration), 0) AS total_duration,
		       COALESCE((
		           SELECT t2.cover_url 
		           FROM playlist_tracks pt2 
		           JOIN tracks t2 ON pt2.track_id = t2.id 
		           WHERE pt2.playlist_id = p.id 
		           ORDER BY pt2.position ASC 
		           LIMIT 1
		       ), '') AS cover_url
		FROM playlists p
		LEFT JOIN playlist_tracks pt ON pt.playlist_id = p.id
		LEFT JOIN tracks t ON pt.track_id = t.id
		GROUP BY p.id
		ORDER BY p.updated_at DESC, p.name ASC
	`
	rows, err := d.reader().Query(query)
	if err != nil {
		return nil, fmt.Errorf("error al listar playlists: %w", err)
	}
	defer rows.Close()

	playlists := make([]Playlist, 0)
	for rows.Next() {
		var pl Playlist
		var created, updated time.Time
		if err := rows.Scan(&pl.ID, &pl.Name, &pl.Description, &created, &updated, &pl.TrackCount, &pl.Duration, &pl.CoverURL); err != nil {
			return nil, err
		}
		pl.CreatedAt = created
		pl.UpdatedAt = updated
		playlists = append(playlists, pl)
	}
	return playlists, rows.Err()
}

// GetPlaylistByID recupera una lista por su identificador único junto con todas sus pistas ordenadas.
func (d *DB) GetPlaylistByID(id int64) (*PlaylistDetail, error) {
	if id <= 0 {
		return nil, fmt.Errorf("identificador de playlist inválido")
	}

	queryHeader := `
		SELECT p.id, p.name, p.description, p.created_at, p.updated_at,
		       COUNT(pt.id) AS track_count,
		       COALESCE(SUM(t.duration), 0) AS total_duration,
		       COALESCE((
		           SELECT t2.cover_url 
		           FROM playlist_tracks pt2 
		           JOIN tracks t2 ON pt2.track_id = t2.id 
		           WHERE pt2.playlist_id = p.id 
		           ORDER BY pt2.position ASC 
		           LIMIT 1
		       ), '') AS cover_url
		FROM playlists p
		LEFT JOIN playlist_tracks pt ON pt.playlist_id = p.id
		LEFT JOIN tracks t ON pt.track_id = t.id
		WHERE p.id = ?
		GROUP BY p.id
	`
	var pl Playlist
	var created, updated time.Time
	err := d.reader().QueryRow(queryHeader, id).Scan(
		&pl.ID, &pl.Name, &pl.Description, &created, &updated,
		&pl.TrackCount, &pl.Duration, &pl.CoverURL,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, fmt.Errorf("playlist no encontrada")
		}
		return nil, fmt.Errorf("error al consultar playlist: %w", err)
	}
	pl.CreatedAt = created
	pl.UpdatedAt = updated

	queryTracks := `
		SELECT t.id, t.path, t.filename, t.title, t.artist, t.album, t.album_artist, t.genre,
		       t.year, t.duration, t.track_number, t.bitrate, t.sample_rate, t.format,
		       t.source, t.cover_url, t.favorite, t.play_count, t.bpm, t.created_at
		FROM playlist_tracks pt
		JOIN tracks t ON pt.track_id = t.id
		WHERE pt.playlist_id = ?
		ORDER BY pt.position ASC
	`
	rows, err := d.reader().Query(queryTracks, id)
	if err != nil {
		return nil, fmt.Errorf("error al obtener pistas de la playlist: %w", err)
	}
	defer rows.Close()

	tracks, err := scanTracksFromRows(rows)
	if err != nil {
		return nil, err
	}

	return &PlaylistDetail{
		Playlist: pl,
		Tracks:   tracks,
	}, nil
}

// UpdatePlaylist modifica el nombre y descripción de una playlist existente.
func (d *DB) UpdatePlaylist(id int64, name, description string) error {
	if id <= 0 {
		return fmt.Errorf("identificador de playlist inválido")
	}
	cleanName := strings.TrimSpace(name)
	if cleanName == "" {
		return fmt.Errorf("el nombre de la playlist no puede estar vacío")
	}
	cleanDesc := strings.TrimSpace(description)

	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	// Comprobar colisión de nombres con otras playlists
	rows, err := d.conn.Query(`SELECT id, name FROM playlists WHERE id != ?`, id)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var otherID int64
			var otherName string
			if err := rows.Scan(&otherID, &otherName); err == nil {
				if strings.EqualFold(otherName, cleanName) {
					return fmt.Errorf("ya existe otra playlist con el nombre '%s'", cleanName)
				}
			}
		}
	}

	res, err := d.conn.Exec(`
		UPDATE playlists 
		SET name = ?, description = ?, updated_at = CURRENT_TIMESTAMP 
		WHERE id = ?
	`, cleanName, cleanDesc, id)
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return fmt.Errorf("ya existe otra playlist con el nombre '%s'", cleanName)
		}
		return fmt.Errorf("error al actualizar playlist: %w", err)
	}

	aff, _ := res.RowsAffected()
	if aff == 0 {
		return fmt.Errorf("playlist no encontrada")
	}
	return nil
}

// DeletePlaylist elimina una lista de reproducción y desvincula sus pistas asociadas.
func (d *DB) DeletePlaylist(id int64) error {
	if id <= 0 {
		return fmt.Errorf("identificador de playlist inválido")
	}

	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	tx, err := d.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	if _, err := tx.Exec(`DELETE FROM playlist_tracks WHERE playlist_id = ?`, id); err != nil {
		return err
	}
	res, err := tx.Exec(`DELETE FROM playlists WHERE id = ?`, id)
	if err != nil {
		return err
	}
	aff, _ := res.RowsAffected()
	if aff == 0 {
		return fmt.Errorf("playlist no encontrada")
	}

	return tx.Commit()
}

// AddTracksToPlaylist añade una o más canciones al final de una lista de reproducción.
func (d *DB) AddTracksToPlaylist(playlistID int64, trackIDs []int64) (int, error) {
	if playlistID <= 0 {
		return 0, fmt.Errorf("identificador de playlist inválido")
	}
	if len(trackIDs) == 0 {
		return 0, nil
	}

	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	tx, err := d.conn.Begin()
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()

	// Obtener la posición máxima actual en la playlist
	var maxPos int
	err = tx.QueryRow(`SELECT COALESCE(MAX(position), -1) FROM playlist_tracks WHERE playlist_id = ?`, playlistID).Scan(&maxPos)
	if err != nil {
		return 0, err
	}

	stmt, err := tx.Prepare(`INSERT INTO playlist_tracks (playlist_id, track_id, position) VALUES (?, ?, ?)`)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	added := 0
	currentPos := maxPos + 1
	for _, trackID := range trackIDs {
		if trackID <= 0 {
			continue
		}
		if _, err := stmt.Exec(playlistID, trackID, currentPos); err != nil {
			return added, fmt.Errorf("error al agregar pista %d: %w", trackID, err)
		}
		currentPos++
		added++
	}

	if added > 0 {
		_, _ = tx.Exec(`UPDATE playlists SET updated_at = CURRENT_TIMESTAMP WHERE id = ?`, playlistID)
	}

	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return added, nil
}

// RemoveTrackFromPlaylist retira una canción de la playlist por su posición ordinal y reordena los índices posteriores.
func (d *DB) RemoveTrackFromPlaylist(playlistID int64, position int) error {
	if playlistID <= 0 || position < 0 {
		return fmt.Errorf("parámetros de posición o playlist inválidos")
	}

	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	tx, err := d.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	res, err := tx.Exec(`DELETE FROM playlist_tracks WHERE playlist_id = ? AND position = ?`, playlistID, position)
	if err != nil {
		return err
	}
	aff, _ := res.RowsAffected()
	if aff == 0 {
		return fmt.Errorf("no existe ninguna pista en la posición indicada")
	}

	// Decrementar las posiciones de las canciones que estaban por detrás
	_, err = tx.Exec(`
		UPDATE playlist_tracks 
		SET position = position - 1 
		WHERE playlist_id = ? AND position > ?
	`, playlistID, position)
	if err != nil {
		return err
	}

	_, _ = tx.Exec(`UPDATE playlists SET updated_at = CURRENT_TIMESTAMP WHERE id = ?`, playlistID)
	return tx.Commit()
}

// ReorderPlaylistTrack desplaza una pista de una posición de origen (fromPos) a una de destino (toPos).
func (d *DB) ReorderPlaylistTrack(playlistID int64, fromPos, toPos int) error {
	if playlistID <= 0 || fromPos < 0 || toPos < 0 {
		return fmt.Errorf("posiciones o identificador de playlist inválidos")
	}
	if fromPos == toPos {
		return nil
	}

	d.writeMu.Lock()
	defer d.writeMu.Unlock()

	tx, err := d.conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	// Comprobar que existe la pista en origen
	var idAtFrom int64
	err = tx.QueryRow(`SELECT id FROM playlist_tracks WHERE playlist_id = ? AND position = ?`, playlistID, fromPos).Scan(&idAtFrom)
	if err != nil {
		return fmt.Errorf("no existe pista en la posición de origen %d", fromPos)
	}

	// Usar una posición temporal negativa (-999) para evitar colisiones con la restricción UNIQUE(playlist_id, position)
	if _, err := tx.Exec(`UPDATE playlist_tracks SET position = -999 WHERE id = ?`, idAtFrom); err != nil {
		return err
	}

	if fromPos < toPos {
		// Mover hacia adelante: desplazar las intermedias hacia atrás (-1)
		_, err = tx.Exec(`
			UPDATE playlist_tracks 
			SET position = position - 1 
			WHERE playlist_id = ? AND position > ? AND position <= ?
		`, playlistID, fromPos, toPos)
	} else {
		// Mover hacia atrás: desplazar las intermedias hacia adelante (+1)
		_, err = tx.Exec(`
			UPDATE playlist_tracks 
			SET position = position + 1 
			WHERE playlist_id = ? AND position >= ? AND position < ?
		`, playlistID, toPos, fromPos)
	}
	if err != nil {
		return err
	}

	// Asignar la posición final definitiva
	if _, err := tx.Exec(`UPDATE playlist_tracks SET position = ? WHERE id = ?`, toPos, idAtFrom); err != nil {
		return err
	}

	_, _ = tx.Exec(`UPDATE playlists SET updated_at = CURRENT_TIMESTAMP WHERE id = ?`, playlistID)
	return tx.Commit()
}

// ExportPlaylistM3U genera el contenido del archivo estándar M3U8 para su descarga o uso en reproductores externos.
func (d *DB) ExportPlaylistM3U(id int64) (string, string, error) {
	detail, err := d.GetPlaylistByID(id)
	if err != nil {
		return "", "", err
	}

	var sb strings.Builder
	sb.WriteString("#EXTM3U\n")
	sb.WriteString(fmt.Sprintf("#PLAYLIST:%s\n", detail.Name))
	if detail.Description != "" {
		sb.WriteString(fmt.Sprintf("#COMMENT:%s\n", detail.Description))
	}

	for _, t := range detail.Tracks {
		dur := int(t.Duration)
		label := fmt.Sprintf("%s - %s", t.Artist, t.Title)
		sb.WriteString(fmt.Sprintf("#EXTINF:%d,%s\n", dur, label))
		sb.WriteString(fmt.Sprintf("%s\n", t.Path))
	}

	safeName := strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '_'
	}, detail.Name)
	if safeName == "" {
		safeName = fmt.Sprintf("playlist_%d", detail.ID)
	}
	filename := fmt.Sprintf("%s.m3u8", safeName)

	return filename, sb.String(), nil
}

// ImportPlaylistM3U analiza un archivo de lista M3U/M3U8 y añade las pistas coincidentes en una nueva playlist.
func (d *DB) ImportPlaylistM3U(name string, m3uContent string) (*Playlist, int, error) {
	lines := strings.Split(m3uContent, "\n")
	extractedName := strings.TrimSpace(name)
	var paths []string

	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			continue
		}
		if strings.HasPrefix(trimmed, "#PLAYLIST:") && extractedName == "" {
			extractedName = strings.TrimSpace(strings.TrimPrefix(trimmed, "#PLAYLIST:"))
			continue
		}
		if strings.HasPrefix(trimmed, "#") {
			continue
		}
		paths = append(paths, trimmed)
	}

	if extractedName == "" {
		extractedName = fmt.Sprintf("Playlist Importada (%s)", time.Now().Format("02-01 15:04"))
	}

	if len(paths) == 0 {
		return nil, 0, fmt.Errorf("el archivo M3U no contiene rutas de pistas reproducibles")
	}

	// Crear la nueva playlist
	pl, err := d.CreatePlaylist(extractedName, "Importada desde archivo M3U")
	if err != nil {
		return nil, 0, err
	}

	// Buscar las pistas en la base de datos por coincidencia exacta o por nombre de archivo
	matchedIDs := make([]int64, 0, len(paths))
	for _, p := range paths {
		var trackID int64
		// 1. Intento por coincidencia de ruta exacta
		err := d.reader().QueryRow(`SELECT id FROM tracks WHERE path = ? LIMIT 1`, p).Scan(&trackID)
		if err == nil && trackID > 0 {
			matchedIDs = append(matchedIDs, trackID)
			continue
		}

		// 2. Intento por sufijo de ruta o nombre de archivo base
		filename := filepath.Base(p)
		if filename != "" && filename != "." && filename != "/" {
			err = d.reader().QueryRow(`SELECT id FROM tracks WHERE filename = ? OR path LIKE ? LIMIT 1`, filename, "%/"+filename).Scan(&trackID)
			if err == nil && trackID > 0 {
				matchedIDs = append(matchedIDs, trackID)
			}
		}
	}

	addedCount := 0
	if len(matchedIDs) > 0 {
		addedCount, err = d.AddTracksToPlaylist(pl.ID, matchedIDs)
		if err != nil {
			return pl, 0, fmt.Errorf("error al vincular canciones a la playlist: %w", err)
		}
	}

	return pl, addedCount, nil
}

// GetLyricsCache recupera letras cacheadas desde SQLite por su clave normalizada sin realizar llamadas HTTP externas.
func (d *DB) GetLyricsCache(cacheKey string) (found bool, instrumental bool, plain string, syncedJSON string, source string, updatedAt time.Time, ok bool) {
	if cacheKey == "" {
		return false, false, "", "", "", time.Time{}, false
	}
	var foundInt, instInt int
	var updatedUnix int64
	err := d.reader().QueryRow(
		`SELECT found, instrumental, plain_lyrics, synced_json, source, updated_at FROM lyrics_cache WHERE cache_key = ? LIMIT 1`,
		cacheKey,
	).Scan(&foundInt, &instInt, &plain, &syncedJSON, &source, &updatedUnix)
	if err != nil {
		return false, false, "", "", "", time.Time{}, false
	}
	if updatedUnix > 0 {
		updatedAt = time.Unix(updatedUnix, 0)
	}
	return foundInt != 0, instInt != 0, plain, syncedJSON, source, updatedAt, true
}

// SaveLyricsCache persiste letras en SQLite de forma segura para disponibilidad offline y respuesta instantánea.
func (d *DB) SaveLyricsCache(cacheKey string, found, instrumental bool, plain, syncedJSON, source string) error {
	if cacheKey == "" {
		return nil
	}
	foundInt := 0
	if found {
		foundInt = 1
	}
	instInt := 0
	if instrumental {
		instInt = 1
	}
	if syncedJSON == "" {
		syncedJSON = "[]"
	}

	d.writeMu.Lock()
	defer d.writeMu.Unlock()
	_, err := d.conn.Exec(
		`INSERT INTO lyrics_cache (cache_key, found, instrumental, plain_lyrics, synced_json, source, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(cache_key) DO UPDATE SET
			found = excluded.found,
			instrumental = excluded.instrumental,
			plain_lyrics = excluded.plain_lyrics,
			synced_json = excluded.synced_json,
			source = excluded.source,
			updated_at = excluded.updated_at`,
		cacheKey, foundInt, instInt, plain, syncedJSON, source, time.Now().Unix(),
	)
	return err
}


// Close cierra las conexiones de lectura y escritura de la base de datos de manera ordenada.
func (d *DB) Close() error {
	if d.readConn != nil && d.readConn != d.conn {
		_ = d.readConn.Close()
	}
	if d.conn != nil {
		return d.conn.Close()
	}
	return nil
}

