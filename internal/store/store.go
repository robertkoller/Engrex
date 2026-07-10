package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/robertkoller/engrex/internal/db"
)

var ErrInvalidDelete = errors.New("Invalid delete syntax. Use engrex delete ?, ?, ?-?, etc...")

// Chunk is a row returned from search.
type Chunk struct {
	ID        int64
	Text      string
	Source    string
	Origin    string
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
func (store *Store) Insert(text, source, origin string, vec []float32) (bool, error) {
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

	response, err := tx.Exec(`INSERT INTO chunks(text, source, origin) VALUES (?, ?, ?)`, text, source, origin)
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
		SELECT id, text, source, origin, created_at
		FROM chunks
		ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var chunks []Chunk
	for rows.Next() {
		var chunk Chunk
		if err := rows.Scan(&chunk.ID, &chunk.Text, &chunk.Source, &chunk.Origin, &chunk.CreatedAt); err != nil {
			return nil, err
		}
		chunks = append(chunks, chunk)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return chunks, nil
}

// Clear drops and recreates the tables, wiping all data AND rebuilding the schema.
// Dropping/recreating (rather than DELETE) means schema changes — like a new column
// — take effect, and the change propagates to the daemon's live connection too.
func (store *Store) Clear() error {
	if _, err := store.db.Exec(`DROP TABLE IF EXISTS vec_chunks`); err != nil {
		return err
	}
	if _, err := store.db.Exec(`DROP TABLE IF EXISTS chunks`); err != nil {
		return err
	}
	return store.db.Migrate()
}

// Deletes user inputted ids from the engrex store
func (store *Store) Delete(args string) error {
	ids, err := getDeletionIDs(args)
	if err != nil {
		return err
	}

	placeholders := make([]string, len(ids))
	for index := range ids {
		placeholders[index] = "?"
	}
	inClause := strings.Join(placeholders, ", ")
	queryChunk := fmt.Sprintf("DELETE FROM chunks WHERE id IN (%s)", inClause)
	queryVec := fmt.Sprintf("DELETE FROM vec_chunks WHERE rowid IN (%s)", inClause)

	tx, _ := store.db.Begin()

	if _, err := tx.Exec(queryVec, ids...); err != nil {
		tx.Rollback() //nolint:errcheck
		return err
	}

	if _, err := tx.Exec(queryChunk, ids...); err != nil {
		tx.Rollback() //nolint:errcheck
		return err
	}

	return tx.Commit()

}

// Returns the ids from the user inputted engrex delete function or an error if they are formatted wrongly
// The format should follow engrex delete ..., where ... can equal ?, or ?-?, or a mixture of the two seperated by commas
// Since the db.Exec takes []any not []int we format the ids and return them as []any
func getDeletionIDs(args string) ([]any, error) {
	var ids []any
	hashSet := make(map[int]struct{}) // we make set to not overlap entries

	split := strings.Split(args, ",")
	for _, entry := range split {
		entry = strings.TrimSpace(entry)
		number, isInt := isInteger(entry)
		if isInt {
			if _, found := hashSet[number]; !found {
				hashSet[number] = struct{}{}
				ids = append(ids, number)
			}
		} else {
			dashSplit := strings.Split(entry, "-")
			if len(dashSplit) != 2 {
				return nil, ErrInvalidDelete
			} else {
				startNumber, isStartInt := isInteger(dashSplit[0])
				endNumber, isEndInt := isInteger(dashSplit[1])

				if !isEndInt || !isStartInt {
					return nil, ErrInvalidDelete
				}

				if startNumber > endNumber { // fancy smancy number swap
					startNumber = startNumber + endNumber
					endNumber = startNumber - endNumber
					startNumber = startNumber - endNumber
				}

				for i := startNumber; i <= endNumber; i++ {
					if _, found := hashSet[i]; !found {
						hashSet[i] = struct{}{}
						ids = append(ids, i)
					}
				}
			}
		}
	}

	return ids, nil

}

// RawSearch returns all chunks with their raw distances, no filtering applied.
// Used for calibrating distance thresholds.
func (store *Store) RawSearch(vec []float32) ([]Chunk, error) {
	jsonVector, _ := json.Marshal(vec)

	rows, err := store.db.Query(`
		SELECT v.rowid, v.distance, c.text, c.source, c.origin, c.created_at
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
		if err := rows.Scan(&chunk.ID, &chunk.Distance, &chunk.Text, &chunk.Source, &chunk.Origin, &chunk.CreatedAt); err != nil {
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
	SELECT v.rowid, v.distance, c.text, c.source, c.origin, c.created_at
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
		err := rows.Scan(&chunk.ID, &chunk.Distance, &chunk.Text, &chunk.Source, &chunk.Origin, &chunk.CreatedAt)
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

func isInteger(s string) (int, bool) {
	number, err := strconv.Atoi(s)
	if err == nil {
		return number, true
	}
	return 0, false
}
