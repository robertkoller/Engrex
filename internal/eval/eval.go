// Package eval measures retrieval quality against a hand-written golden set.
//
// Every threshold in Engrex was originally set by feel. This package exists so that
// stops being true: it turns "does this change help?" into a number, and it is what
// every later retrieval change gets justified against.
package eval

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"
)

// Case is one golden-set entry: a question, and the sources that genuinely answer it.
type Case struct {
	// Question is asked verbatim, exactly as a user would type it.
	Question string `json:"question"`

	// Relevant lists substrings of the source path or origin URL of chunks that count
	// as correct hits — "sqlite-vec.md" matches any chunk from that file. Substrings
	// rather than exact paths so a golden set survives files being moved.
	Relevant []string `json:"relevant"`

	// Hops marks a question as needing evidence from more than one document. Lets the
	// report separate single-hop from multi-hop performance, which is the comparison
	// that justifies (or sinks) agentic retrieval later.
	Hops int `json:"hops,omitempty"`

	// Note is free text for why the case is in the set. Never used in scoring.
	Note string `json:"note,omitempty"`
}

// Set is a golden set loaded from disk.
type Set struct {
	Cases []Case `json:"cases"`
}

// Result is one case's outcome.
type Result struct {
	Case Case

	// Retrieved is the ordered list of source identifiers the retriever returned.
	Retrieved []string

	// FirstRelevantRank is the 1-based rank of the first correct hit, or 0 if none was
	// retrieved at all.
	FirstRelevantRank int

	// HitsAtK[k] is how many distinct relevant sources appeared in the top k.
	HitsAtK map[int]int

	Latency time.Duration
}

// Report aggregates results across a whole set.
type Report struct {
	Results []Result
	Ks      []int

	RecallAtK   map[int]float64
	MRR         float64
	Latencies   []time.Duration
	GeneratedAt time.Time
}

// DefaultKs are the cutoffs reported. 1 and 3 show whether the right answer is at the
// very top (what matters without a reranker); 10 and 20 show whether it is anywhere in
// the pool a reranker could later pull up.
var DefaultKs = []int{1, 3, 5, 10, 20}

// Load reads a golden set from a JSON file.
func Load(path string) (Set, error) {
	contents, err := os.ReadFile(path)
	if err != nil {
		return Set{}, err
	}

	var set Set
	if err := json.Unmarshal(contents, &set); err != nil {
		return Set{}, fmt.Errorf("parsing %s: %w", path, err)
	}
	if len(set.Cases) == 0 {
		return Set{}, fmt.Errorf("%s contains no cases", path)
	}
	for index, testCase := range set.Cases {
		if strings.TrimSpace(testCase.Question) == "" {
			return Set{}, fmt.Errorf("case %d has an empty question", index)
		}
		if len(testCase.Relevant) == 0 {
			return Set{}, fmt.Errorf("case %d (%q) lists no relevant sources", index, testCase.Question)
		}
	}
	return set, nil
}

// Retriever is the retrieval path under test. Returning source identifiers rather than
// chunks keeps the harness independent of how retrieval is implemented, so the same
// golden set can score the current pipeline, a reranked one, or an agentic one.
type Retriever func(question string, topK int) ([]string, error)

