package chunker

// Chunk splits text into overlapping segments suitable for embedding.
// It splits on paragraph boundaries first, then sentence boundaries,
// targeting ~400 words per chunk with ~50-word overlap.
func Chunk(text string) []string {
	// TODO: split on "\n\n" into paragraphs
	// TODO: accumulate paragraphs into chunks up to the word limit
	// TODO: when a chunk is full, append it and start next chunk from overlap point
	// TODO: handle the case where a single paragraph exceeds the word limit (split by sentence)
	return nil
}
