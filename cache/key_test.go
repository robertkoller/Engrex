package cache

import "testing"

func generateRequest() Request {
	return Request{
		Endpoint: "/api/generate",
		Model:    "llama3.2",
		Options:  map[string]any{"temperature": 0, "num_predict": 400, "num_ctx": 8192},
	}
}

// The whole point of splitting: two questions asked over the same passages land in one
// bucket, where similarity can tell them apart, rather than being compared as two
// near-identical walls of text.
func TestIdenticalContextSharesANamespace(t *testing.T) {
	request := generateRequest()
	if Namespace(request, "same passages") != Namespace(request, "same passages") {
		t.Error("the same context should produce the same namespace")
	}
	if Namespace(request, "some passages") == Namespace(request, "other passages") {
		t.Error("different context must never share a namespace")
	}
}

func TestModelAndSamplingSplitTheNamespace(t *testing.T) {
	base := generateRequest()

	deeper := generateRequest()
	deeper.Model = "qwen3:4b"
	if Namespace(base, "ctx") == Namespace(deeper, "ctx") {
		t.Error("two models must not share cache entries")
	}

	hotter := generateRequest()
	hotter.Options = map[string]any{"temperature": 0.8, "num_predict": 400, "num_ctx": 8192}
	if Namespace(base, "ctx") == Namespace(hotter, "ctx") {
		t.Error("different sampling parameters must not share cache entries")
	}

	embedding := generateRequest()
	embedding.Endpoint = "/api/embed"
	if Namespace(base, "ctx") == Namespace(embedding, "ctx") {
		t.Error("different endpoints must not share cache entries")
	}
}

// num_ctx is derived from the prompt's own length, so a longer wording of the same
// question can shift it. Keying on it would guarantee a miss in exactly the case the
// cache exists for.
func TestContextWindowDoesNotSplitTheNamespace(t *testing.T) {
	small := generateRequest()
	small.Options = map[string]any{"temperature": 0, "num_predict": 400, "num_ctx": 4096}

	large := generateRequest()
	large.Options = map[string]any{"temperature": 0, "num_predict": 400, "num_ctx": 32768}

	if Namespace(small, "ctx") != Namespace(large, "ctx") {
		t.Error("num_ctx should not affect the namespace")
	}
}

// Go randomizes map iteration, so an unsorted rendering of the options would give the
// same request a different key on every single call and the cache would never hit.
func TestOptionOrderIsIrrelevant(t *testing.T) {
	first := Request{Endpoint: "/api/generate", Model: "llama3.2"}
	second := first
	for round := 0; round < 50; round++ {
		first.Options = map[string]any{"a": 1, "b": 2, "c": 3, "d": 4, "e": 5}
		second.Options = map[string]any{"e": 5, "d": 4, "c": 3, "b": 2, "a": 1}
		if Namespace(first, "ctx") != Namespace(second, "ctx") {
			t.Fatal("option ordering changed the namespace")
		}
	}
}

func TestExactKeyTracksTheWholeText(t *testing.T) {
	request := generateRequest()
	request.Text = "search_document: the daemon debounces for 500ms"

	same := request
	if ExactKey(request) != ExactKey(same) {
		t.Error("identical text should produce the same exact key")
	}

	different := request
	different.Text = "search_document: the daemon debounces for 400ms"
	if ExactKey(request) == ExactKey(different) {
		t.Error("different text must produce a different exact key")
	}
}

// fmt's %v printed both of these as [a b], so they shared a key
func TestOptionsAreRenderedUnambiguously(t *testing.T) {
	split := generateRequest()
	split.Options = map[string]any{"stop": []any{"a", "b"}}
	joined := generateRequest()
	joined.Options = map[string]any{"stop": []any{"a b"}}

	if ExactKey(split) == ExactKey(joined) {
		t.Error("two different stop lists produced the same key")
	}
}

func TestVariantSplitsTheKey(t *testing.T) {
	plain := generateRequest()
	formatted := generateRequest()
	formatted.Variant = `{"format":"json"}`

	if ExactKey(plain) == ExactKey(formatted) || Namespace(plain, "context") == Namespace(formatted, "context") {
		t.Error("a request asking for JSON shares a key with one that does not")
	}
}
