package chunker

import (
	"fmt"
	"strings"
)

// Content types recognised by ChunkDocument. Anything else falls back to prose.
const (
	TypeMarkdown   = "markdown"
	TypeText       = "text"
	TypeCodePrefix = "code:"
)

// Piece is one chunk plus the structural context it came from.
type Piece struct {
	Text string

	// HeadingPath is the chain of enclosing headings, outermost first, joined with
	// " > " — e.g. "Deployment > Rollback > Manual steps". Empty for content with no
	// headings above it.
	HeadingPath string

	// Index is the chunk's ordinal within its document, in reading order.
	Index int
}

// ChunkDocument splits text using whatever structure its content type provides, and
// returns each chunk with the heading it lives under.
//
// The split respects the document's own boundaries rather than a fixed word budget
// alone: markdown breaks at headings first and only packs sentences within a section,
// and code breaks at top-level declarations. A section shorter than the budget stays
// whole even if that wastes most of the budget — a chunk that is exactly one coherent
// section retrieves far better than a chunk that is the tail of one section plus the
// head of the next.
func ChunkDocument(text, contentType string) ([]Piece, error) {
	if len(text) > maxInputChars {
		return nil, fmt.Errorf("input too large: %d characters (max %d)", len(text), maxInputChars)
	}

	var pieces []Piece
	switch {
	case contentType == TypeMarkdown:
		pieces = chunkMarkdown(text)
	case strings.HasPrefix(contentType, TypeCodePrefix):
		pieces = chunkCode(text, strings.TrimPrefix(contentType, TypeCodePrefix))
	default:
		for _, chunk := range chunkProse(text) {
			pieces = append(pieces, Piece{Text: chunk})
		}
	}

	for index := range pieces {
		pieces[index].Index = index
	}
	return pieces, nil
}

// section is a run of body text under a chain of headings.
type section struct {
	headingPath string
	body        string
}

// chunkMarkdown splits at heading boundaries, then packs each section's prose to the
// word budget. Every resulting chunk is prefixed with its heading path so the embedding
// carries the section's topic — a chunk reading "It defaults to 30 seconds" embeds
// almost meaninglessly on its own, but "Configuration > Timeouts: It defaults to 30
// seconds" lands near queries about timeout configuration.
func chunkMarkdown(text string) []Piece {
	var pieces []Piece
	for _, current := range splitSections(text) {
		body := strings.TrimSpace(current.body)
		if body == "" {
			continue
		}
		for _, chunk := range chunkProse(body) {
			pieces = append(pieces, Piece{
				Text:        withHeading(current.headingPath, chunk),
				HeadingPath: current.headingPath,
			})
		}
	}
	return pieces
}

// splitSections walks the document line by line, maintaining a stack of open headings
// so each body line knows its full ancestry. A level-2 heading closes any deeper
// headings above it but keeps the level-1 it sits under.
func splitSections(text string) []section {
	var sections []section
	var stack []string
	var body strings.Builder

	flush := func() {
		if strings.TrimSpace(body.String()) != "" {
			sections = append(sections, section{
				headingPath: strings.Join(stack, " > "),
				body:        body.String(),
			})
		}
		body.Reset()
	}

	inFence := false
	for _, line := range strings.Split(text, "\n") {
		// Fenced code blocks are opaque: a "#" inside one is a comment, not a heading.
		if strings.HasPrefix(strings.TrimSpace(line), "```") {
			inFence = !inFence
			body.WriteString(line)
			body.WriteString("\n")
			continue
		}
		if inFence {
			body.WriteString(line)
			body.WriteString("\n")
			continue
		}

		match := headingRegex.FindStringSubmatch(line)
		if match == nil {
			body.WriteString(line)
			body.WriteString("\n")
			continue
		}

		flush()
		level := len(match[1])
		title := strings.TrimSpace(match[2])

		// Drop any headings at this level or deeper, then open this one.
		if level-1 < len(stack) {
			stack = stack[:level-1]
		}
		for len(stack) < level-1 {
			stack = append(stack, "") // a skipped level (h1 -> h3) leaves a gap
		}
		stack = append(stack, title)
	}
	flush()

	return sections
}

// withHeading prefixes a chunk with its heading path so the section topic is part of
// what gets embedded, not just what gets stored alongside it.
func withHeading(headingPath, chunk string) string {
	if headingPath == "" {
		return chunk
	}
	return headingPath + ": " + chunk
}

// chunkCode splits source files on top-level declaration boundaries, falling back to
// fixed line windows inside anything oversized.
//
// The point is to never route code through the sentence regex, which splits on the "."
// in method calls, decimals, and ellipses — producing chunks that start and end
// mid-expression. Declaration boundaries are approximated by unindented non-blank
// lines, which holds for brace and indentation languages alike.
func chunkCode(text, language string) []Piece {
	lines := strings.Split(text, "\n")
	var pieces []Piece
	var current []string

	emit := func() {
		if len(current) == 0 {
			return
		}
		body := strings.TrimRight(strings.Join(current, "\n"), "\n")
		if strings.TrimSpace(body) != "" {
			pieces = append(pieces, Piece{Text: body, HeadingPath: language})
		}
		current = nil
	}

	for _, line := range lines {
		atBoundary := isTopLevelDeclaration(line)

		// Break before a new top-level declaration, but only once the current chunk has
		// enough in it to be worth closing — otherwise a file of one-line declarations
		// becomes one chunk per line.
		if atBoundary && len(current) >= codeChunkLines/2 {
			emit()
		} else if len(current) >= codeChunkLines {
			// An oversized declaration (a long function) has no boundary to break on, so
			// window it, carrying trailing lines forward to keep context across the seam.
			overlap := current[max(0, len(current)-codeOverlapLines):]
			emit()
			current = append(current, overlap...)
		}

		current = append(current, line)
	}
	emit()

	return pieces
}

// isTopLevelDeclaration reports whether a line starts a new top-level construct: any
// non-blank line with no leading whitespace that isn't a closing brace.
func isTopLevelDeclaration(line string) bool {
	if line == "" || line != strings.TrimLeft(line, " \t") {
		return false
	}
	trimmed := strings.TrimSpace(line)
	switch trimmed {
	case "}", ")", "};", "):", "]":
		return false
	}
	return true
}
