package catalog

import (
	"database/sql"
	"errors"
	"fmt"

	// Register modernc pure-Go SQLite driver (CGO_ENABLED=0 safe).
	_ "modernc.org/sqlite"
)

// ErrNotFound is returned by GetRelease and GetLink when the row is absent.
var ErrNotFound = errors.New("catalog: not found")

// sqliteStore is the SQLite-backed implementation of Store.
//
// Concurrency strategy: modernc/sqlite opens the DB with WAL mode and a
// busy_timeout so concurrent readers never block each other. Writes are
// naturally serialised by SQLite's single-writer model; the busy_timeout
// (set via PRAGMA below) makes concurrent writers retry rather than
// immediately fail. We do NOT add an application-level sync.Mutex because
// busy_timeout + WAL is sufficient for the expected workload (one writer at a
// time from the materialize/reaper goroutines) and avoids deadlocking if a
// caller holds a tx while another goroutine tries to write. The connection
// pool is set to a single connection (SetMaxOpenConns(1)) which eliminates all
// write contention and is the recommended approach for SQLite; reads are fast
// enough that a single connection is not a bottleneck here.
type sqliteStore struct {
	db *sql.DB
}

// OpenSQLite opens (or creates) the SQLite database at path and returns a
// Store. Migrations are idempotent (CREATE TABLE IF NOT EXISTS).
func OpenSQLite(path string) (Store, error) {
	// Set pragmas via the DSN so they apply to EVERY connection the pool opens,
	// not just the first — robust even if the pool is later resized (docs/15
	// §4.G). applyPragmas below remains as belt-and-suspenders. The modernc
	// driver name is "sqlite"; _pragma values use the func(arg) form.
	dsn := "file:" + path + "?" + dsnPragmas
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("catalog: open sqlite %q: %w", path, err)
	}

	// Single connection: eliminates write contention without a mutex.
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)

	if err := applyPragmas(db); err != nil {
		db.Close()
		return nil, err
	}
	if err := applyMigrations(db); err != nil {
		db.Close()
		return nil, err
	}

	return &sqliteStore{db: db}, nil
}

// dsnPragmas are the per-connection pragmas, applied via the DSN query string so
// they survive any future pool resize (see OpenSQLite). foreign_keys + WAL are
// the load-bearing ones; busy_timeout makes concurrent writers retry.
const dsnPragmas = "_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&" +
	"_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"

func applyPragmas(db *sql.DB) error {
	pragmas := []string{
		"PRAGMA foreign_keys = ON",
		"PRAGMA busy_timeout = 5000",  // ms; retry writes for up to 5 s
		"PRAGMA journal_mode = WAL",   // allow concurrent readers
		"PRAGMA synchronous = NORMAL", // safe with WAL, faster than FULL
	}
	for _, p := range pragmas {
		if _, err := db.Exec(p); err != nil {
			return fmt.Errorf("catalog: pragma %q: %w", p, err)
		}
	}
	return nil
}

const schemaSQL = `
CREATE TABLE IF NOT EXISTS release (
    hash        TEXT    PRIMARY KEY,
    name        TEXT    NOT NULL DEFAULT '',
    category    TEXT    NOT NULL DEFAULT '',
    magnet      TEXT    NOT NULL DEFAULT '',
    total_size  INTEGER NOT NULL DEFAULT 0,
    state       TEXT    NOT NULL DEFAULT 'virtual',
    cached      INTEGER NOT NULL DEFAULT 0,
    torbox_id   INTEGER NOT NULL DEFAULT 0,
    added_on    INTEGER NOT NULL DEFAULT 0,
    last_access INTEGER NOT NULL DEFAULT 0,
    created_at  INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS file (
    hash     TEXT    NOT NULL,
    file_id  INTEGER NOT NULL,
    rel_path TEXT    NOT NULL DEFAULT '',
    size     INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (hash, file_id),
    FOREIGN KEY (hash) REFERENCES release(hash) ON DELETE CASCADE
);

CREATE TABLE IF NOT EXISTS dl_link (
    hash        TEXT    NOT NULL,
    file_id     INTEGER NOT NULL,
    url         TEXT    NOT NULL DEFAULT '',
    fetched_at  INTEGER NOT NULL DEFAULT 0,
    expires_at  INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (hash, file_id),
    FOREIGN KEY (hash) REFERENCES release(hash) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_release_category    ON release(category);
CREATE INDEX IF NOT EXISTS idx_release_state       ON release(state);
CREATE INDEX IF NOT EXISTS idx_release_last_access ON release(state, last_access);
CREATE INDEX IF NOT EXISTS idx_release_added_on    ON release(state, added_on);
`

func applyMigrations(db *sql.DB) error {
	if _, err := db.Exec(schemaSQL); err != nil {
		return fmt.Errorf("catalog: apply migrations: %w", err)
	}
	return nil
}

// ---- Store implementation --------------------------------------------------

