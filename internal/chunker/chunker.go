package chunker

import "strings"

const chunkLength = 400
const chunkOverlap = 50

// Chunk splits text into overlapping segments suitable for embedding.
// It splits on paragraph boundaries first, then sentence boundaries,
func Chunk(text string) []string {
	var chunks []string

	paragraphs := strings.Split(text, "\n\n")
	var allWords []string

	for _, paragraph := range paragraphs {
		allWords = append(allWords, strings.Fields(paragraph)...)

		for len(allWords) >= chunkLength {
			chunks = append(chunks, strings.Join(allWords[:chunkLength], " "))
			allWords = allWords[chunkLength-chunkOverlap:]
		}
	}
	if len(allWords) > 0 {
		chunks = append(chunks, strings.Join(allWords, " "))
	}

	return chunks
}
