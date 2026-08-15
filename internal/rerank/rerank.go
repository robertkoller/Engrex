// Rerank reorders the retrieved passages by relevance to the question.
// This happens after retrieval but before the anwer prompt is actually built
package rerank

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Passage is one candidate to be ranked. Kept free of any store types so the package
// has no dependency on how passages are stored or retrieved.
type Passage struct {
	ID    int64
	Text  string
	Label string
}

// Reranker scores passages against a question and returns them best-first.
//
// Implementations may return fewer passages than they were given, but must never
// invent one. A reranker that fails should return the input order unchanged rather
// than an error, so a reranking outage degrades to plain retrieval instead of taking
// the whole query down.
type Reranker interface {
	Rerank(question string, passages []Passage, topN int) ([]Passage, error)
	Name() string
}

// Ordering is what a reranker produces internally: an index into the input slice plus
// the score that put it there.
type Ordering struct {
	Index int
	Score float64
}

// LLMReranker is a listwise reranker: it shows the model every candidate at once and
// asks for their order, in a single generation call.
//
// Listwise rather than pointwise because one call is far cheaper than N, and because
// seeing the candidates together lets the model judge relative relevance — which is
// the actual question — instead of assigning absolute scores that then have to be
// compared across independent calls.
//
// This is the weaker of the two standard options. A cross-encoder (bge-reranker-v2-m3,
// Qwen3-Reranker) scores query and passage in a single forward pass with full
// cross-attention between them and would rank better, but needs an ONNX runtime or a
// llama.cpp sidecar — a second inference dependency in a project that currently has
// exactly one. It would implement this same interface; see docs/reranking.md.
type LLMReranker struct {
	BaseURL string
	Model   string
	Timeout time.Duration

	// SnippetChars caps how much of each passage is shown. The whole point is to fit
	// many candidates in one prompt, and full passages would blow the context — the
	// opening of a passage is usually enough to judge its topic.
	SnippetChars int
}

// NewLLM returns a listwise reranker backed by an Ollama generation model.
func NewLLM(baseURL, model string) *LLMReranker {
	return &LLMReranker{
		BaseURL:      baseURL,
		Model:        model,
		Timeout:      60 * time.Second,
		SnippetChars: 400,
	}
}

func (reranker *LLMReranker) Name() string { return "llm-listwise:" + reranker.Model }

// Rerank asks the model to order the candidates, and falls back to the input order on
// any failure. Retrieval already produced a usable ranking; a reranker that errors
// should cost precision, never availability.
func (reranker *LLMReranker) Rerank(question string, passages []Passage, topN int) ([]Passage, error) {
	if len(passages) <= 1 || topN <= 0 {
		return truncate(passages, topN), nil
	}

	prompt := reranker.buildPrompt(question, passages)
	response, err := reranker.generate(prompt)
	if err != nil {
		return truncate(passages, topN), nil
	}

	order := parseOrder(response, len(passages))
	if len(order) == 0 {
		return truncate(passages, topN), nil
	}

	ranked := make([]Passage, 0, len(order))
	for _, index := range order {
		ranked = append(ranked, passages[index])
	}

	// The model routinely omits candidates it considers irrelevant. Appending the
	// leftovers in their original order keeps the output a permutation of the input,
	// so nothing silently disappears when topN is larger than what the model listed.
	listed := make(map[int]bool, len(order))
	for _, index := range order {
		listed[index] = true
	}
	for index := range passages {
		if !listed[index] {
			ranked = append(ranked, passages[index])
		}
	}

	return truncate(ranked, topN), nil
}

// buildPrompt renders the candidates as a numbered list and asks for a ranked ordering.
// The output format is constrained hard — a bare comma-separated list — because
// anything looser turns parsing into guesswork.
func (reranker *LLMReranker) buildPrompt(question string, passages []Passage) string {
	var builder strings.Builder

	builder.WriteString("You rank search results by relevance. ")
	builder.WriteString("Below is a question and numbered passages retrieved for it.\n\n")
	fmt.Fprintf(&builder, "QUESTION: %s\n\nPASSAGES:\n", question)

	for index, passage := range passages {
		snippet := passage.Text
		if len(snippet) > reranker.SnippetChars {
			snippet = snippet[:reranker.SnippetChars] + "..."
		}
		snippet = strings.ReplaceAll(snippet, "\n", " ")
		fmt.Fprintf(&builder, "[%d] %s\n", index+1, snippet)
	}

	fmt.Fprintf(&builder, "\nList the passage numbers from most to least relevant to the question. ")
	builder.WriteString("Include only passages that actually help answer it. ")
	builder.WriteString("Reply with ONLY the numbers separated by commas, nothing else.\n")
	builder.WriteString("Example reply: 3,1,7,2\n\nANSWER:")

	return builder.String()
}

// generate calls Ollama and returns the completion.
func (reranker *LLMReranker) generate(prompt string) (string, error) {
	body, err := json.Marshal(map[string]any{
		"model":  reranker.Model,
		"prompt": prompt,
		"stream": false,
		"options": map[string]any{
			// Ranking should be deterministic — sampling here means the same question
			// reorders differently run to run, which makes the eval numbers noise.
			"temperature": 0,
			"num_ctx":     contextWindowFor(prompt),
		},
	})
	if err != nil {
		return "", err
	}

	client := &http.Client{Timeout: reranker.Timeout}
	response, err := client.Post(reranker.BaseURL+"/api/generate", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer response.Body.Close()

	var decoded struct {
		Response string `json:"response"`
	}
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		return "", err
	}
	return decoded.Response, nil
}

var numberPattern = regexp.MustCompile(`\d+`)

// parseOrder pulls a ranked list of zero-based indices out of the model's reply.
//
// Deliberately lenient about surrounding text and strict about the numbers: models
// wrap the answer in prose ("Here is the ranking: 3, 1, 7") often enough that
// requiring a clean line would throw away good rankings. Out-of-range and duplicate
// numbers are dropped rather than clamped, since a hallucinated index carries no
// information about what should rank where.
func parseOrder(response string, candidateCount int) []int {
	matches := numberPattern.FindAllString(response, -1)
	seen := make(map[int]bool, len(matches))
	var order []int

	for _, match := range matches {
		value, err := strconv.Atoi(match)
		if err != nil {
			continue
		}
		index := value - 1 // the prompt numbers from 1
		if index < 0 || index >= candidateCount || seen[index] {
			continue
		}
		seen[index] = true
		order = append(order, index)
	}
	return order
}

func truncate(passages []Passage, topN int) []Passage {
	if topN > 0 && len(passages) > topN {
		return passages[:topN]
	}
	return passages
}

// contextWindowFor mirrors the sizing in package rag: Ollama silently truncates
// anything past its 4096-token default, and a reranking prompt holding 40 candidates
// comfortably exceeds that. A truncated prompt would drop the last candidates entirely,
// so they could never be ranked.
func contextWindowFor(prompt string) int {
	const (
		minimum  = 4096
		maximum  = 32768
		perToken = 3
		headroom = 512
	)
	needed := len(prompt)/perToken + headroom
	if needed < minimum {
		return minimum
	}
	if needed > maximum {
		return maximum
	}
	return needed
}
