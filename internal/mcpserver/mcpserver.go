package mcpserver

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/robertkoller/engrex/internal/config"
	"github.com/robertkoller/engrex/internal/protocol"
)

const serverName = "engrex"
const serverVersion = "1.0.0"

const serverInstructions = `Engrex is the user's local, private second brain: notes they typed, files they dropped
into ~/Engrex, and web pages they saved. Everything lives on this machine.

Use search_notes whenever the user asks about something they might have saved, or refers
to their own notes, files, or reading. It runs hybrid retrieval (semantic vector search
fused with BM25 keyword search), so it handles both vague, conceptual questions and exact
strings like identifiers or error codes.

Each search result carries a document_id. Pass it to get_document for the full text of the
source, or to query_knowledge_graph to find documents the user saved that are semantically
adjacent to it — useful for "what else did I write about this".

All three tools are read-only; none of them can modify the knowledge base.`

func Serve(ctx context.Context) error {
	configuration, err := config.Load()
	if err != nil {
		return fmt.Errorf("reading config: %w", err)
	}
	if !configuration.MCPEnabled {
		return errors.New("the MCP interface is disabled — enable it with `engrex mcp enable`")
	}

	client, err := newDaemonClient()
	if err != nil {
		return err
	}

	server := mcp.NewServer(
		&mcp.Implementation{
			Name:        serverName,
			Title:       "Engrex",
			Description: "Local-first search over the user's own notes, files, and saved pages.",
			Version:     serverVersion,
		},
		&mcp.ServerOptions{Instructions: serverInstructions},
	)
	registerTools(server, client)

	log.SetOutput(os.Stderr)

	return server.Run(ctx, &mcp.StdioTransport{})
}

func readOnlyTool(name, title, description string) *mcp.Tool {
	closedWorld := false
	return &mcp.Tool{
		Name:        name,
		Title:       title,
		Description: description,
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:   true,
			IdempotentHint: true,
			OpenWorldHint:  &closedWorld,
		},
	}
}

type searchInput struct {
	Query string `json:"query" jsonschema:"the natural-language question or exact keywords to search for"`
	Limit int    `json:"limit,omitempty" jsonschema:"maximum number of chunks to return; 1-50, defaults to 10"`
}

type documentInput struct {
	DocumentID string `json:"document_id,omitempty" jsonschema:"the document_id from a search result or graph node"`
	Path       string `json:"path,omitempty" jsonschema:"a file path, URL, or filename, if you do not have a document_id"`
}

type graphInput struct {
	NodeID     int64  `json:"node_id,omitempty" jsonschema:"the node_id of a graph node, if you have one"`
	DocumentID string `json:"document_id,omitempty" jsonschema:"the document_id from a search result; use this if you have no node_id"`
	Depth      int    `json:"depth,omitempty" jsonschema:"how many hops out from the starting document to walk; 1-5, defaults to 1"`
}

func registerTools(server *mcp.Server, client *daemonClient) {
	mcp.AddTool(server,
		readOnlyTool("search_notes", "Search notes",
			"Search the user's private knowledge base — their own notes, dropped files, and saved web "+
				"pages — using hybrid retrieval: dense vector similarity fused with BM25 keyword matching. "+
				"Returns ranked chunks, each with the document it came from, its position within that "+
				"document, and a relevance score. Use the returned document_id with get_document or "+
				"query_knowledge_graph. An empty result list means nothing relevant is stored, not an error."),
		func(ctx context.Context, _ *mcp.CallToolRequest, input searchInput) (*mcp.CallToolResult, protocol.SearchPayload, error) {
			payload, err := call[protocol.SearchPayload](ctx, client, protocol.Command{
				Type:   protocol.CommandSearch,
				Text:   input.Query,
				Source: "mcp",
				Limit:  input.Limit,
			})
			if err != nil {
				return nil, protocol.SearchPayload{}, toolError(err)
			}
			return nil, payload, nil
		})

	mcp.AddTool(server,
		readOnlyTool("get_document", "Get document",
			"Fetch the full text of one stored document, along with its format, size, last-modified "+
				"time, and the content hash recorded at its last ingestion. Files still present on disk "+
				"are re-read from the file itself; web captures and typed notes are reassembled from "+
				"their stored chunks. Very large documents are returned truncated, with a flag saying so."),
		func(ctx context.Context, _ *mcp.CallToolRequest, input documentInput) (*mcp.CallToolResult, protocol.DocumentPayload, error) {
			identifier := input.DocumentID
			if identifier == "" {
				identifier = input.Path
			}
			if identifier == "" {
				return nil, protocol.DocumentPayload{}, toolError(&daemonError{
					Code:    protocol.CodeInvalidInput,
					Message: "supply either document_id or path",
				})
			}

			payload, err := call[protocol.DocumentPayload](ctx, client, protocol.Command{
				Type:   protocol.CommandDocument,
				Text:   identifier,
				Source: "mcp",
			})
			if err != nil {
				return nil, protocol.DocumentPayload{}, toolError(err)
			}
			return nil, payload, nil
		})

	mcp.AddTool(server,
		readOnlyTool("query_knowledge_graph", "Query knowledge graph",
			"Walk the knowledge graph outward from one document to find the documents stored nearest "+
				"to it in embedding space. The edges were recorded when each chunk was ingested — this "+
				"reads them, it does not recompute similarity — so it is fast and matches the graph the "+
				"Engrex web UI draws. Use it to find related material the user saved that a direct "+
				"search for their words would miss."),
		func(ctx context.Context, _ *mcp.CallToolRequest, input graphInput) (*mcp.CallToolResult, protocol.GraphPayload, error) {
			identifier := input.DocumentID
			if identifier == "" && input.NodeID > 0 {
				identifier = fmt.Sprintf("%d", input.NodeID)
			}
			if identifier == "" {
				return nil, protocol.GraphPayload{}, toolError(&daemonError{
					Code:    protocol.CodeInvalidInput,
					Message: "supply either node_id or document_id",
				})
			}

			payload, err := call[protocol.GraphPayload](ctx, client, protocol.Command{
				Type:   protocol.CommandGraph,
				Text:   identifier,
				Source: "mcp",
				Depth:  input.Depth,
			})
			if err != nil {
				return nil, protocol.GraphPayload{}, toolError(err)
			}
			return nil, payload, nil
		})
}

func toolError(err error) error {
	var daemonErr *daemonError
	if !errors.As(err, &daemonErr) {
		return fmt.Errorf("%s: %s", protocol.CodeInternalError, err.Error())
	}

	hint := ""
	switch daemonErr.Code {
	case protocol.CodeIndexUnavailable:
		hint = " (retrying shortly will usually succeed)"
	case protocol.CodeEmbedderUnavailable:
		hint = " (the user needs to start Ollama)"
	case protocol.CodeMCPDisabled:
		hint = " (the user must run `engrex mcp enable`)"
	case protocol.CodeDaemonUnavailable:
		hint = " (the user must start the Engrex daemon)"
	}
	return fmt.Errorf("%s: %s%s", daemonErr.Code, daemonErr.Message, hint)
}
