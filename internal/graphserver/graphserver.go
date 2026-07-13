package graphserver

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/robertkoller/engrex/internal/rag"
	"github.com/robertkoller/engrex/internal/store"
)

// GServer serves the graph web UI and its data/query endpoints on localhost:7778.
type GServer struct {
	rag    *rag.RAG
	store  *store.Store
	server *http.Server
}

// New returns a graph server backed by the given RAG pipeline and store.
func New(ragPipeline *rag.RAG, store *store.Store) *GServer {
	return &GServer{rag: ragPipeline, store: store}
}

// Start registers the routes, binds to localhost only, and blocks until Stop.
func (server *GServer) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/graph", server.handleGraph)
	mux.HandleFunc("/query", server.handleQuery)
	mux.Handle("/", http.FileServer(http.Dir("web")))

	serv := &http.Server{
		Addr:    "127.0.0.1:7778",
		Handler: mux,
	}
	server.server = serv

	return server.server.ListenAndServe()
}

// Stop gracefully shuts the server down so Start returns.
func (server *GServer) Stop() {
	server.server.Shutdown(context.Background()) //nolint:errcheck
}

// handleGraph returns the whole knowledge graph as JSON.
func (server *GServer) handleGraph(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")

	graph, err := server.store.GraphData()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if err := json.NewEncoder(w).Encode(graph); err != nil {
		return
	}
}

// handleQuery answers a question against the knowledge base and returns the answer
// plus its sources — used by the graph's "Ask about this" button.
func (server *GServer) handleQuery(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	if r.Method != http.MethodPost {
		http.Error(w, "POST only", http.StatusMethodNotAllowed)
		return
	}

	var payload struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(payload.Text) == "" {
		http.Error(w, "no text", http.StatusBadRequest)
		return
	}

	// rag.Query streams to a writer: first line is the JSON sources, then the answer.
	// Buffer it and split so we can return a single JSON object.
	var buffer bytes.Buffer
	if err := server.rag.Query(&buffer, payload.Text, rag.DefaultSearchDistance, rag.DefaultSearchResults); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	output := buffer.String()
	sources := []string{}
	answer := output
	if newline := strings.IndexByte(output, '\n'); newline >= 0 {
		var head struct {
			Sources []string `json:"sources"`
		}
		if json.Unmarshal([]byte(output[:newline]), &head) == nil {
			if head.Sources != nil {
				sources = head.Sources
			}
			answer = output[newline+1:]
		}
	}

	json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
		"answer":  strings.TrimSpace(answer),
		"sources": sources,
	})
}
