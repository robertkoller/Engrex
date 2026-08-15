package rag

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/robertkoller/engrex/internal/chunker"
	"github.com/robertkoller/engrex/internal/config"
	"github.com/robertkoller/engrex/internal/embedder"
	"github.com/robertkoller/engrex/internal/ingest"
	"github.com/robertkoller/engrex/internal/rerank"
	"github.com/robertkoller/engrex/internal/rewrite"
	"github.com/robertkoller/engrex/internal/store"
	"github.com/robertkoller/engrex/internal/verify"
)

const ollamaBaseURL = "http://localhost:11434"

// DefaultSearchDistance is the maximum cosine distance for a vector hit to count.
// Converted from the previously calibrated L2 0.95 — see the derivation on
// store.DefaultEdgeThreshold. Not freshly calibrated: that needs a multi-topic corpus.
const DefaultSearchDistance = 0.451

// DefaultSearchResults is how many passages go into the answer prompt.
//
// Measured at 5 vs 10 on qwen3:4b: 56s vs 100s per query — roughly 1.8x faster — with
// no loss of correctness on hand-checked facts, and one case where the smaller context
// produced the better answer because there was less irrelevant text competing for
// attention. Worth revisiting against the eval harness once the corpus spans more than
// one document, since a narrow corpus makes any 5 passages look as good as any 10.
const DefaultSearchResults = 5

// hybridCandidates is how many results to pull from each of the vector and keyword
// searches before fusing them down to the final topK.
const hybridCandidates = 20

// rerankCandidates is how wide retrieval casts when a reranker is attached. Over-
// fetching is only safe with something downstream to filter it: the reranker scores
// each passage against the query directly, so recall can be bought cheaply here and
// precision restored afterward.
const rerankCandidates = 40

// rrfK is the Reciprocal Rank Fusion constant. 60 is the value from the original RRF
// paper and the de-facto default; larger flattens the contribution of top ranks.
const rrfK = 60

// maxAnswerTokens caps how long an answer can run.
//
// Generation dominates query latency, and length dominates generation — so this is the
// single largest lever on how long a query takes. Measured on qwen3:4b answering "what
// is a residual connection": uncapped it produced 977 tokens in 53s, capped at 250 it
// produced a complete answer in 14s. Nearly 4x, for output nobody was reading.
//
// Verbosity varies enormously by model rather than by question — llama3.2 answered the
// same prompt in 52 tokens — so the cap is what keeps worst-case latency bounded when
// the generation model changes underneath.
const maxAnswerTokens = 400

// RAG wires the embedder, store, and LLM together into the add/query pipeline.
type RAG struct {
	embedder *embedder.Embedder
	store    *store.Store

	// reranker is optional. When nil, retrieval returns the fused hybrid ranking
	// directly — which is what shipped before reranking existed, and remains the
	// default until the eval numbers say the extra latency is worth it.
	reranker rerank.Reranker

	// rewriter is optional and works the same way: when nil, the question is searched
	// verbatim.
	rewriter rewrite.Rewriter

	// verifier is optional. When set, answers are checked against the passages they
	// were generated from and a grounding report is appended.
	verifier verify.Verifier

	// generateModel is resolved once at construction so every stage — answering,
	// reranking, rewriting, verifying — uses the same model. Resolving per call would
	// let an environment change mid-run split a single query across two models.
	generateModel string
}

// New returns a RAG instance. Checks that Ollama is reachable before returning.
func New(s *store.Store) (*RAG, error) {
	embed := embedder.New(ollamaBaseURL)
	if err := embed.Ping(); err != nil {
		return nil, err
	}
	return &RAG{
		embedder:      embed,
		store:         s,
		generateModel: config.GenerateModelName(),
	}, nil
}

// GenerateModel reports which model this instance generates with.
func (r *RAG) GenerateModel() string { return r.generateModel }

// WithModel returns a copy of the pipeline that generates with a different model, so a
// single query can be pointed elsewhere without changing saved configuration.
//
// A copy rather than a mutation because the daemon shares one RAG across concurrent
// connections: mutating it in place would let one request's model selection leak into
// another's, or race outright. An empty model returns the receiver unchanged.
func (r *RAG) WithModel(model string) *RAG {
	model = strings.TrimSpace(model)
	if model == "" || model == r.generateModel {
		return r
	}
	copied := *r
	copied.generateModel = model
	return &copied
}

// WithReranker enables reranking. Separate from New so the plain pipeline stays the
// default and the eval harness can score both paths against the same golden set.
func (r *RAG) WithReranker(reranker rerank.Reranker) *RAG {
	r.reranker = reranker
	return r
}

