package rag

import (
	"github.com/robertkoller/engrex/internal/embedder"
	"github.com/robertkoller/engrex/internal/store"
)

const ollamaBaseURL = "http://localhost:11434"
const generateModel = "llama3.2"
const defaultTopK = 5

// RAG wires the embedder, store, and LLM together into the add/query pipeline.
type RAG struct {
	embedder *embedder.Embedder
	store    *store.Store
}

// New returns a RAG instance. Checks that Ollama is reachable before returning.
func New(s *store.Store) (*RAG, error) {
	// TODO: create embedder.New(ollamaBaseURL)
	// TODO: call embedder.Ping(), return error if Ollama is not running
	// TODO: return &RAG{embedder: emb, store: s}, nil
	return nil, nil
}

// Add chunks the text, embeds each chunk, and stores them with the given source label.
func (r *RAG) Add(text, source string) error {
	// TODO: chunker.Chunk(text) → []string
	// TODO: for each chunk: r.embedder.Embed(chunk) → vec, then r.store.Insert(chunk, source, vec)
	// TODO: print "Saved N chunk(s)." to stdout
	return nil
}

// Query embeds the question, retrieves the top-K most relevant chunks,
// builds a RAG prompt, and streams the LLM response to stdout.
func (r *RAG) Query(question string, topK int) error {
	// TODO: r.embedder.Embed(question) → queryVec
	// TODO: r.store.Search(queryVec, topK) → []Chunk
	// TODO: if no chunks found, print "No relevant notes found." and return
	// TODO: buildPrompt(question, chunks) → prompt string
	// TODO: POST ollamaBaseURL+"/api/generate" with {"model": generateModel, "prompt": prompt, "stream": true}
	// TODO: read response line by line, print each "response" token to stdout
	return nil
}

// buildPrompt formats retrieved chunks and the user question into a RAG prompt.
func buildPrompt(question string, chunks []store.Chunk) string {
	// TODO: write system instruction header
	// TODO: for each chunk, write "[N] (saved <date>, source: <source>)\n<text>"
	// TODO: append "Question: <question>"
	// TODO: return assembled prompt string
	return ""
}
