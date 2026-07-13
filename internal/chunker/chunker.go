package chunker

import (
	"fmt"
	"regexp"
	"strings"
)

const chunkLength = 400
const chunkOverlap = 50
const maxInputChars = 500000

var sentenceRegex = regexp.MustCompile(`[^.!?]+[.!?]*`)

// Chunk splits text into overlapping segments suitable for embedding, breaking on
// sentence boundaries so no chunk ever cuts a sentence in half. Sentences are packed
// into a chunk up to chunkLength words; consecutive chunks overlap by whole sentences
// totalling roughly chunkOverlap words.
func Chunk(text string) ([]string, error) {
	if len(text) > maxInputChars {
		return nil, fmt.Errorf("input too large: %d characters (max %d)", len(text), maxInputChars)
	}

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

	return output, nil
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
func splitSentences(text string) []string {
	matches := sentenceRegex.FindAllString(text, -1)
	sentences := make([]string, 0, len(matches))
	for _, match := range matches {
		trimmed := strings.TrimSpace(match)
		if trimmed != "" {
			sentences = append(sentences, trimmed)
		}
	}
	return sentences
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
