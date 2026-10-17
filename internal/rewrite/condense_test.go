package rewrite

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/robertkoller/engrex/internal/protocol"
)

var conversation = []protocol.Turn{
	{Role: protocol.RoleUser, Content: "what are the MCP tools?"},
	{Role: protocol.RoleAssistant, Content: "search_notes, get_document, and query_knowledge_graph."},
}

func TestCondenseUsesTheModelsRewrite(t *testing.T) {
	server := fakeOllama(t, "STANDALONE QUESTION: \"What does the search_notes MCP tool return?\"\n")
	defer server.Close()
	got := NewCondenser(server.URL, "test").Condense(conversation, "what does the first one return?")
	if got != "What does the search_notes MCP tool return?" {
		t.Errorf("got %q", got)
	}
}

func TestCondenseFallsBackToTheQuestion(t *testing.T) {
	question := "what does the first one return?"
	cases := map[string]string{
		"empty reply":   "",
		"answers again": strings.Repeat("search_notes returns ranked chunks with scores. ", 10),
	}
	for name, reply := range cases {
		server := fakeOllama(t, reply)
		defer server.Close()
		if got := NewCondenser(server.URL, "test").Condense(conversation, question); got != question {
			t.Errorf("%s: got %q, want the original question", name, got)
		}
	}

	unreachable := NewCondenser("http://127.0.0.1:1", "test")
	if got := unreachable.Condense(conversation, question); got != question {
		t.Errorf("unreachable: got %q, want the original question", got)
	}
}

// With no history there is nothing to resolve, so no generation should happen at all
func TestCondenseSkipsFirstTurns(t *testing.T) {
	called := false
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		called = true
	}))
	defer server.Close()

	if got := NewCondenser(server.URL, "test").Condense(nil, " what is RRF? "); got != "what is RRF?" {
		t.Errorf("got %q", got)
	}
	if called {
		t.Error("condensed a question with no history")
	}
}

func TestCondensePromptKeepsOnlyRecentTurns(t *testing.T) {
	var history []protocol.Turn
	for index := 0; index < 10; index++ {
		history = append(history, protocol.Turn{Role: protocol.RoleUser, Content: "turn " + string(rune('A'+index))})
	}
	prompt := buildCondensePrompt(history, "next")
	if strings.Contains(prompt, "turn F") || !strings.Contains(prompt, "turn G") || !strings.Contains(prompt, "turn J") {
		t.Errorf("expected only the last %d turns:\n%s", condenseTurns, prompt)
	}
}
