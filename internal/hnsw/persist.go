package hnsw

import (
	"encoding/gob"
	"fmt"
	"io"
	"math"
	"math/rand"
	"os"
	"path/filepath"
)

// formatVersion guards against loading a file written by an incompatible layout. Bump
// it whenever the serialized shape changes; an older file is then rejected outright
// rather than decoded into something subtly wrong.
const formatVersion = 1

// snapshot is the on-disk form. The graph is stored as plain slices rather than the
// live pointer structure, since gob cannot round-trip the cyclic references that a
// proximity graph is made of.
type snapshot struct {
	Version    int
	Params     Params
	Dimensions int
	EntryPoint int64
	MaxLayer   int
	Nodes      []nodeSnapshot
}

type nodeSnapshot struct {
	ID        int64
	Vector    []float32
	Neighbors [][]int64
}

// Save writes the index to a file, atomically. The index is written to a temporary
// file in the same directory and renamed over the target, so a crash mid-write leaves
// the previous index intact instead of a truncated one.
func (index *Index) Save(path string) error {
	index.mutex.RLock()
	defer index.mutex.RUnlock()

	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}

	temporary, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath) // no-op once the rename succeeds

	if err := index.encode(temporary); err != nil {
		temporary.Close() //nolint:errcheck
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, path)
}

func (index *Index) encode(writer io.Writer) error {
	stored := snapshot{
		Version:    formatVersion,
		Params:     index.params,
		Dimensions: index.dimensions,
		EntryPoint: index.entryPoint,
		MaxLayer:   index.maxLayer,
		Nodes:      make([]nodeSnapshot, 0, len(index.nodes)),
	}
	for _, item := range index.nodes {
		stored.Nodes = append(stored.Nodes, nodeSnapshot{
			ID:        item.id,
			Vector:    item.vector,
			Neighbors: item.neighbors,
		})
	}
	return gob.NewEncoder(writer).Encode(stored)
}

// Load reads an index previously written by Save.
func Load(path string) (*Index, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	var stored snapshot
	if err := gob.NewDecoder(file).Decode(&stored); err != nil {
		return nil, fmt.Errorf("decoding %s: %w", path, err)
	}
	if stored.Version != formatVersion {
		return nil, fmt.Errorf("index %s is format version %d, expected %d — rebuild it",
			path, stored.Version, formatVersion)
	}

	index := &Index{
		params:          stored.Params,
		random:          rand.New(rand.NewSource(stored.Params.Seed)),
		nodes:           make(map[int64]*node, len(stored.Nodes)),
		entryPoint:      stored.EntryPoint,
		maxLayer:        stored.MaxLayer,
		levelMultiplier: 1 / math.Log(float64(stored.Params.M)),
		dimensions:      stored.Dimensions,
	}
	for _, item := range stored.Nodes {
		index.nodes[item.ID] = &node{
			id:        item.ID,
			vector:    item.Vector,
			neighbors: item.Neighbors,
		}
	}
	return index, nil
}
