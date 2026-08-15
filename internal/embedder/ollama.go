package embedder

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
)

// DefaultModel is the Ollama embedding model used by Engrex.
const DefaultModel = "nomic-embed-text"

// nomic-embed-text is an asymmetric retrieval model: it was trained with a task prefix
// on every input, and stored text and queries get different ones. Without them the two
// end up in the same space instead of the paired spaces the model was trained to
// produce, which measurably degrades retrieval.
const (
	documentPrefix = "search_document: "
	queryPrefix    = "search_query: "
)

// Embedder calls the Ollama /api/embed endpoint to produce vectors.
type Embedder struct {
	baseURL string
	model   string
}

// New returns an Embedder pointing at the given Ollama base URL.
func New(baseURL string) *Embedder {
	return &Embedder{baseURL: baseURL, model: DefaultModel}
}

// EmbedDocument embeds text that is being stored and searched over.
func (embedder *Embedder) EmbedDocument(text string) ([]float32, error) {
	return embedder.embed(documentPrefix + text)
}

// EmbedQuery embeds a question being asked against the store. Pairs with
// EmbedDocument — mixing the two prefixes up is silently wrong rather than an error,
// so callers should never reach for embed directly.
func (embedder *Embedder) EmbedQuery(text string) ([]float32, error) {
	return embedder.embed(queryPrefix + text)
}

// EmbedRaw embeds text with no task prefix. Only for diagnostics that need to see the
// model's unprefixed output — never for storage or retrieval.
func (embedder *Embedder) EmbedRaw(text string) ([]float32, error) {
	return embedder.embed(text)
}

// embed sends text to Ollama and returns a 768-dimensional unit-length float32 vector.
func (embedder *Embedder) embed(text string) ([]float32, error) {
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

	return Normalize(result.Embeddings[0]), nil
}

// Normalize scales a vector to unit length, returning it unchanged if it has no
// magnitude. Ollama doesn't normalize its output, and vec_chunks ranks by cosine
// distance — which is only well behaved on unit vectors. Normalizing here also keeps
// the calibrated distance thresholds meaningful, since an unnormalized vector's
// distances depend on its magnitude as much as its direction.
func Normalize(vector []float32) []float32 {
	var sumOfSquares float64
	for _, component := range vector {
		sumOfSquares += float64(component) * float64(component)
	}
	if sumOfSquares == 0 {
		return vector
	}

	magnitude := math.Sqrt(sumOfSquares)
	normalized := make([]float32, len(vector))
	for index, component := range vector {
		normalized[index] = float32(float64(component) / magnitude)
	}
	return normalized
}

// Magnitude returns a vector's Euclidean length. Used by `engrex doctor` to show
// whether the raw model output was already normalized.
func Magnitude(vector []float32) float64 {
	var sumOfSquares float64
	for _, component := range vector {
		sumOfSquares += float64(component) * float64(component)
	}
	return math.Sqrt(sumOfSquares)
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
