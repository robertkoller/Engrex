package cache

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestJournalRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "entries.jsonl")
	journal, err := OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}

	stored := testEntry(1, "alpha", 1)
	stored.Payload = Payload{Text: "the answer"}
	stored.Semantic = "what is the answer?"
	if err := journal.Put(stored); err != nil {
		t.Fatal(err)
	}
	if err := journal.Put(testEntry(2, "beta", 2)); err != nil {
		t.Fatal(err)
	}
	if err := journal.Delete(2); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}

	entries, err := ReadJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("replayed %d entries, want 1", len(entries))
	}
	if entries[0].ID != 1 || entries[0].Payload.Text != "the answer" {
		t.Errorf("replayed the wrong entry: %+v", entries[0].ID)
	}
	if entries[0].Semantic != "what is the answer?" {
		t.Errorf("semantic = %q", entries[0].Semantic)
	}
	if len(entries[0].Embedding) != 768 {
		t.Fatalf("embedding came back with %d dimensions, want 768", len(entries[0].Embedding))
	}
	if cosineDistance(entries[0].Embedding, stored.Embedding) > 1e-6 {
		t.Error("the embedding did not survive the base64 round trip")
	}
}

// The process can be killed between a write and its flush. Losing the last cached
// response is fine; refusing to start is not.
func TestTornFinalLineIsDropped(t *testing.T) {
	path := filepath.Join(t.TempDir(), "entries.jsonl")
	journal, err := OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Put(testEntry(1, "alpha", 1)); err != nil {
		t.Fatal(err)
	}
	if err := journal.Close(); err != nil {
		t.Fatal(err)
	}

	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	file.WriteString(`{"op":"put","entry":{"id":2,"names`) //nolint:errcheck
	file.Close()                                           //nolint:errcheck

	entries, err := ReadJournal(path)
	if err != nil {
		t.Fatalf("a torn line should not fail the replay: %v", err)
	}
	if len(entries) != 1 {
		t.Errorf("replayed %d entries, want the 1 complete one", len(entries))
	}
}

func TestCompactDropsHistory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "entries.jsonl")
	journal, err := OpenJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	defer journal.Close() //nolint:errcheck

	for id := int64(1); id <= 10; id++ {
		if err := journal.Put(testEntry(id, "alpha", id)); err != nil {
			t.Fatal(err)
		}
	}
	for id := int64(1); id <= 8; id++ {
		if err := journal.Delete(id); err != nil {
			t.Fatal(err)
		}
	}

	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	live := []*Entry{testEntry(9, "alpha", 9), testEntry(10, "alpha", 10)}
	if err := journal.Compact(live); err != nil {
		t.Fatal(err)
	}

	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Size() >= before.Size() {
		t.Errorf("compaction grew the journal: %d -> %d", before.Size(), after.Size())
	}
	if journal.Records() != 2 {
		t.Errorf("Records = %d, want 2", journal.Records())
	}

	entries, err := ReadJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("replayed %d entries after compaction, want 2", len(entries))
	}

	// The journal has to stay usable afterwards — it is reopened, not abandoned.
	fresh := testEntry(11, "alpha", 11)
	fresh.CreatedAt = time.Now()
	if err := journal.Put(fresh); err != nil {
		t.Fatal(err)
	}
	entries, err = ReadJournal(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Errorf("replayed %d entries after appending post-compaction, want 3", len(entries))
	}
}
