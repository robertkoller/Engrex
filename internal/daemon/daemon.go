package daemon

import (
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/robertkoller/engrex/internal/db"
	"github.com/robertkoller/engrex/internal/rag"
	"github.com/robertkoller/engrex/internal/socket"
	"github.com/robertkoller/engrex/internal/store"
	"github.com/robertkoller/engrex/internal/watcher"
)

// Daemon owns the database, RAG pipeline, file watcher, and socket listener.
// It is the single long-running process that everything else talks to.
type Daemon struct {
	database *db.DB
	store    *store.Store
	rag      *rag.RAG
	watcher  *watcher.Watcher
	socket   *socket.Socket
}

// Start opens all resources, launches the watcher and socket listener in
// goroutines, then blocks until a SIGTERM or SIGINT is received.
func Start() (*Daemon, error) {
	database, err := db.Open()
	if err != nil {
		log.Fatal(err)
	}

	store := store.New(database)

	rag, err := rag.New(store)
	if err != nil {
		log.Fatal(err)
	}

	watcher := watcher.New(rag)
	socket := socket.New(rag, store)

	return &Daemon{
		database: database,
		store:    store,
		rag:      rag,
		watcher:  watcher,
		socket:   socket,
	}, nil
}

// Runs the daemon until its killed
func (daemon *Daemon) Run() error {
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGTERM, syscall.SIGINT)

	go daemon.watcher.Start()
	go daemon.socket.Start()
	<-signals

	daemon.watcher.Stop()
	daemon.socket.Stop()
	daemon.database.Close()
	return nil
}
