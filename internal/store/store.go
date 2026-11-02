// Package store owns the wave SQLite database: schema, library sync upserts,
// like import, play/skip signals and stats queries.
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

// Track is one library row.
type Track struct {
	ID         string // MPD URI (stable per song)
	Artist     string
	Title      string
	Album      string
	Path       string
	DurationMS int64
}

// ArtistStats aggregates per-artist signals.
type ArtistStats struct {
	Artist string
	Tracks int64
	Likes  int64
	Plays  int64
	Skips  int64
}

// Play is a recorded listen.
type Play struct {
	TrackID  string
	TS       int64 // unix ms
	MSPlayed int64
}

// Skip is a recorded manual skip.
type Skip struct {
	TrackID string
	TS      int64 // unix ms
}

// Store wraps the database.
type Store struct {
	db *sql.DB
}

// DefaultPath returns the database path (WAVE_DB overrides).
func DefaultPath() string {
	if p := os.Getenv("WAVE_DB"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "wave.db"
	}
	return filepath.Join(home, ".local", "share", "wave", "wave.db")
}

// Open opens (and initializes) the database.
func Open(path string) (*Store, error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("mkdir %s: %w", dir, err)
		}
	}
	dsn := "file:" + path +
		"?_pragma=busy_timeout(5000)" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // single writer/reader: avoids SQLITE_BUSY surprises
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	schema := `
CREATE TABLE IF NOT EXISTS tracks(
  id TEXT PRIMARY KEY,
  artist TEXT NOT NULL DEFAULT '',
  title TEXT NOT NULL DEFAULT '',
  album TEXT NOT NULL DEFAULT '',
  path TEXT NOT NULL DEFAULT '',
  duration_ms INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS plays(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  track_id TEXT NOT NULL,
  ts INTEGER NOT NULL,
  ms_played INTEGER NOT NULL DEFAULT 0
);
CREATE TABLE IF NOT EXISTS likes(
  track_id TEXT PRIMARY KEY,
  ts INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS skips(
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  track_id TEXT NOT NULL,
  ts INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS meta(
  key TEXT PRIMARY KEY,
  value TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_plays_ts ON plays(ts DESC);
CREATE INDEX IF NOT EXISTS idx_plays_track ON plays(track_id);
CREATE INDEX IF NOT EXISTS idx_skips_ts ON skips(ts DESC);
CREATE INDEX IF NOT EXISTS idx_skips_track ON skips(track_id);
CREATE INDEX IF NOT EXISTS idx_tracks_artist ON tracks(artist);
`
	_, err := s.db.Exec(schema)
	return err
}

// ---------- tracks ----------

// UpsertTracks batch-upserts library rows in one transaction.
func (s *Store) UpsertTracks(tracks []Track) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	stmt, err := tx.Prepare(`INSERT INTO tracks(id,artist,title,album,path,duration_ms)
		VALUES(?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET artist=excluded.artist, title=excluded.title,
			album=excluded.album, path=excluded.path, duration_ms=excluded.duration_ms`)
	if err != nil {
		return err
	}
	defer stmt.Close()
	for _, t := range tracks {
		if _, err := stmt.Exec(t.ID, t.Artist, t.Title, t.Album, t.Path, t.DurationMS); err != nil {
			return fmt.Errorf("upsert %s: %w", t.ID, err)
		}
	}
	return tx.Commit()
}

// TrackByID fetches a single track by id.
func (s *Store) TrackByID(id string) (Track, error) {
	var t Track
	err := s.db.QueryRow(`SELECT id, artist, title, album, path, duration_ms FROM tracks WHERE id=?`, id).
		Scan(&t.ID, &t.Artist, &t.Title, &t.Album, &t.Path, &t.DurationMS)
	if err == sql.ErrNoRows {
		return Track{ID: id}, nil // unknown to library: treat as empty-artist track
	}
	return t, err
}

