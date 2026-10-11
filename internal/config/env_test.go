package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReadEnvValue(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	contents := "# comment\n\nOTHER=1\nexport GEMINI_API_KEY=\"abc 123\"\nGEMINI_API_KEY_OLD=nope\n"
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}

	if got := readEnvValue(path, "GEMINI_API_KEY"); got != "abc 123" {
		t.Errorf("got %q, want %q", got, "abc 123")
	}
	if got := readEnvValue(path, "MISSING"); got != "" {
		t.Errorf("missing key gave %q", got)
	}
	if got := readEnvValue(filepath.Join(t.TempDir(), "nope"), "GEMINI_API_KEY"); got != "" {
		t.Errorf("missing file gave %q", got)
	}
}

func TestGeminiAPIKeyPrecedence(t *testing.T) {
	path := filepath.Join(t.TempDir(), ".env")
	if err := os.WriteFile(path, []byte("GEMINI_API_KEY=from-file\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", t.TempDir())
	t.Setenv("ENGREX_ENV_FILE", path)

	t.Setenv("GEMINI_API_KEY", "")
	if got := GeminiAPIKey(); got != "from-file" {
		t.Errorf("file key: got %q", got)
	}
	t.Setenv("GEMINI_API_KEY", "from-env")
	if got := GeminiAPIKey(); got != "from-env" {
		t.Errorf("environment should win, got %q", got)
	}

	// An empty value in the file means no key, so Gemini isn't offered
	if err := os.WriteFile(path, []byte("GEMINI_API_KEY=\n"), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GEMINI_API_KEY", "")
	if got := GeminiModelIfConfigured(); got != "" {
		t.Errorf("no key should offer no model, got %q", got)
	}
}
