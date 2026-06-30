package rag

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/robertkoller/engrex/internal/chunker"
	"github.com/robertkoller/engrex/internal/embedder"
	"github.com/robertkoller/engrex/internal/store"
)

const ollamaBaseURL = "http://localhost:11434"
const generateModel = "llama3.2"

// RAG wires the embedder, store, and LLM together into the add/query pipeline.
type RAG struct {
	embedder *embedder.Embedder
	store    *store.Store
}

// New returns a RAG instance. Checks that Ollama is reachable before returning.
func New(s *store.Store) (*RAG, error) {
	embed := embedder.New(ollamaBaseURL)
	if err := embed.Ping(); err != nil {
		return nil, err
	}
	return &RAG{embedder: embed, store: s}, nil
}

// Add chunks the text, embeds each chunk, and stores them with the given source label.
func (r *RAG) Add(text string, source string) error {
	chunks := chunker.Chunk(text)
	savedCount := 0
	for _, chunk := range chunks {
		vector, err := r.embedder.Embed(chunk)
		if err != nil {
			return err
		}

		inserted, err := r.store.Insert(chunk, source, vector)
		if err != nil {
			return err
		}
		if inserted {
			savedCount++
		} else {
			fmt.Println("Skipped: too similar to something already stored.")
		}
	}

	fmt.Printf("Saved %d chunk(s).\n", savedCount)
	return nil
}

// DebugSearch embeds the question and returns all chunks with raw distances, no filtering.
func (r *RAG) DebugSearch(question string) ([]store.Chunk, error) {
	queryVec, err := r.embedder.Embed(question)
	if err != nil {
		return nil, err
	}
	return r.store.RawSearch(queryVec)
}

// Query embeds the question, retrieves the top-K most relevant chunks,
// builds a RAG prompt, and streams the LLM response to stdout.
func (r *RAG) Query(question string, maxDistance float64, topK int) error {
	queryVec, err := r.embedder.Embed(question)
	if err != nil {
		return err
	}

	chunks, err := r.store.Search(queryVec, maxDistance, topK)
	if err != nil {
		return err
	}

	var prompt string
	if len(chunks) == 0 {
		fmt.Println("[No relevant notes found — answering from outside knowledge]")
		prompt = buildNoContextPrompt(question)
	} else {
		prompt = buildPrompt(question, chunks)
	}

	body, err := json.Marshal(map[string]any{
		"model":  generateModel,
		"prompt": prompt,
		"stream": true,
	})
	if err != nil {
		return err
	}
	response, err := http.Post(ollamaBaseURL+"/api/generate", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer response.Body.Close()

	scanner := bufio.NewScanner(response.Body)
	for scanner.Scan() {
		var token struct {
			Response string `json:"response"`
			Done     bool   `json:"done"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &token); err != nil {
			fmt.Print(err)
		}
		fmt.Print(token.Response)
		if token.Done {
			break
		}
	}
	fmt.Println()
	return nil
}

// buildPrompt formats retrieved chunks and the user question into a RAG prompt.
func buildPrompt(question string, chunks []store.Chunk) string {
	var builder strings.Builder // yes im using string builder, no its not ai who wrote this ik that string builder is less compute

	builder.WriteString("You are a personal knowledge assistant. Answer the question using ONLY the provided context below — these are notes the user has personally saved. Do not use any outside knowledge. If you include information that is not explicitly stated in the context, you MUST prefix that sentence with \"[outside knowledge]:\" so the user knows it did not come from their notes. Be direct and specific.\n\nContext:\n")

	for index, chunk := range chunks {
		fmt.Fprintf(&builder, "[%d] (saved %s, source: %s)\n%s\n\n", index+1, chunk.CreatedAt.Format("2006-01-02"), chunk.Source, chunk.Text)
	}

	fmt.Fprintf(&builder, "Question: %s", question)
	return builder.String()
}

// buildNoContextPrompt builds a prompt for when no stored notes are relevant.
// Instructs the LLM to answer from its own knowledge and label it clearly.
func buildNoContextPrompt(question string) string {
	return fmt.Sprintf(
		"You are a personal knowledge assistant. The user has no saved notes relevant to this question. "+
			"Answer from your own training knowledge, but start your response with \"[outside knowledge]: \" "+
			"to make clear this answer does not come from their saved notes.\n\nQuestion: %s",
		question,
	)
}
