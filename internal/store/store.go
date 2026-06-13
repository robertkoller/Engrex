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

// Insert writes a text chunk and its embedding vector to the database.
func (store *Store) Insert(text, source string, vec []float32) error {
	tx, _ := store.db.Begin()

	res, err := tx.Exec(`INSERT INTO chunks(text, source) VALUES (?, ?)`, text, source)

	if err != nil {
		return err
	}

	// We are inserting ts vector into with a pointer to the original text
	// Json encoding is needed for sql
	id, _ := res.LastInsertId()
	blob, _ := json.Marshal(vec)
	_, err = tx.Exec(`INSERT INTO vec_chunks(rowid, embedding) VALUES (?, ?)`, id, string(blob))

	if err != nil {
		return err
	}

	return tx.Commit()
}

// Search performs a K Nearest Neighbors search for the most similar chunks
// I like maxDistance to be 0.42 dont ask me how i chose that its just feeling
func (store *Store) Search(vec []float32, maxDistance float64) ([]Chunk, error) {
	jsonVector, _ := json.Marshal(vec)

	rows, err := store.db.Query(`
	SELECT v.rowid, v.distance, c.text, c.source, c.created_at
	FROM vec_chunks v
	JOIN chunks c ON c.id = v.rowid
	WHERE v.embedding MATCH ?
	ORDER BY v.distance
	LIMIT ?`, string(jsonVector), 20)

	if err != nil {
		return nil, err
	}

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
	return outputChunks, nil
}
