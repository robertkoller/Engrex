package httpserver

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/robertkoller/engrex/internal/chunker"
	"github.com/robertkoller/engrex/internal/ingest"
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
	mux.HandleFunc("/capture-file", server.handleCaptureFile)

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

	source := page.Title
	if len(source) == 0 {
		source = page.Url
	}

	// Google Docs exports come in as markdown so the chunker can split on headings
	// Slides' text export marks line breaks inside a text box with vertical tabs
	text := strings.ReplaceAll(page.Text, "\v", "\n")
	contentType := chunker.TypeText
	if page.Format == chunker.TypeMarkdown {
		text = ingest.CleanMarkdown(text)
		contentType = chunker.TypeMarkdown
	}

	if err := server.rag.AddTyped(text, source, page.Url, contentType); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

type WebPage struct {
	Text  string `json:"text"`
	Url   string `json:"url"`
	Title string `json:"title"`
	// Format is "markdown" or empty for plain text
	Format string `json:"format"`
}

// maxCaptureFileBytes caps a /capture-file upload, which is bigger than anything Drive
// will hand over without its virus scan page anyway
const maxCaptureFileBytes = 100 << 20

// handleCaptureFile ingests a raw file the extension downloaded (a Drive PDF, .docx,
// .pptx...). The body is the file and name, url, and title are query parameters. It's
// written to a temp file so it goes through the same extractors as a dropped file
func (server *Server) handleCaptureFile(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	query := r.URL.Query()
	name := filepath.Base(query.Get("name"))
	pageURL := query.Get("url")
	if name == "." || name == "/" || pageURL == "" {
		http.Error(w, "name and url are required", http.StatusBadRequest)
		return
	}
	if !ingest.IsSupported(name) {
		http.Error(w, "unsupported file type: "+filepath.Ext(name), http.StatusUnsupportedMediaType)
		return
	}

	title := query.Get("title")
	if title == "" {
		title = name
	}

	text, err := extractUpload(http.MaxBytesReader(w, r.Body, maxCaptureFileBytes), filepath.Ext(name))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			http.Error(w, "file too large", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	if text == "" {
		http.Error(w, "no readable text", http.StatusUnprocessableEntity)
		return
	}

	if err := server.rag.AddTyped(text, title, pageURL, ingest.ContentType(name)); err != nil {
		w.WriteHeader(http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
}

// extractUpload writes the upload to a temp file with the right extension, extracts its
// text, and removes the temp file
func extractUpload(body io.Reader, extension string) (string, error) {
	temporary, err := os.CreateTemp("", "engrex-capture-*"+extension)
	if err != nil {
		return "", err
	}
	defer os.Remove(temporary.Name()) //nolint:errcheck

	if _, err := io.Copy(temporary, body); err != nil {
		temporary.Close() //nolint:errcheck
		return "", err
	}
	if err := temporary.Close(); err != nil {
		return "", err
	}
	return ingest.ExtractText(temporary.Name())
}
