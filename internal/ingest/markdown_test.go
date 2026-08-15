package ingest

import (
	"strings"
	"testing"

	"github.com/robertkoller/engrex/internal/chunker"
)

// Headings are what the section-aware chunker splits on, so extraction must leave them
// alone. This is the regression guard on the bug that made section-aware chunking
// impossible: the old stripMarkdown deleted every "#" before the chunker ever ran.
func TestCleanMarkdownPreservesStructure(t *testing.T) {
	input := "# Title\n\n## Section\n\n- a bullet\n- another\n\n> quoted line\n\nBody text.\n"

	cleaned := cleanMarkdown(input)

	for _, marker := range []string{"# Title", "## Section", "- a bullet", "> quoted line"} {
		if !strings.Contains(cleaned, marker) {
			t.Errorf("structure marker %q was stripped:\n%s", marker, cleaned)
		}
	}
}

// Inline formatting carries no structure, so it still goes.
func TestCleanMarkdownStripsInlineNoise(t *testing.T) {
	cleaned := cleanMarkdown("Some **bold** and *italic* and `code` and [a link](http://x.test) and ![img](y.png).")

	for _, noise := range []string{"**", "`", "](", "!["} {
		if strings.Contains(cleaned, noise) {
			t.Errorf("inline markup %q survived: %q", noise, cleaned)
		}
	}
	for _, kept := range []string{"bold", "italic", "code", "a link"} {
		if !strings.Contains(cleaned, kept) {
			t.Errorf("content %q was lost: %q", kept, cleaned)
		}
	}
}

func TestContentType(t *testing.T) {
	cases := map[string]string{
		"/notes/guide.md":       chunker.TypeMarkdown,
		"/notes/guide.markdown": chunker.TypeMarkdown,
		"/src/main.go":          chunker.TypeCodePrefix + "go",
		"/src/script.py":        chunker.TypeCodePrefix + "python",
		"/config/app.yml":       chunker.TypeCodePrefix + "yaml",
		"/notes/plain.txt":      chunker.TypeText,
		"/papers/thesis.pdf":    chunker.TypeText,
		"/page.html":            chunker.TypeText,
	}
	for path, want := range cases {
		if got := ContentType(path); got != want {
			t.Errorf("ContentType(%q) = %q, want %q", path, got, want)
		}
	}
}
