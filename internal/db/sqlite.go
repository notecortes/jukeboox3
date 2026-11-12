package db

import (
	"database/sql"
	_ "embed"
	"encoding/json"
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
	`
	if _, err := d.conn.Exec(schema); err != nil {
		return err
	}

	// Migraciones seguras para columnas agregadas dinámicamente
	_, _ = d.conn.Exec(`ALTER TABLE radio_stations ADD COLUMN broadcaster TEXT DEFAULT ''`)
	_, _ = d.conn.Exec(`ALTER TABLE radio_stations ADD COLUMN country TEXT DEFAULT ''`)
	_, _ = d.conn.Exec(`ALTER TABLE radio_stations ADD COLUMN format TEXT DEFAULT 'MP3'`)

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

// GetAlbums devuelve la lista consolidada de álbumes reproducibles (más de 5 canciones), agrupados por título sin duplicados.
func (d *DB) GetAlbums() ([]Album, error) {
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
			MAX(t.source) as source
		FROM tracks t
		WHERE t.album != ''
		GROUP BY t.album COLLATE NOCASE
		HAVING COUNT(t.id) > ?
		ORDER BY t.album COLLATE NOCASE ASC
	`, minAlbumTracksExclusive)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	albums := make([]Album, 0)
	for rows.Next() {
		var a Album
		if err := rows.Scan(&a.ID, &a.Title, &a.Artist, &a.Year, &a.Genre, &a.CoverURL, &a.TrackCount, &a.TotalDuration, &a.Source); err != nil {
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
		       source, cover_url, favorite, play_count, bpm, created_at
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
		       source, cover_url, favorite, play_count, bpm, created_at
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
		       source, cover_url, favorite, play_count, bpm, created_at
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
			       source, cover_url, favorite, play_count, bpm, created_at
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

// Search efectúa búsquedas instantáneas en pistas, álbumes y artistas con coincidencia parcial.
func (d *DB) Search(query string) ([]Track, []Album, []Artist, error) {
	pattern := "%" + escapeLikePattern(strings.TrimSpace(query)) + "%"
	rDB := d.reader()

	// Pistas
	tRows, err := rDB.Query(`
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

// GetRadios devuelve la lista de emisoras de radio web guardadas.
func (d *DB) GetRadios() ([]RadioStation, error) {
	rows, err := d.reader().Query("SELECT id, name, stream_url, COALESCE(logo_url, ''), COALESCE(genre, 'General'), COALESCE(broadcaster, ''), COALESCE(country, ''), COALESCE(bitrate, '128'), COALESCE(format, 'MP3'), is_default FROM radio_stations ORDER BY name COLLATE NOCASE ASC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	list := make([]RadioStation, 0)
	for rows.Next() {
		var r RadioStation
		var def int
		if err := rows.Scan(&r.ID, &r.Name, &r.StreamURL, &r.LogoURL, &r.Genre, &r.Broadcaster, &r.Country, &r.Bitrate, &r.Format, &def); err == nil {
			r.IsDefault = (def == 1)
			list = append(list, r)
		}
	}
	return list, nil
}

// GetRadioByStreamURL busca una emisora registrada en la base de datos por su URL de flujo.
func (d *DB) GetRadioByStreamURL(streamURL string) (*RadioStation, error) {
	var r RadioStation
	var def int
	err := d.reader().QueryRow(`
		SELECT id, name, stream_url, COALESCE(logo_url, ''), COALESCE(genre, 'General'), COALESCE(broadcaster, ''), COALESCE(country, ''), COALESCE(bitrate, '128'), COALESCE(format, 'MP3'), is_default
		FROM radio_stations
		WHERE stream_url = ?
		LIMIT 1
	`, streamURL).Scan(&r.ID, &r.Name, &r.StreamURL, &r.LogoURL, &r.Genre, &r.Broadcaster, &r.Country, &r.Bitrate, &r.Format, &def)
	if err != nil {
		return nil, err
	}
	r.IsDefault = (def == 1)
	return &r, nil
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

