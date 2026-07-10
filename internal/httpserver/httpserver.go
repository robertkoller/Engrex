package httpserver

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/robertkoller/engrex/internal/rag"
)

// Server exposes a local HTTP endpoint so the browser extension can POST captures
// (selected text or full page) along with the page URL and title.
type Server struct {
	rag    *rag.RAG
	server *http.Server
}

// New returns a Server that ingests captures using the given RAG pipeline.
func New(ragPipeline *rag.RAG) *Server {
	return &Server{rag: ragPipeline}
}

// Start registers the routes, binds to localhost only, and blocks until Stop.
func (server *Server) Start() error {
	mux := http.NewServeMux()
	mux.HandleFunc("/capture", server.handleCapture)

	serv := &http.Server{
		Addr:    "127.0.0.1:7777",
		Handler: mux,
	}
	server.server = serv

	return server.server.ListenAndServe()
}

// Stop gracefully shuts the server down so Start returns.
func (server *Server) Stop() {
	server.server.Shutdown(context.Background()) //nolint:errcheck
}

// handleCapture reads a JSON capture {text, url, title} and ingests it via RAG.
func (server *Server) handleCapture(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var page WebPage
	if err := json.NewDecoder(r.Body).Decode(&page); err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if len(page.Text) == 0 {
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	if len(page.Title) == 0 {
		if err := server.rag.Add(page.Text, page.Url, page.Url); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	} else {
		if err := server.rag.Add(page.Text, page.Title, page.Url); err != nil {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}

	w.WriteHeader(http.StatusOK)
}

type WebPage struct {
	Text  string `json:"text"`
	Url   string `json:"url"`
	Title string `json:"title"`
}