// UpsertRelease inserts or replaces the release row and replaces all its file
// rows atomically. File rows are deleted first so stale entries (from a
// re-grab with fewer files) are not left behind.
func (s *sqliteStore) UpsertRelease(r *Release, files []File) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("catalog: upsert release begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	_, err = tx.Exec(`
		INSERT INTO release
			(hash, name, category, magnet, total_size, state, cached,
			 torbox_id, added_on, last_access, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(hash) DO UPDATE SET
			name        = excluded.name,
			category    = excluded.category,
			magnet      = excluded.magnet,
			total_size  = excluded.total_size,
			state       = excluded.state,
			cached      = excluded.cached,
			torbox_id   = excluded.torbox_id,
			added_on    = excluded.added_on,
			last_access = excluded.last_access,
			created_at  = excluded.created_at`,
		r.Hash, r.Name, r.Category, r.Magnet, r.TotalSize,
		string(r.State), boolToInt(r.Cached), r.TorBoxID,
		r.AddedOn, r.LastAccess, r.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("catalog: upsert release %q: %w", r.Hash, err)
	}

	// Replace file rows: delete existing, then insert fresh set.
	if _, err = tx.Exec(`DELETE FROM file WHERE hash = ?`, r.Hash); err != nil {
		return fmt.Errorf("catalog: delete files for %q: %w", r.Hash, err)
	}
	for _, f := range files {
		_, err = tx.Exec(
			`INSERT INTO file (hash, file_id, rel_path, size) VALUES (?, ?, ?, ?)`,
			f.Hash, f.FileID, f.RelPath, f.Size,
		)
		if err != nil {
			return fmt.Errorf("catalog: insert file (%q,%d): %w", f.Hash, f.FileID, err)
		}
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("catalog: upsert release commit: %w", err)
	}
	return nil
}

// GetRelease fetches a release and its files. Returns ErrNotFound when absent.
func (s *sqliteStore) GetRelease(hash string) (*Release, []File, error) {
	row := s.db.QueryRow(`
		SELECT hash, name, category, magnet, total_size, state, cached,
		       torbox_id, added_on, last_access, created_at
		FROM release WHERE hash = ?`, hash)

	r, err := scanRelease(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, ErrNotFound
		}
		return nil, nil, fmt.Errorf("catalog: get release %q: %w", hash, err)
	}

	files, err := s.queryFiles(hash)
	if err != nil {
		return nil, nil, err
	}
	return r, files, nil
}

// ListByCategory returns all releases for a given category.
func (s *sqliteStore) ListByCategory(category string) ([]*Release, error) {
	rows, err := s.db.Query(`
		SELECT hash, name, category, magnet, total_size, state, cached,
		       torbox_id, added_on, last_access, created_at
		FROM release WHERE category = ?`, category)
	if err != nil {
		return nil, fmt.Errorf("catalog: list by category %q: %w", category, err)
	}
	defer rows.Close()

	var releases []*Release
	for rows.Next() {
		r, err := scanReleaseRow(rows)
		if err != nil {
			return nil, fmt.Errorf("catalog: scan release row: %w", err)
		}
		releases = append(releases, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: iterate releases: %w", err)
	}
	return releases, nil
}

// SetState updates a release's state and torbox_id. Per the spec, torbox_id
// is set only while materialized; clearing it (0) happens when releasing.
func (s *sqliteStore) SetState(hash string, st State, torboxID int64) error {
	res, err := s.db.Exec(
		`UPDATE release SET state = ?, torbox_id = ? WHERE hash = ?`,
		string(st), torboxID, hash,
	)
	if err != nil {
		return fmt.Errorf("catalog: set state %q->%q: %w", hash, st, err)
	}
	return requireOneRow(res, "set state", hash)
}

// TouchAccess updates last_access for a release.
func (s *sqliteStore) TouchAccess(hash string, ts int64) error {
	res, err := s.db.Exec(
		`UPDATE release SET last_access = ? WHERE hash = ?`, ts, hash,
	)
	if err != nil {
		return fmt.Errorf("catalog: touch access %q: %w", hash, err)
	}
	return requireOneRow(res, "touch access", hash)
}

// IdleCandidates returns materialized releases whose last_access < before.
func (s *sqliteStore) IdleCandidates(before int64) ([]*Release, error) {
	rows, err := s.db.Query(`
		SELECT hash, name, category, magnet, total_size, state, cached,
		       torbox_id, added_on, last_access, created_at
		FROM release
		WHERE state = 'materialized' AND last_access < ?`, before)
	if err != nil {
		return nil, fmt.Errorf("catalog: idle candidates: %w", err)
	}
	defer rows.Close()
	return collectReleases(rows)
}

// OverMaxHold returns materialized releases whose added_on < before.
func (s *sqliteStore) OverMaxHold(before int64) ([]*Release, error) {
	rows, err := s.db.Query(`
		SELECT hash, name, category, magnet, total_size, state, cached,
		       torbox_id, added_on, last_access, created_at
		FROM release
		WHERE state = 'materialized' AND added_on < ?`, before)
	if err != nil {
		return nil, fmt.Errorf("catalog: over max hold: %w", err)
	}
	defer rows.Close()
	return collectReleases(rows)
}

// MaterializedIDs returns TorBox IDs for all materialized releases (ToS audit).
func (s *sqliteStore) MaterializedIDs() ([]int64, error) {
	rows, err := s.db.Query(
		`SELECT torbox_id FROM release WHERE state = 'materialized'`,
	)
	if err != nil {
		return nil, fmt.Errorf("catalog: materialized ids: %w", err)
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("catalog: scan torbox_id: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: iterate materialized ids: %w", err)
	}
	return ids, nil
}

// GetLink returns the cached DLLink for (hash, fileID). Returns ErrNotFound
// when absent.
func (s *sqliteStore) GetLink(hash string, fileID int) (*DLLink, error) {
	row := s.db.QueryRow(`
		SELECT hash, file_id, url, fetched_at, expires_at
		FROM dl_link WHERE hash = ? AND file_id = ?`, hash, fileID)

	var l DLLink
	err := row.Scan(&l.Hash, &l.FileID, &l.URL, &l.FetchedAt, &l.ExpiresAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("catalog: get link (%q,%d): %w", hash, fileID, err)
	}
	return &l, nil
}

// SetLink upserts a DLLink row (insert or replace on conflict).
func (s *sqliteStore) SetLink(l *DLLink) error {
	_, err := s.db.Exec(`
		INSERT INTO dl_link (hash, file_id, url, fetched_at, expires_at)
		VALUES (?, ?, ?, ?, ?)
		ON CONFLICT(hash, file_id) DO UPDATE SET
			url        = excluded.url,
			fetched_at = excluded.fetched_at,
			expires_at = excluded.expires_at`,
		l.Hash, l.FileID, l.URL, l.FetchedAt, l.ExpiresAt,
	)
	if err != nil {
		return fmt.Errorf("catalog: set link (%q,%d): %w", l.Hash, l.FileID, err)
	}
	return nil
}

// DeleteRelease deletes a release and, via ON DELETE CASCADE, its file and
// dl_link rows.
func (s *sqliteStore) DeleteRelease(hash string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("catalog: delete release begin tx: %w", err)
	}
	defer tx.Rollback() //nolint:errcheck

	res, err := tx.Exec(`DELETE FROM release WHERE hash = ?`, hash)
	if err != nil {
		return fmt.Errorf("catalog: delete release %q: %w", hash, err)
	}
	if err := requireOneRow(res, "delete release", hash); err != nil {
		return err
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("catalog: delete release commit: %w", err)
	}
	return nil
}

// Close closes the underlying database connection.
func (s *sqliteStore) Close() error {
	return s.db.Close()
}

// ---- helpers ---------------------------------------------------------------

// rowScanner is satisfied by both *sql.Row and *sql.Rows (for scanReleaseRow).
type rowScanner interface {
	Scan(dest ...any) error
}

func scanRelease(r rowScanner) (*Release, error) {
	var rel Release
	var state string
	var cached int
	err := r.Scan(
		&rel.Hash, &rel.Name, &rel.Category, &rel.Magnet,
		&rel.TotalSize, &state, &cached,
		&rel.TorBoxID, &rel.AddedOn, &rel.LastAccess, &rel.CreatedAt,
	)
	if err != nil {
		return nil, err
	}
	rel.State = State(state)
	rel.Cached = cached != 0
	return &rel, nil
}

// scanReleaseRow wraps *sql.Rows to satisfy rowScanner for scanRelease.
func scanReleaseRow(rows *sql.Rows) (*Release, error) {
	return scanRelease(rows)
}

func collectReleases(rows *sql.Rows) ([]*Release, error) {
	var releases []*Release
	for rows.Next() {
		r, err := scanReleaseRow(rows)
		if err != nil {
			return nil, fmt.Errorf("catalog: scan release: %w", err)
		}
		releases = append(releases, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: iterate releases: %w", err)
	}
	return releases, nil
}

func (s *sqliteStore) queryFiles(hash string) ([]File, error) {
	rows, err := s.db.Query(
		`SELECT hash, file_id, rel_path, size FROM file WHERE hash = ? ORDER BY file_id`,
		hash,
	)
	if err != nil {
		return nil, fmt.Errorf("catalog: query files for %q: %w", hash, err)
	}
	defer rows.Close()

	var files []File
	for rows.Next() {
		var f File
		if err := rows.Scan(&f.Hash, &f.FileID, &f.RelPath, &f.Size); err != nil {
			return nil, fmt.Errorf("catalog: scan file: %w", err)
		}
		files = append(files, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("catalog: iterate files: %w", err)
	}
	return files, nil
}

func requireOneRow(res sql.Result, op, hash string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("catalog: %s %q rows affected: %w", op, hash, err)
	}
	if n == 0 {
		return fmt.Errorf("catalog: %s %q: %w", op, hash, ErrNotFound)
	}
	return nil
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
