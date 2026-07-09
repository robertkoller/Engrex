package socket

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"path/filepath"

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
		if err := socket.rag.Add(command.Text, command.Source); err != nil {
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
	}
}
