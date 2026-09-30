package cache

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeOllama streams a completion back the way the real server does: one NDJSON frame
// per piece, then a frame with done set.
func fakeOllama(t *testing.T, completion string, calls *int) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if calls != nil {
			*calls++
		}

		var body struct {
			Stream *bool  `json:"stream"`
			Model  string `json:"model"`
		}
		raw, _ := io.ReadAll(request.Body)
		json.Unmarshal(raw, &body) //nolint:errcheck

		if request.URL.Path == embedEndpoint {
			json.NewEncoder(writer).Encode(map[string]any{ //nolint:errcheck
				"embeddings": [][]float32{unitVector(7, 768)},
			})
			return
		}

		streaming := body.Stream == nil || *body.Stream
		if !streaming {
			json.NewEncoder(writer).Encode(map[string]any{ //nolint:errcheck
				"model": body.Model, "response": completion, "done": true, "done_reason": "stop",
			})
			return
		}

		encoder := json.NewEncoder(writer)
		for offset := 0; offset < len(completion); offset += 16 {
			end := min(offset+16, len(completion))
			encoder.Encode(map[string]any{"response": completion[offset:end], "done": false}) //nolint:errcheck
		}
		encoder.Encode(map[string]any{ //nolint:errcheck
			"response": "", "done": true, "done_reason": "stop", "eval_count": 42,
		})
	}))
	t.Cleanup(server.Close)
	return server
}

func newTestProxy(t *testing.T, upstream string) (*Proxy, *Cache, *Metrics) {
	t.Helper()
	cache := newTestCache(t, newFakeEmbedder(), nil)
	metrics := NewMetrics()
	proxy, err := NewProxy(cache, NewRegistry(NewOllama(upstream)), metrics, upstream)
	if err != nil {
		t.Fatal(err)
	}
	return proxy, cache, metrics
}

func generateRequestBody(prompt string, stream bool) []byte {
	body, _ := json.Marshal(map[string]any{
		"model": "llama3.2", "prompt": prompt, "stream": stream,
		"options": map[string]any{"temperature": 0, "num_predict": 400},
	})
	return body
}