// WithRewriter enables query rewriting.
func (r *RAG) WithRewriter(rewriter rewrite.Rewriter) *RAG {
	r.rewriter = rewriter
	return r
}

// NewLLMReranker builds the listwise reranker over the same Ollama instance and model
// the answer path uses.
func (r *RAG) NewLLMReranker() rerank.Reranker {
	return rerank.NewLLM(ollamaBaseURL, r.generateModel)
}

// NewLLMRewriter builds the query rewriter over the same Ollama instance and model.
func (r *RAG) NewLLMRewriter() rewrite.Rewriter {
	return rewrite.NewLLM(ollamaBaseURL, r.generateModel)
}

// WithVerifier enables post-hoc citation verification.
func (r *RAG) WithVerifier(verifier verify.Verifier) *RAG {
	r.verifier = verifier
	return r
}

// NewLLMVerifier builds the entailment verifier over the same Ollama instance and model.
func (r *RAG) NewLLMVerifier() verify.Verifier {
	return verify.NewLLM(ollamaBaseURL, r.generateModel)
}

// Add chunks the text, embeds each chunk, and stores them with the given source
// label and origin (the original path a file was added from; "" when unknown).
//
// Documents with a stable identity (files, web pages) are re-ingested as a unit: if the
// content is unchanged since last time it's skipped outright, otherwise the previous
// version's chunks are deleted before the new ones are stored — so editing and re-saving
// a file updates it in place instead of piling up stale, overlapping copies. Typed
// cli/hotkey notes have no such identity and are always appended (with dedup).
func (r *RAG) Add(text string, source string, origin string) error {
	contentType := ingest.ContentType(source)
	docTitle := documentTitle(source, origin)

	pieces, err := chunker.ChunkDocument(text, contentType)
	if err != nil {
		return err
	}

	if key, replaceable := store.DocumentIdentity(source, origin); replaceable {
		return r.addDocument(text, source, origin, key, pieces, contentType, docTitle)
	}

	savedCount := 0
	for _, piece := range pieces {
		vector, err := r.embedder.EmbedDocument(piece.Text)
		if err != nil {
			return err
		}

		inserted, err := r.store.Insert(piece.Text, source, origin, vector,
			metadataFor(piece, contentType, docTitle))
		if err != nil {
			return err
		}
		if inserted {
			savedCount++
		} else {
			fmt.Println("Skipped: too similar to something already stored.")
		}
	}

	r.writeStubIfNeeded(text, source, origin)
	fmt.Printf("Saved %d chunk(s).\n", savedCount)
	return nil
}

// metadataFor turns a chunker piece plus its document-level facts into the row
// metadata the store persists.
func metadataFor(piece chunker.Piece, contentType, docTitle string) store.Metadata {
	return store.Metadata{
		HeadingPath: piece.HeadingPath,
		ChunkIndex:  piece.Index,
		DocTitle:    docTitle,
		ContentType: contentType,
	}
}

// documentTitle is the human-readable name for a document: the page title for web
// captures (carried in source when origin is a URL), otherwise the file's base name.
// Typed cli/hotkey notes have no title.
func documentTitle(source, origin string) string {
	if isWebURL(origin) {
		return source
	}
	if filepath.IsAbs(source) {
		return filepath.Base(source)
	}
	return ""
}

// addDocument re-ingests a replaceable document (a file or web page). It skips work
// when the content hash matches the last ingest, and otherwise deletes the document's
// previous chunks before storing the fresh ones so nothing stale accumulates.
func (r *RAG) addDocument(text, source, origin, key string, pieces []chunker.Piece, contentType, docTitle string) error {
	hash := contentHash(text)
	existing, seen, err := r.store.DocumentHash(key)
	if err != nil {
		return err
	}
	if seen && existing == hash {
		fmt.Println("Unchanged since last ingest — skipped.")
		return nil
	}

	if _, err := r.store.DeleteBySource(source, origin); err != nil {
		return err
	}

	savedCount := 0
	for _, piece := range pieces {
		vector, err := r.embedder.EmbedDocument(piece.Text)
		if err != nil {
			return err
		}
		if err := r.store.InsertDocumentChunk(piece.Text, source, origin, vector,
			metadataFor(piece, contentType, docTitle)); err != nil {
			return err
		}
		savedCount++
	}

	if err := r.store.UpsertDocument(key, hash); err != nil {
		return err
	}

	r.writeStubIfNeeded(text, source, origin)
	fmt.Printf("Saved %d chunk(s).\n", savedCount)
	return nil
}

