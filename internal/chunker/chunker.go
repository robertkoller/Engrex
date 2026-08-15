package chunker

import (
	"fmt"
	"regexp"
	"strings"
)

const chunkLength = 400
const chunkOverlap = 50
const maxInputChars = 500000

// codeChunkLines caps how many lines of code go in one chunk, and how many trailing
// lines carry over into the next. Code is measured in lines rather than words because
// its information density per word is far higher than prose — 400 words of Go is most
// of a file.
const codeChunkLines = 80
const codeOverlapLines = 10

// abbreviations are tokens whose trailing period doesn't end a sentence. Without them
// "Fig. 4" and "et al. 2016" split mid-reference, which matters for the academic PDFs
// this corpus is mostly made of.
var abbreviations = map[string]bool{
	"mr": true, "mrs": true, "ms": true, "dr": true, "prof": true, "st": true,
	"fig": true, "figs": true, "eq": true, "eqs": true, "ref": true, "refs": true,
	"sec": true, "ch": true, "vol": true, "no": true, "pp": true, "al": true,
	"e.g": true, "i.e": true, "vs": true, "etc": true, "cf": true, "approx": true,
	"jan": true, "feb": true, "mar": true, "apr": true, "jun": true, "jul": true,
	"aug": true, "sep": true, "sept": true, "oct": true, "nov": true, "dec": true,
}

// headingRegex matches an ATX markdown heading, capturing its level and text.
var headingRegex = regexp.MustCompile(`^(#{1,6})\s+(.+?)\s*#*$`)

// Chunk splits prose into overlapping segments suitable for embedding, breaking on
// sentence boundaries so no chunk ever cuts a sentence in half. Sentences are packed
// into a chunk up to chunkLength words; consecutive chunks overlap by whole sentences
// totalling roughly chunkOverlap words.
//
// This is the structure-blind path, kept for plain text. Markdown and code go through
// ChunkDocument, which preserves the document's own boundaries.
func Chunk(text string) ([]string, error) {
	if len(text) > maxInputChars {
		return nil, fmt.Errorf("input too large: %d characters (max %d)", len(text), maxInputChars)
	}
	return chunkProse(text), nil
}

// chunkProse packs sentences into word-budgeted chunks with sentence-granular overlap.
func chunkProse(text string) []string {
	sentences := splitSentences(text)
	var output []string
	var currChunk []string
	remaining := chunkLength

	for _, sentence := range sentences {
		words := wordCount(sentence)
		if words > chunkLength {
			if len(currChunk) > 0 {
				output = append(output, strings.Join(currChunk, " "))
				currChunk = nil
				remaining = chunkLength
			}
			output = append(output, splitLongSentence(sentence)...)
			continue
		}
		if words > remaining && len(currChunk) > 0 {
			output = append(output, strings.Join(currChunk, " "))
			overlap, overlapWords := overlapSentences(currChunk)
			currChunk = overlap
			remaining = chunkLength - overlapWords
		}

		currChunk = append(currChunk, sentence)
		remaining -= words
	}

	if len(currChunk) > 0 {
		output = append(output, strings.Join(currChunk, " "))
	}

	return output
}

// splitLongSentence hard-splits an oversized sentence into pieces of at most
// chunkLength words each — the last resort when a single sentence has no internal
// boundaries to break on.
func splitLongSentence(sentence string) []string {
	words := strings.Fields(sentence)
	var pieces []string
	for start := 0; start < len(words); start += chunkLength {
		end := min(start+chunkLength, len(words))
		pieces = append(pieces, strings.Join(words[start:end], " "))
	}
	return pieces
}

// splitSentences breaks text into trimmed, non-empty sentences, keeping terminal
// punctuation attached and capturing a final sentence even if it has no terminator.
//
// Sentences are returned as slices of the original text, never reassembled. The
// previous regex approach split on every "." and rejoined with a space, which silently
// rewrote "0.01" as "0. 01" and "arXiv:1207.0580" as "arXiv:1207. 0580" — corrupting
// the stored text, its embedding, and what the LLM eventually read.
func splitSentences(text string) []string {
	var sentences []string
	start := 0

	for index := 0; index < len(text); index++ {
		if !isTerminator(text[index]) {
			continue
		}

		// Absorb a run of terminators so "?!" or "..." ends one sentence, not three.
		end := index
		for end+1 < len(text) && isTerminator(text[end+1]) {
			end++
		}
		if !endsSentence(text, index, end) {
			index = end
			continue
		}

		if trimmed := strings.TrimSpace(text[start : end+1]); trimmed != "" {
			sentences = append(sentences, trimmed)
		}
		start = end + 1
		index = end
	}

	if trimmed := strings.TrimSpace(text[start:]); trimmed != "" {
		sentences = append(sentences, trimmed)
	}
	return sentences
}

func isTerminator(character byte) bool {
	return character == '.' || character == '!' || character == '?'
}

// endsSentence decides whether the terminator run ending at last actually closes a
// sentence. The rule that does the real work is the first one: a terminator only ends
// a sentence when whitespace or the end of the text follows it, which is what keeps
// decimals, version numbers, domains, and identifiers intact.
func endsSentence(text string, first, last int) bool {
	if last+1 < len(text) && !isSpace(text[last+1]) {
		return false
	}
	if text[first] != '.' {
		return true // "!" and "?" don't appear inside tokens the way "." does
	}
	return !endsWithAbbreviation(text[:first])
}

func isSpace(character byte) bool {
	return character == ' ' || character == '\t' || character == '\n' || character == '\r'
}

// endsWithAbbreviation reports whether the text immediately before a period is a known
// abbreviation or a single-letter initial ("R." in "R. R. Salakhutdinov").
func endsWithAbbreviation(before string) bool {
	wordStart := len(before)
	for wordStart > 0 && !isSpace(before[wordStart-1]) {
		wordStart--
	}
	word := strings.ToLower(before[wordStart:])
	if word == "" {
		return false
	}
	if len(word) == 1 && word[0] >= 'a' && word[0] <= 'z' {
		return true
	}
	return abbreviations[word]
}

// overlapSentences returns the trailing sentences of a chunk whose combined word
// count is about chunkOverlap words — used to seed the next chunk with context.
// it also returns the word count of those sentences.
func overlapSentences(sentences []string) ([]string, int) {
	var overlap []string
	length := 0

	for i := len(sentences) - 1; i >= 0; i-- {
		count := wordCount(sentences[i])
		if length+count > chunkOverlap && len(overlap) > 0 {
			break
		}
		overlap = append([]string{sentences[i]}, overlap...)
		length += count
	}

	return overlap, length
}

// wordCount returns the number of whitespace separated words in s.
func wordCount(s string) int {
	return len(strings.Fields(s))
}
