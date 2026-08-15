package rewrite

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fakeOllama(t *testing.T, completion string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		json.NewEncoder(writer).Encode(map[string]string{"response": completion}) //nolint:errcheck
	}))
}

func TestNeedsDecomposition(t *testing.T) {
	multiPart := []string{
		"What did I save about ResNet, and how does it compare to VGG?",
		"What is batch norm? What is dropout?",
		"Show me the difference between L1 and L2 regularization",
		"resnet versus densenet",
	}
	for _, question := range multiPart {
		if !NeedsDecomposition(question) {
			t.Errorf("expected multi-part: %q", question)
		}
	}

	simple := []string{
		"what is a residual connection",
		"top-5 error rate",
		"how does batch normalization work?",
	}
	for _, question := range simple {
		if NeedsDecomposition(question) {
			t.Errorf("expected simple: %q", question)
		}
	}
}

// The gate exists so simple questions never pay for a generation call.
func TestSimpleQuestionSkipsTheModel(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		called = true
		json.NewEncoder(writer).Encode(map[string]string{"response": "a\nb"}) //nolint:errcheck
	}))
	defer server.Close()

	rewriter := NewLLM(server.URL, "test-model")
	queries, err := rewriter.Rewrite("what is a residual connection")
	if err != nil {
		t.Fatal(err)
	}
	if called {
		t.Error("simple question triggered a model call")
	}
	if len(queries) != 1 {
		t.Errorf("got %d queries, want 1: %v", len(queries), queries)
	}
}

func TestRewriteDecomposesMultiPart(t *testing.T) {
	server := fakeOllama(t, "ResNet architecture\nVGG architecture\nResNet compared to VGG")
	defer server.Close()

	rewriter := NewLLM(server.URL, "test-model")
	queries, err := rewriter.Rewrite("What did I save about ResNet, and how does it compare to VGG?")
	if err != nil {
		t.Fatal(err)
	}

	if len(queries) != 4 {
		t.Fatalf("got %d queries, want original + 3: %v", len(queries), queries)
	}
	// The original must survive, so a bad rewrite can only add candidates.
	if !strings.Contains(queries[0], "ResNet, and how does it compare") {
		t.Errorf("original question was not kept first: %q", queries[0])
	}
}

func TestParseQueriesStripsModelFraming(t *testing.T) {
	response := "LOOKUPS:\n1. ResNet architecture\n- VGG architecture\n* something else\nHeading:\n\nx\n"

	queries := parseQueries(response)
	for _, query := range queries {
		if strings.HasPrefix(query, "1.") || strings.HasPrefix(query, "-") || strings.HasPrefix(query, "*") {
			t.Errorf("list marker survived: %q", query)
		}
		if strings.EqualFold(query, "LOOKUPS:") || strings.HasSuffix(query, ":") {
			t.Errorf("framing line kept: %q", query)
		}
	}
	if len(queries) != 3 {
		t.Errorf("got %d queries, want 3: %v", len(queries), queries)
	}
}

// A rewriting failure must degrade to the original question, not to no search.
func TestRewriteFallsBackWhenModelUnreachable(t *testing.T) {
	rewriter := NewLLM("http://127.0.0.1:1", "test-model")
	question := "What did I save about ResNet, and how does it compare to VGG?"

	queries, err := rewriter.Rewrite(question)
	if err != nil {
		t.Fatalf("unreachable model should not error: %v", err)
	}
	if len(queries) != 1 || queries[0] != question {
		t.Errorf("fallback did not preserve the original: %v", queries)
	}
}

func TestRewriteCapsSubQueries(t *testing.T) {
	var lines []string
	for index := range 20 {
		lines = append(lines, string(rune('a'+index))+" lookup query")
	}
	server := fakeOllama(t, strings.Join(lines, "\n"))
	defer server.Close()

	rewriter := NewLLM(server.URL, "test-model")
	queries, err := rewriter.Rewrite("a and b and c and d and e and f")
	if err != nil {
		t.Fatal(err)
	}
	if len(queries) > maxSubQueries+1 {
		t.Errorf("got %d queries, cap is %d + original", len(queries), maxSubQueries)
	}
}

// A decomposition that just restates the question adds cost and no candidates.
func TestRewriteDropsDuplicateOfOriginal(t *testing.T) {
	question := "ResNet and VGG"
	server := fakeOllama(t, "ResNet and VGG\nResNet architecture")
	defer server.Close()

	rewriter := NewLLM(server.URL, "test-model")
	queries, err := rewriter.Rewrite(question)
	if err != nil {
		t.Fatal(err)
	}
	if len(queries) != 2 {
		t.Errorf("duplicate of the original was not dropped: %v", queries)
	}
}

func TestRewriteHandlesBlankInput(t *testing.T) {
	rewriter := NewLLM("http://127.0.0.1:1", "test-model")
	if queries, _ := rewriter.Rewrite("   "); len(queries) != 0 {
		t.Errorf("blank question returned %v", queries)
	}
}
