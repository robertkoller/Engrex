package watcher

import (
	"log"
	"os"
	"path/filepath"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/robertkoller/engrex/internal/ingest"
	"github.com/robertkoller/engrex/internal/rag"
)

const debounceDelay = 500 // milliseconds

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

	if err := os.MkdirAll(filepath.Join(path, "RawText"), os.ModePerm); err != nil {
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
	// Skip files the socket handler is ingesting itself (it records their origin).
	if ingest.ClaimPending(path) {
		return nil
	}

	text, err := ingest.ExtractText(path)
	if err != nil {
		return err
	}
	if text == "" {
		return nil
	}

	// Files that land in the finder directly have no known origin :/
	return watcher.rag.Add(text, path, "")
}
