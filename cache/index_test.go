package cache

import (
	"testing"
	"time"
)

func testEntry(id int64, namespace string, seed int64) *Entry {
	return &Entry{
		ID:        id,
		Namespace: namespace,
		Class:     ClassAnswer,
		Embedding: unitVector(seed, 768),
		CreatedAt: time.Now(),
	}
}

func TestNamespaceSearchIgnoresOtherNamespaces(t *testing.T) {
	index := NewIndex()
	wanted := testEntry(0, "alpha", 1)
	if err := index.Add(wanted); err != nil {
		t.Fatal(err)
	}
	if err := index.Add(testEntry(0, "beta", 2)); err != nil {
		t.Fatal(err)
	}

	match, found := index.SearchNamespace("alpha", wanted.Embedding, time.Now())
	if !found {
		t.Fatal("the entry in this namespace was not found")
	}
	if match.Entry.ID != wanted.ID {
		t.Errorf("matched id %d, want %d", match.Entry.ID, wanted.ID)
	}
	if match.Similarity < 0.999 {
		t.Errorf("a vector against itself scored %.4f", match.Similarity)
	}

	if _, found := index.SearchNamespace("gamma", wanted.Embedding, time.Now()); found {
		t.Error("an empty namespace returned a match")
	}
}

func TestExpiredEntriesAreNotReturned(t *testing.T) {
	index := NewIndex()
	entry := testEntry(0, "alpha", 1)
	entry.ExpiresAt = time.Now().Add(-time.Minute)
	if err := index.Add(entry); err != nil {
		t.Fatal(err)
	}

	if _, found := index.SearchNamespace("alpha", entry.Embedding, time.Now()); found {
		t.Error("an expired entry was served")
	}
}

func TestRemovedEntriesAreNotReturned(t *testing.T) {
	index := NewIndex()
	entry := testEntry(0, "alpha", 1)
	if err := index.Add(entry); err != nil {
		t.Fatal(err)
	}
	if !index.Remove(entry.ID) {
		t.Fatal("Remove reported nothing to remove")
	}

	if _, found := index.SearchNamespace("alpha", entry.Embedding, time.Now()); found {
		t.Error("a removed entry was still served from its namespace")
	}
	matches, err := index.SearchGlobal(entry.Embedding, ClassAnswer, 5, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) != 0 {
		t.Errorf("a removed entry was still reachable through the graph: %d matches", len(matches))
	}
	if index.Len() != 0 {
		t.Errorf("Len = %d, want 0", index.Len())
	}
}

// Eviction leaves dead nodes in the graph until enough pile up. The rebuild has to keep
// every live entry findable and drop every dead one.
func TestRebuildKeepsLiveEntriesAndClearsTheDead(t *testing.T) {
	index := NewIndex()
	kept := make([]*Entry, 0, 40)
	for seed := int64(1); seed <= 40; seed++ {
		entry := testEntry(0, "alpha", seed)
		if err := index.Add(entry); err != nil {
			t.Fatal(err)
		}
		kept = append(kept, entry)
	}

	// Half of them, which is past rebuildFraction, so this rebuilds along the way.
	for position := 0; position < 20; position++ {
		index.Remove(kept[position].ID)
	}
	index.Rebuild()

	if index.Len() != 20 {
		t.Fatalf("Len = %d, want 20", index.Len())
	}
	for _, entry := range kept[20:] {
		match, found := index.SearchGlobal(entry.Embedding, ClassAnswer, 1, time.Now())
		if found != nil {
			t.Fatal(found)
		}
		if len(match) == 0 || match[0].Entry.ID != entry.ID {
			t.Errorf("entry %d was not findable after the rebuild", entry.ID)
		}
	}
	for _, entry := range kept[:20] {
		if _, live := index.Get(entry.ID); live {
			t.Errorf("evicted entry %d is still live", entry.ID)
		}
	}
}

func TestAddCopiesTheEmbedding(t *testing.T) {
	index := NewIndex()
	entry := testEntry(0, "alpha", 1)
	original := make([]float32, len(entry.Embedding))
	copy(original, entry.Embedding)

	if err := index.Add(entry); err != nil {
		t.Fatal(err)
	}

	// A caller reusing its buffer must not be able to rewrite what is in the graph.
	for position := range entry.Embedding {
		entry.Embedding[position] = 0
	}
	matches, err := index.SearchGlobal(original, ClassAnswer, 1, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 || matches[0].Similarity < 0.999 {
		t.Error("mutating the caller's slice corrupted the indexed vector")
	}
}
