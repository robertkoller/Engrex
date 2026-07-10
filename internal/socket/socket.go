package socket

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/robertkoller/engrex/internal/ingest"
	"github.com/robertkoller/engrex/internal/rag"
	"github.com/robertkoller/engrex/internal/store"
)

// Socket listens on a Unix socket and handles commands from the CLI.
type Socket struct {
	rag      *rag.RAG
	store    *store.Store
	listener net.Listener
}

// Json for the command recieved by daemon
type Command struct {
	Type   string `json:"type"`
	Text   string `json:"text"`
	Source string `json:"source"`
}

type Response struct {
	Error string `json:"error,omitempty"`
}

// New returns a Socket that will handle commands using the given RAG pipeline and store.
func New(ragPipeline *rag.RAG, store *store.Store) *Socket {
	return &Socket{rag: ragPipeline, store: store}
}

// Start begins listening on the Unix socket and blocks until Stop is called.
func (socket *Socket) Start() error {
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".engrex")

	if err := os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	path := filepath.Join(dir, "daemon.sock")

	_ = os.Remove(path)
	sock, err := net.Listen("unix", path)
	if err != nil {
		return err
	}

	socket.listener = sock

	for {
		conn, err := sock.Accept()
		if err != nil {
			return nil
		}
		go socket.handleConnection(conn)
	}
}

// Stop closes the listener so Start returns.
func (socket *Socket) Stop() {
	socket.listener.Close()
}

// handleConnection reads a JSON command from the connection, dispatches it,
// and writes the response back.
func (socket *Socket) handleConnection(conn net.Conn) {
	defer conn.Close()
	var command Command
	if err := json.NewDecoder(conn).Decode(&command); err != nil {
		log.Printf("failed to decode command: %v", err)
		return
	}

	switch command.Type {
	case "add":
		if err := socket.rag.Add(command.Text, command.Source, ""); err != nil {
			if err := json.NewEncoder(conn).Encode(Response{Error: err.Error()}); err != nil {
				log.Printf("failed encoding error response: %v", err)
			}
		} else {
			if err := json.NewEncoder(conn).Encode(Response{}); err != nil {
				log.Printf("failed encoding success response: %v", err)
			}
		}
	case "query":
		// Tokens are streamed directly to conn via io.Writer — the CLI reads until
		// the connection closes, so we must not write a JSON response after.
		if err := socket.rag.Query(conn, command.Text, rag.DefaultSearchDistance, rag.DefaultSearchResults); err != nil {
			fmt.Fprintf(conn, "\n[Error: %v]\n", err)
		}
	case "delete":
		if err := socket.store.Delete(command.Text); err != nil {
			if err := json.NewEncoder(conn).Encode(Response{Error: err.Error()}); err != nil {
				log.Printf("failed encoding error response: %v", err)
			}
		} else {
			if err := json.NewEncoder(conn).Encode(Response{}); err != nil {
				log.Printf("failed encoding success response: %v", err)
			}
		}
	case "addfile":
		if err := socket.addFile(command.Text); err != nil {
			if err := json.NewEncoder(conn).Encode(Response{Error: err.Error()}); err != nil {
				log.Printf("failed encoding error response: %v", err)
			}
		} else {
			if err := json.NewEncoder(conn).Encode(Response{}); err != nil {
				log.Printf("failed encoding success response: %v", err)
			}
		}
	}
}

// addFile copies the file at originalPath into ~/Engrex, ingests it, and records
// the original path as the chunk origin. The watcher is told to skip the copy so
// it doesn't double-ingest it with an empty origin.
func (socket *Socket) addFile(originalPath string) error {
	// Reject unsupported types before copying anything into ~/Engrex.
	if !ingest.IsSupported(originalPath) {
		return fmt.Errorf("unsupported file type: %s", filepath.Ext(originalPath))
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	engrexDir := filepath.Join(home, "Engrex")
	if err := os.MkdirAll(engrexDir, os.ModePerm); err != nil {
		return err
	}

	destination := uniqueDestination(engrexDir, filepath.Base(originalPath))

	// Mark before creating so we dont like double create because of the watcher
	ingest.MarkPending(destination)

	if err := copyFile(originalPath, destination); err != nil {
		return err
	}

	text, err := ingest.ExtractText(destination)
	if err != nil {
		os.Remove(destination) //nolint:errcheck
		return err
	}
	if text == "" {
		// Supported extension but nothing readable (empty/binary) — don't leave an orphan.
		os.Remove(destination) //nolint:errcheck
		return fmt.Errorf("no readable text found in %s", filepath.Base(originalPath))
	}

	return socket.rag.Add(text, destination, originalPath)
}

// copies a file over
func copyFile(source string, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()

	output, err := os.Create(destination)
	if err != nil {
		return err
	}
	defer output.Close()

	_, err = io.Copy(output, input)
	return err
}

// If a file already exists we want to keep like adding a (1) or whatever to make sure a file is added
func uniqueDestination(dir string, name string) string {
	destination := filepath.Join(dir, name)
	if _, err := os.Stat(destination); os.IsNotExist(err) {
		return destination
	}
	extension := filepath.Ext(name)
	base := strings.TrimSuffix(name, extension)
	for counter := 1; ; counter++ {
		candidate := filepath.Join(dir, fmt.Sprintf("%s (%d)%s", base, counter, extension))
		if _, err := os.Stat(candidate); os.IsNotExist(err) {
			return candidate
		}
	}
}
