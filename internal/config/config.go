package config

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// DefaultGenerateModel is the Ollama model used for answers and for the reranking,
// rewriting, and verification stages when nothing overrides it.
const DefaultGenerateModel = "llama3.2"

type Config struct {
	MCPEnabled bool `json:"mcp_enabled"`

	// OllamaURL is where generation and embedding calls go. Empty means
	// DefaultOllamaURL. Configurable so the semantic cache in cache/ can be put in front
	// of Ollama by changing this one value — see docs/caching.md. Pointing it at a proxy
	// that is not running fails loudly at startup, which is the intended trade: whether
	// the cache is in the path should never be a guess.
	OllamaURL string `json:"ollama_url,omitempty"`

	// GenerateModel is the Ollama model used for generation. Empty means
	// DefaultGenerateModel. Configurable because model choice is the single biggest
	// lever on answer quality here — a 3B follows instructions over long context far
	// less reliably than an 8B — and comparing two models should not require editing
	// a constant and rebuilding.
	GenerateModel string `json:"generate_model,omitempty"`

	// DeepModel is the slower, more capable model a client can switch to per query.
	// Empty means DefaultDeepModel.
	DeepModel string `json:"deep_model,omitempty"`

	// GeminiModel is the cloud model offered when a Gemini API key is set. Empty means
	// DefaultGeminiModel. The key itself never goes in this file, see GeminiAPIKey
	GeminiModel string `json:"gemini_model,omitempty"`
}

// DefaultDeepModel is the "think harder" alternative offered alongside the default.
//
// llama3.2 answers in ~11s but contradicts its own context on harder questions. qwen3:8b
// with thinking off got both of the questions llama3.2 failed right in ~22s. qwen3:4b
// was the old pick, but it ignores think:false and writes its reasoning into the answer
const DefaultDeepModel = "qwen3:8b"

// DefaultGeminiModel is the newest stable Gemini Flash as of 2026-10-11
const DefaultGeminiModel = "gemini-3.8-flash"

// GeminiModelName resolves the cloud model the same way GenerateModelName does
func GeminiModelName() string {
	if override := strings.TrimSpace(os.Getenv("ENGREX_GEMINI_MODEL")); override != "" {
		return override
	}
	configuration, err := Load()
	if err == nil && strings.TrimSpace(configuration.GeminiModel) != "" {
		return strings.TrimSpace(configuration.GeminiModel)
	}
	return DefaultGeminiModel
}

// DeepModelName resolves the deep model the same way GenerateModelName resolves the
// default one.
func DeepModelName() string {
	if override := strings.TrimSpace(os.Getenv("ENGREX_DEEP_MODEL")); override != "" {
		return override
	}
	configuration, err := Load()
	if err == nil && strings.TrimSpace(configuration.DeepModel) != "" {
		return strings.TrimSpace(configuration.DeepModel)
	}
	return DefaultDeepModel
}

// GenerateModelName resolves which model to generate with, in precedence order:
// the ENGREX_GENERATE_MODEL environment variable, then the config file, then the
// default. The environment variable comes first so a single query or eval run can be
// pointed at a different model without touching saved configuration — which is exactly
// what an A/B comparison needs.
func GenerateModelName() string {
	if override := strings.TrimSpace(os.Getenv("ENGREX_GENERATE_MODEL")); override != "" {
		return override
	}
	configuration, err := Load()
	if err == nil && strings.TrimSpace(configuration.GenerateModel) != "" {
		return strings.TrimSpace(configuration.GenerateModel)
	}
	return DefaultGenerateModel
}

// DefaultOllamaURL is where Ollama itself listens.
const DefaultOllamaURL = "http://localhost:11434"

// OllamaURL resolves which server to send generation and embedding calls to, the same
// way GenerateModelName resolves the model: environment first, then the config file,
// then the default.
func OllamaURL() string {
	if override := strings.TrimSpace(os.Getenv("ENGREX_OLLAMA_URL")); override != "" {
		return strings.TrimRight(override, "/")
	}
	configuration, err := Load()
	if err == nil && strings.TrimSpace(configuration.OllamaURL) != "" {
		return strings.TrimRight(strings.TrimSpace(configuration.OllamaURL), "/")
	}
	return DefaultOllamaURL
}

func Path() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".engrex", "config.json"), nil
}

func Load() (Config, error) {
	var configuration Config

	path, err := Path()
	if err != nil {
		return configuration, err
	}

	contents, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return configuration, nil
	}
	if err != nil {
		return configuration, err
	}
	if err := json.Unmarshal(contents, &configuration); err != nil {
		return configuration, err
	}
	return configuration, nil
}

func Save(configuration Config) error {
	path, err := Path()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}

	contents, err := json.MarshalIndent(configuration, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(contents, '\n'), 0600)
}