// Reindex re-embeds every stored chunk from its existing text and rewrites the vector
// index, reporting progress through the supplied writer.
//
// Needed after any change to how text is embedded — the task-prefix and normalization
// fix, or an embedding model swap — because vectors produced by the old scheme sit in
// a different space and can't be compared against new query vectors. It works from the
// chunk text already in the database, so nothing has to be re-read from disk and the
// chunk ids, sources, and metadata are all preserved.
func (r *RAG) Reindex(out io.Writer) error {
	chunks, err := r.store.List()
	if err != nil {
		return err
	}
	if len(chunks) == 0 {
		fmt.Fprintln(out, "Nothing to reindex.")
		return nil
	}

	fmt.Fprintf(out, "Re-embedding %d chunk(s)...\n", len(chunks))
	for index, chunk := range chunks {
		vector, err := r.embedder.EmbedDocument(chunk.Text)
		if err != nil {
			return fmt.Errorf("embedding chunk %d: %w", chunk.ID, err)
		}
		if err := r.store.ReplaceVector(chunk.ID, vector); err != nil {
			return fmt.Errorf("storing vector for chunk %d: %w", chunk.ID, err)
		}
		if (index+1)%25 == 0 {
			fmt.Fprintf(out, "  %d/%d\n", index+1, len(chunks))
		}
	}

	fmt.Fprintln(out, "Rebuilding graph edges...")
	edges, err := r.store.ReindexEdges(store.DefaultEdgeThreshold)
	if err != nil {
		return err
	}
	fmt.Fprintf(out, "Reindexed %d chunk(s), %d edge(s).\n", len(chunks), edges)
	return nil
}

// Diagnosis is what `engrex doctor` reports: the facts needed to tell whether the
// embedding and index layers are in the state the retrieval math assumes.
type Diagnosis struct {
	Model      string
	Dimensions int

	// RawMagnitude is the Euclidean length of the model's unmodified output. vec0 ranks
	// by cosine distance, which is only well behaved on unit vectors — so a value far
	// from 1.0 is why Engrex normalizes before storing.
	RawMagnitude float64

	// StoredMagnitude is the length after normalization, and should be 1.0.
	StoredMagnitude float64

	SchemaVersion int
	ChunkCount    int
	VectorCount   int
}

// Diagnose embeds a probe string and inspects the index, so the embedding-space and
// schema assumptions can be checked rather than assumed.
func (r *RAG) Diagnose() (Diagnosis, error) {
	const probe = "engrex embedding diagnostic probe"

	raw, err := r.embedder.EmbedRaw(probe)
	if err != nil {
		return Diagnosis{}, err
	}
	stored, err := r.embedder.EmbedDocument(probe)
	if err != nil {
		return Diagnosis{}, err
	}

	diagnosis := Diagnosis{
		Model:           embedder.DefaultModel,
		Dimensions:      len(raw),
		RawMagnitude:    embedder.Magnitude(raw),
		StoredMagnitude: embedder.Magnitude(stored),
	}

	diagnosis.SchemaVersion, err = r.store.SchemaVersion()
	if err != nil {
		return Diagnosis{}, err
	}
	diagnosis.ChunkCount, diagnosis.VectorCount, err = r.store.IndexCounts()
	if err != nil {
		return Diagnosis{}, err
	}
	return diagnosis, nil
}

// contentHash returns a hex SHA-256 of the document text, used to detect whether a
// re-ingested document has actually changed.
func contentHash(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:])
}

// writeStubIfNeeded writes a browsable .txt into ~/Engrex/RawText for content that
// didn't come from a real file on disk (cli/hotkey text and web captures). Files that
// already exist on disk get no stub.
func (r *RAG) writeStubIfNeeded(text, source, origin string) {
	if _, statErr := os.Stat(source); statErr == nil {
		return
	}
	if isWebURL(origin) {
		header := fmt.Sprintf("Title: %s\nSource: %s\n\n", source, origin)
		name := sanitizeFilename(source) + ".txt"
		if err := cliTextStub(name, []byte(header+text)); err != nil {
			log.Printf("failed to write stub file: %v", err)
		}
		return
	}
	var title string
	if len(text) > 20 {
		title = text[:20]
	} else {
		title = text
	}
	if err := cliTextStub(fmt.Sprintf("%v.txt", title), []byte(text)); err != nil {
		log.Printf("failed to write stub file: %v", err)
	}
}

