// Package rewrite turns a user's question into the queries retrieval should actually
// run.
//
// A single embedding of a raw question is a poor search key in two common cases: a
// multi-part question ("what did I save about X, and how does it compare to Y?")
// averages into a vector that sits between both topics and matches neither well, and a
// follow-up question ("what about the second one?") carries almost no retrievable
// content on its own. Decomposing the first and expanding the second both raise recall
// for the cost of one generation call.
package rewrite

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// maxSubQueries caps decomposition. Each sub-query costs an embedding call and a
// search, and questions that genuinely need more than a handful of independent lookups
// are rare enough that the cap is cheaper than the runaway case.
const maxSubQueries = 4

// Rewriter produces the queries to run for a question.
type Rewriter interface {
	// Rewrite returns one or more queries. It must always return at least the original
	// question, so a rewriting failure degrades to today's behavior rather than
	// searching for nothing.
	Rewrite(question string) ([]string, error)
	Name() string
}

// LLMRewriter decomposes and expands questions with a local generation model.
type LLMRewriter struct {
	BaseURL string
	Model   string
	Timeout time.Duration
}

// NewLLM returns a rewriter backed by an Ollama generation model.
func NewLLM(baseURL, model string) *LLMRewriter {
	return &LLMRewriter{BaseURL: baseURL, Model: model, Timeout: 45 * time.Second}
}

func (rewriter *LLMRewriter) Name() string { return "llm-decompose:" + rewriter.Model }

// Rewrite splits a question into sub-queries, always including the original.
//
// The original is kept unconditionally rather than replaced by the decomposition. A
// rewrite that drops a constraint the user actually cared about would silently lose
// recall, and keeping the original costs one extra search while making the rewrite
// strictly additive — it can only add candidates, never remove them.
func (rewriter *LLMRewriter) Rewrite(question string) ([]string, error) {
	trimmed := strings.TrimSpace(question)
	if trimmed == "" {
		return nil, nil
	}
	if !NeedsDecomposition(trimmed) {
		return []string{trimmed}, nil
	}

	response, err := rewriter.generate(buildPrompt(trimmed))
	if err != nil {
		return []string{trimmed}, nil
	}

	queries := []string{trimmed}
	for _, candidate := range parseQueries(response) {
		if !containsFold(queries, candidate) {
			queries = append(queries, candidate)
		}
		if len(queries) >= maxSubQueries+1 {
			break
		}
	}
	return queries, nil
}

// conjunctions are the markers that most reliably indicate a question is really
// several questions.
var conjunctions = []string{
	" and ", " also ", " as well as ", " plus ",
	" compare ", " versus ", " vs ", " difference between ",
}

// NeedsDecomposition reports whether a question looks like it has independent parts
// worth searching separately.
//
// A cheap syntactic gate rather than an LLM classifier: most questions are simple, and
// asking a model "is this multi-part?" before every search would double the latency of
// the common case to help the rare one. False negatives here cost nothing beyond
// today's behavior.
func NeedsDecomposition(question string) bool {
	lowered := strings.ToLower(question)

	if strings.Count(question, "?") > 1 {
		return true
	}
	for _, marker := range conjunctions {
		if strings.Contains(lowered, marker) {
			return true
		}
	}
	// Long questions tend to bundle several asks even without an explicit conjunction.
	return len(strings.Fields(question)) > 25
}

func buildPrompt(question string) string {
	var builder strings.Builder

	builder.WriteString("Break a search question into the separate things that must be looked up.\n\n")
	builder.WriteString("Rules:\n")
	builder.WriteString("- Write each lookup on its own line.\n")
	builder.WriteString("- Each line must stand alone: replace pronouns and vague references with the actual subject.\n")
	builder.WriteString("- Expand abbreviations and acronyms where the meaning is clear.\n")
	builder.WriteString("- Do not answer the question. Do not number the lines. Do not add commentary.\n")
	builder.WriteString("- If the question only asks one thing, output that one line.\n\n")

	builder.WriteString("QUESTION: What did I save about ResNet, and how does it compare to VGG?\n")
	builder.WriteString("LOOKUPS:\nResNet architecture\nVGG architecture\nResNet compared to VGG performance\n\n")

	builder.WriteString("QUESTION: " + question + "\nLOOKUPS:")
	return builder.String()
}

func (rewriter *LLMRewriter) generate(prompt string) (string, error) {
	body, err := json.Marshal(map[string]any{
		"model":  rewriter.Model,
		"prompt": prompt,
		"stream": false,
		"options": map[string]any{
			// Deterministic, so the same question decomposes identically across eval
			// runs and a measured delta reflects the change under test.
			"temperature": 0,
			"num_ctx":     4096,
		},
	})
	if err != nil {
		return "", err
	}

	client := &http.Client{Timeout: rewriter.Timeout}
	response, err := client.Post(rewriter.BaseURL+"/api/generate", "application/json", bytes.NewReader(body))
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

// parseQueries pulls usable lookup lines out of a completion, discarding the framing
// that small models add despite being told not to.
func parseQueries(response string) []string {
	var queries []string

	for _, line := range strings.Split(response, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "-")
		line = strings.TrimPrefix(line, "*")
		line = stripLeadingNumber(line)
		line = strings.TrimSpace(line)

		if line == "" || len(line) < 3 {
			continue
		}
		// Models reliably echo the label back despite the instruction not to.
		if strings.EqualFold(line, "LOOKUPS:") || strings.HasPrefix(strings.ToLower(line), "question:") {
			continue
		}
		// A line ending in a colon is a heading, not a lookup.
		if strings.HasSuffix(line, ":") {
			continue
		}
		queries = append(queries, line)
	}
	return queries
}

// stripLeadingNumber removes "1." / "2)" style prefixes.
func stripLeadingNumber(line string) string {
	index := 0
	for index < len(line) && line[index] >= '0' && line[index] <= '9' {
		index++
	}
	if index == 0 || index >= len(line) {
		return line
	}
	if line[index] == '.' || line[index] == ')' {
		return line[index+1:]
	}
	return line
}

func containsFold(haystack []string, needle string) bool {
	for _, item := range haystack {
		if strings.EqualFold(strings.TrimSpace(item), strings.TrimSpace(needle)) {
			return true
		}
	}
	return false
}
