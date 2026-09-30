package cache

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func postJSON(t *testing.T, proxy *Proxy, path string, fields map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	return post(t, proxy, path, body)
}

// A request asking for JSON output and one asking for prose are different requests even
// with the same prompt. Before the key covered them, the second got the first's answer
func TestRequestFieldsOutsideThePromptSplitTheKey(t *testing.T) {
	calls := 0
	upstream := fakeOllama(t, "an answer", &calls)
	proxy, _, _ := newTestProxy(t, upstream.URL)

	base := map[string]any{"model": "llama3.2", "prompt": "list three colors", "stream": false}
	withFormat := map[string]any{"model": "llama3.2", "prompt": "list three colors", "stream": false, "format": "json"}
	withSystem := map[string]any{"model": "llama3.2", "prompt": "list three colors", "stream": false, "system": "answer in french"}

	for _, fields := range []map[string]any{base, withFormat, withSystem} {
		if got := postJSON(t, proxy, generateEndpoint, fields).Header().Get(HeaderStatus); got != string(StatusMiss) {
			t.Errorf("a request differing only outside the prompt reported %q, want a miss", got)
		}
	}
	if got := postJSON(t, proxy, generateEndpoint, withFormat).Header().Get(HeaderStatus); got != string(StatusHit) {
		t.Errorf("repeating the format request reported %q, want a hit", got)
	}
	if calls != 3 {
		t.Errorf("the provider was called %d times, want 3", calls)
	}
}

// Empty values mean the same to Ollama as absent ones, so a client that always sends
// "images": [] should still be cached
func TestEmptyFieldsDoNotSplitTheKeyOrBlockCaching(t *testing.T) {
	calls := 0
	upstream := fakeOllama(t, "an answer", &calls)
	proxy, _, _ := newTestProxy(t, upstream.URL)

	postJSON(t, proxy, generateEndpoint, map[string]any{"model": "llama3.2", "prompt": "hello there", "stream": false})
	repeat := postJSON(t, proxy, generateEndpoint, map[string]any{
		"model": "llama3.2", "prompt": "hello there", "stream": false, "images": []string{}, "system": "", "keep_alive": "5m",
	})
	if got := repeat.Header().Get(HeaderStatus); got != string(StatusHit) {
		t.Errorf("a repeat with only empty extras reported %q, want a hit", got)
	}
}

// Images and tools are not in the text the cache keys on, so requests carrying them are
// passed through rather than risk serving one picture's caption for another
func TestUnreplayableRequestsBypassTheCache(t *testing.T) {
	calls := 0
	upstream := fakeOllama(t, "an answer", &calls)
	proxy, cache, _ := newTestProxy(t, upstream.URL)

	requests := []struct {
		path   string
		fields map[string]any
	}{
		{generateEndpoint, map[string]any{"model": "llava", "prompt": "what is this", "stream": false, "images": []string{"aGVsbG8="}}},
		{generateEndpoint, map[string]any{"model": "llama3.2", "prompt": "continue", "stream": false, "context": []int{1, 2, 3}}},
		{chatEndpoint, map[string]any{"model": "llama3.2", "stream": false, "tools": []any{map[string]any{"type": "function"}},
			"messages": []any{map[string]any{"role": "user", "content": "what is the weather"}}}},
		{chatEndpoint, map[string]any{"model": "llava", "stream": false,
			"messages": []any{map[string]any{"role": "user", "content": "what is this", "images": []string{"aGVsbG8="}}}}},
	}
	for _, request := range requests {
		for range 2 {
			if got := postJSON(t, proxy, request.path, request.fields).Header().Get(HeaderStatus); got != string(StatusBypass) {
				t.Errorf("%s with %v reported %q, want bypass", request.path, request.fields, got)
			}
		}
	}
	if cache.Len() != 0 {
		t.Errorf("stored %d unreplayable responses, want none", cache.Len())
	}
}

// thinkingOllama answers the way a reasoning model does on Ollama 0.30: reasoning in a
// thinking field, the answer in response, on separate frames
func thinkingOllama(t *testing.T, calls *int) *httptest.Server {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		*calls++
		var body struct {
			Stream *bool `json:"stream"`
		}
		raw, _ := io.ReadAll(request.Body)
		json.Unmarshal(raw, &body) //nolint:errcheck

		if body.Stream != nil && !*body.Stream {
			json.NewEncoder(writer).Encode(map[string]any{ //nolint:errcheck
				"response": "hi", "thinking": "the user wants a greeting", "done": true, "done_reason": "stop",
			})
			return
		}
		encoder := json.NewEncoder(writer)
		encoder.Encode(map[string]any{"response": "", "thinking": "the user wants ", "done": false}) //nolint:errcheck
		encoder.Encode(map[string]any{"response": "", "thinking": "a greeting", "done": false})      //nolint:errcheck
		encoder.Encode(map[string]any{"response": "hi", "done": false})                              //nolint:errcheck
		encoder.Encode(map[string]any{"response": "", "done": true, "done_reason": "stop"})          //nolint:errcheck
	}))
	t.Cleanup(server.Close)
	return server
}

func TestReasoningSurvivesTheCache(t *testing.T) {
	for _, stream := range []bool{true, false} {
		calls := 0
		upstream := thinkingOllama(t, &calls)
		proxy, _, _ := newTestProxy(t, upstream.URL)
		body := generateRequestBody("say hi", stream)

		post(t, proxy, generateEndpoint, body)
		hit := post(t, proxy, generateEndpoint, body)
		if got := hit.Header().Get(HeaderStatus); got != string(StatusHit) {
			t.Fatalf("stream=%v: the repeat reported %q, want a hit", stream, got)
		}

		var response, thinking strings.Builder
		for _, line := range strings.Split(strings.TrimSpace(hit.Body.String()), "\n") {
			var frame struct {
				Response string `json:"response"`
				Thinking string `json:"thinking"`
			}
			if err := json.Unmarshal([]byte(line), &frame); err != nil {
				t.Fatalf("stream=%v: unparseable frame %q", stream, line)
			}
			response.WriteString(frame.Response)
			thinking.WriteString(frame.Thinking)
		}
		if response.String() != "hi" || thinking.String() != "the user wants a greeting" {
			t.Errorf("stream=%v: replayed response %q thinking %q", stream, response.String(), thinking.String())
		}
		if calls != 1 {
			t.Errorf("stream=%v: the provider was called %d times, want 1", stream, calls)
		}
	}
}

// A missing model is Ollama's 404 with its own message. The proxy used to turn every
// upstream failure into a bare 502, which hid the reason from the caller
func TestUpstreamErrorsAreRelayedAsSent(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusNotFound)
		writer.Write([]byte(`{"error":"model \"nope\" not found, try pulling it first"}`)) //nolint:errcheck
	}))
	t.Cleanup(upstream.Close)
	proxy, cache, _ := newTestProxy(t, upstream.URL)

	response := postJSON(t, proxy, generateEndpoint, map[string]any{"model": "nope", "prompt": "hello", "stream": false})
	if response.Code != http.StatusNotFound {
		t.Errorf("status %d, want the upstream's 404", response.Code)
	}
	if !strings.Contains(response.Body.String(), "not found, try pulling it first") {
		t.Errorf("body %q lost the upstream's message", response.Body.String())
	}
	if cache.Len() != 0 {
		t.Error("an error response was stored")
	}
}
