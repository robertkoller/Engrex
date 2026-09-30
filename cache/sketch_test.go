package cache

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

// The overlap guard sits at 0.65 between one swapped passage (about 0.67) and two (about
// 0.43), so the estimate has to be close to the true Jaccard. 64 independent hashes give
// a standard error near 0.06 and this holds the sketch to that
func TestSketchEstimatesJaccardAccurately(t *testing.T) {
	const words = 1000
	for _, shared := range []int{200, 500, 800} {
		var squaredError float64
		const trials = 200
		for trial := range trials {
			var first, second []string
			for position := range words {
				if position < shared {
					first = append(first, fmt.Sprintf("s%d_%d", trial, position))
					second = append(second, fmt.Sprintf("s%d_%d", trial, position))
					continue
				}
				first = append(first, fmt.Sprintf("a%d_%d", trial, position))
				second = append(second, fmt.Sprintf("b%d_%d", trial, position))
			}

			// Only shingles lying wholly inside the shared prefix are common to both
			sharedShingles := float64(shared - shingleWords + 1)
			perSide := float64(words - shingleWords + 1)
			truth := sharedShingles / (2*perSide - sharedShingles)

			estimate := Overlap(Sketch(strings.Join(first, " ")), Sketch(strings.Join(second, " ")))
			squaredError += (estimate - truth) * (estimate - truth)
		}
		rootMeanSquare := math.Sqrt(squaredError / trials)
		if rootMeanSquare > 0.08 {
			t.Errorf("with %d of %d words shared, the overlap estimate is off by %.3f on average, want under 0.08", shared, words, rootMeanSquare)
		}
	}
}
