package socket

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"strings"
	"time"

	"github.com/robertkoller/engrex/internal/config"
	"github.com/robertkoller/engrex/internal/ingest"
	"github.com/robertkoller/engrex/internal/protocol"
	"github.com/robertkoller/engrex/internal/rag"
	"github.com/robertkoller/engrex/internal/store"
)

const (
	maxSearchResults    = 50
	maxSnippetRunes     = 600
	maxDocumentRunes    = 200_000
	maxNodePreviewRunes = 200
	maxGraphDepth       = 5
)

type codedError struct {
	code    string
	message string
}

func (err *codedError) Error() string { return err.message }

func failure(code, format string, args ...any) error {
	return &codedError{code: code, message: fmt.Sprintf(format, args...)}
}

func classify(err error) (string, string) {
	var coded *codedError
	if errors.As(err, &coded) {
		return coded.code, coded.message
	}
	if errors.Is(err, store.ErrDocumentNotFound) {
		return protocol.CodeNotFound, err.Error()
	}

	text := strings.ToLower(err.Error())
	switch {
	case strings.Contains(text, "database is locked"), strings.Contains(text, "table is locked"),
		strings.Contains(text, "database is busy"):
		return protocol.CodeIndexUnavailable, "the index is busy — an ingestion is probably in flight; try again in a moment"
	case strings.Contains(text, "connection refused"), strings.Contains(text, "no such host"),
		strings.Contains(text, "11434"):
		return protocol.CodeEmbedderUnavailable, "the local embedding model (Ollama) is not reachable"
	}
	return protocol.CodeInternalError, err.Error()
}

func (socket *Socket) handleReadOnly(conn net.Conn, command protocol.Command) {
	configuration, err := config.Load()
	if err != nil {
		log.Printf("failed to read config, treating MCP as disabled: %v", err)
	}
	if !configuration.MCPEnabled {
		writeResponse(conn, protocol.Response{
			Code:  protocol.CodeMCPDisabled,
			Error: "the MCP interface is disabled; enable it with `engrex mcp enable`",
		})
		return
	}

	var payload any
	switch command.Type {
	case protocol.CommandSearch:
		payload, err = socket.search(command)
	case protocol.CommandDocument:
		payload, err = socket.document(command)
	case protocol.CommandGraph:
		payload, err = socket.graph(command)
	default:
		err = failure(protocol.CodeInvalidInput, "unknown read-only command %q", command.Type)
	}

	if err != nil {
		code, message := classify(err)
		writeResponse(conn, protocol.Response{Code: code, Error: message})
		return
	}

	encoded, err := json.Marshal(payload)
	if err != nil {
		writeResponse(conn, protocol.Response{Code: protocol.CodeInternalError, Error: err.Error()})
		return
	}
	writeResponse(conn, protocol.Response{Data: encoded})
}

func writeResponse(conn net.Conn, response protocol.Response) {
	if err := json.NewEncoder(conn).Encode(response); err != nil {
		log.Printf("failed encoding read-only response: %v", err)
	}
}

func (socket *Socket) search(command protocol.Command) (protocol.SearchPayload, error) {
	query := strings.TrimSpace(command.Text)
	if query == "" {
		return protocol.SearchPayload{}, failure(protocol.CodeInvalidInput, "query must not be empty")
	}

	limit := command.Limit
	if limit <= 0 {
		limit = rag.DefaultSearchResults
	}
	if limit > maxSearchResults {
		limit = maxSearchResults
	}

	chunks, err := socket.rag.Retrieve(query, rag.DefaultSearchDistance, limit)
	if err != nil {
		return protocol.SearchPayload{}, err
	}

	ordinalsByKey := make(map[string]map[int64]int)

	results := make([]protocol.SearchResult, 0, len(chunks))
	for index, chunk := range chunks {
		key := store.DocumentKey(chunk.Source, chunk.Origin, chunk.ID)

		ordinals, cached := ordinalsByKey[key]
		if !cached {
			ordinals, err = socket.store.ChunkOrdinals(key)
			if err != nil {
				return protocol.SearchPayload{}, err
			}
			ordinalsByKey[key] = ordinals
		}

		path := chunk.Origin
		if path == "" {
			path = chunk.Source
		}

		results = append(results, protocol.SearchResult{
			Rank:       index + 1,
			ChunkID:    chunk.ID,
			DocumentID: key,
			Path:       path,
			Label:      chunk.Source,
			ChunkIndex: ordinals[chunk.ID],
			Snippet:    truncateRunes(chunk.Text, maxSnippetRunes),
			Score:      chunk.Score,
			Distance:   chunk.Distance,
			SavedAt:    formatTime(chunk.CreatedAt),
		})
	}

	return protocol.SearchPayload{Query: query, Count: len(results), Results: results}, nil
}

