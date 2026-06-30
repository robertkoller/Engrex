package embedder

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
)

// DefaultModel is the Ollama embedding model used by Engrex.
const DefaultModel = "nomic-embed-text"

// Embedder calls the Ollama /api/embed endpoint to produce vectors.
type Embedder struct {
	baseURL string
	model   string
}

// New returns an Embedder pointing at the given Ollama base URL.
func New(baseURL string) *Embedder {
	return &Embedder{baseURL: baseURL, model: DefaultModel}
}

// Embed sends text to Ollama and returns a 768-dimensional float32 vector.
func (embedder *Embedder) Embed(text string) ([]float32, error) {
	body, err := json.Marshal(map[string]string{
		"model": embedder.model,
		"input": text,
	})
	if err != nil {
		return nil, err
	}

	response, err := http.Post(embedder.baseURL+"/api/embed", "application/json", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	defer response.Body.Close()

	var result struct {
		Embeddings [][]float32 `json:"embeddings"`
	}

	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		return nil, err
	}
	if len(result.Embeddings) == 0 {
		return nil, fmt.Errorf("ollama returned no embeddings")
	}

	return result.Embeddings[0], nil
}

// Pings to check that ollama is reachable
func (embedder *Embedder) Ping() error {
	response, err := http.Get(embedder.baseURL + "/api/tags")
	if err != nil {
		return fmt.Errorf("ollama isn't running, start it with: ollama serve")
	}
	defer response.Body.Close()
	return nil
}