// Run scores every case in the set, retrieving max(ks) results per question.
func Run(set Set, retrieve Retriever, ks []int) (Report, error) {
	if len(ks) == 0 {
		ks = DefaultKs
	}
	sorted := append([]int(nil), ks...)
	sort.Ints(sorted)
	maxK := sorted[len(sorted)-1]

	report := Report{
		Ks:          sorted,
		RecallAtK:   make(map[int]float64),
		GeneratedAt: time.Now(),
	}

	reciprocalRankTotal := 0.0
	recallTotals := make(map[int]float64)

	for _, testCase := range set.Cases {
		start := time.Now()
		retrieved, err := retrieve(testCase.Question, maxK)
		latency := time.Since(start)
		if err != nil {
			return Report{}, fmt.Errorf("retrieving %q: %w", testCase.Question, err)
		}

		result := Result{
			Case:      testCase,
			Retrieved: retrieved,
			HitsAtK:   make(map[int]int),
			Latency:   latency,
		}

		for rank, source := range retrieved {
			if matches(source, testCase.Relevant) && result.FirstRelevantRank == 0 {
				result.FirstRelevantRank = rank + 1
			}
		}
		if result.FirstRelevantRank > 0 {
			reciprocalRankTotal += 1.0 / float64(result.FirstRelevantRank)
		}

		// Recall counts distinct expected sources found, so a case with three relevant
		// documents isn't scored as solved by retrieving the same one three times.
		for _, k := range sorted {
			found := map[string]bool{}
			for rank, source := range retrieved {
				if rank >= k {
					break
				}
				for _, want := range testCase.Relevant {
					if strings.Contains(source, want) {
						found[want] = true
					}
				}
			}
			result.HitsAtK[k] = len(found)
			recallTotals[k] += float64(len(found)) / float64(len(testCase.Relevant))
		}

		report.Results = append(report.Results, result)
		report.Latencies = append(report.Latencies, latency)
	}

	caseCount := float64(len(set.Cases))
	report.MRR = reciprocalRankTotal / caseCount
	for _, k := range sorted {
		report.RecallAtK[k] = recallTotals[k] / caseCount
	}
	return report, nil
}

// matches reports whether a retrieved source identifier satisfies any expected entry.
func matches(source string, relevant []string) bool {
	for _, want := range relevant {
		if strings.Contains(source, want) {
			return true
		}
	}
	return false
}

// LatencyPercentile returns the given percentile (0-100) of measured query latency.
func (report Report) LatencyPercentile(percentile float64) time.Duration {
	if len(report.Latencies) == 0 {
		return 0
	}
	sorted := append([]time.Duration(nil), report.Latencies...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	index := int(percentile / 100 * float64(len(sorted)-1))
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return sorted[index]
}

// Misses returns the cases where nothing relevant was retrieved at all. These are the
// ones worth reading — an aggregate score says a change helped, the miss list says why.
func (report Report) Misses() []Result {
	var misses []Result
	for _, result := range report.Results {
		if result.FirstRelevantRank == 0 {
			misses = append(misses, result)
		}
	}
	return misses
}

// Slice returns a sub-report over only the cases matching a predicate, so single-hop
// and multi-hop performance can be reported separately.
func (report Report) Slice(include func(Case) bool) Report {
	var subset Set
	byQuestion := map[string]Result{}
	for _, result := range report.Results {
		if include(result.Case) {
			subset.Cases = append(subset.Cases, result.Case)
			byQuestion[result.Case.Question] = result
		}
	}
	if len(subset.Cases) == 0 {
		return Report{Ks: report.Ks, RecallAtK: map[int]float64{}}
	}

	// Rescoring from the stored results rather than re-retrieving keeps a slice free
	// and guarantees it reflects exactly the same run as the parent report.
	sliced := Report{
		Ks:          report.Ks,
		RecallAtK:   make(map[int]float64),
		GeneratedAt: report.GeneratedAt,
	}
	reciprocalRankTotal := 0.0
	recallTotals := make(map[int]float64)
	for _, testCase := range subset.Cases {
		result := byQuestion[testCase.Question]
		if result.FirstRelevantRank > 0 {
			reciprocalRankTotal += 1.0 / float64(result.FirstRelevantRank)
		}
		for _, k := range report.Ks {
			recallTotals[k] += float64(result.HitsAtK[k]) / float64(len(testCase.Relevant))
		}
		sliced.Results = append(sliced.Results, result)
		sliced.Latencies = append(sliced.Latencies, result.Latency)
	}

	caseCount := float64(len(subset.Cases))
	sliced.MRR = reciprocalRankTotal / caseCount
	for _, k := range report.Ks {
		sliced.RecallAtK[k] = recallTotals[k] / caseCount
	}
	return sliced
}
