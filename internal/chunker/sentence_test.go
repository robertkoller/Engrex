package chunker

import (
	"strings"
	"testing"
)

// The regression that matters most: chunking must never alter the text it is given.
// The old splitter rewrote every decimal, corrupting the stored chunk, its embedding,
// and what the LLM read.
func TestChunkPreservesTextExactly(t *testing.T) {
	inputs := []string{
		"We use 0.01 to warm up. The error was 3.57 percent.",
		"See arXiv:1207.0580 for details.",
		"Version 1.2.3 shipped. Then 1.2.4 followed.",
		"Visit example.com/path.html today.",
		"Top-5 error of 3.57% beat the 4.49% baseline.",
	}

	for _, input := range inputs {
		chunks, err := Chunk(input)
		if err != nil {
			t.Fatal(err)
		}
		joined := strings.Join(chunks, " ")
		if joined != input {
			t.Errorf("text altered:\n  in:  %q\n  out: %q", input, joined)
		}
	}
}

func TestSplitSentencesKeepsNumbersWhole(t *testing.T) {
	sentences := splitSentences("We use 0.01 here. Then 3.57 later.")

	if len(sentences) != 2 {
		t.Fatalf("got %d sentences, want 2: %q", len(sentences), sentences)
	}
	if sentences[0] != "We use 0.01 here." {
		t.Errorf("first sentence = %q", sentences[0])
	}
	if sentences[1] != "Then 3.57 later." {
		t.Errorf("second sentence = %q", sentences[1])
	}
}

func TestSplitSentencesHandlesAbbreviations(t *testing.T) {
	cases := map[string]int{
		"See Fig. 4 for the curve.":                1,
		"Dr. Smith wrote it.":                      1,
		"R. R. Salakhutdinov et al. published it.": 1,
		"One thing. Another thing.":                2,
		"Really?! Yes.":                            2,
		"Wait... then go.":                         2,
		"No terminator here":                       1,
	}
	for input, want := range cases {
		if got := len(splitSentences(input)); got != want {
			t.Errorf("splitSentences(%q) = %d sentences, want %d: %q",
				input, got, want, splitSentences(input))
		}
	}
}

func TestSplitSentencesIgnoresEmptyInput(t *testing.T) {
	if got := splitSentences("   \n\t "); len(got) != 0 {
		t.Errorf("whitespace-only input produced %d sentences: %q", len(got), got)
	}
}
