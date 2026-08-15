package rerank

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func makePassages(count int) []Passage {
	passages := make([]Passage, count)
	for index := range passages {
		passages[index] = Passage{ID: int64(index), Text: "passage text"}
	}
	return passages
}

// fakeOllama serves a canned completion, so reranking logic can be tested without a
// model.
func fakeOllama(t *testing.T, completion string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		json.NewEncoder(writer).Encode(map[string]string{"response": completion}) //nolint:errcheck
	}))
}

func TestParseOrderExtractsIndices(t *testing.T) {
	// Zero-based indices out of one-based prompt numbering.
	got := parseOrder("3,1,7", 10)
	want := []int{2, 0, 6}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

// Models wrap the answer in prose often enough that requiring a clean line would throw
// away usable rankings.
func TestParseOrderToleratesSurroundingProse(t *testing.T) {
	got := parseOrder("Here is the ranking: 2, 5, 1. Hope that helps!", 10)
	want := []int{1, 4, 0}
	for index := range want {
		if got[index] != want[index] {
			t.Errorf("got %v, want %v", got, want)
		}
	}
}

// A hallucinated index says nothing about what should rank where, so it is dropped
// rather than clamped into a real position.
func TestParseOrderDropsOutOfRangeAndDuplicates(t *testing.T) {
	got := parseOrder("3,3,99,0,1", 5)
	want := []int{2, 0}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}

func TestRerankAppliesModelOrder(t *testing.T) {
	server := fakeOllama(t, "3,1,2")
	defer server.Close()

	reranker := NewLLM(server.URL, "test-model")
	passages := []Passage{{ID: 10}, {ID: 20}, {ID: 30}}

	ranked, err := reranker.Rerank("question", passages, 3)
	if err != nil {
		t.Fatal(err)
	}

	want := []int64{30, 10, 20}
	for index, id := range want {
		if ranked[index].ID != id {
			t.Errorf("position %d = id %d, want %d", index, ranked[index].ID, id)
		}
	}
}

// Passages the model omits must still survive, in their original order, so nothing
// silently disappears when topN exceeds what was listed.
func TestRerankKeepsOmittedPassages(t *testing.T) {
	server := fakeOllama(t, "2")
	defer server.Close()

	reranker := NewLLM(server.URL, "test-model")
	passages := []Passage{{ID: 10}, {ID: 20}, {ID: 30}}

	ranked, err := reranker.Rerank("question", passages, 3)
	if err != nil {
		t.Fatal(err)
	}
	if len(ranked) != 3 {
		t.Fatalf("got %d passages, want all 3 preserved", len(ranked))
	}
	if ranked[0].ID != 20 {
		t.Errorf("model's pick is not first: got id %d", ranked[0].ID)
	}
}

// A reranker outage must cost precision, not availability.
func TestRerankFallsBackWhenModelUnreachable(t *testing.T) {
	reranker := NewLLM("http://127.0.0.1:1", "test-model")
	passages := makePassages(5)

	ranked, err := reranker.Rerank("question", passages, 3)
	if err != nil {
		t.Fatalf("unreachable model should not error: %v", err)
	}
	if len(ranked) != 3 {
		t.Fatalf("got %d passages, want 3", len(ranked))
	}
	for index := range ranked {
		if ranked[index].ID != int64(index) {
			t.Errorf("fallback did not preserve input order: %+v", ranked)
		}
	}
}

// Unparseable output is the same failure as no output.
func TestRerankFallsBackOnGarbageOutput(t *testing.T) {
	server := fakeOllama(t, "I cannot rank these passages.")
	defer server.Close()

	reranker := NewLLM(server.URL, "test-model")
	passages := makePassages(4)

	ranked, err := reranker.Rerank("question", passages, 2)
	if err != nil {
		t.Fatal(err)
	}
	if ranked[0].ID != 0 || ranked[1].ID != 1 {
		t.Errorf("garbage output did not fall back to input order: %+v", ranked)
	}
}

func TestRerankHandlesTrivialInput(t *testing.T) {
	reranker := NewLLM("http://127.0.0.1:1", "test-model")

	if ranked, _ := reranker.Rerank("q", nil, 5); len(ranked) != 0 {
		t.Errorf("nil input returned %d passages", len(ranked))
	}
	single := []Passage{{ID: 1}}
	if ranked, _ := reranker.Rerank("q", single, 5); len(ranked) != 1 {
		t.Errorf("single passage returned %d", len(ranked))
	}
}

// The reranking prompt holds many candidates and will exceed Ollama's 4096 default,
// which would silently drop the last ones so they could never be ranked.
func TestContextWindowGrowsWithCandidateCount(t *testing.T) {
	reranker := NewLLM("http://localhost", "test-model")
	passages := make([]Passage, 40)
	for index := range passages {
		passages[index] = Passage{ID: int64(index), Text: string(make([]byte, 400))}
	}

	prompt := reranker.buildPrompt("a question", passages)
	if window := contextWindowFor(prompt); window <= 4096 {
		t.Errorf("window %d for a %d-char prompt would truncate candidates", window, len(prompt))
	}
}
