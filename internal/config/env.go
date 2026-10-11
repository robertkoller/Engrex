package config

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"
)

// RepoEnvFile is the repo's .env, filled in at build time by the Makefile
// (-ldflags -X) so the installed binary can find it from any working directory
var RepoEnvFile = ""

// GeminiAPIKey returns the Gemini key, or "" when none is set. It's looked up on every
// call, so filling in .env takes effect without restarting the daemon. In order:
// GEMINI_API_KEY in the environment, ENGREX_ENV_FILE, the repo's .env, ~/.engrex/.env
func GeminiAPIKey() string {
	if key := strings.TrimSpace(os.Getenv("GEMINI_API_KEY")); key != "" {
		return key
	}
	for _, path := range envFiles() {
		if key := readEnvValue(path, "GEMINI_API_KEY"); key != "" {
			return key
		}
	}
	return ""
}

// GeminiModelIfConfigured is the Gemini model to offer, or "" when there's no key
func GeminiModelIfConfigured() string {
	if GeminiAPIKey() == "" {
		return ""
	}
	return GeminiModelName()
}

func envFiles() []string {
	var paths []string
	if path := strings.TrimSpace(os.Getenv("ENGREX_ENV_FILE")); path != "" {
		paths = append(paths, path)
	}
	if RepoEnvFile != "" {
		paths = append(paths, RepoEnvFile)
	}
	if home, err := os.UserHomeDir(); err == nil {
		paths = append(paths, filepath.Join(home, ".engrex", ".env"))
	}
	return paths
}

// readEnvValue pulls one KEY=value out of a .env file. Handles comments, blank lines,
// an optional "export ", and surrounding quotes. Missing files read as ""
func readEnvValue(path, name string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		key, value, found := strings.Cut(line, "=")
		if !found || strings.TrimSpace(key) != name {
			continue
		}
		value = strings.TrimSpace(value)
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		return value
	}
	return ""
}