// readStream reads a streamed reply exactly the way internal/rag.Query does: a default
// bufio.Scanner over NDJSON, accumulating the response field until done.
func readStream(t *testing.T, body []byte) string {
	t.Helper()
	var assembled strings.Builder
	scanner := bufio.NewScanner(bytes.NewReader(body))
	for scanner.Scan() {
		var frame struct {
			Response string `json:"response"`
			Done     bool   `json:"done"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
			t.Fatalf("unparseable frame: %v", err)
		}
		assembled.WriteString(frame.Response)
		if frame.Done {
			break
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("the scanner gave up: %v", err)
	}
	return assembled.String()
}

func post(t *testing.T, proxy *Proxy, path string, body []byte) *httptest.ResponseRecorder {
	t.Helper()
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(body))
	proxy.Handler().ServeHTTP(recorder, request)
	return recorder
}

func TestStreamingMissIsRelayedThenServedFromCache(t *testing.T) {
	upstreamCalls := 0
	upstream := fakeOllama(t, "Deduplication uses a cosine distance of 0.451.", &upstreamCalls)
	proxy, _, metrics := newTestProxy(t, upstream.URL)

	body := generateRequestBody(answerPrompt("what threshold?", "Dedup uses 0.451."), true)

	first := post(t, proxy, generateEndpoint, body)
	if got := first.Header().Get(HeaderStatus); got != string(StatusMiss) {
		t.Errorf("first request reported %q, want a miss", got)
	}
	if answer := readStream(t, first.Body.Bytes()); answer != "Deduplication uses a cosine distance of 0.451." {
		t.Errorf("relayed %q", answer)
	}

	second := post(t, proxy, generateEndpoint, body)
	if got := second.Header().Get(HeaderStatus); got != string(StatusHit) {
		t.Fatalf("second request reported %q, want a hit", got)
	}
	if answer := readStream(t, second.Body.Bytes()); answer != "Deduplication uses a cosine distance of 0.451." {
		t.Errorf("replayed %q", answer)
	}
	if upstreamCalls != 1 {
		t.Errorf("the provider was called %d times, want 1", upstreamCalls)
	}
	if snapshot := metrics.Snapshot(nil); snapshot.Hits != 1 || snapshot.Misses != 1 {
		t.Errorf("metrics recorded %d hits and %d misses", snapshot.Hits, snapshot.Misses)
	}
}

// The trap this design exists to avoid. internal/rag.Query reads the stream with a
// default bufio.Scanner, which abandons any line over 64KB without returning an error.
// Replaying a long cached answer as one frame would truncate it silently — and long
// answers are exactly the ones worth caching.
func TestLongCachedAnswerSurvivesADefaultScanner(t *testing.T) {
	long := strings.Repeat("The daemon serializes writes through a single owner. ", 4000)
	if len(long) < 200_000 {
		t.Fatalf("the fixture is only %d bytes; it has to be well over 64KB", len(long))
	}

	upstream := fakeOllama(t, long, nil)
	proxy, _, _ := newTestProxy(t, upstream.URL)
	body := generateRequestBody(answerPrompt("summarize the daemon", "The daemon owns the database."), true)

	post(t, proxy, generateEndpoint, body)
	hit := post(t, proxy, generateEndpoint, body)

	if got := hit.Header().Get(HeaderStatus); got != string(StatusHit) {
		t.Fatalf("expected a hit, got %q", got)
	}
	replayed := readStream(t, hit.Body.Bytes())
	if replayed != long {
		t.Errorf("the replayed answer was %d bytes, want %d", len(replayed), len(long))
	}
}

func TestNonStreamingCallsRoundTrip(t *testing.T) {
	upstream := fakeOllama(t, "3,1,2", nil)
	proxy, _, _ := newTestProxy(t, upstream.URL)
	body := generateRequestBody("You rank search results by relevance. \n\nQUESTION: which passage?\n\nPASSAGES:\n[1] one\n", false)

	post(t, proxy, generateEndpoint, body)
	hit := post(t, proxy, generateEndpoint, body)

	if got := hit.Header().Get(HeaderStatus); got != string(StatusHit) {
		t.Fatalf("expected a hit, got %q", got)
	}
	var frame struct {
		Response string `json:"response"`
		Done     bool   `json:"done"`
	}
	if err := json.Unmarshal(hit.Body.Bytes(), &frame); err != nil {
		t.Fatalf("a non-streaming hit should be one JSON object: %v", err)
	}
	if frame.Response != "3,1,2" || !frame.Done {
		t.Errorf("replayed %+v", frame)
	}
}

// A stream that dies halfway has produced a partial answer. The caller sees what arrived,
// which is what it would have seen talking to the provider directly, but a half-written
// answer must never become a cache entry.
func TestInterruptedStreamIsNotCached(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		encoder := json.NewEncoder(writer)
		encoder.Encode(map[string]any{"response": "the beginning", "done": false}) //nolint:errcheck
		// No done frame: the connection just ends.
	}))
	defer upstream.Close()

	proxy, cache, _ := newTestProxy(t, upstream.URL)
	body := generateRequestBody(answerPrompt("a question", "a passage"), true)

	post(t, proxy, generateEndpoint, body)
	if cache.Len() != 0 {
		t.Errorf("a truncated stream was cached: %d entries", cache.Len())
	}

	second := post(t, proxy, generateEndpoint, body)
	if got := second.Header().Get(HeaderStatus); got == string(StatusHit) {
		t.Error("a truncated stream was served as a hit")
	}
}

func TestEmbeddingsRoundTripThroughTheProxy(t *testing.T) {
	calls := 0
	upstream := fakeOllama(t, "", &calls)
	proxy, _, _ := newTestProxy(t, upstream.URL)

	body, _ := json.Marshal(map[string]any{
		"model": "nomic-embed-text", "input": "search_document: the daemon debounces for 500ms",
	})

	first := post(t, proxy, embedEndpoint, body)
	var decoded struct {
		Embeddings [][]float32 `json:"embeddings"`
	}
	if err := json.Unmarshal(first.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Embeddings) != 1 || len(decoded.Embeddings[0]) != 768 {
		t.Fatalf("relayed a malformed embedding response")
	}

	second := post(t, proxy, embedEndpoint, body)
	if got := second.Header().Get(HeaderStatus); got != string(StatusHit) {
		t.Fatalf("an identical embedding request reported %q", got)
	}
	if err := json.Unmarshal(second.Body.Bytes(), &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Embeddings[0]) != 768 {
		t.Error("the cached embedding did not come back in the provider's shape")
	}
	if calls != 1 {
		t.Errorf("the provider was called %d times, want 1", calls)
	}
}

// Anything the cache does not understand still has to work, or putting this in front of
// Ollama would quietly remove capabilities.
func TestUnknownPathsArePassedThrough(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		fmt.Fprintf(writer, `{"path":%q}`, request.URL.Path)
	}))
	defer upstream.Close()

	proxy, _, _ := newTestProxy(t, upstream.URL)
	recorder := httptest.NewRecorder()
	proxy.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, tagsEndpoint, nil))

	if !strings.Contains(recorder.Body.String(), tagsEndpoint) {
		t.Errorf("an unknown path did not reach the provider: %q", recorder.Body.String())
	}
}

// fakeOllamaChat serves /api/chat, where the text lives in message.content rather than
// in response. Engrex never calls this endpoint, but the Ollama desktop app and most
// third-party clients do, and a proxy that quietly stopped caching for them would not be
// much of a drop-in.
func fakeOllamaChat(t *testing.T, completion string, calls *int) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if calls != nil {
			*calls++
		}
		var body struct {
			Stream *bool `json:"stream"`
		}
		raw, _ := io.ReadAll(request.Body)
		json.Unmarshal(raw, &body) //nolint:errcheck

		message := func(content string, done bool) map[string]any {
			return map[string]any{
				"model":   "llama3.2",
				"message": map[string]any{"role": "assistant", "content": content},
				"done":    done,
			}
		}
		if body.Stream != nil && !*body.Stream {
			json.NewEncoder(writer).Encode(message(completion, true)) //nolint:errcheck
			return
		}
		encoder := json.NewEncoder(writer)
		for offset := 0; offset < len(completion); offset += 8 {
			encoder.Encode(message(completion[offset:min(offset+8, len(completion))], false)) //nolint:errcheck
		}
		encoder.Encode(message("", true)) //nolint:errcheck
	}))
	t.Cleanup(server.Close)
	return server
}

func chatBodyFor(system, question string, stream bool) []byte {
	messages := []map[string]string{}
	if system != "" {
		messages = append(messages, map[string]string{"role": "system", "content": system})
	}
	messages = append(messages, map[string]string{"role": "user", "content": question})
	body, _ := json.Marshal(map[string]any{
		"model": "llama3.2", "messages": messages, "stream": stream,
		"options": map[string]any{"temperature": 0},
	})
	return body
}

func TestChatRoundTripsAndCaches(t *testing.T) {
	calls := 0
	upstream := fakeOllamaChat(t, "The threshold is 0.451.", &calls)
	proxy, _, _ := newTestProxy(t, upstream.URL)
	body := chatBodyFor("", "what is the dedup threshold?", true)

	first := post(t, proxy, chatEndpoint, body)
	if got := first.Header().Get(HeaderStatus); got != string(StatusMiss) {
		t.Fatalf("first chat call reported %q", got)
	}

	second := post(t, proxy, chatEndpoint, body)
	if got := second.Header().Get(HeaderStatus); got != string(StatusHit) {
		t.Fatalf("second chat call reported %q, want a hit", got)
	}
	if calls != 1 {
		t.Errorf("the provider was called %d times, want 1", calls)
	}

	// A replayed chat hit has to come back in the chat shape, not the generate shape:
	// the client is reading message.content and would see nothing in response.
	var assembled strings.Builder
	scanner := bufio.NewScanner(bytes.NewReader(second.Body.Bytes()))
	for scanner.Scan() {
		var frame struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &frame); err != nil {
			t.Fatalf("unparseable chat frame: %v", err)
		}
		assembled.WriteString(frame.Message.Content)
	}
	if assembled.String() != "The threshold is 0.451." {
		t.Errorf("replayed chat content = %q", assembled.String())
	}
}

// The guide is explicit that two identical user prompts under different system prompts
// must not share an entry. Here that falls out of the split rather than being a special
// case: the system prompt lands in the context half, which is matched exactly.
func TestSystemPromptSplitsTheChatCache(t *testing.T) {
	calls := 0
	upstream := fakeOllamaChat(t, "an answer", &calls)
	proxy, _, _ := newTestProxy(t, upstream.URL)

	question := "what should I do?"
	terse := chatBodyFor(strings.Repeat("You are a terse assistant. ", 20), question, false)
	verbose := chatBodyFor(strings.Repeat("You are a verbose assistant. ", 20), question, false)

	post(t, proxy, chatEndpoint, terse)
	if got := post(t, proxy, chatEndpoint, terse).Header().Get(HeaderStatus); got != string(StatusHit) {
		t.Fatalf("the same system prompt should hit, got %q", got)
	}

	if got := post(t, proxy, chatEndpoint, verbose).Header().Get(HeaderStatus); got == string(StatusHit) {
		t.Fatal("a different system prompt was served the other one's cached answer")
	}
}
