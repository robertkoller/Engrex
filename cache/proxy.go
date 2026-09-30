package cache

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"sync"
	"time"
)

// Proxy is the drop-in front end: it speaks the provider's own API, so pointing a client
// at it is a change of base URL and nothing else.
type Proxy struct {
	cache    *Cache
	registry *Registry
	metrics  *Metrics

	// passthrough carries everything the cache does not understand — model management,
	// version checks, anything added upstream since — so putting this in front of Ollama
	// never removes a capability.
	passthrough *httputil.ReverseProxy

	// inflight holds one channel per exact request currently being generated. A second
	// identical request waits on it and is then served from cache, so a burst of the same
	// prompt costs one generation instead of one each
	inflightMutex sync.Mutex
	inflight      map[string]chan struct{}
}

// Response headers describing what the cache did. Named so a caller can see it in curl
// output without needing the dashboard.
const (
	HeaderStatus     = "X-Engrex-Cache"
	HeaderTier       = "X-Engrex-Cache-Tier"
	HeaderSimilarity = "X-Engrex-Cache-Similarity"
	HeaderSaved      = "X-Engrex-Cache-Saved-Ms"
)

// NewProxy builds a proxy in front of an upstream base URL.
func NewProxy(cache *Cache, registry *Registry, metrics *Metrics, upstream string) (*Proxy, error) {
	target, err := url.Parse(upstream)
	if err != nil {
		return nil, err
	}
	return &Proxy{
		cache:       cache,
		registry:    registry,
		metrics:     metrics,
		passthrough: httputil.NewSingleHostReverseProxy(target),
		inflight:    make(map[string]chan struct{}),
	}, nil
}

// Handler returns the mux the proxy serves.
func (proxy *Proxy) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/cache/stats", proxy.handleStats)
	mux.HandleFunc("/cache/invalidate", proxy.handleInvalidate)
	mux.HandleFunc("/", proxy.handle)
	return mux
}

func (proxy *Proxy) handle(writer http.ResponseWriter, request *http.Request) {
	provider, known := proxy.registry.For(request.URL.Path)
	if !known || request.Method != http.MethodPost {
		proxy.passthrough.ServeHTTP(writer, request)
		return
	}

	body, err := io.ReadAll(request.Body)
	if err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}

	parsed, cacheable, err := provider.ParseRequest(request.URL.Path, body)
	if err != nil || !cacheable {
		proxy.replay(writer, request, body)
		return
	}

	// Wait out an identical request that is already generating, then look up. By then
	// its answer is stored, so this one becomes an exact hit instead of a second
	// generation. If it failed, this one simply misses and generates for itself
	exactKey := ExactKey(parsed)
	leader, done := proxy.claim(exactKey)
	if !leader {
		select {
		case <-done:
		case <-request.Context().Done():
			return
		}
	} else {
		defer proxy.release(exactKey, done)
	}

	result, err := proxy.cache.Lookup(parsed)

	// Only a cacheable miss produces something worth waiting for. On a hit, a bypass or a
	// failed lookup, anyone queued behind this request can go straight away
	if leader && (err != nil || result.Status != StatusMiss) {
		proxy.release(exactKey, done)
	}
	if err != nil {
		// A cache that cannot look something up must not take the request down with it.
		// Falling through to the provider costs latency; failing costs the answer.
		log.Printf("cache lookup failed, forwarding instead: %v", err)
		proxy.forward(writer, provider, request.URL.Path, body, Result{Status: StatusBypass}, parsed)
		return
	}

	if result.Status == StatusHit {
		writer.Header().Set(HeaderStatus, string(StatusHit))
		writer.Header().Set(HeaderTier, string(result.Tier))
		writer.Header().Set(HeaderSimilarity, strconv.FormatFloat(result.Similarity, 'f', 4, 64))
		writer.Header().Set(HeaderSaved, strconv.FormatInt(result.Entry.GenerationTime.Milliseconds(), 10))

		started := time.Now()
		if err := provider.WriteHit(writer, parsed, result.Entry.Payload); err != nil {
			log.Printf("failed to write a cache hit: %v", err)
			return
		}
		proxy.metrics.ObserveHit(result, time.Since(started))
		return
	}

	proxy.forward(writer, provider, request.URL.Path, body, result, parsed)
}

