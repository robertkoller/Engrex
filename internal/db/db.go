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

// migrate creates all tables and virtual tables on first run.
func migrate(db *sql.DB) error {
	_, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS chunks (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			text       TEXT NOT NULL,
			source     TEXT NOT NULL DEFAULT 'cli',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);

		CREATE VIRTUAL TABLE IF NOT EXISTS vec_chunks USING vec0(
			embedding float[768]
		);
	`)
	return err
}