// CountTracks returns the number of tracks.
func (s *Store) CountTracks() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM tracks`).Scan(&n)
	return n, err
}

// ArtistTracks returns all tracks of an artist.

// SearchTracks does a substring search over artist/title/album.
func (s *Store) SearchTracks(q string, limit int) ([]Track, error) {
	if q == "" {
		return nil, nil
	}
	pat := "%" + q + "%"
	rows, err := s.db.Query(`SELECT id, artist, title, album, path, duration_ms FROM tracks
		WHERE artist LIKE ?1 OR title LIKE ?1 OR album LIKE ?1
		ORDER BY artist COLLATE NOCASE, album COLLATE NOCASE, path LIMIT ?2`,
		pat, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Track
	for rows.Next() {
		var t Track
		if err := rows.Scan(&t.ID, &t.Artist, &t.Title, &t.Album, &t.Path, &t.DurationMS); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// AlbumTracks returns all tracks of an album in path order.
func (s *Store) AlbumTracks(artist, album string) ([]Track, error) {
	rows, err := s.db.Query(`SELECT id, artist, title, album, path, duration_ms FROM tracks
		WHERE artist=?1 AND album=?2 ORDER BY path`, artist, album)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTracks(rows)
}

// AllTracks returns id, artist, title, path for likes matching.
func (s *Store) AllTracks() ([]Track, error) {
	rows, err := s.db.Query(`SELECT id, artist, title, path FROM tracks`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Track
	for rows.Next() {
		var t Track
		if err := rows.Scan(&t.ID, &t.Artist, &t.Title, &t.Path); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ArtistTracks returns all tracks of an artist.
func (s *Store) ArtistTracks(artist string) ([]Track, error) {
	rows, err := s.db.Query(`SELECT id, artist, title, album, path, duration_ms FROM tracks
		WHERE artist=?1 ORDER BY album, path`, artist)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanTracks(rows)
}

func scanTracks(rows *sql.Rows) ([]Track, error) {
	var out []Track
	for rows.Next() {
		var t Track
		if err := rows.Scan(&t.ID, &t.Artist, &t.Title, &t.Album, &t.Path, &t.DurationMS); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RandomAlbums returns artist/album pairs shuffled by SQL.
func (s *Store) AllAlbums() ([][2]string, error) {
	rows, err := s.db.Query(`SELECT artist, album FROM tracks GROUP BY artist, album`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out [][2]string
	for rows.Next() {
		var a, al string
		if err := rows.Scan(&a, &al); err != nil {
			return nil, err
		}
		out = append(out, [2]string{a, al})
	}
	return out, rows.Err()
}

// ---------- meta ----------

// MetaGet returns a meta value ("" if missing).
func (s *Store) MetaGet(key string) (string, error) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM meta WHERE key=?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

// MetaSet stores a meta value.
func (s *Store) MetaSet(key, value string) error {
	_, err := s.db.Exec(`INSERT INTO meta(key,value) VALUES(?,?)
		ON CONFLICT(key) DO UPDATE SET value=excluded.value`, key, value)
	return err
}

// ---------- likes ----------

// LikesImported reports the guard flag.
func (s *Store) LikesImported() (bool, error) {
	v, err := s.MetaGet("likes_imported")
	if err != nil {
		return false, err
	}
	return v == "1", nil
}

// UpsertLike records a like.
func (s *Store) UpsertLike(trackID string, ts int64) error {
	_, err := s.db.Exec(`INSERT INTO likes(track_id,ts) VALUES(?,?)
		ON CONFLICT(track_id) DO UPDATE SET ts=excluded.ts`, trackID, ts)
	return err
}

// ImportLikes stores matched likes (trackID, ts) and sets the guard atomically.
func (s *Store) ImportLikes(matches []string, ts int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, id := range matches {
		if _, err := tx.Exec(`INSERT INTO likes(track_id,ts) VALUES(?,?)
			ON CONFLICT(track_id) DO UPDATE SET ts=excluded.ts`, id, ts); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(`INSERT INTO meta(key,value) VALUES('likes_imported','1')
		ON CONFLICT(key) DO UPDATE SET value='1'`); err != nil {
		return err
	}
	return tx.Commit()
}

// CountLikes returns total likes rows.
func (s *Store) CountLikes() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM likes`).Scan(&n)
	return n, err
}

