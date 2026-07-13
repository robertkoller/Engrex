package store

import (
	"strings"
	"testing"

	"github.com/robertkoller/engrex/internal/db"
)

// testStore opens a throwaway database under a temp HOME so tests never touch the real
// ~/.engrex data.
func testStore(t *testing.T) *Store {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	database, err := db.Open()
	if err != nil {
		t.Fatalf("db.Open: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	return New(database)
}

func vec768(value float32) []float32 {
	vector := make([]float32, 768)
	for index := range vector {
		vector[index] = value
	}
	return vector
}

func countRows(t *testing.T, store *Store, query string) int {
	t.Helper()
	var count int
	if err := store.db.QueryRow(query).Scan(&count); err != nil {
		t.Fatalf("count %q: %v", query, err)
	}
	return count
}

func TestDocumentIdentity(t *testing.T) {
	cases := []struct {
		source, origin string
		wantKey        string
		wantReplace    bool
	}{
		{"/Users/x/Engrex/notes.md", "", "/Users/x/Engrex/notes.md", true},
		{"Some Page Title", "https://example.com/a", "https://example.com/a", true},
		{"https://example.com/b", "", "https://example.com/b", true},
		{"cli", "", "", false},
		{"hotkey", "", "", false},
	}
	for _, testCase := range cases {
		key, replaceable := DocumentIdentity(testCase.source, testCase.origin)
		if key != testCase.wantKey || replaceable != testCase.wantReplace {
			t.Errorf("DocumentIdentity(%q, %q) = (%q, %v), want (%q, %v)",
				testCase.source, testCase.origin, key, replaceable, testCase.wantKey, testCase.wantReplace)
		}
	}
}

func TestDeleteBySourceRemovesChunksVecsAndRelations(t *testing.T) {
	store := testStore(t)
	path := "/tmp/Engrex/doc.md"

	// Two chunks of the same file; identical vectors so relate() links them.
	if err := store.InsertDocumentChunk("chunk one", path, "", vec768(0.5)); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertDocumentChunk("chunk two", path, "", vec768(0.5)); err != nil {
		t.Fatal(err)
	}

	if got := countRows(t, store, "SELECT COUNT(*) FROM chunks"); got != 2 {
		t.Fatalf("chunks = %d, want 2", got)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM relations"); got == 0 {
		t.Fatal("relations = 0, want > 0 (relate should have linked the two chunks)")
	}

	removed, err := store.DeleteBySource(path, "")
	if err != nil {
		t.Fatal(err)
	}
	if removed != 2 {
		t.Fatalf("DeleteBySource removed %d, want 2", removed)
	}
	for _, check := range []struct {
		name, query string
	}{
		{"chunks", "SELECT COUNT(*) FROM chunks"},
		{"vec_chunks", "SELECT COUNT(*) FROM vec_chunks"},
		{"relations", "SELECT COUNT(*) FROM relations"},
	} {
		if got := countRows(t, store, check.query); got != 0 {
			t.Errorf("%s after delete = %d, want 0", check.name, got)
		}
	}
}

func TestDeleteBySourceScopesToOneDocument(t *testing.T) {
	store := testStore(t)
	if err := store.InsertDocumentChunk("page a", "Title A", "https://a.test", vec768(0.3)); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertDocumentChunk("page b", "Title B", "https://b.test", vec768(0.9)); err != nil {
		t.Fatal(err)
	}

	removed, err := store.DeleteBySource("Title A", "https://a.test")
	if err != nil {
		t.Fatal(err)
	}
	if removed != 1 {
		t.Fatalf("removed %d, want 1", removed)
	}
	if got := countRows(t, store, "SELECT COUNT(*) FROM chunks"); got != 1 {
		t.Fatalf("chunks = %d, want 1 (the other document must survive)", got)
	}
}

func TestKeywordSearchFindsByExactTerm(t *testing.T) {
	store := testStore(t)

	// A chunk whose distinctive keyword ("Kubernetes") a semantic search might rank low.
	if err := store.InsertDocumentChunk(
		"Kubernetes handles container orchestration across a cluster of machines.",
		"/tmp/k8s.md", "", vec768(0.5)); err != nil {
		t.Fatal(err)
	}
	if err := store.InsertDocumentChunk(
		"The weather this afternoon is sunny and pleasantly warm outside.",
		"/tmp/weather.md", "", vec768(0.2)); err != nil {
		t.Fatal(err)
	}

	// The INSERT triggers should have populated the FTS index automatically.
	hits, err := store.KeywordSearch(`"Kubernetes"`, 10)
	if err != nil {
		t.Fatalf("KeywordSearch: %v", err)
	}
	if len(hits) != 1 {
		t.Fatalf("KeywordSearch returned %d hits, want 1 (FTS triggers may not be firing)", len(hits))
	}
	if !strings.Contains(hits[0].Text, "Kubernetes") {
		t.Fatalf("wrong chunk returned: %q", hits[0].Text)
	}
}

func TestDeleteBySourceUpdatesFTS(t *testing.T) {
	store := testStore(t)
	path := "/tmp/doc.md"
	if err := store.InsertDocumentChunk("uniquetokenalpha appears here in the text", path, "", vec768(0.4)); err != nil {
		t.Fatal(err)
	}
	if hits, _ := store.KeywordSearch(`"uniquetokenalpha"`, 10); len(hits) != 1 {
		t.Fatalf("before delete: %d hits, want 1", len(hits))
	}
	if _, err := store.DeleteBySource(path, ""); err != nil {
		t.Fatal(err)
	}
	// The delete trigger should have removed the row from the FTS index too.
	if hits, _ := store.KeywordSearch(`"uniquetokenalpha"`, 10); len(hits) != 0 {
		t.Fatalf("after delete: %d hits, want 0 (FTS not kept in sync on delete)", len(hits))
	}
}

func TestDocumentHashRoundtrip(t *testing.T) {
	store := testStore(t)

	if _, seen, err := store.DocumentHash("k"); err != nil || seen {
		t.Fatalf("fresh key: seen=%v err=%v, want seen=false", seen, err)
	}
	if err := store.UpsertDocument("k", "hash1"); err != nil {
		t.Fatal(err)
	}
	got, seen, err := store.DocumentHash("k")
	if err != nil || !seen || got != "hash1" {
		t.Fatalf("got (%q, %v, %v), want (hash1, true, nil)", got, seen, err)
	}
	if err := store.UpsertDocument("k", "hash2"); err != nil {
		t.Fatal(err)
	}
	if got, _, _ := store.DocumentHash("k"); got != "hash2" {
		t.Fatalf("after update got %q, want hash2", got)
	}
}
