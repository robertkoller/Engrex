package cache

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/robertkoller/engrex/internal/embedder"
)

// ServerOptions configure the cache service.
type ServerOptions struct {
	// ProxyAddress is where clients point instead of at Ollama.
	ProxyAddress string

	// DashboardAddress serves /metrics and the dashboard, on its own port so scrape and
	// browser traffic never share a handler with the hot path.
	DashboardAddress string

	// Upstream is the real provider.
	Upstream string

	// DataDirectory holds the journal and the lookup log.
	DataDirectory string

	Policy          Policy
	ContextTolerant bool
	LogLookups      bool

	// SyntheticLatency, when set, replaces the generation provider with one that returns
	// canned text after this delay, so the load test can run at size without waiting for
	// a real model. It replaces generation only — the cache still embeds through real
	// Ollama, because embedding is fast and because a fake embedder would make every
	// paraphrase orthogonal and quietly turn a semantic cache into a hash map.
	SyntheticLatency time.Duration
}

// DefaultServerOptions places the proxy next to Ollama's own port, which is the clearest
// signal of what it is: something you point an Ollama client at.
func DefaultServerOptions() ServerOptions {
	return ServerOptions{
		ProxyAddress:     "127.0.0.1:11435",
		DashboardAddress: "127.0.0.1:11436",
		Upstream:         "http://localhost:11434",
		DataDirectory:    DefaultDataDirectory(),
		Policy:           DefaultPolicy(),
		LogLookups:       true,
	}
}

// DefaultDataDirectory is where the cache keeps its files — under ~/.engrex like
// everything else this project stores, and never inside the repository.
func DefaultDataDirectory() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return filepath.Join(".engrex", "cache")
	}
	return filepath.Join(home, ".engrex", "cache")
}

// Server runs the proxy and the dashboard together.
type Server struct {
	cache   *Cache
	metrics *Metrics
	journal *Journal
	lookups *LookupLog

	proxy     *http.Server
	dashboard *http.Server
}

// NewServer builds the whole service: journal, index, cache, proxy and dashboard.
func NewServer(options ServerOptions) (*Server, error) {
	if options.ProxyAddress == "" {
		options = DefaultServerOptions()
	}

	journal, err := OpenJournal(filepath.Join(options.DataDirectory, "entries.jsonl"))
	if err != nil {
		return nil, fmt.Errorf("opening the journal: %w", err)
	}

	metrics := NewMetrics()
	recorders := Recorders{metrics}

	var lookups *LookupLog
	if options.LogLookups {
		lookups, err = OpenLookupLog(filepath.Join(options.DataDirectory, "lookups.jsonl"))
		if err != nil {
			return nil, fmt.Errorf("opening the lookup log: %w", err)
		}
		recorders = append(recorders, lookups)
	}

	// The cache embeds through the upstream provider directly rather than through itself.
	// Routing its own embedding calls back through the proxy would be a loop, and those
	// vectors are an internal detail — they are not what any client asked for.
	cache, err := New(Options{
		Embedder:        embedder.New(options.Upstream),
		Journal:         journal,
		Policy:          options.Policy,
		Recorder:        recorders,
		ContextTolerant: options.ContextTolerant,
	})
	if err != nil {
		return nil, err
	}

	var generator Provider = NewOllama(options.Upstream)
	if options.SyntheticLatency > 0 {
		generator = NewSyntheticOllama(options.SyntheticLatency)
	}

	proxy, err := NewProxy(cache, NewRegistry(generator, OpenAIProvider(), AnthropicProvider()), metrics, options.Upstream)
	if err != nil {
		return nil, err
	}

	return &Server{
		cache:   cache,
		metrics: metrics,
		journal: journal,
		lookups: lookups,
		proxy: &http.Server{
			Addr:    options.ProxyAddress,
			Handler: proxy.Handler(),

			// No write timeout. A generation that takes a minute is normal here, and a
			// proxy that gave up sooner than the model would turn slowness into failure.
			ReadHeaderTimeout: 10 * time.Second,
		},
		dashboard: &http.Server{
			Addr:              options.DashboardAddress,
			Handler:           NewDashboard(cache, metrics).Handler(),
			ReadHeaderTimeout: 10 * time.Second,
		},
	}, nil
}

// Cache exposes the cache, for the CLI commands that act on it directly.
func (server *Server) Cache() *Cache { return server.cache }

// Metrics exposes the counters, for reporting.
func (server *Server) Metrics() *Metrics { return server.metrics }

// Start runs both listeners and blocks until one of them fails or Stop is called.
//
// Errors come back on a channel rather than being dropped by a bare `go x.Start()`. A
// failed bind is exactly the failure worth hearing about — the alternative is a proxy
// that looks like it started, answers nothing, and leaves every client timing out
// against a port with nobody on it.
func (server *Server) Start() error {
	failures := make(chan error, 2)

	go func() {
		if err := server.proxy.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			failures <- fmt.Errorf("proxy on %s: %w", server.proxy.Addr, err)
			return
		}
		failures <- nil
	}()
	go func() {
		if err := server.dashboard.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			failures <- fmt.Errorf("dashboard on %s: %w", server.dashboard.Addr, err)
			return
		}
		failures <- nil
	}()

	return <-failures
}

// Stop shuts both listeners down and closes the files.
func (server *Server) Stop() error {
	shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	server.proxy.Shutdown(shutdown)     //nolint:errcheck
	server.dashboard.Shutdown(shutdown) //nolint:errcheck

	if server.lookups != nil {
		server.lookups.Close() //nolint:errcheck
	}
	return server.journal.Close()
}

// Recorders fans one event out to several recorders.
type Recorders []Recorder

func (recorders Recorders) RecordLookup(result Result) {
	for _, recorder := range recorders {
		recorder.RecordLookup(result)
	}
}

func (recorders Recorders) RecordStore(entry *Entry) {
	for _, recorder := range recorders {
		recorder.RecordStore(entry)
	}
}

func (recorders Recorders) RecordEviction(count int) {
	for _, recorder := range recorders {
		recorder.RecordEviction(count)
	}
}