func (socket *Socket) document(command protocol.Command) (protocol.DocumentPayload, error) {
	identifier := strings.TrimSpace(command.Text)
	if identifier == "" {
		return protocol.DocumentPayload{}, failure(protocol.CodeInvalidInput, "document_id or path must not be empty")
	}

	key, err := socket.store.ResolveDocumentKey(identifier)
	if err != nil {
		if errors.Is(err, store.ErrDocumentNotFound) {
			return protocol.DocumentPayload{}, failure(protocol.CodeNotFound,
				"no stored document matches %q", identifier)
		}
		return protocol.DocumentPayload{}, err
	}

	document, err := socket.store.DocumentByKey(key)
	if err != nil {
		return protocol.DocumentPayload{}, err
	}

	content := document.Text
	contentSource := "chunks"
	var sizeBytes int64
	var lastModified string

	for _, candidate := range []string{document.Source, document.Origin} {
		info, statErr := os.Stat(candidate)
		if statErr != nil || info.IsDir() {
			continue
		}
		sizeBytes = info.Size()
		lastModified = formatTime(info.ModTime())
		if extracted, extractErr := ingest.ExtractText(candidate); extractErr == nil && extracted != "" {
			content = extracted
			contentSource = "file"
		}
		break
	}

	truncated := false
	if capped := truncateRunes(content, maxDocumentRunes); capped != content {
		content = capped
		truncated = true
	}

	return protocol.DocumentPayload{
		DocumentID:    document.Key,
		Label:         document.Label,
		Path:          document.Open,
		Format:        document.Format,
		Content:       content,
		ContentSource: contentSource,
		Truncated:     truncated,
		ChunkCount:    len(document.ChunkIDs),
		ChunkIDs:      document.ChunkIDs,
		IngestionHash: document.Hash,
		IngestedAt:    formatTime(document.IngestedAt),
		SavedAt:       formatTime(document.SavedAt),
		LastModified:  lastModified,
		SizeBytes:     sizeBytes,
	}, nil
}

func (socket *Socket) graph(command protocol.Command) (protocol.GraphPayload, error) {
	identifier := strings.TrimSpace(command.Text)
	if identifier == "" {
		return protocol.GraphPayload{}, failure(protocol.CodeInvalidInput, "node_id or document_id must not be empty")
	}

	depth := command.Depth
	if depth <= 0 {
		depth = 1
	}
	if depth > maxGraphDepth {
		depth = maxGraphDepth
	}

	key, err := socket.store.ResolveDocumentKey(identifier)
	if err != nil {
		if errors.Is(err, store.ErrDocumentNotFound) {
			return protocol.GraphPayload{}, failure(protocol.CodeNotFound,
				"no stored document or graph node matches %q", identifier)
		}
		return protocol.GraphPayload{}, err
	}

	neighborhood, rootID, err := socket.store.GraphNeighborhood(key, depth)
	if err != nil {
		if errors.Is(err, store.ErrDocumentNotFound) {
			return protocol.GraphPayload{}, failure(protocol.CodeNotFound,
				"%q resolved to a document that has no graph node", identifier)
		}
		return protocol.GraphPayload{}, err
	}

	payload := protocol.GraphPayload{
		RootNodeID:     rootID,
		RootDocumentID: key,
		Depth:          depth,
		Nodes:          make([]protocol.GraphNodePayload, 0, len(neighborhood.Nodes)),
		Edges:          make([]protocol.GraphEdgePayload, 0, len(neighborhood.Edges)),
	}
	for _, node := range neighborhood.Nodes {
		if node.ID == rootID {
			payload.RootLabel = node.Label
		}
		payload.Nodes = append(payload.Nodes, protocol.GraphNodePayload{
			NodeID:     node.ID,
			DocumentID: node.Key,
			Label:      node.Label,
			Path:       node.Open,
			Depth:      node.Depth,
			Preview:    truncateRunes(node.Text, maxNodePreviewRunes),
		})
	}
	for _, edge := range neighborhood.Edges {
		payload.Edges = append(payload.Edges, protocol.GraphEdgePayload{
			SourceNodeID: edge.Source,
			TargetNodeID: edge.Target,
			Distance:     edge.Distance,
			Similarity:   1 - edge.Distance,
		})
	}
	payload.NodeCount = len(payload.Nodes)
	payload.EdgeCount = len(payload.Edges)
	return payload, nil
}

func truncateRunes(text string, limit int) string {
	runes := []rune(text)
	if len(runes) <= limit {
		return text
	}
	return string(runes[:limit]) + "\n\n[truncated]"
}

func formatTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}
	return value.Format(time.RFC3339)
}
