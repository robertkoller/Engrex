package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var ErrDocumentNotFound = errors.New("no stored document matches that identifier")

type Document struct {
	Key      string
	Label    string
	Source   string
	Origin   string
	Open     string
	Format   string
	Text     string
	ChunkIDs []int64

	SavedAt time.Time

	Hash       string
	IngestedAt time.Time
}

func (store *Store) ResolveDocumentKey(identifier string) (string, error) {
	identifier = strings.TrimSpace(identifier)
	if identifier == "" {
		return "", ErrDocumentNotFound
	}

	if chunkID, err := strconv.ParseInt(strings.TrimPrefix(identifier, "chunk:"), 10, 64); err == nil {
		var source, origin string
		err := store.db.QueryRow(`SELECT source, origin FROM chunks WHERE id = ?`, chunkID).Scan(&source, &origin)
		if errors.Is(err, sql.ErrNoRows) {
			return "", ErrDocumentNotFound
		}
		if err != nil {
			return "", err
		}
		return DocumentKey(source, origin, chunkID), nil
	}

	lookups := []struct {
		query string
		args  []any
	}{
		{`SELECT id, source, origin FROM chunks WHERE origin = ? ORDER BY id LIMIT 1`, []any{identifier}},
		{`SELECT id, source, origin FROM chunks WHERE source = ? ORDER BY id LIMIT 1`, []any{identifier}},
		{`SELECT id, source, origin FROM chunks WHERE source LIKE ? OR origin LIKE ? ORDER BY id LIMIT 1`,
			[]any{"%" + identifier, "%" + identifier}},
	}
	for _, lookup := range lookups {
		var id int64
		var source, origin string
		err := store.db.QueryRow(lookup.query, lookup.args...).Scan(&id, &source, &origin)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return "", err
		}
		return DocumentKey(source, origin, id), nil
	}
	return "", ErrDocumentNotFound
}

func documentChunkQuery(key string) (string, []any) {
	if chunkID, err := strconv.ParseInt(strings.TrimPrefix(key, "chunk:"), 10, 64); err == nil && strings.HasPrefix(key, "chunk:") {
		return `SELECT id, text, source, origin, created_at FROM chunks WHERE id = ?`, []any{chunkID}
	}

	return `SELECT id, text, source, origin, created_at FROM chunks
	        WHERE origin = ? OR (origin = '' AND source = ?)
	        ORDER BY id`, []any{key, key}
}

func (store *Store) DocumentByKey(key string) (Document, error) {
	query, args := documentChunkQuery(key)
	rows, err := store.db.Query(query, args...)
	if err != nil {
		return Document{}, err
	}
	defer rows.Close()

	document := Document{Key: key, ChunkIDs: []int64{}}
	var texts []string
	for rows.Next() {
		var chunk Chunk
		if err := rows.Scan(&chunk.ID, &chunk.Text, &chunk.Source, &chunk.Origin, &chunk.CreatedAt); err != nil {
			return Document{}, err
		}
		if len(document.ChunkIDs) == 0 {
			document.Source = chunk.Source
			document.Origin = chunk.Origin
		}
		document.ChunkIDs = append(document.ChunkIDs, chunk.ID)
		texts = append(texts, chunk.Text)
		if chunk.CreatedAt.After(document.SavedAt) {
			document.SavedAt = chunk.CreatedAt
		}
	}
	if err := rows.Err(); err != nil {
		return Document{}, err
	}
	if len(document.ChunkIDs) == 0 {
		return Document{}, ErrDocumentNotFound
	}

	document.Text = stitchChunks(texts)
	document.Label = graphLabel(texts[0], document.Source, document.Origin)
	document.Open = openableSource(document.Source, document.Origin)
	document.Format = documentFormat(key, document.Source, document.Origin)

	hash, ingestedAt, found, err := store.DocumentMeta(key)
	if err != nil {
		return Document{}, err
	}
	if found {
		document.Hash = hash
		document.IngestedAt = ingestedAt
	}
	return document, nil
}

