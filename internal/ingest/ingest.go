package ingest

import (
	"bytes"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"

	"github.com/ledongthuc/pdf"
)

const minFileSize = 20

// pending tracks files the socket handler is ingesting itself, so the watcher
// skips them instead of double-ingesting with an empty origin.
var (
	pendingMu sync.Mutex
	pending   = make(map[string]bool)
)

// MarkPending records that a path is being ingested via the socket so the watcher skips it.
func MarkPending(path string) {
	pendingMu.Lock()
	pending[path] = true
	pendingMu.Unlock()
}

// ClaimPending reports whether a path was socket-ingested, consuming the mark.
func ClaimPending(path string) bool {
	pendingMu.Lock()
	defer pendingMu.Unlock()
	if pending[path] {
		delete(pending, path)
		return true
	}
	return false
}

// IsSupported reports whether the file type can be ingested.
func IsSupported(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".txt", ".html", ".htm", ".pdf":
		return true
	default:
		return false
	}
}

// ExtractText returns plain text ready for RAG, or "" for unsupported/empty/binary
// files (the caller should skip those).
func ExtractText(path string) (string, error) {
	info, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if info.IsDir() {
		return "", nil
	}

	extension := strings.ToLower(filepath.Ext(path))
	if extension == ".pdf" {
		text, err := extractPDF(path)
		if err != nil {
			return "", err
		}
		if len(text) < minFileSize {
			return "", nil
		}
		return text, nil
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if len(content) < minFileSize {
		return "", nil
	}
	if bytes.Contains(content, []byte{0}) {
		return "", nil
	}
	switch extension {
	case ".md":
		return stripMarkdown(string(content)), nil
	case ".txt":
		return string(content), nil
	case ".html", ".htm":
		return stripHTML(string(content)), nil
	default:
		return "", nil
	}
}

func extractPDF(path string) (string, error) {
	file, reader, err := pdf.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	var builder strings.Builder
	for pageIndex := 1; pageIndex <= reader.NumPage(); pageIndex++ {
		page := reader.Page(pageIndex)
		if page.V.IsNull() {
			continue
		}
		text, err := page.GetPlainText(nil)
		if err != nil {
			continue
		}
		builder.WriteString(text)
	}
	return builder.String(), nil
}

func stripMarkdown(input string) string {
	text := input

	text = regexp.MustCompile("(?s)```[a-zA-Z]*\\n?(.*?)```").ReplaceAllString(text, "$1")

	text = regexp.MustCompile("`([^`]+)`").ReplaceAllString(text, "$1")
	text = regexp.MustCompile(`!\[.*?\]\(.*?\)`).ReplaceAllString(text, "")
	text = regexp.MustCompile(`\[(.+?)\]\(.*?\)`).ReplaceAllString(text, "$1")
	text = regexp.MustCompile(`(?m)^#{1,6}\s+`).ReplaceAllString(text, "")
	text = regexp.MustCompile(`\*\*(.+?)\*\*`).ReplaceAllString(text, "$1")
	text = regexp.MustCompile(`__(.+?)__`).ReplaceAllString(text, "$1")
	text = regexp.MustCompile(`\*(.+?)\*`).ReplaceAllString(text, "$1")
	text = regexp.MustCompile(`_(.+?)_`).ReplaceAllString(text, "$1")
	text = regexp.MustCompile(`(?m)^>\s?`).ReplaceAllString(text, "")
	text = regexp.MustCompile(`(?s)\$\$(.+?)\$\$`).ReplaceAllString(text, "$1")
	text = regexp.MustCompile(`\$(.+?)\$`).ReplaceAllString(text, "$1")
	text = regexp.MustCompile(`(?m)^[-*]{3,}\s*$`).ReplaceAllString(text, "")
	text = regexp.MustCompile(`(?m)^\s*[-*+]\s+`).ReplaceAllString(text, "")
	text = regexp.MustCompile(`(?m)^\s*\d+\.\s+`).ReplaceAllString(text, "")

	return text
}

func stripHTML(input string) string {
	text := regexp.MustCompile(`<[^>]+>`).ReplaceAllString(input, "")
	text = strings.ReplaceAll(text, "&amp;", "&")
	text = strings.ReplaceAll(text, "&lt;", "<")
	text = strings.ReplaceAll(text, "&gt;", ">")
	text = strings.ReplaceAll(text, "&quot;", "\"")
	text = strings.ReplaceAll(text, "&#39;", "'")
	text = strings.ReplaceAll(text, "&nbsp;", " ")
	return text
}
