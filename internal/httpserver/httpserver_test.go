package httpserver

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// These requests all fail before the RAG pipeline is touched, so a nil one is fine
func TestCaptureFileRejectsBadRequests(t *testing.T) {
	server := &Server{}
	cases := []struct {
		name   string
		method string
		target string
		want   int
	}{
		{"get", http.MethodGet, "/capture-file?name=a.pdf&url=https://x", http.StatusMethodNotAllowed},
		{"no name", http.MethodPost, "/capture-file?url=https://x", http.StatusBadRequest},
		{"no url", http.MethodPost, "/capture-file?name=a.pdf", http.StatusBadRequest},
		{"unsupported", http.MethodPost, "/capture-file?name=movie.mp4&url=https://x", http.StatusUnsupportedMediaType},
		{"no extension", http.MethodPost, "/capture-file?name=section%201&url=https://x", http.StatusUnsupportedMediaType},
		{"empty text", http.MethodPost, "/capture-file?name=a.txt&url=https://x", http.StatusUnprocessableEntity},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			request := httptest.NewRequest(testCase.method, testCase.target, strings.NewReader(""))
			recorder := httptest.NewRecorder()
			server.handleCaptureFile(recorder, request)
			if recorder.Code != testCase.want {
				t.Errorf("status %d, want %d (%s)", recorder.Code, testCase.want, recorder.Body.String())
			}
		})
	}
}

func TestCaptureFileRejectsOversizedUpload(t *testing.T) {
	server := &Server{}
	body := strings.NewReader(strings.Repeat("a", maxCaptureFileBytes+1))
	request := httptest.NewRequest(http.MethodPost, "/capture-file?name=big.txt&url=https://x", body)
	recorder := httptest.NewRecorder()
	server.handleCaptureFile(recorder, request)
	if recorder.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status %d, want 413", recorder.Code)
	}
}

func TestExtractUploadUsesExtensionAndCleansUp(t *testing.T) {
	before, _ := filepath.Glob(filepath.Join(os.TempDir(), "engrex-capture-*"))

	text, err := extractUpload(strings.NewReader("# Notes\n\nSome **bold** text that is long enough."), ".md")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(text, "Some bold text") {
		t.Errorf("markdown wasn't cleaned, got %q", text)
	}

	after, _ := filepath.Glob(filepath.Join(os.TempDir(), "engrex-capture-*"))
	if len(after) > len(before) {
		t.Errorf("temp file left behind: %v", after)
	}
}
