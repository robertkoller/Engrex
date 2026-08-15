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

	// GenerateModel is the Ollama model used for generation. Empty means
	// DefaultGenerateModel. Configurable because model choice is the single biggest
	// lever on answer quality here — a 3B follows instructions over long context far
	// less reliably than an 8B — and comparing two models should not require editing
	// a constant and rebuilding.
	GenerateModel string `json:"generate_model,omitempty"`

	// DeepModel is the slower, more capable model a client can switch to per query.
	// Empty means DefaultDeepModel.
	DeepModel string `json:"deep_model,omitempty"`
}

// DefaultDeepModel is the "think harder" alternative offered alongside the default.
//
// The pairing is deliberate: llama3.2 answers in ~11s but invents sources when asked
// which document something came from, while qwen3:4b answers correctly and takes
// ~29s. Neither is strictly better, so the choice belongs to the person asking rather
// than to a config file.
const DefaultDeepModel = "qwen3:4b"

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
