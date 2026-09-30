package cache

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"math"
	"time"
)

// Class is the kind of call a prompt came from. It decides how much approximation the
// lookup will tolerate: a reranking prompt has a constrained answer space and can match
// loosely, an answer prompt cannot, and an embedding must not match approximately at
// all.
type Class string

const (
	ClassAnswer  Class = "answer"
	ClassRerank  Class = "rerank"
	ClassRewrite Class = "rewrite"
	ClassVerify  Class = "verify"
	ClassEmbed   Class = "embed"

	// ClassOther is anything the splitter did not recognize — another application
	// pointed at the proxy, or an Engrex prompt whose format has drifted.
	ClassOther Class = "other"
)

// Entry is one cached response and everything needed to decide whether it may be served
// again.
type Entry struct {
	// ID is the key into the vector index. Starts at 1: internal/hnsw reserves -1 as
	// its empty-index sentinel, and 0 is too easy to produce by accident.
	ID int64 `json:"id"`

	// Namespace is the exact-match bucket — model, endpoint, sampling parameters, and
	// the context part of the prompt, hashed together. Similarity is only ever compared
	// within one namespace, which is what stops an answer built from one set of
	// passages being served for another.
	Namespace string `json:"namespace"`

	// ExactKey is the hash of the whole request, so a verbatim repeat is recognized
	// without embedding anything. Persisted, or the fast path would be empty after every
	// restart until each prompt had been seen again.
	ExactKey string `json:"exact_key"`

	Class    Class  `json:"class"`
	Model    string `json:"model"`
	Endpoint string `json:"endpoint"`

	// Prompt is kept whole for debugging. Nothing matches against it.
	Prompt string `json:"prompt"`

	// Semantic is the part of the prompt that was embedded — usually just the question.
	Semantic string `json:"semantic"`

	Embedding Vector `json:"embedding"`

	// ContextSketch is a MinHash of the context part, used only by the optional
	// context-tolerant lookup to check that two prompts retrieved mostly the same
	// passages. Empty when that mode is off.
	ContextSketch []uint64 `json:"context_sketch,omitempty"`

	// Payload is the provider's reply, normalized rather than stored verbatim. Whether
	// the caller streamed makes no difference to what the model produced, only to how it
	// was framed, so storing the content and re-framing it on the way out lets a
	// streaming and a non-streaming call share one entry.
	Payload Payload `json:"payload"`

	PromptTokens   int    `json:"prompt_tokens"`
	ResponseTokens int    `json:"response_tokens"`
	FinishReason   string `json:"finish_reason,omitempty"`

	// GenerationTime is how long the provider took the one time this was really
	// generated. Every later hit saves roughly this much, and summing it is where the
	// "time saved" number comes from.
	GenerationTime time.Duration `json:"generation_time"`

	CreatedAt  time.Time `json:"created_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	LastUsedAt time.Time `json:"last_used_at"`
	HitCount   int       `json:"hit_count"`

	// Tags are invalidation handles. Callers group entries under whatever label they
	// will later want to drop as a unit.
	Tags []string `json:"tags,omitempty"`
}

// Vector is an embedding that serializes as base64 rather than as a JSON array of
// floats. The journal is almost entirely vectors, and 768 dimensions cost about 4KB
// written this way against roughly 15KB as decimal text.
type Vector []float32

func (vector Vector) MarshalJSON() ([]byte, error) {
	raw := make([]byte, 4*len(vector))
	for position, component := range vector {
		binary.LittleEndian.PutUint32(raw[4*position:], math.Float32bits(component))
	}
	return json.Marshal(base64.StdEncoding.EncodeToString(raw))
}

func (vector *Vector) UnmarshalJSON(data []byte) error {
	var encoded string
	if err := json.Unmarshal(data, &encoded); err != nil {
		return err
	}
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return err
	}
	if len(raw)%4 != 0 {
		return fmt.Errorf("cache: embedding is %d bytes, not a whole number of float32s", len(raw))
	}

	decoded := make(Vector, len(raw)/4)
	for position := range decoded {
		decoded[position] = math.Float32frombits(binary.LittleEndian.Uint32(raw[4*position:]))
	}
	*vector = decoded
	return nil
}

// Payload is what a provider returned, in a shape that does not depend on how the
// caller framed the request.
type Payload struct {
	// Text is the whole completion, for generation calls.
	Text string `json:"text,omitempty"`

	// Thinking is a reasoning model's separate reasoning output, replayed ahead of Text
	Thinking string `json:"thinking,omitempty"`

	// Uncacheable marks a response that came back fine but cannot be replayed, such as a
	// chat reply made of tool calls. The caller still gets it; the cache just never stores it
	Uncacheable bool `json:"-"`

	// Embedding is the vector, for embedding calls.
	Embedding Vector `json:"embedding,omitempty"`

	// Meta carries the provider's own trailing fields — token counts, durations, the
	// finish reason — so a replayed response still reports them. Ollama's context array
	// is deliberately not kept: nothing in Engrex reads it, and it is large enough to
	// push a replayed frame past the 64KB line limit bufio.Scanner applies by default.
	Meta map[string]any `json:"meta,omitempty"`
}

// Expired reports whether the entry has passed its TTL. A zero ExpiresAt never expires.
func (entry *Entry) Expired(now time.Time) bool {
	if entry.ExpiresAt.IsZero() {
		return false
	}
	return now.After(entry.ExpiresAt)
}

// Age is how long ago the entry was really generated.
func (entry *Entry) Age(now time.Time) time.Duration {
	return now.Sub(entry.CreatedAt)
}

// HasTag reports whether the entry carries a tag, used by invalidation.
func (entry *Entry) HasTag(tag string) bool {
	for _, candidate := range entry.Tags {
		if candidate == tag {
			return true
		}
	}
	return false
}
