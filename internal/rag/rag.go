package rag

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/robertkoller/engrex/internal/chunker"
	"github.com/robertkoller/engrex/internal/embedder"
	"github.com/robertkoller/engrex/internal/store"
)

const ollamaBaseURL = "http://localhost:11434"
const generateModel = "llama3.2"
const DefaultSearchDistance = 0.95
const DefaultSearchResults = 10

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

// Add chunks the text, embeds each chunk, and stores them with the given source
// label and origin (the original path a file was added from; "" when unknown).
func (r *RAG) Add(text string, source string, origin string) error {
	chunks, err := chunker.Chunk(text)
	if err != nil {
		return err
	}
	savedCount := 0
	for _, chunk := range chunks {
		vector, err := r.embedder.Embed(chunk)
		if err != nil {
			return err
		}

		inserted, err := r.store.Insert(chunk, source, origin, vector)
		if err != nil {
			return err
		}
		if inserted {
			savedCount++
		} else {
			fmt.Println("Skipped: too similar to something already stored.")
		}
	}

	if _, statErr := os.Stat(source); statErr != nil {
		if isWebURL(origin) {
			header := fmt.Sprintf("Title: %s\nSource: %s\n\n", source, origin)
			name := sanitizeFilename(source) + ".txt"
			if err := cliTextStub(name, []byte(header+text)); err != nil {
				log.Printf("failed to write stub file: %v", err)
			}
		} else {
			var title string
			if len(text) > 20 {
				title = text[:20]
			} else {
				title = text
			}
			if err := cliTextStub(fmt.Sprintf("%v.txt", title), []byte(text)); err != nil {
				log.Printf("failed to write stub file: %v", err)
			}
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
func (r *RAG) Query(out io.Writer, question string, maxDistance float64, topK int) error {
	question, options := parseQueryFlags(question)
	queryVec, err := r.embedder.Embed(question)
	if err != nil {
		return err
	}

	chunks, err := r.store.Search(queryVec, maxDistance, topK)
	if err != nil {
		return err
	}

	if err := json.NewEncoder(out).Encode(map[string][]string{"sources": collectSources(chunks)}); err != nil {
		return err
	}

	var prompt string
	if len(chunks) == 0 {
		fmt.Fprintln(out, "[No relevant notes found — answering from outside knowledge]")
		prompt = buildNoContextPrompt(question)
	} else {
		prompt = buildPrompt(question, chunks, options)
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
			fmt.Fprint(out, err)
		}
		fmt.Fprint(out, token.Response)
		if token.Done {
			break
		}
	}
	fmt.Fprintln(out)
	return nil
}

// collectSources returns the deduplicated real-file sources of the retrieved chunks,
// in order, skipping non-file sources like "cli" and "hotkey". Prefers the origin
// (where a file was added from) over the internal ~/Engrex copy path when known.
func collectSources(chunks []store.Chunk) []string {
	seen := make(map[string]bool)
	sources := make([]string, 0)
	for _, chunk := range chunks {
		source := chunk.Source
		if chunk.Origin != "" {
			source = chunk.Origin
		}
		if !isLinkableSource(source) {
			continue
		}
		if seen[source] {
			continue
		}
		seen[source] = true
		sources = append(sources, source)
	}
	return sources
}

// isLinkableSource reports whether a source is something the UI can open —
// an absolute file path or a web URL (skips labels like "cli" and "hotkey").
func isLinkableSource(source string) bool {
	return filepath.IsAbs(source) || isWebURL(source)
}

func isWebURL(source string) bool {
	return strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://")
}

// sanitizeFilename makes a page title safe to use as a filename: strips path
// separators, trims whitespace, and caps the length.
func sanitizeFilename(name string) string {
	name = strings.ReplaceAll(name, "/", "-")
	name = strings.ReplaceAll(name, "\\", "-")
	name = strings.TrimSpace(name)
	if runes := []rune(name); len(runes) > 60 {
		name = strings.TrimSpace(string(runes[:60]))
	}
	if name == "" {
		name = "untitled"
	}
	return name
}

// Flag options
type queryOptions struct {
	includeDate   bool
	includeSource bool
}

// Reformats the query to seperate the quuery and flags
func parseQueryFlags(question string) (string, queryOptions) {
	fields := strings.Fields(question)
	kept := make([]string, 0, len(fields))
	var options queryOptions
	for _, field := range fields {
		switch field {
		case "--date":
			options.includeDate = true
		case "--source":
			options.includeSource = true
		default:
			kept = append(kept, field)
		}
	}
	return strings.Join(kept, " "), options
}

// buildPrompt formats retrieved chunks and the user question into a RAG prompt.
func buildPrompt(question string, chunks []store.Chunk, options queryOptions) string {
	var builder strings.Builder // yes im using string builder, no its not ai who wrote this ik that string builder is less compute

	builder.WriteString("You are a personal knowledge assistant. The notes below are the user's OWN private notes that they saved themselves, so treat everything in them as factual and answer directly from them. Never refuse, moralize, or add disclaimers about privacy, personal relationships, or opinions — the user is only asking about their own saved information. Answer the question as completely as possible using ALL of the relevant notes provided — do not skip or summarize away details, cover everything that is relevant. If the notes do not fully answer the question, supplement with your own knowledge but prefix every sentence that comes from outside the notes with \"[outside knowledge]:\" so the user can clearly tell the difference. Be direct and comprehensive.")

	// Citation instruction goes with the system instructions at the top — NOT next to
	// the notes/question, or the model tends to echo it back into its answer.
	if options.includeDate || options.includeSource {
		var parts []string
		if options.includeDate {
			parts = append(parts, "the date it was saved")
		}
		if options.includeSource {
			parts = append(parts, "its source file")
		}
		fmt.Fprintf(&builder, " Whenever you use information from a note, cite %s once, in parentheses right where you use it (for example: \"(source: notes.md, saved 2025-01-02)\"). Cite each note only once — not on every sentence.", strings.Join(parts, " and "))
	}

	builder.WriteString("\n\nContext:\n")

	for index, chunk := range chunks {
		fmt.Fprintf(&builder, "[%d] (saved %s, source: %s)\n%s\n\n", index+1, chunk.CreatedAt.Format("2006-01-02"), filepath.Base(chunk.Source), chunk.Text)
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

// Writes a .txt stub for raw CLI/hotkey text into ~/Engrex/RawText so the user can
// browse it as a file. RawText is a subfolder of the watched dir but is not watched
// itself, so these stubs are never re-ingested.
func cliTextStub(name string, content []byte) error {
	home, _ := os.UserHomeDir()
	path := filepath.Join(home, "Engrex", "RawText")

	err := os.MkdirAll(path, os.ModePerm)
	if err != nil {
		return err
	}
	path = filepath.Join(path, name)
	extension := filepath.Ext(path)
	base := strings.TrimSuffix(path, extension)
	_, err = os.Stat(path)

	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	for i := 1; err == nil; i++ {
		numberedPath := fmt.Sprintf("%s (%d)%s", base, i, extension)
		_, err = os.Stat(numberedPath)

		if errors.Is(err, fs.ErrNotExist) {
			path = numberedPath
			break
		}

		if err != nil {
			return err
		}

		if i >= 1000 {
			return errors.New("Too many duplicate files found, aborting creation")
		}
	}
	return os.WriteFile(path, content, 0644)

}
