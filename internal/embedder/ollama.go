package embedder

// DefaultModel is the Ollama embedding model used by Engrex.
const DefaultModel = "nomic-embed-text"

// Embedder calls the Ollama /api/embed endpoint to produce vectors.
type Embedder struct {
	baseURL string
	model   string
}

// New returns an Embedder pointing at the given Ollama base URL.
func New(baseURL string) *Embedder {
	// TODO: return &Embedder{baseURL: baseURL, model: DefaultModel}
	return nil
}

// Embed sends text to Ollama and returns a 768-dimensional float32 vector.
func (e *Embedder) Embed(text string) ([]float32, error) {
	// TODO: POST baseURL+"/api/embed" with {"model": e.model, "input": text}
	// TODO: decode response {"embeddings": [[...floats...]]}
	// TODO: return embeddings[0], nil
	return nil, nil
}

// Ping checks that Ollama is reachable. Returns an error with a human-friendly
// message if not, so callers can show a clear error before doing any real work.
func (e *Embedder) Ping() error {
	// TODO: GET baseURL+"/api/tags", check for non-200 or connection refused
	return nil
}
