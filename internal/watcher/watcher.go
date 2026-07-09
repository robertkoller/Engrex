package watcher

import (
	"bytes"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/ledongthuc/pdf"
	"github.com/robertkoller/engrex/internal/rag"
)

const debounceDelay = 500 // milliseconds
const minFileSize = 20    // characters — skip tiny files

// Watcher monitors a directory for file saves and ingests them via RAG.
type Watcher struct {
	rag       *rag.RAG
	fsWatcher *fsnotify.Watcher
}

// New returns a Watcher that will ingest saved files using the given RAG pipeline.
func New(ragPipeline *rag.RAG) *Watcher {
	fsWatcher, err := fsnotify.NewWatcher()
	if err != nil {
		log.Fatal(err)
	}
	return &Watcher{rag: ragPipeline, fsWatcher: fsWatcher}
}

// Start begins watching the directory and blocks until Stop is called.
func (watcher *Watcher) Start() error {
	home, _ := os.UserHomeDir()
	path := filepath.Join(home, "Engrex")

	err := os.MkdirAll(path, os.ModePerm)
	if err != nil {
		return err
	}

	if err := watcher.fsWatcher.Add(path); err != nil {
		return err
	}

	var debounceTimer *time.Timer
	for {
		select {
		case event, ok := <-watcher.fsWatcher.Events:
			// a file changed
			if !ok {
				return nil
			}

			if event.Op.Has(fsnotify.Write) || event.Op.Has(fsnotify.Create) {
				if debounceTimer != nil {
					debounceTimer.Stop()
				}
				debounceTimer = time.AfterFunc(debounceDelay*time.Millisecond, func() {
					if err := watcher.ingest(event.Name); err != nil {
						log.Printf("Failed to ingest %s: %v", event.Name, err)
					}

				})

			}

		case err, ok := <-watcher.fsWatcher.Errors:
			// something went wrong
			if !ok {
				return nil
			}

			return err
		}
	}
}

// Stop shuts down the fsnotify watcher cleanly.
func (watcher *Watcher) Stop() {
	watcher.fsWatcher.Close()
}

// ingest reads the file at path and passes its content to the RAG pipeline.
func (watcher *Watcher) ingest(path string) error {
	extension := strings.ToLower(filepath.Ext(path))

	// PDFs are binary so they take a separate path — skip the text-file checks
	if extension == ".pdf" {
		output, err := extractPDF(path)
		if err != nil {
			return err
		}
		if len(output) < minFileSize {
			return nil
		}
		return watcher.rag.Add(output, path)
	}

	content, err := os.ReadFile(path)
	if err != nil {
		return err
	}

	if len(content) < minFileSize {
		return nil
	}

	if bytes.Contains(content, []byte{0}) {
		return nil
	}

	var output string
	switch extension {
	case ".md":
		output = stripMarkdown(string(content))
	case ".txt":
		output = string(content)
	case ".html", ".htm":
		output = stripHTML(string(content))
	default:
		return nil
	}

	if err := watcher.rag.Add(output, path); err != nil {
		return err
	}
	return nil
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
