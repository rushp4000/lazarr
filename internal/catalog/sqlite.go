package catalog

import (
	"database/sql"
	"errors"
	"fmt"
	"strings"

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
    hash             TEXT    PRIMARY KEY,
    name             TEXT    NOT NULL DEFAULT '',
    category         TEXT    NOT NULL DEFAULT '',
    magnet           TEXT    NOT NULL DEFAULT '',
    total_size       INTEGER NOT NULL DEFAULT 0,
    state            TEXT    NOT NULL DEFAULT 'virtual',
    cached           INTEGER NOT NULL DEFAULT 0,
    torbox_id        INTEGER NOT NULL DEFAULT 0,
    added_on         INTEGER NOT NULL DEFAULT 0,
    last_access      INTEGER NOT NULL DEFAULT 0,
    materialized_at  INTEGER NOT NULL DEFAULT 0,
    created_at       INTEGER NOT NULL DEFAULT 0,
    cache_status     TEXT    NOT NULL DEFAULT '',
    last_cache_check INTEGER NOT NULL DEFAULT 0
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

// materializedAtIndexSQL is created AFTER the materialized_at column is ensured (it may be
// added by ALTER on a pre-existing DB), so it cannot live in schemaSQL above.
const materializedAtIndexSQL = `
CREATE INDEX IF NOT EXISTS idx_release_materialized_at ON release(state, materialized_at);
`

func applyMigrations(db *sql.DB) error {
	if _, err := db.Exec(schemaSQL); err != nil {
		return fmt.Errorf("catalog: apply migrations: %w", err)
	}
	// B1: materialized_at drives the max-hold reaper from materialize time (not grab time).
	// On a fresh DB schemaSQL already created the column; on a pre-existing DB (the canary)
	// the table exists without it, so add it idempotently here.
	if err := ensureColumn(db, "release", "materialized_at", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if _, err := db.Exec(materializedAtIndexSQL); err != nil {
		return fmt.Errorf("catalog: create materialized_at index: %w", err)
	}
	// Repair-scan columns: added after initial schema so existing DBs get them.
	if err := ensureColumn(db, "release", "cache_status", "TEXT NOT NULL DEFAULT ''"); err != nil {
		return err
	}
	if err := ensureColumn(db, "release", "last_cache_check", "INTEGER NOT NULL DEFAULT 0"); err != nil {
		return err
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_release_cache_status ON release(cache_status)`); err != nil {
		return fmt.Errorf("catalog: create cache_status index: %w", err)
	}
	// Backfill pre-existing materialized rows to NOW so they are not instantly
	// reap-eligible (materialized_at=0 would otherwise read as "the epoch", i.e. ancient).
	// New materialized rows get a real stamp via SetState; the > 0 guard in OverMaxHold
	// keeps any that slip through (e.g. a crash mid-backfill) out of the reap set.
	if _, err := db.Exec(
		`UPDATE release SET materialized_at = CAST(strftime('%s','now') AS INTEGER)
		 WHERE state = 'materialized' AND materialized_at = 0`,
	); err != nil {
		return fmt.Errorf("catalog: backfill materialized_at: %w", err)
	}
	return nil
}

// ensureColumn adds a column to a table if it is not already present. Idempotent: a
// re-run (or a fresh DB where schemaSQL already created the column) is a no-op.
func ensureColumn(db *sql.DB, table, column, decl string) error {
	rows, err := db.Query(fmt.Sprintf("PRAGMA table_info(%s)", table))
	if err != nil {
		return fmt.Errorf("catalog: table_info(%s): %w", table, err)
	}
	defer rows.Close()
	for rows.Next() {
		var (
			cid        int
			name, typ  string
			notNull    int
			dflt       sql.NullString
			primaryKey int
		)
		if err := rows.Scan(&cid, &name, &typ, &notNull, &dflt, &primaryKey); err != nil {
			return fmt.Errorf("catalog: scan table_info(%s): %w", table, err)
		}
		if name == column {
			return rows.Err() // already present
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("catalog: iterate table_info(%s): %w", table, err)
	}
	if _, err := db.Exec(fmt.Sprintf("ALTER TABLE %s ADD COLUMN %s %s", table, column, decl)); err != nil {
		return fmt.Errorf("catalog: add column %s.%s: %w", table, column, err)
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
			 torbox_id, added_on, last_access, materialized_at, created_at,
			 cache_status, last_cache_check)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(hash) DO UPDATE SET
			name             = excluded.name,
			category         = excluded.category,
			magnet           = excluded.magnet,
			total_size       = excluded.total_size,
			state            = excluded.state,
			cached           = excluded.cached,
			torbox_id        = excluded.torbox_id,
			added_on         = excluded.added_on,
			last_access      = excluded.last_access,
			materialized_at  = excluded.materialized_at,
			created_at       = excluded.created_at,
			cache_status     = excluded.cache_status,
			last_cache_check = excluded.last_cache_check`,
		r.Hash, r.Name, r.Category, r.Magnet, r.TotalSize,
		string(r.State), boolToInt(r.Cached), r.TorBoxID,
		r.AddedOn, r.LastAccess, r.MaterializedAt, r.CreatedAt,
		string(r.CacheStatus), r.LastCacheCheck,
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
		       torbox_id, added_on, last_access, materialized_at, created_at,
		       cache_status, last_cache_check
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
		       torbox_id, added_on, last_access, materialized_at, created_at,
		       cache_status, last_cache_check
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

const defaultListLimit = 50

// ListReleases returns releases matching f with the total matching count. Used by
// the Web UI table; supports text search, state/category filters, and pagination.
func (s *sqliteStore) ListReleases(f ReleaseFilter) ([]*Release, int, error) {
	if f.Limit <= 0 {
		f.Limit = defaultListLimit
	}

	// Build WHERE predicates. We use LIKE for the q search so it works without
	// loading extensions; case sensitivity is acceptable for hashes.
	var where []string
	var args []any
	if f.Q != "" {
		pat := "%" + f.Q + "%"
		where = append(where, "(name LIKE ? OR hash LIKE ?)")
		args = append(args, pat, pat)
	}
	if f.State != "" {
		where = append(where, "state = ?")
		args = append(args, string(f.State))
	}
	if f.Category != "" {
		where = append(where, "category = ?")
		args = append(args, f.Category)
	}

	clause := ""
	if len(where) > 0 {
		clause = "WHERE " + strings.Join(where, " AND ")
	}

	// Total count.
	var total int
	if err := s.db.QueryRow(
		"SELECT COUNT(*) FROM release "+clause, args...,
	).Scan(&total); err != nil {
		return nil, 0, fmt.Errorf("catalog: list releases count: %w", err)
	}

	// Paginated rows, newest-grabbed first.
	rows, err := s.db.Query(
		`SELECT hash, name, category, magnet, total_size, state, cached,
		        torbox_id, added_on, last_access, materialized_at, created_at,
		        cache_status, last_cache_check
		 FROM release `+clause+`
		 ORDER BY added_on DESC
		 LIMIT ? OFFSET ?`,
		append(args, f.Limit, f.Offset)...,
	)
	if err != nil {
		return nil, 0, fmt.Errorf("catalog: list releases: %w", err)
	}
	defer rows.Close()
	rels, err := collectReleases(rows)
	if err != nil {
		return nil, 0, err
	}
	return rels, total, nil
}

// SetState updates a release's state and torbox_id. Per the spec, torbox_id
// is set only while materialized; clearing it (0) happens when releasing.
//
// materialized_at (B1) is stamped to NOW when entering StateMaterialized and zeroed
// on every other transition (virtual/error) so the max-hold reaper measures the hold
// window from materialize time, not grab time.
func (s *sqliteStore) SetState(hash string, st State, torboxID int64) error {
	var (
		res sql.Result
		err error
	)
	if st == StateMaterialized {
		res, err = s.db.Exec(
			`UPDATE release SET state = ?, torbox_id = ?,
			        materialized_at = CAST(strftime('%s','now') AS INTEGER)
			 WHERE hash = ?`,
			string(st), torboxID, hash,
		)
	} else {
		res, err = s.db.Exec(
			`UPDATE release SET state = ?, torbox_id = ?, materialized_at = 0 WHERE hash = ?`,
			string(st), torboxID, hash,
		)
	}
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
		       torbox_id, added_on, last_access, materialized_at, created_at,
		       cache_status, last_cache_check
		FROM release
		WHERE state = 'materialized' AND last_access < ?`, before)
	if err != nil {
		return nil, fmt.Errorf("catalog: idle candidates: %w", err)
	}
	defer rows.Close()
	return collectReleases(rows)
}

// OverMaxHold returns materialized releases whose materialized_at < before. The window
// is measured from materialize time (B1): a release grabbed long before its first
// playback must not be an instant max-hold candidate the moment it materializes. The
// materialized_at > 0 guard excludes any unstamped row (defence-in-depth vs the epoch).
func (s *sqliteStore) OverMaxHold(before int64) ([]*Release, error) {
	rows, err := s.db.Query(`
		SELECT hash, name, category, magnet, total_size, state, cached,
		       torbox_id, added_on, last_access, materialized_at, created_at,
		       cache_status, last_cache_check
		FROM release
		WHERE state = 'materialized' AND materialized_at > 0 AND materialized_at < ?`, before)
	if err != nil {
		return nil, fmt.Errorf("catalog: over max hold: %w", err)
	}
	defer rows.Close()
	return collectReleases(rows)
}

// MaterializedReleases returns all releases currently in StateMaterialized (B2 reconcile).
func (s *sqliteStore) MaterializedReleases() ([]*Release, error) {
	rows, err := s.db.Query(`
		SELECT hash, name, category, magnet, total_size, state, cached,
		       torbox_id, added_on, last_access, materialized_at, created_at,
		       cache_status, last_cache_check
		FROM release WHERE state = 'materialized'`)
	if err != nil {
		return nil, fmt.Errorf("catalog: materialized releases: %w", err)
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

// ListAllHashes returns every hash in the catalog (repair scanner batch input).
func (s *sqliteStore) ListAllHashes() ([]string, error) {
	rows, err := s.db.Query(`SELECT hash FROM release`)
	if err != nil {
		return nil, fmt.Errorf("catalog: list all hashes: %w", err)
	}
	defer rows.Close()
	var hashes []string
	for rows.Next() {
		var h string
		if err := rows.Scan(&h); err != nil {
			return nil, fmt.Errorf("catalog: scan hash: %w", err)
		}
		hashes = append(hashes, h)
	}
	return hashes, rows.Err()
}

// SetCacheStatus updates the availability status of a release (repair scanner output).
func (s *sqliteStore) SetCacheStatus(hash string, status CacheStatus, checkedAt int64) error {
	res, err := s.db.Exec(
		`UPDATE release SET cache_status = ?, last_cache_check = ? WHERE hash = ?`,
		string(status), checkedAt, hash,
	)
	if err != nil {
		return fmt.Errorf("catalog: set cache status %q: %w", hash, err)
	}
	return requireOneRow(res, "set cache status", hash)
}

// ListEvicted returns releases whose content is no longer available on TorBox's CDN,
// newest-grabbed first. Used by the repair tab in the Web UI.
func (s *sqliteStore) ListEvicted() ([]*Release, error) {
	rows, err := s.db.Query(`
		SELECT hash, name, category, magnet, total_size, state, cached,
		       torbox_id, added_on, last_access, materialized_at, created_at,
		       cache_status, last_cache_check
		FROM release WHERE cache_status = 'evicted'
		ORDER BY added_on DESC`)
	if err != nil {
		return nil, fmt.Errorf("catalog: list evicted: %w", err)
	}
	defer rows.Close()
	return collectReleases(rows)
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
	var cacheStatus string
	err := r.Scan(
		&rel.Hash, &rel.Name, &rel.Category, &rel.Magnet,
		&rel.TotalSize, &state, &cached,
		&rel.TorBoxID, &rel.AddedOn, &rel.LastAccess, &rel.MaterializedAt, &rel.CreatedAt,
		&cacheStatus, &rel.LastCacheCheck,
	)
	if err != nil {
		return nil, err
	}
	rel.State = State(state)
	rel.Cached = cached != 0
	rel.CacheStatus = CacheStatus(cacheStatus)
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