func (store *Store) ChunkOrdinals(key string) (map[int64]int, error) {
	query, args := documentChunkQuery(key)
	rows, err := store.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	ordinals := make(map[int64]int)
	position := 0
	for rows.Next() {
		var chunk Chunk
		if err := rows.Scan(&chunk.ID, &chunk.Text, &chunk.Source, &chunk.Origin, &chunk.CreatedAt); err != nil {
			return nil, err
		}
		ordinals[chunk.ID] = position
		position++
	}
	return ordinals, rows.Err()
}

func (store *Store) DocumentMeta(key string) (string, time.Time, bool, error) {
	var hash string
	var updatedAt time.Time
	err := store.db.QueryRow(`SELECT hash, updated_at FROM documents WHERE doc_key = ?`, key).Scan(&hash, &updatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return "", time.Time{}, false, nil
	}
	if err != nil {
		return "", time.Time{}, false, err
	}
	return hash, updatedAt, true, nil
}

const maxOverlapScan = 4096

func stitchChunks(texts []string) string {
	if len(texts) == 0 {
		return ""
	}

	var builder strings.Builder
	builder.WriteString(texts[0])
	previous := texts[0]

	for _, text := range texts[1:] {
		overlap := overlapLength(previous, text)
		remainder := text[overlap:]
		if remainder == "" {
			continue
		}

		if overlap == 0 {
			builder.WriteString("\n\n")
		} else if !strings.HasPrefix(remainder, " ") && !strings.HasSuffix(previous, " ") {
			builder.WriteString(" ")
		}
		builder.WriteString(remainder)
		previous = text
	}
	return builder.String()
}

func overlapLength(previous, next string) int {
	limit := min(min(len(previous), len(next)), maxOverlapScan)
	for length := limit; length > 0; length-- {
		if previous[len(previous)-length:] == next[:length] {
			return length
		}
	}
	return 0
}

func documentFormat(key, source, origin string) string {
	if strings.HasPrefix(key, "chunk:") {
		return "note"
	}
	if strings.HasPrefix(origin, "http") || strings.HasPrefix(source, "http") {
		return "web"
	}

	path := origin
	if !filepath.IsAbs(path) {
		path = source
	}
	if extension := strings.TrimPrefix(filepath.Ext(path), "."); extension != "" {
		return strings.ToLower(extension)
	}
	return "text"
}

func (store *Store) GraphNeighborhood(key string, depth int) (Graph, int64, error) {
	graph, err := store.GraphData()
	if err != nil {
		return Graph{}, 0, err
	}

	rootID := int64(-1)
	for _, node := range graph.Nodes {
		if node.Key == key {
			rootID = node.ID
			break
		}
	}
	if rootID < 0 {
		return Graph{}, 0, ErrDocumentNotFound
	}
	if depth < 0 {
		depth = 0
	}

	adjacency := make(map[int64][]int64, len(graph.Nodes))
	for _, edge := range graph.Edges {
		adjacency[edge.Source] = append(adjacency[edge.Source], edge.Target)
		adjacency[edge.Target] = append(adjacency[edge.Target], edge.Source)
	}

	depthByID := map[int64]int{rootID: 0}
	frontier := []int64{rootID}
	for hop := 1; hop <= depth && len(frontier) > 0; hop++ {
		var next []int64
		for _, id := range frontier {
			for _, neighbor := range adjacency[id] {
				if _, seen := depthByID[neighbor]; seen {
					continue
				}
				depthByID[neighbor] = hop
				next = append(next, neighbor)
			}
		}
		frontier = next
	}

	neighborhood := Graph{Nodes: []GraphNode{}, Edges: []GraphEdge{}}
	for _, node := range graph.Nodes {
		hop, included := depthByID[node.ID]
		if !included {
			continue
		}
		node.Depth = hop
		neighborhood.Nodes = append(neighborhood.Nodes, node)
	}
	for _, edge := range graph.Edges {
		_, hasSource := depthByID[edge.Source]
		_, hasTarget := depthByID[edge.Target]
		if hasSource && hasTarget {
			neighborhood.Edges = append(neighborhood.Edges, edge)
		}
	}
	return neighborhood, rootID, nil
}
