package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/robertkoller/engrex/internal/db"
)

func testVector(seed float32) []float32 {
	vector := make([]float32, 768)
	for index := range vector {
		vector[index] = seed + float32(index)*0.0001
	}
	return vector
}

func newTestStore(t *testing.T) *Store {
	t.Helper()
	t.Setenv("HOME", t.TempDir())

	database, err := db.Open()
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return New(database)
}

func TestStitchChunksRemovesOverlap(t *testing.T) {

	chunks := []string{
		"Alpha sentence one. Beta sentence two. Gamma sentence three.",
		"Gamma sentence three. Delta sentence four.",
	}
	stitched := stitchChunks(chunks)

	if got := strings.Count(stitched, "Gamma sentence three."); got != 1 {
		t.Errorf("overlapping sentence appears %d times, want 1:\n%s", got, stitched)
	}
	if !strings.Contains(stitched, "Alpha sentence one.") || !strings.Contains(stitched, "Delta sentence four.") {
		t.Errorf("stitch dropped content: %s", stitched)
	}
}

func TestStitchChunksKeepsDisjointChunks(t *testing.T) {
	stitched := stitchChunks([]string{"First part.", "Wholly unrelated part."})
	if !strings.Contains(stitched, "First part.") || !strings.Contains(stitched, "Wholly unrelated part.") {
		t.Errorf("disjoint chunks were not both kept: %s", stitched)
	}
}

func TestDocumentFormat(t *testing.T) {
	cases := []struct {
		key, source, origin, want string
	}{
		{"/notes/todo.md", "/notes/todo.md", "", "md"},
		{"https://example.com/post", "A Blog Post", "https://example.com/post", "web"},
		{"chunk:7", "cli", "", "note"},
		{"/Users/x/Engrex/paper.PDF", "/Users/x/Engrex/paper.PDF", "", "pdf"},
	}
	for _, testCase := range cases {
		if got := documentFormat(testCase.key, testCase.source, testCase.origin); got != testCase.want {
			t.Errorf("documentFormat(%q) = %q, want %q", testCase.key, got, testCase.want)
		}
	}
}

func TestResolveAndFetchDocument(t *testing.T) {
	chunkStore := newTestStore(t)

	const source = "/tmp/engrex-test/notes.md"
	if err := chunkStore.InsertDocumentChunk("Kettle facts. The water boils.", source, "", testVector(0.10)); err != nil {
		t.Fatalf("insert first chunk: %v", err)
	}
	if err := chunkStore.InsertDocumentChunk("The water boils. Steam rises.", source, "", testVector(0.20)); err != nil {
		t.Fatalf("insert second chunk: %v", err)
	}
	if err := chunkStore.UpsertDocument(source, "deadbeef"); err != nil {
		t.Fatalf("UpsertDocument: %v", err)
	}

	for _, identifier := range []string{source, "notes.md", "1"} {
		key, err := chunkStore.ResolveDocumentKey(identifier)
		if err != nil {
			t.Fatalf("ResolveDocumentKey(%q): %v", identifier, err)
		}
		if key != source {
			t.Errorf("ResolveDocumentKey(%q) = %q, want %q", identifier, key, source)
		}
	}

	if _, err := chunkStore.ResolveDocumentKey("nothing-like-this"); !errors.Is(err, ErrDocumentNotFound) {
		t.Errorf("unknown identifier error = %v, want ErrDocumentNotFound", err)
	}

	document, err := chunkStore.DocumentByKey(source)
	if err != nil {
		t.Fatalf("DocumentByKey: %v", err)
	}
	if len(document.ChunkIDs) != 2 {
		t.Errorf("chunk count = %d, want 2", len(document.ChunkIDs))
	}
	if document.Hash != "deadbeef" {
		t.Errorf("ingestion hash = %q, want deadbeef", document.Hash)
	}
	if document.Format != "md" {
		t.Errorf("format = %q, want md", document.Format)
	}
	if got := strings.Count(document.Text, "The water boils."); got != 1 {
		t.Errorf("chunk overlap survived into document text (%d copies):\n%s", got, document.Text)
	}

	ordinals, err := chunkStore.ChunkOrdinals(source)
	if err != nil {
		t.Fatalf("ChunkOrdinals: %v", err)
	}
	if ordinals[document.ChunkIDs[0]] != 0 || ordinals[document.ChunkIDs[1]] != 1 {
		t.Errorf("ordinals = %v, want the chunks numbered 0 and 1 in order", ordinals)
	}
}

func TestDocumentByKeyMissing(t *testing.T) {
	chunkStore := newTestStore(t)
	if _, err := chunkStore.DocumentByKey("/no/such/file.md"); !errors.Is(err, ErrDocumentNotFound) {
		t.Errorf("DocumentByKey on empty store = %v, want ErrDocumentNotFound", err)
	}
}

func TestGraphNeighborhoodTrimsByDepth(t *testing.T) {
	chunkStore := newTestStore(t)

	for index, source := range []string{"/tmp/a.md", "/tmp/b.md", "/tmp/c.md"} {
		if err := chunkStore.InsertDocumentChunk("body "+source, source, "", testVector(float32(index)*0.5)); err != nil {
			t.Fatalf("insert %s: %v", source, err)
		}
	}
	graph, err := chunkStore.GraphData()
	if err != nil {
		t.Fatalf("GraphData: %v", err)
	}
	idByKey := map[string]int64{}
	for _, node := range graph.Nodes {
		idByKey[node.Key] = node.ID
	}
	if len(idByKey) != 3 {
		t.Fatalf("expected 3 graph nodes, got %d", len(idByKey))
	}

	if _, err := chunkStore.db.Exec(`DELETE FROM relations`); err != nil {
		t.Fatal(err)
	}
	link := func(from, to string) {
		t.Helper()
		if _, err := chunkStore.db.Exec(
			`INSERT INTO relations(source_id, target_id, distance) VALUES (?, ?, ?)`,
			idByKey[from], idByKey[to], 0.2); err != nil {
			t.Fatal(err)
		}
	}
	link("/tmp/a.md", "/tmp/b.md")
	link("/tmp/b.md", "/tmp/c.md")

	oneHop, rootID, err := chunkStore.GraphNeighborhood("/tmp/a.md", 1)
	if err != nil {
		t.Fatalf("GraphNeighborhood depth 1: %v", err)
	}
	if rootID != idByKey["/tmp/a.md"] {
		t.Errorf("root node = %d, want %d", rootID, idByKey["/tmp/a.md"])
	}
	if len(oneHop.Nodes) != 2 {
		t.Errorf("depth 1 returned %d nodes, want 2 (A and B)", len(oneHop.Nodes))
	}

	twoHops, _, err := chunkStore.GraphNeighborhood("/tmp/a.md", 2)
	if err != nil {
		t.Fatalf("GraphNeighborhood depth 2: %v", err)
	}
	if len(twoHops.Nodes) != 3 {
		t.Errorf("depth 2 returned %d nodes, want 3 (A, B and C)", len(twoHops.Nodes))
	}
	for _, node := range twoHops.Nodes {
		if node.Key == "/tmp/c.md" && node.Depth != 2 {
			t.Errorf("node C reported at depth %d, want 2", node.Depth)
		}
	}

	if _, _, err := chunkStore.GraphNeighborhood("/tmp/missing.md", 1); !errors.Is(err, ErrDocumentNotFound) {
		t.Errorf("neighborhood of unknown document = %v, want ErrDocumentNotFound", err)
	}
}
