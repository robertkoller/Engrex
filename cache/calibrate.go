package cache

import (
	"fmt"
	"io"
	"sort"
	"strings"
)

// CalibrationPair is one labelled comparison: two prompts and whether the cache ought to
// treat them as the same question.
type CalibrationPair struct {
	First      string
	Second     string
	Similarity float64

	// SameQuestion is the ground truth. Paraphrases from the workload are true, two
	// different questions are false.
	SameQuestion bool
}

// CalibrationRow is what one candidate threshold would do against that ground truth.
type CalibrationRow struct {
	Threshold float64

	// Recall is the share of genuine paraphrases this threshold would serve from cache.
	Recall float64

	// FalseHits is the share of unrelated questions it would wrongly serve. This is the
	// number that matters: a false hit is a confidently wrong answer.
	FalseHits float64

	Precision float64
	Served    int
	Wrong     int
}

// Calibrate measures the two distributions that decide every threshold in this package:
// how similar two wordings of the same question are, and how similar two different
// questions are.
//
// It has to be measured rather than assumed, because the answer is a property of the
// embedding model and nothing else. The guide suggests starting at 0.95, which is sound
// for OpenAI's embeddings, where unrelated text already sits high. On nomic-embed-text,
// which is what Engrex runs, unrelated questions sit near 0.48 and genuine paraphrases
// near 0.73 — so a 0.95 threshold produces a cache that never fires, and the number has
// to come from the model in front of you.
func Calibrate(embedder Embedder, workload Workload) ([]CalibrationPair, error) {
	vectors := map[string][]float32{}
	embed := func(text string) ([]float32, error) {
		if vector, done := vectors[text]; done {
			return vector, nil
		}
		vector, err := embedder.EmbedQuery(text)
		if err != nil {
			return nil, err
		}
		vectors[text] = vector
		return vector, nil
	}

	var pairs []CalibrationPair
	compare := func(first, second string, same bool) error {
		firstVector, err := embed(first)
		if err != nil {
			return err
		}
		secondVector, err := embed(second)
		if err != nil {
			return err
		}
		pairs = append(pairs, CalibrationPair{
			First: first, Second: second, SameQuestion: same,
			Similarity: 1 - cosineDistance(firstVector, secondVector),
		})
		return nil
	}

	for _, question := range workload.Questions {
		for _, paraphrase := range question.Paraphrases {
			if err := compare(question.Question, paraphrase, true); err != nil {
				return nil, err
			}
		}
	}
	for first := range workload.Questions {
		for second := first + 1; second < len(workload.Questions); second++ {
			if err := compare(workload.Questions[first].Question, workload.Questions[second].Question, false); err != nil {
				return nil, err
			}
		}
	}
	return pairs, nil
}

// CalibrationSweep scores each candidate threshold against the labelled pairs.
func CalibrationSweep(pairs []CalibrationPair, thresholds []float64) []CalibrationRow {
	var sameTotal, differentTotal int
	for _, pair := range pairs {
		if pair.SameQuestion {
			sameTotal++
		} else {
			differentTotal++
		}
	}

	rows := make([]CalibrationRow, 0, len(thresholds))
	for _, threshold := range thresholds {
		row := CalibrationRow{Threshold: threshold}
		for _, pair := range pairs {
			if pair.Similarity < threshold {
				continue
			}
			row.Served++
			if pair.SameQuestion {
				continue
			}
			row.Wrong++
		}
		if sameTotal > 0 {
			row.Recall = float64(row.Served-row.Wrong) / float64(sameTotal)
		}
		if differentTotal > 0 {
			row.FalseHits = float64(row.Wrong) / float64(differentTotal)
		}
		if row.Served > 0 {
			row.Precision = float64(row.Served-row.Wrong) / float64(row.Served)
		}
		rows = append(rows, row)
	}
	return rows
}

// CalibrationThresholds spans the range the two distributions actually occupy. Wider and
// finer than the tuner's, because this is the sweep that decides where to start.
var CalibrationThresholds = []float64{
	0.50, 0.55, 0.60, 0.65, 0.70, 0.72, 0.75, 0.78, 0.80, 0.85, 0.90, 0.95, 0.97,
}

