package db

import (
	"database/sql"
	"os"
	"path/filepath"

	vec "github.com/asg017/sqlite-vec-go-bindings/cgo"
	_ "github.com/mattn/go-sqlite3" //nolint:blank-imports — registers the "sqlite3" driver
)

// DB wraps a *sql.DB and owns the connection lifecycle.
type DB struct {
	*sql.DB
}

func init() {
	vec.Auto()
}

// Open opens (or creates) the engrex SQLite database at ~/.engrex/engrex.db,
// loads the sqlite-vec extension, and runs all migrations shenanigans
func Open() (*DB, error) {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".engrex")
	os.MkdirAll(dir, 0700)
	path := filepath.Join(dir, "engrex.db")

	sqlDB, err := sql.Open("sqlite3", path)
	if err != nil {
		return nil, err
	}
	if err := sqlDB.Ping(); err != nil {
		return nil, err
	}
	if err := migrate(sqlDB); err != nil {
		return nil, err
	}
	return &DB{sqlDB}, nil
}

// Migrate re-runs schema creation. Exported so callers (e.g. clear) can rebuild
// the schema after dropping tables.
func (d *DB) Migrate() error {
	return migrate(d.DB)
}

// migrate creates all tables and virtual tables on first run.
func migrate(db *sql.DB) error {
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS chunks (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			text       TEXT NOT NULL,
			source     TEXT NOT NULL DEFAULT 'cli',
			origin     TEXT NOT NULL DEFAULT '',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);

		CREATE VIRTUAL TABLE IF NOT EXISTS vec_chunks USING vec0(
			embedding float[768]
		);

		CREATE TABLE IF NOT EXISTS relations (
    		source_id INTEGER NOT NULL,
    		target_id INTEGER NOT NULL,
    		distance  REAL NOT NULL,
    		PRIMARY KEY (source_id, target_id)
		);

		CREATE TABLE IF NOT EXISTS documents (
			doc_key    TEXT PRIMARY KEY,
			hash       TEXT NOT NULL,
			updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);

		-- Full-text (BM25) index over chunk text, used alongside vector search for
		-- hybrid retrieval. External-content table: it indexes chunks.text without
		-- duplicating it, and the triggers below keep it in sync on every write.
		CREATE VIRTUAL TABLE IF NOT EXISTS fts_chunks USING fts5(
			text,
			content='chunks',
			content_rowid='id',
			tokenize='porter unicode61'
		);

		CREATE TRIGGER IF NOT EXISTS chunks_after_insert AFTER INSERT ON chunks BEGIN
			INSERT INTO fts_chunks(rowid, text) VALUES (new.id, new.text);
		END;
		CREATE TRIGGER IF NOT EXISTS chunks_after_delete AFTER DELETE ON chunks BEGIN
			INSERT INTO fts_chunks(fts_chunks, rowid, text) VALUES('delete', old.id, old.text);
		END;
		CREATE TRIGGER IF NOT EXISTS chunks_after_update AFTER UPDATE ON chunks BEGIN
			INSERT INTO fts_chunks(fts_chunks, rowid, text) VALUES('delete', old.id, old.text);
			INSERT INTO fts_chunks(rowid, text) VALUES (new.id, new.text);
		END;
	`); err != nil {
		return err
	}
	return backfillFTS(db)
}

// backfillFTS populates the full-text index from any chunks that predate it. The triggers
// keep it in sync going forward, so this only does work when the index is empty but chunks
// already exist — i.e. the first run after FTS was added to an existing database. On every
// later startup it's two cheap COUNT queries and a no-op.
func backfillFTS(db *sql.DB) error {
	var ftsCount, chunkCount int
	if err := db.QueryRow(`SELECT count(*) FROM fts_chunks`).Scan(&ftsCount); err != nil {
		return err
	}
	if err := db.QueryRow(`SELECT count(*) FROM chunks`).Scan(&chunkCount); err != nil {
		return err
	}
	if ftsCount == 0 && chunkCount > 0 {
		if _, err := db.Exec(`INSERT INTO fts_chunks(fts_chunks) VALUES('rebuild')`); err != nil {
			return err
		}
	}
	return nil
}
