package db

import (
	"database/sql"
	"fmt"
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

// Migrate brings the schema up to date, running any migrations the database hasn't
// seen yet.
func (d *DB) Migrate() error {
	return migrate(d.DB)
}

// Rebuild recreates the whole schema from scratch after the caller has dropped the
// tables. Resetting user_version first is what makes this work — migrations are
// skipped once they've been stamped, so without the reset a dropped database would
// come back empty of tables but still claiming to be fully migrated.
func (d *DB) Rebuild() error {
	if err := setSchemaVersion(d.DB, 0); err != nil {
		return err
	}
	return migrate(d.DB)
}

// SchemaVersion returns the migration version the database is currently at.
func (d *DB) SchemaVersion() (int, error) {
	return schemaVersion(d.DB)
}

// migration is one ordered, forward-only schema step. Each runs exactly once, inside a
// transaction, and bumps PRAGMA user_version on success.
type migration struct {
	version int
	name    string
	apply   func(*sql.Tx) error
}

// migrations is the ordered schema history. Append only — never edit or reorder an
// entry that has shipped, or databases in the field will disagree with the code about
// what version 3 (say) actually means.
var migrations = []migration{
	{version: 1, name: "initial schema", apply: migrateInitial},
	{version: 2, name: "chunk metadata columns", apply: migrateChunkMetadata},
	{version: 3, name: "cosine vector index", apply: migrateCosineVectors},
}

// SchemaVersion is the version a freshly migrated database ends up at.
const SchemaVersion = 3

// migrate brings the database up to SchemaVersion by running each pending migration in
// order. Existing databases created before versioning was introduced are detected by
// their tables already existing and are stamped at version 1 rather than re-created.
func migrate(database *sql.DB) error {
	current, err := schemaVersion(database)
	if err != nil {
		return err
	}

	// Databases predating the migration table report user_version 0 but already have the
	// v1 schema. Stamp them so migrateInitial isn't re-run against live data.
	if current == 0 {
		existing, err := hasChunksTable(database)
		if err != nil {
			return err
		}
		if existing {
			if err := setSchemaVersion(database, 1); err != nil {
				return err
			}
			current = 1
		}
	}

	for _, step := range migrations {
		if step.version <= current {
			continue
		}
		if err := runMigration(database, step); err != nil {
			return fmt.Errorf("migration %d (%s): %w", step.version, step.name, err)
		}
	}

	return backfillFTS(database)
}

// runMigration applies one migration in a transaction and stamps user_version on
// success, so a failure part-way leaves the database at its previous version rather
// than in a half-migrated state.
func runMigration(database *sql.DB, step migration) error {
	tx, err := database.Begin()
	if err != nil {
		return err
	}
	if err := step.apply(tx); err != nil {
		tx.Rollback() //nolint:errcheck
		return err
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	return setSchemaVersion(database, step.version)
}

func schemaVersion(database *sql.DB) (int, error) {
	var version int
	if err := database.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil {
		return 0, err
	}
	return version, nil
}

// setSchemaVersion stamps user_version. PRAGMA doesn't accept bound parameters, so the
// value is formatted in — safe here because it only ever comes from the migrations
// table above, never from user input.
func setSchemaVersion(database *sql.DB, version int) error {
	_, err := database.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, version))
	return err
}

func hasChunksTable(database *sql.DB) (bool, error) {
	var name string
	err := database.QueryRow(
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'chunks'`).Scan(&name)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// migrateInitial creates the schema as it stood before versioning: chunks and their
// vectors, the semantic graph edges, document ingest hashes, and the FTS5 index.
func migrateInitial(tx *sql.Tx) error {
	_, err := tx.Exec(`
		CREATE TABLE IF NOT EXISTS chunks (
			id         INTEGER PRIMARY KEY AUTOINCREMENT,
			text       TEXT NOT NULL,
			source     TEXT NOT NULL DEFAULT 'cli',
			origin     TEXT NOT NULL DEFAULT '',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		);

		CREATE VIRTUAL TABLE IF NOT EXISTS vec_chunks USING vec0(
			embedding float[768] distance_metric=cosine
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
	`)
	return err
}

// migrateChunkMetadata adds the structural metadata the section-aware chunker emits.
// heading_path is the chain of enclosing headings ("Setup > Prerequisites"), chunk_index
// is the chunk's ordinal within its document, doc_title is the document's display name,
// and content_type records how the text was split ("markdown", "code:go", "text").
//
// Backfilled as empty/zero for existing rows: they were chunked before structure was
// preserved, so there is no correct value to infer. They pick up real metadata on the
// next re-ingest.
func migrateChunkMetadata(tx *sql.Tx) error {
	statements := []string{
		`ALTER TABLE chunks ADD COLUMN heading_path  TEXT    NOT NULL DEFAULT ''`,
		`ALTER TABLE chunks ADD COLUMN chunk_index   INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE chunks ADD COLUMN doc_title     TEXT    NOT NULL DEFAULT ''`,
		`ALTER TABLE chunks ADD COLUMN content_type  TEXT    NOT NULL DEFAULT ''`,
		`CREATE INDEX IF NOT EXISTS chunks_by_source ON chunks(source, chunk_index)`,
	}
	for _, statement := range statements {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	return nil
}

// migrateCosineVectors rebuilds vec_chunks with an explicit cosine distance metric.
//
// vec0 defaults to L2, which only ranks identically to cosine for unit-length vectors —
// and Ollama's embeddings are not normalized, so ranking was biased toward longer
// chunks. A virtual table's declaration can't be altered, so the table is dropped and
// recreated empty.
//
// Every stored vector is discarded here rather than copied. That is deliberate: this
// migration ships alongside the embedder switching to nomic-embed-text's required
// search_document:/search_query: prefixes and unit normalization, so the old vectors
// are in a different space and would poison results if kept. Clearing the documents
// hash table makes re-ingest see every document as changed. Run `engrex reindex` after
// upgrading to re-embed from the chunk text already in the database.
func migrateCosineVectors(tx *sql.Tx) error {
	statements := []string{
		`DROP TABLE IF EXISTS vec_chunks`,
		`CREATE VIRTUAL TABLE vec_chunks USING vec0(embedding float[768] distance_metric=cosine)`,
		`DELETE FROM relations`,
		`DELETE FROM documents`,
	}
	for _, statement := range statements {
		if _, err := tx.Exec(statement); err != nil {
			return err
		}
	}
	return nil
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
