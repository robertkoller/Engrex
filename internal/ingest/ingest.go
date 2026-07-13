package ingest

import (
	"archive/zip"
	"bytes"
	"encoding/xml"
	"io"
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
	case ".md", ".txt", ".html", ".htm", ".pdf", ".go", ".py", ".js", ".ts", ".java", ".c",
		".cpp", ".rs", ".sh", ".json", ".yaml", ".yml", ".toml", ".csv", ".tsv", ".org", ".rst", ".tex", ".log", ".docx":
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
	// Binary container formats are unpacked by their own readers, before the text
	// path's null-byte guard below would reject them.
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
	if extension == ".docx" {
		text, err := extractDOCX(path)
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
	case ".txt", ".go", ".py", ".js", ".ts", ".java", ".c",
		".cpp", ".rs", ".sh", ".json", ".yaml", ".yml", ".toml", ".csv", ".tsv", ".org", ".rst", ".tex", ".log":
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

// extractDOCX pulls the body text out of a .docx file. A .docx is a ZIP archive whose
// main content lives in word/document.xml as WordprocessingML; we read that entry and
// flatten its runs to plain text. Returns "" (no error) for a zip with no main document.
func extractDOCX(path string) (string, error) {
	reader, err := zip.OpenReader(path)
	if err != nil {
		return "", err
	}
	defer reader.Close()

	var documentEntry *zip.File
	for _, file := range reader.File {
		if file.Name == "word/document.xml" {
			documentEntry = file
			break
		}
	}
	if documentEntry == nil {
		return "", nil
	}

	content, err := documentEntry.Open()
	if err != nil {
		return "", err
	}
	defer content.Close()

	return parseWordDocument(content)
}

// parseWordDocument walks WordprocessingML and returns its text: the contents of each
// <w:t> run, with paragraphs (<w:p>) separated by newlines and tabs/breaks preserved.
func parseWordDocument(source io.Reader) (string, error) {
	decoder := xml.NewDecoder(source)
	var builder strings.Builder
	insideText := false

	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}

		switch element := token.(type) {
		case xml.StartElement:
			switch element.Name.Local {
			case "t":
				insideText = true
			case "tab":
				builder.WriteByte('\t')
			case "br", "cr":
				builder.WriteByte('\n')
			}
		case xml.CharData:
			if insideText {
				builder.Write(element)
			}
		case xml.EndElement:
			switch element.Name.Local {
			case "t":
				insideText = false
			case "p":
				builder.WriteByte('\n')
			}
		}
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
