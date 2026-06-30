package store

import (
	"encoding/json"
	"time"

	"github.com/robertkoller/engrex/internal/db"
)

// Chunk is a row returned from search.
type Chunk struct {
	ID        int64
	Text      string
	Source    string
	CreatedAt time.Time
	Distance  float64
}

// Store wraps the database and exposes chunk insert and search operations.
type Store struct {
	db *db.DB
}

// New returns a Store backed by the given database.
func New(database *db.DB) *Store {
	return &Store{db: database}
}

const deduplicationThreshold = 0.35

// Insert writes a text chunk and its embedding vector to the database.
// Returns true if inserted, false if skipped as a near-duplicate.
func (store *Store) Insert(text, source string, vec []float32) (bool, error) {
	blob, _ := json.Marshal(vec)

	var nearestDistance float64
	err := store.db.QueryRow(`
		SELECT distance FROM vec_chunks
		WHERE embedding MATCH ?
		ORDER BY distance
		LIMIT 1
	`, string(blob)).Scan(&nearestDistance)
	if err == nil && nearestDistance <= deduplicationThreshold {
		return false, nil
	}

	tx, _ := store.db.Begin()

	response, err := tx.Exec(`INSERT INTO chunks(text, source) VALUES (?, ?)`, text, source)
	if err != nil {
		tx.Rollback() //nolint:errcheck
		return false, err
	}

	id, _ := response.LastInsertId()
	_, err = tx.Exec(`INSERT INTO vec_chunks(rowid, embedding) VALUES (?, ?)`, id, string(blob))

	if err != nil {
		tx.Rollback() //nolint:errcheck
		return false, err
	}

	return true, tx.Commit()
}

// List returns every chunk in the database ordered by most recent first.
func (store *Store) List() ([]Chunk, error) {
	rows, err := store.db.Query(`
		SELECT id, text, source, created_at
		FROM chunks
		ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var chunks []Chunk
	for rows.Next() {
		var chunk Chunk
		if err := rows.Scan(&chunk.ID, &chunk.Text, &chunk.Source, &chunk.CreatedAt); err != nil {
			return nil, err
		}
		chunks = append(chunks, chunk)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return chunks, nil
}

// Clear deletes all chunks and vectors from the database.
func (store *Store) Clear() error {
	tx, _ := store.db.Begin()
	if _, err := tx.Exec(`DELETE FROM vec_chunks`); err != nil {
		tx.Rollback() //nolint:errcheck
		return err
	}
	if _, err := tx.Exec(`DELETE FROM chunks`); err != nil {
		tx.Rollback() //nolint:errcheck
		return err
	}
	return tx.Commit()
}

// RawSearch returns all chunks with their raw distances, no filtering applied.
// Used for calibrating distance thresholds.
func (store *Store) RawSearch(vec []float32) ([]Chunk, error) {
	jsonVector, _ := json.Marshal(vec)

	rows, err := store.db.Query(`
		SELECT v.rowid, v.distance, c.text, c.source, c.created_at
		FROM (
			SELECT rowid, distance
			FROM vec_chunks
			WHERE embedding MATCH ?
			ORDER BY distance
			LIMIT 20
		) v
		JOIN chunks c ON c.id = v.rowid`, string(jsonVector))
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var chunks []Chunk
	for rows.Next() {
		var chunk Chunk
		if err := rows.Scan(&chunk.ID, &chunk.Distance, &chunk.Text, &chunk.Source, &chunk.CreatedAt); err != nil {
			return nil, err
		}
		chunks = append(chunks, chunk)
	}
	return chunks, rows.Err()
}

// Search performs a K Nearest Neighbors search for the most similar chunks
// I like maxDistance to be 0.42 dont ask me how i chose that its just feeling
func (store *Store) Search(vec []float32, maxDistance float64, topK int) ([]Chunk, error) {
	jsonVector, _ := json.Marshal(vec)

	rows, err := store.db.Query(`
	SELECT v.rowid, v.distance, c.text, c.source, c.created_at
	FROM (
		SELECT rowid, distance
		FROM vec_chunks
		WHERE embedding MATCH ?
		ORDER BY distance
		LIMIT 20
	) v
	JOIN chunks c ON c.id = v.rowid`, string(jsonVector))

	if err != nil {
		return nil, err
	}

	defer rows.Close()

	var outputChunks []Chunk
	for rows.Next() {
		var chunk Chunk
		err := rows.Scan(&chunk.ID, &chunk.Distance, &chunk.Text, &chunk.Source, &chunk.CreatedAt)
		if err != nil {
			return nil, err
		}
		if chunk.Distance <= maxDistance {
			outputChunks = append(outputChunks, chunk)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	if len(outputChunks) > topK {
		outputChunks = outputChunks[:topK]
	}
	return outputChunks, nil
}