// claim registers this request as the one generating for its exact key. It returns
// false, with the channel to wait on, when another request already holds the key
func (proxy *Proxy) claim(exactKey string) (bool, chan struct{}) {
	proxy.inflightMutex.Lock()
	defer proxy.inflightMutex.Unlock()
	if waiting, busy := proxy.inflight[exactKey]; busy {
		return false, waiting
	}
	done := make(chan struct{})
	proxy.inflight[exactKey] = done
	return true, done
}

// release wakes everything waiting on the key. Safe to call twice, since a hit releases
// early and the deferred call then finds the key already gone
func (proxy *Proxy) release(exactKey string, done chan struct{}) {
	proxy.inflightMutex.Lock()
	defer proxy.inflightMutex.Unlock()
	if proxy.inflight[exactKey] == done {
		delete(proxy.inflight, exactKey)
		close(done)
	}
}

// forward relays a miss to the provider and stores what comes back.
func (proxy *Proxy) forward(writer http.ResponseWriter, provider Provider, path string, body []byte, result Result, parsed Request) {
	writer.Header().Set(HeaderStatus, string(result.Status))
	writer.Header().Set("Content-Type", "application/x-ndjson")
	if !parsed.Stream || parsed.Endpoint == embedEndpoint {
		writer.Header().Set("Content-Type", "application/json")
	}

	started := time.Now()
	sink := newFlushWriter(writer)
	payload, err := provider.Forward(path, body, sink)
	elapsed := time.Since(started)
	if err != nil {
		var upstreamError *UpstreamError
		switch {
		case sink.written:
		case errors.As(err, &upstreamError):
			// Relayed as Ollama sent it, so a missing model is still a 404 with Ollama's
			// own message rather than a 502 that hides the reason
			if upstreamError.ContentType != "" {
				writer.Header().Set("Content-Type", upstreamError.ContentType)
			}
			writer.WriteHeader(upstreamError.StatusCode)
			writer.Write(upstreamError.Body) //nolint:errcheck
		default:
			http.Error(writer, "upstream call failed", http.StatusBadGateway)
		}
		// The response has already started going out, so the status line is spent. The
		// caller sees a truncated stream, which is what it would have seen talking to
		// the provider directly.
		log.Printf("upstream call failed: %v", err)
		proxy.metrics.ObserveError()
		return
	}

	proxy.metrics.ObserveMiss(result, elapsed)
	if result.Status == StatusBypass || payload.Uncacheable {
		return
	}
	if _, err := proxy.cache.Store(result, payload, elapsed); err != nil {
		log.Printf("failed to store a response: %v", err)
	}
}

// replay passes a request through to the provider without caching it, re-attaching the
// body that was already read.
func (proxy *Proxy) replay(writer http.ResponseWriter, request *http.Request, body []byte) {
	writer.Header().Set(HeaderStatus, string(StatusBypass))
	request.Body = io.NopCloser(bytes.NewReader(body))
	request.ContentLength = int64(len(body))
	proxy.passthrough.ServeHTTP(writer, request)
}

func (proxy *Proxy) handleStats(writer http.ResponseWriter, _ *http.Request) {
	writer.Header().Set("Content-Type", "application/json")
	json.NewEncoder(writer).Encode(proxy.metrics.Snapshot(proxy.cache)) //nolint:errcheck
}

// handleInvalidate drops entries by model, class, or tag. The guide asks for invalidation
// by prefix; here the useful axes are the model (upgrade it and every stored answer is
// from the old one) and the class (edit a prompt builder and only that class is stale).
func (proxy *Proxy) handleInvalidate(writer http.ResponseWriter, request *http.Request) {
	if request.Method != http.MethodPost {
		http.Error(writer, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	var criteria InvalidateCriteria
	if err := json.NewDecoder(request.Body).Decode(&criteria); err != nil {
		http.Error(writer, err.Error(), http.StatusBadRequest)
		return
	}

	removed := proxy.cache.Invalidate(criteria)
	writer.Header().Set("Content-Type", "application/json")
	json.NewEncoder(writer).Encode(map[string]int{"invalidated": removed}) //nolint:errcheck
}
