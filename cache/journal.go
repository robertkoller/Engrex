package cache

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
)

// Journal is the cache's durable record: an append-only log of what was stored and what
// was dropped, replayed at startup to rebuild the index.
//
// A log rather than a rewritten file because writing is on the hot path — every cache
// miss ends with one — and appending a line is the cheapest durable thing available.
// It is compacted when replaying it gets more expensive than rewriting it.
type Journal struct {
	path string

	mutex  sync.Mutex
	file   *os.File
	writer *bufio.Writer

	// records counts what has been appended, live or not, which is what decides when
	// compaction is worth doing.
	records int
}

// record is one line of the journal.
type record struct {
	Op    string `json:"op"`
	Entry *Entry `json:"entry,omitempty"`
	ID    int64  `json:"id,omitempty"`
}

const (
	opPut    = "put"
	opDelete = "del"
)

// OpenJournal opens or creates the journal at path.
func OpenJournal(path string) (*Journal, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	return &Journal{path: path, file: file, writer: bufio.NewWriter(file)}, nil
}

// Put records a stored entry.
func (journal *Journal) Put(entry *Entry) error {
	return journal.append(record{Op: opPut, Entry: entry})
}

// Delete records an eviction.
func (journal *Journal) Delete(id int64) error {
	return journal.append(record{Op: opDelete, ID: id})
}

func (journal *Journal) append(item record) error {
	journal.mutex.Lock()
	defer journal.mutex.Unlock()

	line, err := json.Marshal(item)
	if err != nil {
		return err
	}
	if _, err := journal.writer.Write(append(line, '\n')); err != nil {
		return err
	}
	journal.records++
	return journal.writer.Flush()
}

// Records is how many lines have been appended since the journal was opened or
// compacted.
func (journal *Journal) Records() int {
	journal.mutex.Lock()
	defer journal.mutex.Unlock()
	return journal.records
}

// setRecords tells a freshly opened journal how many lines it already holds, which only a
// replay can count
func (journal *Journal) setRecords(records int) {
	journal.mutex.Lock()
	defer journal.mutex.Unlock()
	journal.records = records
}

// Close flushes and releases the file.
func (journal *Journal) Close() error {
	journal.mutex.Lock()
	defer journal.mutex.Unlock()

	if err := journal.writer.Flush(); err != nil {
		journal.file.Close() //nolint:errcheck
		return err
	}
	return journal.file.Close()
}

// ReadJournal replays a journal into the entries that are still live.
//
// A half-written final line is dropped rather than treated as an error. The process can
// be killed between the write and the flush, and losing the last cached response is not
// a reason to refuse to start.
func ReadJournal(path string) ([]*Entry, error) {
	entries, _, err := replayJournal(path)
	return entries, err
}

// replayJournal is ReadJournal plus how many records the file held, live or not, which is
// what decides whether it is due for compaction
func replayJournal(path string) ([]*Entry, int, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	defer file.Close() //nolint:errcheck

	// Entries carry a whole embedding, so a line is a few kilobytes — well past what
	// bufio.Scanner allows by default.
	reader := bufio.NewReader(file)
	live := make(map[int64]*Entry)
	var order []int64
	records := 0

	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 && err == nil {
			var item record
			if json.Unmarshal(line, &item) != nil {
				continue
			}
			records++
			switch item.Op {
			case opPut:
				if item.Entry == nil {
					continue
				}
				if _, seen := live[item.Entry.ID]; !seen {
					order = append(order, item.Entry.ID)
				}
				live[item.Entry.ID] = item.Entry
			case opDelete:
				delete(live, item.ID)
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, 0, err
		}
	}

	entries := make([]*Entry, 0, len(live))
	for _, id := range order {
		if entry, still := live[id]; still {
			entries = append(entries, entry)
		}
	}
	return entries, records, nil
}

// Compact rewrites the journal as one put per live entry, dropping the history of
// everything that has since been evicted or replaced.
//
// Written to a temporary file and renamed, so a crash mid-compaction leaves the old
// journal intact rather than a half-written one — the same approach internal/hnsw takes
// for its snapshots.
func (journal *Journal) Compact(entries []*Entry) error {
	journal.mutex.Lock()
	defer journal.mutex.Unlock()

	directory := filepath.Dir(journal.path)
	temporary, err := os.CreateTemp(directory, filepath.Base(journal.path)+".tmp-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath) //nolint:errcheck — a no-op once the rename succeeds

	writer := bufio.NewWriter(temporary)
	for _, entry := range entries {
		line, err := json.Marshal(record{Op: opPut, Entry: entry})
		if err != nil {
			temporary.Close() //nolint:errcheck
			return err
		}
		if _, err := writer.Write(append(line, '\n')); err != nil {
			temporary.Close() //nolint:errcheck
			return err
		}
	}
	if err := writer.Flush(); err != nil {
		temporary.Close() //nolint:errcheck
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}

	if err := journal.writer.Flush(); err != nil {
		return err
	}
	if err := journal.file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, journal.path); err != nil {
		return fmt.Errorf("replacing the journal: %w", err)
	}

	file, err := os.OpenFile(journal.path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	journal.file = file
	journal.writer = bufio.NewWriter(file)
	journal.records = len(entries)
	return nil
}
