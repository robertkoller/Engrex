package cache

import (
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/rand"
	"strings"
	"time"
)

// SyntheticOllama answers like Ollama but without a model behind it: canned text after a
// fixed delay.
//
// It exists because the load test the guide asks for wants a couple of thousand
// requests, and a couple of thousand real local generations is hours. Swapping the
// provider makes the run take minutes and still measures the things worth measuring —
// hit rate, how fast a hit is served, how the rate climbs as the cache fills. What it
// does not measure is the model, and every report that uses it says so.
type SyntheticOllama struct {
	*Ollama

	// Latency stands in for how long a generation takes. 800ms is a fair stand-in for a
	// short answer from a small local model; the answer at rag.go:60 that took 14s is
	// the other end of the range.
	Latency time.Duration

	// Answer is the body of what it returns. Its length matters, because replaying it is
	// what the hit path is actually timed doing.
	Answer string

	// Splitter finds the question inside the prompt so the answer can be stamped with
	// which question it was written for. That stamp is what lets the load test tell a
	// correct hit from a confidently wrong one, which is the measurement the whole
	// threshold argument turns on.
	Splitter Splitter
}

// SyntheticStamp prefixes every synthetic answer with the identity of the question it
// was generated for.
const SyntheticStamp = "answer-for:"

// StampFor is the identity a synthetic answer carries. Exported so the load test can work
// out what it should have been handed.
func StampFor(question string) string {
	return fmt.Sprintf("%s%016x", SyntheticStamp, fnv64(strings.TrimSpace(question)))
}

// StampIn recovers the identity from a served answer, cached or fresh.
func StampIn(answer string) string {
	start := strings.Index(answer, SyntheticStamp)
	if start < 0 {
		return ""
	}
	end := start + len(SyntheticStamp) + 16
	if end > len(answer) {
		return ""
	}
	return answer[start:end]
}

func NewSyntheticOllama(latency time.Duration) *SyntheticOllama {
	return &SyntheticOllama{
		Ollama:   NewOllama("http://synthetic.invalid"),
		Latency:  latency,
		Splitter: DefaultSplitter(),
		Answer: strings.Repeat(
			"The daemon owns the database and serializes every write through one process. ", 12),
	}
}

// answerFor builds the reply, stamped with the question it belongs to.
func (provider *SyntheticOllama) answerFor(body []byte) string {
	var decoded struct {
		Prompt string `json:"prompt"`
	}
	json.Unmarshal(body, &decoded) //nolint:errcheck

	question := decoded.Prompt
	if parts, recognized := provider.Splitter.Split(decoded.Prompt); recognized {
		question = parts.Semantic
	}
	return StampFor(question) + " " + provider.Answer
}

func (provider *SyntheticOllama) Name() string { return "synthetic" }

func (provider *SyntheticOllama) Forward(path string, body []byte, sink io.Writer) (Payload, error) {
	time.Sleep(provider.Latency)

	if path == embedEndpoint {
		vector := unitVectorFor(body)
		if sink != nil {
			json.NewEncoder(sink).Encode(map[string]any{"embeddings": [][]float32{vector}}) //nolint:errcheck
		}
		return Payload{Embedding: vector}, nil
	}

	answer := provider.answerFor(body)
	if sink != nil {
		encoder := json.NewEncoder(sink)
		for offset := 0; offset < len(answer); offset += 64 {
			end := min(offset+64, len(answer))
			encoder.Encode(map[string]any{"response": answer[offset:end], "done": false}) //nolint:errcheck
		}
		encoder.Encode(map[string]any{"response": "", "done": true, "done_reason": "stop"}) //nolint:errcheck
	}
	return Payload{
		Text: answer,
		Meta: map[string]any{"done_reason": "stop", "eval_count": len(answer) / 4},
	}, nil
}

// unitVectorFor makes a deterministic normalized vector for some input, so a synthetic
// run behaves consistently across repeats the way a real embedding model would.
func unitVectorFor(seed []byte) []float32 {
	return unitVector(int64(fnv64(string(seed))%1_000_000), 768)
}

// unitVector is a seeded normalized vector, the same shape embedder.Normalize produces.
// Deterministic so a synthetic run and a failing test are both reproducible.
func unitVector(seed int64, dimensions int) []float32 {
	random := rand.New(rand.NewSource(seed))
	vector := make([]float32, dimensions)
	var sumOfSquares float64
	for position := range vector {
		vector[position] = float32(random.NormFloat64())
		sumOfSquares += float64(vector[position]) * float64(vector[position])
	}

	magnitude := math.Sqrt(sumOfSquares)
	for position := range vector {
		vector[position] /= float32(magnitude)
	}
	return vector
}