// DebugSearch embeds the question and returns the nearest chunks with raw distances,
// no filtering.
func (r *RAG) DebugSearch(question string) ([]store.Chunk, error) {
	queryVec, err := r.embedder.EmbedQuery(question)
	if err != nil {
		return nil, err
	}
	return r.store.RawSearch(queryVec, store.DefaultRawSearchLimit)
}

// DebugPrompt returns the exact prompt Query would send for a question, so the
// assembled context can be inspected without generating an answer.
func (r *RAG) DebugPrompt(question string, maxDistance float64, topK int) (string, error) {
	question, options := parseQueryFlags(question)
	chunks, err := r.Retrieve(question, maxDistance, topK)
	if err != nil {
		return "", err
	}
	if len(chunks) == 0 {
		return buildNoContextPrompt(question), nil
	}
	return buildPrompt(question, chunks, options), nil
}

func (r *RAG) Retrieve(question string, maxDistance float64, topK int) ([]store.Chunk, error) {
	// Without a reranker there is nothing to filter the extra candidates back down, so
	// over-fetching would just push noise into the prompt.
	fetch := topK
	if r.reranker != nil {
		fetch = max(rerankCandidates, topK)
	}

	queries := []string{question}
	if r.rewriter != nil {
		rewritten, err := r.rewriter.Rewrite(question)
		if err == nil && len(rewritten) > 0 {
			queries = rewritten
		}
	}

	fused, err := r.retrieveAcross(queries, maxDistance, fetch)
	if err != nil {
		return nil, err
	}
	if r.reranker == nil || len(fused) <= 1 {
		return fused, nil
	}
	// Reranking scores against the user's actual question, never a rewritten
	// sub-query — the sub-queries exist to widen recall, but relevance is still
	// judged against what was asked.
	return r.rerankChunks(question, fused, topK), nil
}

// retrieveAcross runs each query and fuses their results into one ranking.
//
// Fusing by rank rather than concatenating matters: a chunk that several sub-queries
// all surface is more likely to be relevant to the whole question than one that only
// the narrowest sub-query found, and RRF is what expresses that. It also keeps the
// single-query case identical to before, since fusing one list is a no-op.
func (r *RAG) retrieveAcross(queries []string, maxDistance float64, limit int) ([]store.Chunk, error) {
	if len(queries) == 1 {
		return r.retrieveCandidates(queries[0], maxDistance, limit)
	}

	var rankings [][]store.Chunk
	for _, query := range queries {
		hits, err := r.retrieveCandidates(query, maxDistance, limit)
		if err != nil {
			return nil, err
		}
		rankings = append(rankings, hits)
	}
	return fuseRankings(rankings, limit), nil
}

// retrieveCandidates runs the hybrid search and fuses the two rankings, with no
// reranking applied.
func (r *RAG) retrieveCandidates(question string, maxDistance float64, limit int) ([]store.Chunk, error) {
	queryVec, err := r.embedder.EmbedQuery(question)
	if err != nil {
		return nil, err
	}

	candidates := max(limit, hybridCandidates)

	vectorHits, err := r.store.Search(queryVec, maxDistance, candidates)
	if err != nil {
		return nil, err
	}

	var keywordHits []store.Chunk
	if ftsQuery := toFTSQuery(question); ftsQuery != "" {
		keywordHits, err = r.store.KeywordSearch(ftsQuery, candidates)
		if err != nil {
			return nil, err
		}
	}
	return fuseRRF(vectorHits, keywordHits, limit), nil
}

// rerankChunks reorders fused candidates by relevance and keeps the best topK.
//
// Reranking is what makes the wide fetch above worthwhile: retrieval maximizes recall
// and accepts noise, and this is the stage that turns that recall back into precision
// before anything reaches the prompt.
func (r *RAG) rerankChunks(question string, chunks []store.Chunk, topK int) []store.Chunk {
	passages := make([]rerank.Passage, len(chunks))
	byID := make(map[int64]store.Chunk, len(chunks))
	for index, chunk := range chunks {
		passages[index] = rerank.Passage{ID: chunk.ID, Text: chunk.Text, Label: chunkLabel(chunk)}
		byID[chunk.ID] = chunk
	}

	ranked, err := r.reranker.Rerank(question, passages, topK)
	if err != nil {
		// The reranker already falls back internally; this is the last guard so a
		// reranking failure costs precision rather than the whole query.
		log.Printf("rerank failed, using retrieval order: %v", err)
		if len(chunks) > topK {
			return chunks[:topK]
		}
		return chunks
	}

	reordered := make([]store.Chunk, 0, len(ranked))
	for _, passage := range ranked {
		if chunk, found := byID[passage.ID]; found {
			reordered = append(reordered, chunk)
		}
	}
	return reordered
}

