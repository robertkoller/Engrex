package store

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/url"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/robertkoller/engrex/internal/db"
)

var ErrInvalidDelete = errors.New("Invalid delete syntax. Use engrex delete ?, ?, ?-?, etc...")

const maxEdges = 8
const edgeThreshold = 0.8

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

	if err := store.insertChunk(text, source, origin, string(blob)); err != nil {
		return false, err
	}
	return true, nil
}

// InsertDocumentChunk stores a chunk unconditionally, skipping the near-duplicate
// check. Used when re-ingesting a whole document (which is deleted and rewritten as a
// unit): every chunk must be stored even if it resembles a chunk in another document,
// otherwise a re-ingested file could silently lose chunks to the cross-document dedup.
func (store *Store) InsertDocumentChunk(text, source, origin string, vec []float32) error {
	blob, _ := json.Marshal(vec)
	return store.insertChunk(text, source, origin, string(blob))
}

// insertChunk writes one chunk + its vector in a transaction, then links it to its
// nearest neighbors. blob is the JSON-encoded embedding.
func (store *Store) insertChunk(text, source, origin, blob string) error {
	tx, err := store.db.Begin()
	if err != nil {
		return err
	}

	response, err := tx.Exec(`INSERT INTO chunks(text, source, origin) VALUES (?, ?, ?)`, text, source, origin)
	if err != nil {
		tx.Rollback() //nolint:errcheck
		return err
	}

	id, _ := response.LastInsertId()
	if _, err := tx.Exec(`INSERT INTO vec_chunks(rowid, embedding) VALUES (?, ?)`, id, blob); err != nil {
		tx.Rollback() //nolint:errcheck
		return err
	}

	if err := tx.Commit(); err != nil {
		return err
	}

	if err := store.relate(id, blob); err != nil {
		log.Print(err)
	}
	return nil
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

// Clear drops and recreates the tables, wiping all data and rebuilding the schema.
func (store *Store) Clear() error {
	// Drop the FTS index first, then the content table (which also drops its triggers).
	if _, err := store.db.Exec(`DROP TABLE IF EXISTS fts_chunks`); err != nil {
		return err
	}
	if _, err := store.db.Exec(`DROP TABLE IF EXISTS vec_chunks`); err != nil {
		return err
	}
	if _, err := store.db.Exec(`DROP TABLE IF EXISTS chunks`); err != nil {
		return err
	}
	return store.db.Migrate()
}

// KeywordSearch runs a BM25 full-text search over chunk text via the FTS5 index and
// returns the best-matching chunks, most relevant first. query must already be a valid
// FTS5 MATCH expression — see rag.toFTSQuery, which quotes user terms so their
// punctuation can't be misread as query syntax.
func (store *Store) KeywordSearch(query string, limit int) ([]Chunk, error) {
	rows, err := store.db.Query(`
		SELECT c.id, c.text, c.source, c.origin, c.created_at
		FROM fts_chunks
		JOIN chunks c ON c.id = fts_chunks.rowid
		WHERE fts_chunks MATCH ?
		ORDER BY bm25(fts_chunks)
		LIMIT ?`, query, limit)
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
	return chunks, rows.Err()
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
	queryRel := fmt.Sprintf("DELETE FROM relations WHERE source_id IN (%s) OR target_id IN (%s)", inClause, inClause)

	tx, _ := store.db.Begin()

	if _, err := tx.Exec(queryVec, ids...); err != nil {
		tx.Rollback() //nolint:errcheck
		return err
	}

	// Purge edges touching the deleted chunks so the relations table doesn't fill with
	// orphaned rows pointing at ids that no longer exist.
	relArgs := append(append([]any{}, ids...), ids...)
	if _, err := tx.Exec(queryRel, relArgs...); err != nil {
		tx.Rollback() //nolint:errcheck
		return err
	}

	if _, err := tx.Exec(queryChunk, ids...); err != nil {
		tx.Rollback() //nolint:errcheck
		return err
	}

	return tx.Commit()

}

// DocumentIdentity returns a stable key for a document and whether it is a replaceable
// document at all. Files (absolute paths) and web pages (URLs, carried in origin) have
// a stable identity, so re-ingesting one should replace its old chunks. Typed cli/hotkey
// notes have no such identity and are always appended, never replaced.
func DocumentIdentity(source, origin string) (string, bool) {
	if origin != "" {
		return origin, true
	}
	if filepath.IsAbs(source) || strings.HasPrefix(source, "http") {
		return source, true
	}
	return "", false
}

// DocumentHash returns the stored content hash for a document key, and whether the
// document has been ingested before.
func (store *Store) DocumentHash(key string) (string, bool, error) {
	var hash string
	err := store.db.QueryRow(`SELECT hash FROM documents WHERE doc_key = ?`, key).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return hash, true, nil
}

// UpsertDocument records (or updates) the content hash for a document key.
func (store *Store) UpsertDocument(key, hash string) error {
	_, err := store.db.Exec(`
		INSERT INTO documents(doc_key, hash, updated_at) VALUES (?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(doc_key) DO UPDATE SET hash = excluded.hash, updated_at = CURRENT_TIMESTAMP`,
		key, hash)
	return err
}

// DeleteBySource removes every chunk (and its vector + edges) belonging to the document
// identified by (source, origin), returning how many chunks were removed. Called before
// re-ingesting an edited document so stale chunks from the previous version don't pile up.
func (store *Store) DeleteBySource(source, origin string) (int64, error) {
	query := `SELECT id FROM chunks WHERE source = ? AND origin = ''`
	args := []any{source}
	if origin != "" {
		query = `SELECT id FROM chunks WHERE origin = ?`
		args = []any{origin}
	}

	rows, err := store.db.Query(query, args...)
	if err != nil {
		return 0, err
	}
	var ids []any
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close() //nolint:errcheck
			return 0, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close() //nolint:errcheck
		return 0, err
	}
	rows.Close() //nolint:errcheck

	if len(ids) == 0 {
		return 0, nil
	}

	placeholders := make([]string, len(ids))
	for index := range ids {
		placeholders[index] = "?"
	}
	inClause := strings.Join(placeholders, ", ")

	tx, err := store.db.Begin()
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(fmt.Sprintf("DELETE FROM vec_chunks WHERE rowid IN (%s)", inClause), ids...); err != nil {
		tx.Rollback() //nolint:errcheck
		return 0, err
	}
	relArgs := append(append([]any{}, ids...), ids...)
	if _, err := tx.Exec(fmt.Sprintf("DELETE FROM relations WHERE source_id IN (%s) OR target_id IN (%s)", inClause, inClause), relArgs...); err != nil {
		tx.Rollback() //nolint:errcheck
		return 0, err
	}
	if _, err := tx.Exec(fmt.Sprintf("DELETE FROM chunks WHERE id IN (%s)", inClause), ids...); err != nil {
		tx.Rollback() //nolint:errcheck
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return int64(len(ids)), nil
}

func (store *Store) relate(id int64, blob string) error {
	rows, err := store.db.Query(`
		SELECT rowid, distance
		FROM vec_chunks
		WHERE embedding MATCH ?
		ORDER BY distance
		LIMIT ?`, blob, maxEdges+1)
	if err != nil {
		return err
	}

	// Read all neighbors first, then write. Inserting while the SELECT's result set is
	// still open holds a read lock on the connection and makes the write fail with
	// "database is locked", so the edges would silently never be created.
	type neighbor struct {
		rowID    int64
		distance float64
	}
	var neighbors []neighbor
	for rows.Next() {
		var rowID int64
		var distance float64
		if err := rows.Scan(&rowID, &distance); err != nil {
			rows.Close() //nolint:errcheck
			return err
		}
		if rowID == id {
			continue
		}
		if distance >= edgeThreshold {
			break
		}
		neighbors = append(neighbors, neighbor{rowID: rowID, distance: distance})
	}
	if err := rows.Err(); err != nil {
		rows.Close() //nolint:errcheck
		return err
	}
	rows.Close() //nolint:errcheck

	for _, neighbor := range neighbors {
		if _, err := store.db.Exec(`INSERT OR IGNORE INTO relations(source_id, target_id, distance) VALUES (?, ?, ?)`, id, neighbor.rowID, neighbor.distance); err != nil {
			return err
		}
	}

	return nil
}

// EdgeDebug is a nearest-neighbor pair, used by the debug/tuning commands.
type EdgeDebug struct {
	SourceID int64
	TargetID int64
	Distance float64
}

// allChunkIDs returns every chunk id.
func (store *Store) allChunkIDs() ([]int64, error) {
	rows, err := store.db.Query(`SELECT id FROM chunks`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// NearestDistances returns, for each chunk, its single closest OTHER chunk and the
// distance between them, sorted ascending. Use it to see what distances exist so you
// can pick an edge threshold.
func (store *Store) NearestDistances() ([]EdgeDebug, error) {
	ids, err := store.allChunkIDs()
	if err != nil {
		return nil, err
	}

	var out []EdgeDebug
	for _, id := range ids {
		rows, err := store.db.Query(`
			SELECT rowid, distance
			FROM vec_chunks
			WHERE embedding MATCH (SELECT embedding FROM vec_chunks WHERE rowid = ?)
			ORDER BY distance
			LIMIT 2`, id)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var rowID int64
			var distance float64
			if err := rows.Scan(&rowID, &distance); err != nil {
				rows.Close()
				return nil, err
			}
			if rowID == id {
				continue // skip self
			}
			out = append(out, EdgeDebug{SourceID: id, TargetID: rowID, Distance: distance})
			break
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return nil, err
		}
		rows.Close()
	}

	sort.Slice(out, func(i, j int) bool { return out[i].Distance < out[j].Distance })
	return out, nil
}

// ReindexEdges wipes and recomputes the relations table for all existing chunks
func (store *Store) ReindexEdges(threshold float64) (int, error) {
	ids, err := store.allChunkIDs()
	if err != nil {
		return 0, err
	}

	type edge struct {
		source, target int64
		dist           float64
	}
	var edges []edge

	// Read all edges first (no writes while a result set is open).
	for _, id := range ids {
		rows, err := store.db.Query(`
			SELECT rowid, distance
			FROM vec_chunks
			WHERE embedding MATCH (SELECT embedding FROM vec_chunks WHERE rowid = ?)
			ORDER BY distance
			LIMIT ?`, id, maxEdges+1)
		if err != nil {
			return 0, err
		}
		for rows.Next() {
			var rowID int64
			var distance float64
			if err := rows.Scan(&rowID, &distance); err != nil {
				rows.Close()
				return 0, err
			}
			if rowID == id {
				continue
			}
			if distance >= threshold {
				break
			}
			edges = append(edges, edge{source: id, target: rowID, dist: distance})
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return 0, err
		}
		rows.Close()
	}

	// Then write them all in one transaction.
	tx, err := store.db.Begin()
	if err != nil {
		return 0, err
	}
	if _, err := tx.Exec(`DELETE FROM relations`); err != nil {
		tx.Rollback() //nolint:errcheck
		return 0, err
	}
	for _, e := range edges {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO relations(source_id, target_id, distance) VALUES (?, ?, ?)`, e.source, e.target, e.dist); err != nil {
			tx.Rollback() //nolint:errcheck
			return 0, err
		}
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return len(edges), nil
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
// I like maxDistance to be 0.95 dont ask me how i chose that its just feeling
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

// documentKey groups chunks into documents: files/pages by their path or URL, and
// each typed note (cli/hotkey) on its own so unrelated notes don't collapse together.
func documentKey(source, origin string, chunkID int64) string {
	if origin != "" {
		return origin
	}
	if filepath.IsAbs(source) || strings.HasPrefix(source, "http") {
		return source
	}
	return fmt.Sprintf("chunk:%d", chunkID)
}

// GraphData returns the knowledge graph with one node per document (not chunks)
func (store *Store) GraphData() (Graph, error) {
	rows, err := store.db.Query(`SELECT id, text, source, origin, created_at FROM chunks`)
	if err != nil {
		return Graph{}, err
	}
	defer rows.Close()

	nodesByKey := map[string]*GraphNode{}
	var order []string
	nodeIDByChunk := map[int64]int64{}

	for rows.Next() {
		var id int64
		var text string
		var source string
		var origin string
		var times time.Time

		if err := rows.Scan(&id, &text, &source, &origin, &times); err != nil {
			return Graph{}, err
		}

		key := documentKey(source, origin, id)
		node, exists := nodesByKey[key]
		if !exists {
			node = &GraphNode{
				ID:        id,
				Label:     graphLabel(text, source, origin),
				Source:    source,
				Open:      openableSource(source, origin),
				CreatedAt: times,
				Text:      text,
			}
			nodesByKey[key] = node
			order = append(order, key)
		} else {
			node.Text += "\n\n" + text
			if times.After(node.CreatedAt) {
				node.CreatedAt = times
			}
		}
		nodeIDByChunk[id] = node.ID
	}
	if err := rows.Err(); err != nil {
		return Graph{}, err
	}

	nodes := make([]GraphNode, 0, len(order))
	for _, key := range order {
		nodes = append(nodes, *nodesByKey[key])
	}

	relations, err := store.db.Query(`SELECT source_id, target_id, distance FROM relations`)
	if err != nil {
		return Graph{}, err
	}
	defer relations.Close()

	edgeDistances := map[[2]int64]float64{}
	for relations.Next() {
		var sourced int64
		var target int64
		var distance float64

		if err := relations.Scan(&sourced, &target, &distance); err != nil {
			return Graph{}, err
		}

		a, okA := nodeIDByChunk[sourced]
		b, okB := nodeIDByChunk[target]
		if !okA || !okB || a == b {
			continue // unknown chunk, or same document
		}

		pair := [2]int64{a, b}
		if pair[0] > pair[1] {
			pair[0], pair[1] = pair[1], pair[0]
		}
		if existing, seen := edgeDistances[pair]; !seen || distance < existing {
			edgeDistances[pair] = distance
		}
	}
	if err := relations.Err(); err != nil {
		return Graph{}, err
	}

	edges := make([]GraphEdge, 0, len(edgeDistances))
	for pair, distance := range edgeDistances {
		edges = append(edges, GraphEdge{Source: pair[0], Target: pair[1], Distance: distance})
	}

	return Graph{
		Nodes: nodes,
		Edges: edges,
	}, nil
}

// graphLabel builds a readable node label: a page title (or domain) for web
// captures, "folder/filename" for files, and a short text preview for typed notes.
func graphLabel(text, source, origin string) string {
	// Web capture: origin is a URL, source is the page title.
	if strings.HasPrefix(origin, "http") {
		if source != "" && !strings.HasPrefix(source, "http") {
			return source
		}
		if parsed, err := url.Parse(origin); err == nil && parsed.Host != "" {
			return parsed.Host
		}
	}

	// File: prefer the original path (origin), fall back to the stored path (source).
	path := origin
	if !filepath.IsAbs(path) {
		path = source
	}
	if filepath.IsAbs(path) {
		return filepath.Join(filepath.Base(filepath.Dir(path)), filepath.Base(path))
	}

	// Typed text (cli / hotkey): short, rune-safe preview.
	runes := []rune(text)
	if len(runes) > 50 {
		return string(runes[:50]) + "..."
	}
	return text
}

type GraphNode struct {
	ID        int64     `json:"id"`
	Label     string    `json:"label"`
	Source    string    `json:"source"`
	Open      string    `json:"open"` // openable path/URL, or "" when there's nothing to open
	CreatedAt time.Time `json:"createdAt"`
	Text      string    `json:"text"`
}

// openableSource returns the path or URL to open for a document, or "" when there's
// nothing openable (typed cli/hotkey notes).
func openableSource(source, origin string) string {
	if origin != "" {
		return origin
	}
	if filepath.IsAbs(source) || strings.HasPrefix(source, "http") {
		return source
	}
	return ""
}

type GraphEdge struct {
	Source   int64   `json:"source"`
	Target   int64   `json:"target"`
	Distance float64 `json:"distance"`
}

type Graph struct {
	Nodes []GraphNode `json:"nodes"`
	Edges []GraphEdge `json:"edges"`
}
