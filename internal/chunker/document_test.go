package chunker

import (
	"strings"
	"testing"
)

func TestSplitSectionsBuildsHeadingPaths(t *testing.T) {
	document := `# Guide

Intro text here.

## Setup

Install the thing.

### Prerequisites

You need Go installed.

## Usage

Run the command.
`

	sections := splitSections(document)

	want := []string{
		"Guide",
		"Guide > Setup",
		"Guide > Setup > Prerequisites",
		"Guide > Usage",
	}
	if len(sections) != len(want) {
		t.Fatalf("got %d sections, want %d: %+v", len(sections), len(want), sections)
	}
	for index, expected := range want {
		if sections[index].headingPath != expected {
			t.Errorf("section %d path = %q, want %q", index, sections[index].headingPath, expected)
		}
	}
}

// A deeper heading must not leak into a later sibling's path — an h3 under "Setup"
// has to close when the next h2 opens, or every following chunk inherits a heading it
// doesn't belong to.
func TestSplitSectionsClosesDeeperHeadings(t *testing.T) {
	sections := splitSections("# A\n\n## B\n\n### C\n\ntext\n\n## D\n\nmore\n")

	last := sections[len(sections)-1]
	if last.headingPath != "A > D" {
		t.Errorf("final section path = %q, want %q", last.headingPath, "A > D")
	}
	if strings.Contains(last.headingPath, "C") {
		t.Errorf("stale deeper heading leaked into %q", last.headingPath)
	}
}

// A "#" inside a fenced code block is a comment, not a heading.
func TestSplitSectionsIgnoresHeadingsInFences(t *testing.T) {
	document := "# Real\n\n```python\n# not a heading\nvalue = 1\n```\n\nbody\n"

	sections := splitSections(document)
	if len(sections) != 1 {
		t.Fatalf("got %d sections, want 1: %+v", len(sections), sections)
	}
	if sections[0].headingPath != "Real" {
		t.Errorf("path = %q, want %q", sections[0].headingPath, "Real")
	}
}

func TestChunkMarkdownPrefixesHeadingPath(t *testing.T) {
	pieces := chunkMarkdown("# Config\n\n## Timeouts\n\nIt defaults to 30 seconds.\n")

	if len(pieces) == 0 {
		t.Fatal("no pieces produced")
	}
	final := pieces[len(pieces)-1]
	if !strings.HasPrefix(final.Text, "Config > Timeouts: ") {
		t.Errorf("chunk text not prefixed with heading path: %q", final.Text)
	}
	if final.HeadingPath != "Config > Timeouts" {
		t.Errorf("HeadingPath = %q", final.HeadingPath)
	}
}

func TestChunkDocumentAssignsSequentialIndexes(t *testing.T) {
	document := "# A\n\n" + strings.Repeat("Sentence one here. ", 300) +
		"\n\n# B\n\n" + strings.Repeat("Different content entirely. ", 300)

	pieces, err := ChunkDocument(document, TypeMarkdown)
	if err != nil {
		t.Fatal(err)
	}
	if len(pieces) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(pieces))
	}
	for index, piece := range pieces {
		if piece.Index != index {
			t.Errorf("piece %d has Index %d", index, piece.Index)
		}
	}
}

// The whole point of the code path: source must never be split by the prose sentence
// regex, which would cut on the "." in method calls and decimals.
func TestChunkCodeDoesNotSplitMidExpression(t *testing.T) {
	source := `package main

func handler() {
	value := config.Timeout.Seconds()
	ratio := 3.14159
	fmt.Println(value, ratio)
}
`

	pieces, err := ChunkDocument(source, TypeCodePrefix+"go")
	if err != nil {
		t.Fatal(err)
	}
	if len(pieces) != 1 {
		t.Fatalf("short file split into %d chunks, want 1", len(pieces))
	}
	if !strings.Contains(pieces[0].Text, "config.Timeout.Seconds()") {
		t.Error("method chain was broken across chunks")
	}
	if !strings.Contains(pieces[0].Text, "3.14159") {
		t.Error("decimal literal was broken across chunks")
	}
	if pieces[0].HeadingPath != "go" {
		t.Errorf("HeadingPath = %q, want the language", pieces[0].HeadingPath)
	}
}

func TestChunkCodeBreaksAtTopLevelDeclarations(t *testing.T) {
	var builder strings.Builder
	builder.WriteString("package main\n\n")
	// Three functions, each long enough that the second and third cross the boundary
	// threshold and start new chunks.
	for function := range 3 {
		builder.WriteString("func handler")
		builder.WriteByte(byte('A' + function))
		builder.WriteString("() {\n")
		for line := range 50 {
			builder.WriteString("\tstep(")
			builder.WriteString(strings.Repeat("x", line%5+1))
			builder.WriteString(")\n")
		}
		builder.WriteString("}\n\n")
	}

	pieces, err := ChunkDocument(builder.String(), TypeCodePrefix+"go")
	if err != nil {
		t.Fatal(err)
	}
	if len(pieces) < 2 {
		t.Fatalf("expected the file to split, got %d chunk(s)", len(pieces))
	}
	for _, piece := range pieces {
		if strings.HasPrefix(strings.TrimSpace(piece.Text), "step(") {
			continue // a windowed continuation of an oversized function is expected
		}
		if !strings.Contains(piece.Text, "func handler") {
			t.Errorf("chunk starts somewhere other than a declaration:\n%s", piece.Text)
		}
	}
}

func TestIsTopLevelDeclaration(t *testing.T) {
	cases := map[string]bool{
		"func main() {":       true,
		"type Store struct {": true,
		"\tindented := 1":     false,
		"    spaced()":        false,
		"}":                   false,
		")":                   false,
		"":                    false,
	}
	for line, want := range cases {
		if got := isTopLevelDeclaration(line); got != want {
			t.Errorf("isTopLevelDeclaration(%q) = %v, want %v", line, got, want)
		}
	}
}

// Plain text has no structure to preserve, so it falls back to prose packing and
// carries no heading path.
func TestChunkDocumentFallsBackToProse(t *testing.T) {
	pieces, err := ChunkDocument("First sentence. Second sentence. Third one.", TypeText)
	if err != nil {
		t.Fatal(err)
	}
	if len(pieces) != 1 {
		t.Fatalf("got %d chunks, want 1", len(pieces))
	}
	if pieces[0].HeadingPath != "" {
		t.Errorf("plain text got a heading path: %q", pieces[0].HeadingPath)
	}
}

func TestChunkDocumentRejectsOversizedInput(t *testing.T) {
	if _, err := ChunkDocument(strings.Repeat("a", maxInputChars+1), TypeText); err == nil {
		t.Error("expected an error for input over the size cap")
	}
}
