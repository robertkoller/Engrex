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

	"github.com/robertkoller/engrex/internal/chunker"
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
	case ".md", ".markdown", ".txt", ".html", ".htm", ".pdf", ".go", ".py", ".js", ".ts", ".java", ".c",
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
	case ".md", ".markdown":
		return cleanMarkdown(string(content)), nil
	case ".txt", ".go", ".py", ".js", ".ts", ".java", ".c",
		".cpp", ".rs", ".sh", ".json", ".yaml", ".yml", ".toml", ".csv", ".tsv", ".org", ".rst", ".tex", ".log":
		return string(content), nil
	case ".html", ".htm":
		return stripHTML(string(content)), nil
	default:
		return "", nil
	}
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

// Precompiled because ingest runs on every watched file save — recompiling a dozen
// regexes per file was pure waste.
var (
	inlineCodeRegex  = regexp.MustCompile("`([^`]+)`")
	imageRegex       = regexp.MustCompile(`!\[.*?\]\(.*?\)`)
	linkRegex        = regexp.MustCompile(`\[(.+?)\]\(.*?\)`)
	boldStarRegex    = regexp.MustCompile(`\*\*(.+?)\*\*`)
	boldUnderRegex   = regexp.MustCompile(`__(.+?)__`)
	italicStarRegex  = regexp.MustCompile(`\*(.+?)\*`)
	italicUnderRegex = regexp.MustCompile(`_(.+?)_`)
	displayMathRegex = regexp.MustCompile(`(?s)\$\$(.+?)\$\$`)
	inlineMathRegex  = regexp.MustCompile(`\$(.+?)\$`)
	horizontalRegex  = regexp.MustCompile(`(?m)^[-*]{3,}\s*$`)
)

// cleanMarkdown strips inline formatting noise while leaving the document's structure
// intact.
//
// Headings, list markers, and blockquote markers are deliberately preserved: the
// chunker splits on headings to build section-aware chunks and heading paths, so
// stripping them here would make that impossible. Only markup that carries no
// structure is removed — emphasis, image tags, link URLs (the link text is kept), and
// math delimiters.
func cleanMarkdown(input string) string {
	text := input

	text = inlineCodeRegex.ReplaceAllString(text, "$1")
	text = imageRegex.ReplaceAllString(text, "")
	text = linkRegex.ReplaceAllString(text, "$1")
	text = boldStarRegex.ReplaceAllString(text, "$1")
	text = boldUnderRegex.ReplaceAllString(text, "$1")
	text = italicStarRegex.ReplaceAllString(text, "$1")
	text = italicUnderRegex.ReplaceAllString(text, "$1")
	text = displayMathRegex.ReplaceAllString(text, "$1")
	text = inlineMathRegex.ReplaceAllString(text, "$1")
	text = horizontalRegex.ReplaceAllString(text, "")

	return text
}

// codeExtensions maps a file extension to the language name recorded in the chunk's
// content type, so the chunker knows to split on declarations rather than sentences.
var codeExtensions = map[string]string{
	".go": "go", ".py": "python", ".js": "javascript", ".ts": "typescript",
	".java": "java", ".c": "c", ".cpp": "cpp", ".rs": "rust", ".sh": "shell",
	".json": "json", ".yaml": "yaml", ".yml": "yaml", ".toml": "toml",
	".tex": "latex",
}

// ContentType classifies a path for the chunker: "markdown", "code:<language>", or
// "text". Formats that are converted to plain prose on extraction (PDF, DOCX, HTML)
// come back as "text" — whatever structure they had is gone by then.
func ContentType(path string) string {
	extension := strings.ToLower(filepath.Ext(path))
	if extension == ".md" || extension == ".markdown" {
		return chunker.TypeMarkdown
	}
	if language, isCode := codeExtensions[extension]; isCode {
		return chunker.TypeCodePrefix + language
	}
	return chunker.TypeText
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
