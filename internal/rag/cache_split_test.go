package rag

import (
	"testing"
	"time"

	"github.com/robertkoller/engrex/cache"
	"github.com/robertkoller/engrex/internal/store"
)

// The semantic cache in cache/ recognizes the answer prompt by its opening line and the
// markers around the question, copied from buildPrompt. It cannot capture this prompt
// itself, because reaching buildPrompt from there needs sqlite. So the check lives here:
// edit buildPrompt so the cache no longer recognizes it and this fails, instead of the
// cache quietly falling back to a looser split and losing hit rate
func TestCacheSplitsTheRealAnswerPrompts(t *testing.T) {
	question := "what did I save about ResNet?"
	chunks := []store.Chunk{
		{Text: "ResNet adds skip connections. QUESTION: a passage can contain this marker.", Source: "/notes/resnet.md", DocTitle: "ResNet notes", HeadingPath: "Architecture", CreatedAt: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)},
		{Text: "VGG stacks 3x3 convolutions.", Source: "/notes/vgg.md", CreatedAt: time.Date(2026, 8, 2, 0, 0, 0, 0, time.UTC)},
	}

	prompts := map[string]string{
		"answer":            buildPrompt(question, chunks, queryOptions{}),
		"answer with flags": buildPrompt(question, chunks, queryOptions{includeDate: true, includeSource: true}),
		"no context":        buildNoContextPrompt(question),
	}
	for name, prompt := range prompts {
		parts, found := cache.EngrexSplitter{}.Split(prompt)
		if !found {
			t.Errorf("%s: the cache no longer recognizes this prompt", name)
			continue
		}
		if parts.Class != cache.ClassAnswer {
			t.Errorf("%s: class = %q, want %q", name, parts.Class, cache.ClassAnswer)
		}
		if parts.Semantic != question {
			t.Errorf("%s: the cache isolated %q as the question, want %q", name, parts.Semantic, question)
		}
	}
}
