package protocol

import "encoding/json"

const (
	CommandAdd      = "add"
	CommandQuery    = "query"
	CommandDelete   = "delete"
	CommandAddFile  = "addfile"
	CommandSearch   = "search"
	CommandDocument = "document"
	CommandGraph    = "graph"
)

const (
	CodeInvalidInput = "invalid_input"

	CodeNotFound = "not_found"

	CodeIndexUnavailable = "index_unavailable"

	CodeEmbedderUnavailable = "embedder_unavailable"

	CodeMCPDisabled = "mcp_disabled"

	CodeDaemonUnavailable = "daemon_unavailable"

	CodeInternalError = "internal_error"
)

type Command struct {
	Type   string `json:"type"`
	Text   string `json:"text"`
	Source string `json:"source"`

	Limit int `json:"limit,omitempty"`

	Depth int `json:"depth,omitempty"`
}

type Response struct {
	Error string          `json:"error,omitempty"`
	Code  string          `json:"code,omitempty"`
	Data  json.RawMessage `json:"data,omitempty"`
}

type SearchResult struct {
	Rank       int     `json:"rank"`
	ChunkID    int64   `json:"chunk_id"`
	DocumentID string  `json:"document_id"`
	Path       string  `json:"path"`
	Label      string  `json:"label"`
	ChunkIndex int     `json:"chunk_index"`
	Snippet    string  `json:"snippet"`
	Score      float64 `json:"score"`
	Distance   float64 `json:"distance"`
	SavedAt    string  `json:"saved_at"`
}

type SearchPayload struct {
	Query   string         `json:"query"`
	Count   int            `json:"count"`
	Results []SearchResult `json:"results,omitempty"`
}

type DocumentPayload struct {
	DocumentID string `json:"document_id"`
	Label      string `json:"label"`
	Path       string `json:"path"`
	Format     string `json:"format"`
	Content    string `json:"content"`

	ContentSource string  `json:"content_source"`
	Truncated     bool    `json:"truncated"`
	ChunkCount    int     `json:"chunk_count"`
	ChunkIDs      []int64 `json:"chunk_ids,omitempty"`

	IngestionHash string `json:"ingestion_hash"`
	IngestedAt    string `json:"ingested_at"`
	SavedAt       string `json:"saved_at"`
	LastModified  string `json:"last_modified"`
	SizeBytes     int64  `json:"size_bytes"`
}

type GraphNodePayload struct {
	NodeID     int64  `json:"node_id"`
	DocumentID string `json:"document_id"`
	Label      string `json:"label"`
	Path       string `json:"path"`
	Depth      int    `json:"depth"`
	Preview    string `json:"preview"`
}

type GraphEdgePayload struct {
	SourceNodeID int64   `json:"source_node_id"`
	TargetNodeID int64   `json:"target_node_id"`
	Distance     float64 `json:"distance"`
	Similarity   float64 `json:"similarity"`
}

type GraphPayload struct {
	RootNodeID     int64              `json:"root_node_id"`
	RootDocumentID string             `json:"root_document_id"`
	RootLabel      string             `json:"root_label"`
	Depth          int                `json:"depth"`
	NodeCount      int                `json:"node_count"`
	EdgeCount      int                `json:"edge_count"`
	Nodes          []GraphNodePayload `json:"nodes,omitempty"`
	Edges          []GraphEdgePayload `json:"edges,omitempty"`
}
