package cache

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countingOllama answers generate and embed calls and counts generations atomically, so
// it is safe to hit from many goroutines at once. The delay keeps several identical
// misses in flight together, which is what the coalescing test needs.
func countingOllama(t *testing.T, delay time.Duration, generations *atomic.Int64) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == embedEndpoint {
			json.NewEncoder(writer).Encode(map[string]any{ //nolint:errcheck
				"embeddings": [][]float32{unitVector(7, 768)},
			})
			return
		}
		generations.Add(1)
		time.Sleep(delay)
		json.NewEncoder(writer).Encode(map[string]any{ //nolint:errcheck
			"response": "an answer", "done": true, "done_reason": "stop",
		})
	}))
	t.Cleanup(server.Close)
	return server
}

// Run under -race. Hits, misses, stores, evictions and journal writes all touch the same
// entries from different goroutines, and before this test existed the hit path mutated an
// entry under one lock while eviction and the journal read it under another.
func TestConcurrentTrafficIsRaceFree(t *testing.T) {
	var generations atomic.Int64
	upstream := countingOllama(t, 0, &generations)

	journal, err := OpenJournal(filepath.Join(t.TempDir(), "entries.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { journal.Close() }) //nolint:errcheck

	policy := DefaultPolicy()
	policy.MaxEntries = 20
	cache := newTestCache(t, &lockedEmbedder{inner: newFakeEmbedder()}, func(options *Options) {
		options.Journal = journal
		options.Policy = policy
		options.ContextTolerant = true
	})
	metrics := NewMetrics()
	proxy, err := NewProxy(cache, NewRegistry(NewOllama(upstream.URL)), metrics, upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	handler := proxy.Handler()

	var group sync.WaitGroup
	for worker := range 8 {
		group.Add(1)
		go func() {
			defer group.Done()
			for request := range 60 {
				// A small key space, so most requests repeat one another and the hit path
				// runs as often as the miss path.
				question := fmt.Sprintf("question %d", (worker*request)%30)
				body := generateRequestBody(answerPrompt(question, "passage one", "passage two"), false)
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, generateEndpoint, bytes.NewReader(body)))
				if recorder.Code != http.StatusOK {
					t.Errorf("status %d: %s", recorder.Code, recorder.Body.String())
					return
				}
			}
		}()
	}
	group.Wait()

	if cache.Len() > policy.MaxEntries {
		t.Errorf("cache holds %d entries, cap is %d", cache.Len(), policy.MaxEntries)
	}
	metrics.Snapshot(cache)
}

// Identical misses arriving together should cost one generation, not one each. Without
// coalescing, a burst of the same question — the eval harness run twice in parallel, or a
// client retrying — sends every copy to the model.
func TestIdenticalConcurrentMissesAreCoalesced(t *testing.T) {
	var generations atomic.Int64
	upstream := countingOllama(t, 150*time.Millisecond, &generations)

	cache := newTestCache(t, &lockedEmbedder{inner: newFakeEmbedder()}, nil)
	proxy, err := NewProxy(cache, NewRegistry(NewOllama(upstream.URL)), NewMetrics(), upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	handler := proxy.Handler()
	body := generateRequestBody(answerPrompt("what is hnsw?", "passage one"), false)

	const callers = 6
	statuses := make([]string, callers)
	var group sync.WaitGroup
	for caller := range callers {
		group.Add(1)
		go func() {
			defer group.Done()
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, generateEndpoint, bytes.NewReader(body)))
			if recorder.Code != http.StatusOK {
				t.Errorf("status %d: %s", recorder.Code, recorder.Body.String())
			}
			statuses[caller] = recorder.Header().Get(HeaderStatus)
		}()
	}
	group.Wait()

	if got := generations.Load(); got != 1 {
		t.Errorf("upstream generated %d times for %d identical concurrent requests, want 1", got, callers)
	}
	hits := 0
	for _, status := range statuses {
		if status == string(StatusHit) {
			hits++
		}
	}
	if hits != callers-1 {
		t.Errorf("%d of %d callers were served from cache, want %d (statuses %v)", hits, callers, callers-1, statuses)
	}
}

// lockedEmbedder makes the map-backed fake safe to call from several goroutines.
type lockedEmbedder struct {
	mutex sync.Mutex
	inner *fakeEmbedder
}

func (embedder *lockedEmbedder) EmbedQuery(text string) ([]float32, error) {
	embedder.mutex.Lock()
	defer embedder.mutex.Unlock()
	return embedder.inner.EmbedQuery(text)
}
