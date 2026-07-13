package rag

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/robertkoller/engrex/internal/db"
	"github.com/robertkoller/engrex/internal/store"
)

func TestToFTSQuery(t *testing.T) {
	if got := toFTSQuery("hello world"); got != `"hello" OR "world"` {
		t.Errorf("toFTSQuery(\"hello world\") = %q", got)
	}
	if got := toFTSQuery("   "); got != "" {
		t.Errorf("blank query = %q, want empty", got)
	}
	// FTS operators and quotes in the raw query must not leak through as syntax.
	nasty := toFTSQuery(`AND "quoted" -x`)
	if strings.Contains(nasty, " AND ") {
		t.Errorf("bare AND operator leaked through: %q", nasty)
	}
	if !strings.HasPrefix(nasty, `"`) {
		t.Errorf("terms not quoted: %q", nasty)
	}
}

func TestFuseRRF(t *testing.T) {
	// Chunk 2 is ranked by both searches, so it should fuse to the top.
	vectorHits := []store.Chunk{{ID: 1}, {ID: 2}, {ID: 3}}
	keywordHits := []store.Chunk{{ID: 5}, {ID: 2}, {ID: 4}}

	fused := fuseRRF(vectorHits, keywordHits, 10)
	if len(fused) != 5 {
		t.Fatalf("fused length = %d, want 5 (deduped union)", len(fused))
	}
	if fused[0].ID != 2 {
		t.Errorf("top result = %d, want 2 (the only chunk in both lists)", fused[0].ID)
	}

	// topK caps the output.
	if capped := fuseRRF(vectorHits, keywordHits, 2); len(capped) != 2 {
		t.Errorf("capped length = %d, want 2", len(capped))
	}

	// A chunk present only in the vector list keeps its vector copy (carrying Distance).
	vectorHits[0].Distance = 0.42
	fused = fuseRRF(vectorHits, nil, 10)
	for _, chunk := range fused {
		if chunk.ID == 1 && chunk.Distance != 0.42 {
			t.Errorf("vector chunk lost its Distance in fusion: %v", chunk.Distance)
		}
	}
}

// countContaining returns how many stored chunks contain the given substring.
func countContaining(t *testing.T, chunkStore *store.Store, substring string) int {
	t.Helper()
	chunks, err := chunkStore.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	count := 0
	for _, chunk := range chunks {
		if strings.Contains(chunk.Text, substring) {
			count++
		}
	}
	return count
}

// TestHybridSurfacesExactKeyword proves the point of hybrid search: an exact token (a
// random reference code) that vector search can't embed meaningfully is still retrieved,
// because BM25 matches it and rank fusion pulls it into the results. Requires Ollama.
func TestHybridSurfacesExactKeyword(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	database, err := db.Open()
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer database.Close()
	chunkStore := store.New(database)

	ragPipeline, err := New(chunkStore)
	if err != nil {
		t.Skipf("Ollama not reachable, skipping integration test: %v", err)
	}

	insert := func(text, source string) {
		t.Helper()
		vector, err := ragPipeline.embedder.Embed(text)
		if err != nil {
			t.Fatal(err)
		}
		if err := chunkStore.InsertDocumentChunk(text, source, "", vector); err != nil {
			t.Fatal(err)
		}
	}

	// The target: a distinctive code buried in otherwise off-topic prose.
	insert("The annual company picnic was held by the lake. Reference code ZBQ-7788 was recorded in the notes.", "/tmp/target.md")
	// Decoys that are semantically about codes/identifiers but lack the exact token.
	insert("Every record in the system is assigned a unique identifier for lookups.", "/tmp/d1.md")
	insert("Reference numbers and tracking codes help you find documents later.", "/tmp/d2.md")
	insert("A good naming scheme makes identifiers easy to remember and search.", "/tmp/d3.md")

	question := "ZBQ-7788"
	queryVec, err := ragPipeline.embedder.Embed(question)
	if err != nil {
		t.Fatal(err)
	}
	vectorHits, err := chunkStore.Search(queryVec, DefaultSearchDistance, hybridCandidates)
	if err != nil {
		t.Fatal(err)
	}
	keywordHits, err := chunkStore.KeywordSearch(toFTSQuery(question), hybridCandidates)
	if err != nil {
		t.Fatal(err)
	}
	fused := fuseRRF(vectorHits, keywordHits, DefaultSearchResults)

	contains := func(hits []store.Chunk) bool {
		for _, chunk := range hits {
			if strings.Contains(chunk.Text, "ZBQ-7788") {
				return true
			}
		}
		return false
	}

	if !contains(keywordHits) {
		t.Error("BM25 keyword search did not find the exact token")
	}
	if !contains(fused) {
		t.Error("hybrid fusion failed to surface the exact-keyword chunk")
	}
	if fused[0].Text != "" && !strings.Contains(fused[0].Text, "ZBQ-7788") {
		t.Logf("note: exact match not ranked #1 (rank fusion still included it); top was %q", fused[0].Text)
	}
}

// TestReingestSkipsUnchangedAndReplacesChanged drives the full rag.Add document path:
// a file re-ingested unchanged is skipped, and a file re-ingested with new content has
// its old chunks replaced rather than piling up. Requires Ollama for embeddings.
func TestReingestSkipsUnchangedAndReplacesChanged(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	database, err := db.Open()
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	defer database.Close()
	chunkStore := store.New(database)

	ragPipeline, err := New(chunkStore)
	if err != nil {
		t.Skipf("Ollama not reachable, skipping integration test: %v", err)
	}

	path := filepath.Join(t.TempDir(), "notes.md")
	textV1 := strings.Repeat("Go uses goroutines for lightweight concurrency. ", 200)
	if err := os.WriteFile(path, []byte(textV1), 0644); err != nil {
		t.Fatal(err)
	}

	// First ingest.
	if err := ragPipeline.Add(textV1, path, ""); err != nil {
		t.Fatalf("first Add: %v", err)
	}
	countV1 := countContaining(t, chunkStore, "goroutines")
	if countV1 == 0 {
		t.Fatal("expected chunks after first ingest")
	}
	allV1, _ := chunkStore.List()

	// Re-ingest identical content: should be skipped, leaving the store untouched.
	if err := ragPipeline.Add(textV1, path, ""); err != nil {
		t.Fatalf("unchanged Add: %v", err)
	}
	if allNow, _ := chunkStore.List(); len(allNow) != len(allV1) {
		t.Fatalf("unchanged re-ingest changed chunk count: %d -> %d", len(allV1), len(allNow))
	}

	// Re-ingest changed content for the same file: old chunks must be replaced.
	textV2 := strings.Repeat("Rust enforces memory safety through ownership and borrowing. ", 200)
	if err := os.WriteFile(path, []byte(textV2), 0644); err != nil {
		t.Fatal(err)
	}
	if err := ragPipeline.Add(textV2, path, ""); err != nil {
		t.Fatalf("changed Add: %v", err)
	}
	if stale := countContaining(t, chunkStore, "goroutines"); stale != 0 {
		t.Fatalf("stale V1 chunks remain after replace: %d", stale)
	}
	if fresh := countContaining(t, chunkStore, "ownership"); fresh == 0 {
		t.Fatal("expected V2 chunks after replace")
	}
}
