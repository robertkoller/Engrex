package rag

import (
	"strings"
	"testing"
)

// The regression this guards: Ollama allocates 4096 tokens when num_ctx is unset and
// silently drops the OLDEST tokens beyond that. Since a RAG prompt leads with its
// instructions, an under-sized window ate the rules and the document manifest, and the
// model looked like it was ignoring instructions it had never received.
func TestContextWindowGrowsWithPrompt(t *testing.T) {
	// A prompt around the size that was being truncated in practice (~25KB / ~7k tokens).
	large := strings.Repeat("x", 25000)

	window := contextWindowFor(large)
	if window <= minContextTokens {
		t.Fatalf("window = %d, want more than the %d default for a 25KB prompt",
			window, minContextTokens)
	}

	estimated := len(large) / charsPerToken
	if window < estimated {
		t.Errorf("window %d is smaller than the estimated %d prompt tokens — would truncate",
			window, estimated)
	}
}

func TestContextWindowRespectsBounds(t *testing.T) {
	if got := contextWindowFor(""); got != minContextTokens {
		t.Errorf("empty prompt window = %d, want the %d floor", got, minContextTokens)
	}
	if got := contextWindowFor("short question"); got != minContextTokens {
		t.Errorf("short prompt window = %d, want the %d floor", got, minContextTokens)
	}
	if got := contextWindowFor(strings.Repeat("x", 10_000_000)); got != maxContextTokens {
		t.Errorf("huge prompt window = %d, want the %d ceiling", got, maxContextTokens)
	}
}

// The window has to hold the answer too, not just the prompt.
func TestContextWindowReservesResponseHeadroom(t *testing.T) {
	prompt := strings.Repeat("x", 60000)

	window := contextWindowFor(prompt)
	promptTokens := len(prompt) / charsPerToken
	if window < promptTokens+responseHeadroom && window != maxContextTokens {
		t.Errorf("window %d leaves no room for a response above %d prompt tokens",
			window, promptTokens)
	}
}