// Query embeds the question, retrieves the top-K most relevant chunks,
// builds a RAG prompt, and streams the LLM response to stdout.
func (r *RAG) Query(out io.Writer, question string, maxDistance float64, topK int) error {
	question, options := parseQueryFlags(question)

	chunks, err := r.Retrieve(question, maxDistance, topK)
	if err != nil {
		return err
	}

	if err := json.NewEncoder(out).Encode(map[string][]string{"sources": collectSources(chunks)}); err != nil {
		return err
	}

	var prompt string
	if len(chunks) == 0 {
		fmt.Fprintln(out, "[No relevant notes found — answering from outside knowledge]")
		prompt = buildNoContextPrompt(question)
	} else {
		prompt = buildPrompt(question, chunks, options)
	}

	body, err := json.Marshal(map[string]any{
		"model":  r.generateModel,
		"prompt": prompt,
		"stream": true,
		"options": map[string]any{
			"num_ctx":     contextWindowFor(prompt),
			"num_predict": maxAnswerTokens,
		},
	})
	if err != nil {
		return err
	}
	response, err := http.Post(ollamaBaseURL+"/api/generate", "application/json", bytes.NewReader(body))
	if err != nil {
		return err
	}
	defer response.Body.Close()

	// The answer is streamed to the caller and accumulated at the same time. Streaming
	// is what makes the wait tolerable, but verification needs the finished text — so
	// it is collected as it goes rather than buying it back with a second pass.
	var answer strings.Builder
	scanner := bufio.NewScanner(response.Body)
	for scanner.Scan() {
		var token struct {
			Response string `json:"response"`
			Done     bool   `json:"done"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &token); err != nil {
			fmt.Fprint(out, err)
		}
		fmt.Fprint(out, token.Response)
		answer.WriteString(token.Response)
		if token.Done {
			break
		}
	}
	fmt.Fprintln(out)

	r.writeGroundingReport(out, answer.String(), chunks)
	return nil
}

// writeGroundingReport checks the answer against the passages it was built from and
// appends the result.
//
// This runs after the answer has already been streamed, so it can only annotate, never
// suppress. That is the right trade for a local assistant: the user has read the answer
// by the time the check finishes, and telling them which sentences their notes do not
// support is more useful than withholding the answer until a 3B model finishes grading
// it. Nothing is written when the answer had no context to be grounded in.
func (r *RAG) writeGroundingReport(out io.Writer, answer string, chunks []store.Chunk) {
	if r.verifier == nil || len(chunks) == 0 || strings.TrimSpace(answer) == "" {
		return
	}

	passages := make([]string, len(chunks))
	for index, chunk := range chunks {
		passages[index] = chunk.Text
	}

	report, err := r.verifier.Verify(answer, passages)
	if err != nil {
		log.Printf("verification failed: %v", err)
		return
	}

	var builder strings.Builder
	verify.WriteReport(report, &builder)
	fmt.Fprint(out, builder.String())
}

// Context window sizing. Ollama does not use a model's full architectural context
// unless asked: with no num_ctx it allocates 4096 tokens regardless of what the model
// supports, then silently discards the OLDEST tokens of anything longer. A RAG prompt
// puts its instructions first, so truncation ate the rules and the document manifest
// while leaving the passages — the model answered without ever seeing what it was told
// to do, and looked like it was ignoring instructions rather than never receiving them.
//
// Sizing the window to the prompt is the fix. The bounds keep it honest: a floor so
// short prompts don't shrink below Ollama's own default, and a ceiling because the KV
// cache grows with the window and this runs on a laptop.
const (
	minContextTokens = 4096
	maxContextTokens = 32768

	// charsPerToken deliberately under-estimates. English averages ~4 chars/token, but
	// these prompts carry code, numbers, and citations that tokenize far denser, and
	// guessing low costs a little memory whereas guessing high silently truncates.
	charsPerToken = 3

	// responseHeadroom reserves room for the answer, which shares the same window.
	responseHeadroom = 1024
)

// contextWindowFor estimates how large a context window a prompt needs.
func contextWindowFor(prompt string) int {
	needed := len(prompt)/charsPerToken + responseHeadroom
	if needed < minContextTokens {
		return minContextTokens
	}
	if needed > maxContextTokens {
		return maxContextTokens
	}
	return needed
}

// toFTSQuery turns a raw user question into a safe FTS5 MATCH expression. Each word is
// wrapped in double quotes so punctuation and reserved words (AND/OR/NEAR) are treated as
// literal search terms instead of query syntax, and the terms are OR'd together to
// maximize recall — rank fusion sorts out relevance afterward. Returns "" when there are
// no usable terms, in which case the caller skips keyword search.
func toFTSQuery(raw string) string {
	var terms []string
	for _, field := range strings.Fields(raw) {
		if isStopword(field) {
			continue
		}
		field = strings.ReplaceAll(field, `"`, `""`) // escape embedded quotes
		terms = append(terms, `"`+field+`"`)
	}
	return strings.Join(terms, " OR ")
}

// stopwords are dropped before keyword search.
//
// The terms are OR'd, and BM25 has no relevance threshold the way vector search has
// maxDistance — so a single common word matching is enough to pull a chunk into the
// results. "what is an orangutan" over a corpus with no orangutan in it still matched
// every chunk containing "is", and those chunks then appeared as cited sources for a
// question the notes could not answer at all.
//
// Only closed-class words are listed: articles, pronouns, prepositions, auxiliaries,
// and question words. Anything that could plausibly be the subject of a search stays.
var stopwords = map[string]bool{
	"a": true, "an": true, "the": true, "and": true, "or": true, "but": true,
	"if": true, "then": true, "than": true, "that": true, "this": true, "these": true,
	"those": true, "is": true, "are": true, "was": true, "were": true, "be": true,
	"been": true, "being": true, "am": true, "do": true, "does": true, "did": true,
	"have": true, "has": true, "had": true, "can": true, "could": true, "will": true,
	"would": true, "should": true, "may": true, "might": true, "must": true,
	"i": true, "me": true, "my": true, "you": true, "your": true, "it": true,
	"its": true, "we": true, "our": true, "they": true, "them": true, "their": true,
	"he": true, "she": true, "his": true, "her": true,
	"of": true, "in": true, "on": true, "at": true, "to": true, "for": true,
	"with": true, "from": true, "by": true, "about": true, "as": true, "into": true,
	"what": true, "when": true, "where": true, "who": true, "whom": true, "which": true,
	"why": true, "how": true, "there": true, "here": true,
	"me?": true, "please": true, "tell": true, "show": true, "give": true,
}

// isStopword reports whether a raw query word carries no retrieval signal. Punctuation
// is trimmed first so "what?" and "the," are recognised.
func isStopword(field string) bool {
	cleaned := strings.ToLower(strings.Trim(field, `.,!?;:'"()[]{}`))
	return cleaned == "" || stopwords[cleaned]
}

// fuseRRF merges the vector and keyword result lists with Reciprocal Rank Fusion: each
// list contributes 1/(rrfK + rank) to a chunk's score, so chunks ranked highly by either
// method — and especially by both — rise to the top. It fuses ranks rather than raw
// scores because cosine distance and BM25 live on different, incomparable scales. When a
// chunk appears in both lists the vector copy is kept (it carries the distance).
// fuseRankings applies Reciprocal Rank Fusion across any number of result lists,
// which is what lets several rewritten sub-queries contribute to one ranking.
func fuseRankings(rankings [][]store.Chunk, topK int) []store.Chunk {
	scoreByID := make(map[int64]float64)
	chunkByID := make(map[int64]store.Chunk)
	var order []int64

	for _, ranking := range rankings {
		for rank, chunk := range ranking {
			if _, seen := chunkByID[chunk.ID]; !seen {
				chunkByID[chunk.ID] = chunk
				order = append(order, chunk.ID)
			}
			scoreByID[chunk.ID] += 1.0 / float64(rrfK+rank+1)
		}
	}

	sort.SliceStable(order, func(i, j int) bool {
		return scoreByID[order[i]] > scoreByID[order[j]]
	})

	fused := make([]store.Chunk, 0, len(order))
	for _, id := range order {
		chunk := chunkByID[id]
		chunk.Score = scoreByID[id]
		fused = append(fused, chunk)
	}
	if len(fused) > topK {
		fused = fused[:topK]
	}
	return fused
}

func fuseRRF(vectorHits, keywordHits []store.Chunk, topK int) []store.Chunk {
	scoreByID := make(map[int64]float64)
	chunkByID := make(map[int64]store.Chunk)
	var order []int64 // first-seen order, so ties stay deterministic

	consume := func(hits []store.Chunk) {
		for rank, chunk := range hits {
			if _, seen := chunkByID[chunk.ID]; !seen {
				chunkByID[chunk.ID] = chunk
				order = append(order, chunk.ID)
			}
			scoreByID[chunk.ID] += 1.0 / float64(rrfK+rank+1)
		}
	}
	consume(vectorHits)
	consume(keywordHits)

	sort.SliceStable(order, func(i, j int) bool {
		return scoreByID[order[i]] > scoreByID[order[j]]
	})

	fused := make([]store.Chunk, 0, len(order))
	for _, id := range order {
		chunk := chunkByID[id]
		chunk.Score = scoreByID[id]
		fused = append(fused, chunk)
	}
	if len(fused) > topK {
		fused = fused[:topK]
	}
	return fused
}

// collectSources returns the deduplicated real-file sources of the retrieved chunks,
// in order, skipping non-file sources like "cli" and "hotkey". Prefers the origin
// (where a file was added from) over the internal ~/Engrex copy path when known.
func collectSources(chunks []store.Chunk) []string {
	seen := make(map[string]bool)
	sources := make([]string, 0)
	for _, chunk := range chunks {
		source := chunk.Source
		if chunk.Origin != "" {
			source = chunk.Origin
		}
		if !isLinkableSource(source) {
			continue
		}
		if seen[source] {
			continue
		}
		seen[source] = true
		sources = append(sources, source)
	}
	return sources
}

// isLinkableSource reports whether a source is something the UI can open —
// an absolute file path or a web URL (skips labels like "cli" and "hotkey").
func isLinkableSource(source string) bool {
	return filepath.IsAbs(source) || isWebURL(source)
}

func isWebURL(source string) bool {
	return strings.HasPrefix(source, "http://") || strings.HasPrefix(source, "https://")
}

// sanitizeFilename makes a page title safe to use as a filename: strips path
// separators, trims whitespace, and caps the length.
func sanitizeFilename(name string) string {
	name = strings.ReplaceAll(name, "/", "-")
	name = strings.ReplaceAll(name, "\\", "-")
	name = strings.TrimSpace(name)
	if runes := []rune(name); len(runes) > 60 {
		name = strings.TrimSpace(string(runes[:60]))
	}
	if name == "" {
		name = "untitled"
	}
	return name
}

// Flag options
type queryOptions struct {
	includeDate   bool
	includeSource bool
}

// Reformats the query to seperate the quuery and flags
func parseQueryFlags(question string) (string, queryOptions) {
	fields := strings.Fields(question)
	kept := make([]string, 0, len(fields))
	var options queryOptions
	for _, field := range fields {
		switch field {
		case "--date":
			options.includeDate = true
		case "--source":
			options.includeSource = true
		default:
			kept = append(kept, field)
		}
	}
	return strings.Join(kept, " "), options
}

// buildPrompt formats retrieved chunks and the user question into a RAG prompt.
//
// The structure is deliberate and tuned for a small local model. Three things matter:
//
// Grounding is stated as a hard constraint, not a preference. The previous version
// invited the model to "supplement with your own knowledge" whenever the notes fell
// short, which a 3B model reads as permission to answer from priors — and it then
// ignores the accompanying instruction to label those sentences, because conditional
// formatting rules are exactly what models that size fail at. The escape hatch is now
// a single narrow instruction: say what the notes do contain, and say plainly if they
// don't cover it.
//
// The constraint is repeated after the context. Small models weight the end of a long
// prompt far more heavily than the beginning, and the context can run to thousands of
// tokens, so an instruction given only up front is effectively gone by generation time.
//
// Each chunk is labelled with the document it came from and its section, so the model
// can answer questions about *which* document says something, not just what it says.
func buildPrompt(question string, chunks []store.Chunk, options queryOptions) string {
	var builder strings.Builder // yes im using string builder, no its not ai who wrote this ik that string builder is less compute

	builder.WriteString("You are a personal knowledge assistant with access to the user's OWN saved notes and documents. ")
	builder.WriteString("Everything in the CONTEXT below was saved by the user, so treat it as factual and authoritative. ")
	builder.WriteString("Never refuse, moralize, or add disclaimers about privacy or personal information — the user is only asking about their own material.\n\n")

	builder.WriteString("RULES:\n")
	builder.WriteString("1. Answer using ONLY the CONTEXT below. Do not use anything you know from training.\n")
	builder.WriteString("2. Never invent titles, authors, dates, numbers, or sources. If it is not in the CONTEXT, it does not exist.\n")
	builder.WriteString("3. If the CONTEXT does not answer the question, say exactly what the saved material DOES cover, then say the rest is not in their notes. Do not fill the gap from memory.\n")
	builder.WriteString("4. Answer directly and concisely. Lead with the answer itself. Include the specific details that answer the question — numbers, names, settings — and leave out everything else.\n")
	builder.WriteString("5. Do not restate the question, summarize the context, or add a closing offer to help.\n")

	if options.includeDate || options.includeSource {
		var parts []string
		if options.includeSource {
			parts = append(parts, "its source document")
		}
		if options.includeDate {
			parts = append(parts, "the date it was saved")
		}
		fmt.Fprintf(&builder, "6. When you use a passage, cite %s once in parentheses where you use it. Cite each document only once, not per sentence.\n", strings.Join(parts, " and "))
	}

	// A manifest of the documents the passages came from, stated once and up front.
	//
	// Per-passage labels alone are not enough: questions like "bring up my paper about
	// X" are about which document exists, not what it says, and no individual passage
	// ever states "this document is a paper titled X". A small model reading ten
	// fragments labelled with the same filename will still answer that it found no
	// such paper. Naming the documents once, as their own section, is what makes that
	// answerable.
	documents := documentManifest(chunks)
	builder.WriteString("\nSAVED DOCUMENTS these passages come from")
	fmt.Fprintf(&builder, " (%d):\n", len(documents))
	for index, document := range documents {
		fmt.Fprintf(&builder, "  %d. %s\n", index+1, document)
	}
	builder.WriteString("\nThese documents ARE in the user's saved collection. If they ask what they have saved on this topic, name them.\n")

	builder.WriteString("\nCONTEXT:\n")
	for index, chunk := range chunks {
		fmt.Fprintf(&builder, "[%d] document: %s", index+1, chunkLabel(chunk))
		if chunk.HeadingPath != "" {
			fmt.Fprintf(&builder, " | section: %s", chunk.HeadingPath)
		}
		fmt.Fprintf(&builder, " | saved: %s\n%s\n\n", chunk.CreatedAt.Format("2006-01-02"), chunk.Text)
	}

	fmt.Fprintf(&builder, "QUESTION: %s\n\n", question)
	builder.WriteString("Answer from the CONTEXT above only. If it is not there, say so rather than guessing.\nANSWER:")

	return builder.String()
}

// documentManifest returns the distinct documents the retrieved passages came from, in
// the order they were first retrieved.
func documentManifest(chunks []store.Chunk) []string {
	seen := make(map[string]bool)
	var documents []string
	for _, chunk := range chunks {
		label := chunkLabel(chunk)
		if label == "" || seen[label] {
			continue
		}
		seen[label] = true
		documents = append(documents, label)
	}
	return documents
}

// chunkLabel names the document a chunk came from, preferring the title recorded at
// ingest and falling back to the file name for chunks stored before titles existed.
func chunkLabel(chunk store.Chunk) string {
	if chunk.DocTitle != "" {
		return chunk.DocTitle
	}
	if chunk.Origin != "" {
		return chunk.Origin
	}
	return filepath.Base(chunk.Source)
}

// buildNoContextPrompt builds a prompt for when no stored notes are relevant.
// Instructs the LLM to answer from its own knowledge and label it clearly.
func buildNoContextPrompt(question string) string {
	return fmt.Sprintf(
		"You are a personal knowledge assistant. The user has no saved notes relevant to this question. "+
			"Answer from your own training knowledge, but start your response with \"[outside knowledge]: \" "+
			"to make clear this answer does not come from their saved notes.\n\nQuestion: %s",
		question,
	)
}

// Writes a .txt stub for raw CLI/hotkey text into ~/Engrex/RawText so the user can
// browse it as a file. RawText is a subfolder of the watched dir but is not watched
// itself, so these stubs are never re-ingested.
func cliTextStub(name string, content []byte) error {
	home, _ := os.UserHomeDir()
	path := filepath.Join(home, "Engrex", "RawText")

	err := os.MkdirAll(path, os.ModePerm)
	if err != nil {
		return err
	}
	path = filepath.Join(path, name)
	extension := filepath.Ext(path)
	base := strings.TrimSuffix(path, extension)
	_, err = os.Stat(path)

	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}

	for i := 1; err == nil; i++ {
		numberedPath := fmt.Sprintf("%s (%d)%s", base, i, extension)
		_, err = os.Stat(numberedPath)

		if errors.Is(err, fs.ErrNotExist) {
			path = numberedPath
			break
		}

		if err != nil {
			return err
		}

		if i >= 1000 {
			return errors.New("Too many duplicate files found, aborting creation")
		}
	}
	return os.WriteFile(path, content, 0644)

}