// TrackIsLiked reports whether track_id is liked.
func (s *Store) TrackIsLiked(id string) (bool, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM likes WHERE track_id=?`, id).Scan(&n)
	return n > 0, err
}

// ---------- plays / skips ----------

// RecordPlay appends one play row.
func (s *Store) RecordPlay(trackID string, ts, msPlayed int64) (err error) {
	_, err = s.db.Exec(`INSERT INTO plays(track_id,ts,ms_played) VALUES(?,?,?)`, trackID, ts, msPlayed)
	return
}

// RecordSkip appends one skip row.
func (s *Store) RecordSkip(trackID string, ts int64) (err error) {
	_, err = s.db.Exec(`INSERT INTO skips(track_id,ts) VALUES(?,?)`, trackID, ts)
	return
}

// RecentPlayTracks returns up to n most-recent play track ids, newest first.
func (s *Store) RecentPlayTracks(n int) ([]string, error) {
	rows, err := s.db.Query(`SELECT track_id FROM plays ORDER BY ts DESC, id DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// RecentSkips returns track ids skipped since the given unix-ms timestamp.
func (s *Store) RecentSkips(since int64) (map[string]bool, error) {
	rows, err := s.db.Query(`SELECT DISTINCT track_id FROM skips WHERE ts>?`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// ---------- stats ----------

// CountPlays returns total recorded plays.
func (s *Store) CountPlays() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM plays`).Scan(&n)
	return n, err
}

// CountSkips returns total recorded skips.
func (s *Store) CountSkips() (int, error) {
	var n int
	err := s.db.QueryRow(`SELECT COUNT(*) FROM skips`).Scan(&n)
	return n, err
}

// TotalListen returns summed listening time (ms, only rows that count as plays).
func (s *Store) TotalListen() (int64, error) {
	var v sql.NullInt64
	err := s.db.QueryRow(`SELECT SUM(ms_played) FROM plays WHERE ms_played>=30000`).Scan(&v)
	return v.Int64, err
}

// ListenPerDay returns listening ms per calendar day for the last `days` days,
// oldest first, zero-padded so index 0 is days-1 ago.
func (s *Store) ListenPerDay(days int) ([]int64, error) {
	since := time.Now().AddDate(0, 0, -days).UnixMilli()
	rows, err := s.db.Query(`SELECT ts/86400000 AS day, SUM(ms_played) FROM plays
		WHERE ts>? AND ms_played>=30000 GROUP BY day`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[int64]int64{}
	scanErr := func() error {
		for rows.Next() {
			var d int64
			var ms sql.NullInt64
			if err := rows.Scan(&d, &ms); err != nil {
				return err
			}
			m[d] = ms.Int64
		}
		return rows.Err()
	}()
	if scanErr != nil {
		return nil, scanErr
	}
	today := time.Now().Unix() / 86400
	out := make([]int64, days)
	for i := 0; i < days; i++ {
		out[i] = m[today-int64(days-1-i)]
	}
	return out, nil
}

// NameCount is a generic top-N row.
type NameCount struct {
	Name    string
	Count   int64
	ListenMS int64
}

// TopArtists returns the top-n artists by play count.
func (s *Store) TopArtists(n int) ([]NameCount, error) {
	rows, err := s.db.Query(`SELECT t.artist, COUNT(*), SUM(p.ms_played)
		FROM plays p LEFT JOIN tracks t ON t.id=p.track_id
		WHERE p.ms_played>=30000
		GROUP BY t.artist ORDER BY COUNT(*) DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NameCount
	for rows.Next() {
		var r NameCount
		var name sql.NullString
		var ms sql.NullInt64
		if err := rows.Scan(&name, &r.Count, &ms); err != nil {
			return nil, err
		}
		r.Name = name.String
		r.ListenMS = ms.Int64
		out = append(out, r)
	}
	return out, rows.Err()
}

// TopTracks returns the top-n tracks by play count.
func (s *Store) TopTracks(n int) ([]NameCount, error) {
	rows, err := s.db.Query(`SELECT p.track_id, t.artist, t.title, COUNT(*)
		FROM plays p LEFT JOIN tracks t ON t.id=p.track_id
		WHERE p.ms_played>=30000
		GROUP BY p.track_id ORDER BY COUNT(*) DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NameCount
	for rows.Next() {
		var r NameCount
		var id, artist, title sql.NullString
		if err := rows.Scan(&id, &artist, &title, &r.Count); err != nil {
			return nil, err
		}
		r.Name = artist.String + " — " + title.String
		out = append(out, r)
	}
	return out, rows.Err()
}

// YearTotals returns (plays, listen ms) since Jan 1 of the given year.
func (s *Store) YearTotals(year int) (int64, int64, error) {
	start := time.Date(year, 1, 1, 0, 0, 0, 0, time.Local).UnixMilli()
	var plays int64
	var ms sql.NullInt64
	err := s.db.QueryRow(`SELECT COUNT(*), SUM(ms_played) FROM plays
		WHERE ts>=? AND ms_played>=30000`, start).Scan(&plays, &ms)
	return plays, ms.Int64, err
}

// RecentPlays returns the most recent plays joined with track info.
func (s *Store) RecentPlays(n int) ([]struct {
	Play
	Artist string
	Title  string
}, error) {
	rows, err := s.db.Query(`SELECT p.ts, p.ms_played, p.track_id, COALESCE(t.artist,''), COALESCE(t.title,'')
		FROM plays p LEFT JOIN tracks t ON t.id=p.track_id
		ORDER BY p.ts DESC, p.id DESC LIMIT ?`, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []struct {
		Play
		Artist string
		Title  string
	}
	for rows.Next() {
		var p struct {
			Play
			Artist string
			Title  string
		}
		if err := rows.Scan(&p.TS, &p.MSPlayed, &p.TrackID, &p.Artist, &p.Title); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ArtistSignals aggregates per-artist library size and signal counts.
func (s *Store) ArtistSignals() ([]ArtistStats, error) {
	stats := map[string]*ArtistStats{}
	var order []string

	add := func(artist string) *ArtistStats {
		if a, ok := stats[artist]; ok {
			return a
		}
		a := &ArtistStats{Artist: artist}
		stats[artist] = a
		order = append(order, artist)
		return a
	}

	rows, err := s.db.Query(`SELECT artist, COUNT(*) FROM tracks GROUP BY artist`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var name string
		var n int64
		if err := rows.Scan(&name, &n); err != nil {
			rows.Close()
			return nil, err
		}
		add(name).Tracks = n
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	if err := bumpSignal(s, stats,
		`SELECT t.artist, COUNT(*) FROM plays p JOIN tracks t ON t.id=p.track_id GROUP BY t.artist`,
		func(a *ArtistStats, n int64) { a.Plays = n }); err != nil {
		return nil, err
	}
	if err := bumpSignal(s, stats,
		`SELECT t.artist, COUNT(*) FROM likes l JOIN tracks t ON t.id=l.track_id GROUP BY t.artist`,
		func(a *ArtistStats, n int64) { a.Likes = n }); err != nil {
		return nil, err
	}
	if err := bumpSignal(s, stats,
		`SELECT t.artist, COUNT(*) FROM skips k JOIN tracks t ON t.id=k.track_id GROUP BY t.artist`,
		func(a *ArtistStats, n int64) { a.Skips = n }); err != nil {
		return nil, err
	}

	out := make([]ArtistStats, 0, len(order))
	for _, name := range order {
		out = append(out, *stats[name])
	}
	return out, nil
}

func bumpSignal(s *Store, stats map[string]*ArtistStats, query string, set func(*ArtistStats, int64)) error {
	rows, err := s.db.Query(query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var artist string
		var n int64
		if err := rows.Scan(&artist, &n); err != nil {
			return err
		}
		if a, ok := stats[artist]; ok {
			set(a, n)
		}
	}
	return rows.Err()
}