// RecommendThreshold picks the loosest threshold that still serves nothing wrong, with a
// safety margin above the highest unrelated pair seen.
//
// Loosest-that-is-safe rather than whatever maximizes some score: a false hit is a
// confidently wrong answer to a question the user did ask, and a miss is only slower.
// Those two are not worth trading one for one.
func RecommendThreshold(pairs []CalibrationPair) (threshold float64, recall float64) {
	var worstUnrelated float64
	var same []float64
	for _, pair := range pairs {
		if pair.SameQuestion {
			same = append(same, pair.Similarity)
			continue
		}
		if pair.Similarity > worstUnrelated {
			worstUnrelated = pair.Similarity
		}
	}
	if len(same) == 0 {
		return 0, 0
	}

	// The margin covers the fact that the unrelated sample is small, and the next
	// unrelated pair seen may sit above every one measured here.
	const margin = 0.05
	threshold = worstUnrelated + margin

	sort.Float64s(same)
	admitted := 0
	for _, similarity := range same {
		if similarity >= threshold {
			admitted++
		}
	}
	return threshold, float64(admitted) / float64(len(same))
}

// WriteCalibrationReport prints the sweep and says where to set the threshold.
func WriteCalibrationReport(out io.Writer, pairs []CalibrationPair, rows []CalibrationRow, current float64) {
	var same, different []float64
	for _, pair := range pairs {
		if pair.SameQuestion {
			same = append(same, pair.Similarity)
		} else {
			different = append(different, pair.Similarity)
		}
	}
	sort.Float64s(same)
	sort.Float64s(different)

	fmt.Fprintf(out, "\n%-34s %8s %8s %8s %8s\n", "DISTRIBUTION", "MIN", "MEDIAN", "MAX", "N")
	fmt.Fprintln(out, strings.Repeat("-", 70))
	writeDistribution(out, "same question, different words", same)
	writeDistribution(out, "different questions", different)

	fmt.Fprintf(out, "\n%-11s %10s %12s %11s   %s\n", "THRESHOLD", "RECALL", "FALSE HITS", "PRECISION", "")
	fmt.Fprintln(out, strings.Repeat("-", 76))
	for _, row := range rows {
		marker := "  "
		if row.Threshold == current {
			marker = "->"
		}
		fmt.Fprintf(out, "%s %-8.2f %9.1f%% %11.1f%% %10.1f%%   %s\n",
			marker, row.Threshold, row.Recall*100, row.FalseHits*100, row.Precision*100, bar(row.Recall))
	}

	writeCalibrationVerdict(out, pairs, same, different)
}

func writeDistribution(out io.Writer, name string, values []float64) {
	if len(values) == 0 {
		return
	}
	fmt.Fprintf(out, "%-34s %8.4f %8.4f %8.4f %8d\n",
		name, values[0], values[len(values)/2], values[len(values)-1], len(values))
}

func writeCalibrationVerdict(out io.Writer, pairs []CalibrationPair, same, different []float64) {
	fmt.Fprintln(out)
	if len(same) == 0 || len(different) == 0 {
		fmt.Fprintln(out, "Not enough labelled pairs to recommend anything.")
		return
	}

	threshold, recall := RecommendThreshold(pairs)
	worstSame, bestDifferent := same[0], different[len(different)-1]

	if worstSame > bestDifferent {
		fmt.Fprintf(out, "The two distributions separate cleanly: every paraphrase scores above\n")
		fmt.Fprintf(out, "%.4f and every different question below %.4f. Anything between them works.\n",
			worstSame, bestDifferent)
	} else {
		fmt.Fprintf(out, "The distributions overlap: the weakest paraphrase scores %.4f, below the\n", worstSame)
		fmt.Fprintf(out, "closest pair of different questions at %.4f. No threshold catches every\n", bestDifferent)
		fmt.Fprintf(out, "rewording without also serving something wrong, so this is a real trade\n")
		fmt.Fprintf(out, "and not a tuning failure.\n")
	}

	fmt.Fprintf(out, "\nRecommended: %.2f — the highest unrelated pair seen plus a margin. It serves\n", threshold)
	fmt.Fprintf(out, "%.0f%% of genuine rewordings from cache and, on this sample, nothing wrong.\n", recall*100)
	fmt.Fprintf(out, "Set it with `serve --threshold %.2f`.\n", threshold)
	fmt.Fprintf(out, "\nThresholds are a property of the embedding model. These numbers are for\n")
	fmt.Fprintf(out, "nomic-embed-text; a different model needs this run again.\n")
}
