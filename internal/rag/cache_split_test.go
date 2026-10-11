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

// Rewordings often retrieve the same passages in a different rank order. The cache
// should treat those as the same material, but never merge different material
func TestCacheContextIgnoresPassageOrderOnly(t *testing.T) {
	date := time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC)
	first := store.Chunk{Text: ".help - list other . commands", DocTitle: "section 1.pdf", Origin: "https://drive.google.com/file/d/a", CreatedAt: date}
	second := store.Chunk{Text: ".import /path/to/file NameOfTable", DocTitle: "Section 2.pptx", Origin: "https://docs.google.com/presentation/d/b", CreatedAt: date}
	third := store.Chunk{Text: "LIMIT number - limits the amount of tuples returned", DocTitle: "Section 2.pptx", Origin: "https://docs.google.com/presentation/d/b", CreatedAt: date}

	contextFor := func(question string, chunks ...store.Chunk) string {
		parts, found := cache.EngrexSplitter{}.Split(buildPrompt(question, chunks, queryOptions{}))
		if !found {
			t.Fatal("answer prompt not recognized")
		}
		return parts.Context
	}

	ranked := contextFor("sql basics", first, second, third)
	reordered := contextFor("give me the sql basics", third, first, second)
	if ranked != reordered {
		t.Error("same passages in a different order landed in different namespaces")
	}

	if ranked == contextFor("sql basics", first, second) {
		t.Error("a missing passage shared a namespace")
	}
	edited := third
	edited.Text += "!"
	if ranked == contextFor("sql basics", first, second, edited) {
		t.Error("a one byte passage change shared a namespace")
	}
}
