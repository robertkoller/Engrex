package cache

import (
	"hash/fnv"
	"math"
	"strings"
)

// sketchSize is how many minimum-hashes make up a sketch. 64 estimates Jaccard to within
// roughly 1/sqrt(64), about 12%, which is fine for a guard that only has to tell "mostly
// the same passages" from "different passages".
const sketchSize = 64

// shingleWords is how many consecutive words make one shingle. Long enough that ordinary
// prose does not collide by accident, short enough that changing one passage out of five
// still leaves most shingles intact.
const shingleWords = 5

// Sketch is a MinHash of a context block, used to ask how much two prompts' retrieved
// passages overlap without storing the passages twice.
//
// This exists for the context-tolerant lookup. Retrieval is itself a function of the
// question, so rewording a question usually changes which passages come back — often by
// one out of five. That changes the context hash and guarantees a miss, in exactly the
// case a semantic cache is supposed to catch. Comparing sketches is what lets a hit
// survive a small change in the retrieved set while still refusing a large one.
func Sketch(context string) []uint64 {
	words := strings.Fields(context)
	if len(words) < shingleWords {
		return nil
	}

	sketch := make([]uint64, sketchSize)
	for position := range sketch {
		sketch[position] = math.MaxUint64
	}

	for start := 0; start+shingleWords <= len(words); start++ {
		shingle := strings.Join(words[start:start+shingleWords], " ")
		base := fnv64(shingle)
		for position := range sketch {
			// Each position reseeds the shingle's hash through a full avalanche mix. The
			// earlier base*(2p+1)+p kept the minimums of different positions correlated,
			// which measured at about twice the error 64 independent hashes should give
			permuted := mix64(base ^ sketchSeeds[position])
			if permuted < sketch[position] {
				sketch[position] = permuted
			}
		}
	}
	return sketch
}

// Overlap estimates the Jaccard similarity of the two contexts the sketches came from.
func Overlap(first, second []uint64) float64 {
	if len(first) == 0 || len(first) != len(second) {
		return 0
	}
	equal := 0
	for position := range first {
		if first[position] == second[position] {
			equal++
		}
	}
	return float64(equal) / float64(len(first))
}

// sketchSeeds gives each sketch position its own hash function. Derived rather than
// random so a sketch written to the journal still compares correctly after a restart
var sketchSeeds = func() [sketchSize]uint64 {
	var seeds [sketchSize]uint64
	for position := range seeds {
		seeds[position] = mix64(uint64(position+1) * 0x9e3779b97f4a7c15)
	}
	return seeds
}()

// mix64 is the splitmix64 finalizer, where every input bit flips about half the output bits
func mix64(value uint64) uint64 {
	value ^= value >> 30
	value *= 0xbf58476d1ce4e5b9
	value ^= value >> 27
	value *= 0x94d049bb133111eb
	value ^= value >> 31
	return value
}

func fnv64(text string) uint64 {
	hash := fnv.New64a()
	hash.Write([]byte(text)) //nolint:errcheck — hash.Write never fails
	return hash.Sum64()
}
