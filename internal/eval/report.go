package eval

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"strings"
	"time"
)

// Baseline is a saved report, reduced to just the numbers worth comparing across runs.
type Baseline struct {
	RecallAtK   map[string]float64 `json:"recall_at_k"`
	MRR         float64            `json:"mrr"`
	CaseCount   int                `json:"case_count"`
	MedianMS    float64            `json:"median_latency_ms"`
	P95MS       float64            `json:"p95_latency_ms"`
	GeneratedAt time.Time          `json:"generated_at"`
	Label       string             `json:"label,omitempty"`
}

// ToBaseline reduces a report to its comparable numbers.
func (report Report) ToBaseline(label string) Baseline {
	recall := make(map[string]float64, len(report.RecallAtK))
	for k, value := range report.RecallAtK {
		recall[fmt.Sprintf("%d", k)] = value
	}
	return Baseline{
		RecallAtK:   recall,
		MRR:         report.MRR,
		CaseCount:   len(report.Results),
		MedianMS:    float64(report.LatencyPercentile(50).Microseconds()) / 1000,
		P95MS:       float64(report.LatencyPercentile(95).Microseconds()) / 1000,
		GeneratedAt: report.GeneratedAt,
		Label:       label,
	}
}

// SaveBaseline writes a baseline to disk for later runs to diff against.
func SaveBaseline(path string, baseline Baseline) error {
	contents, err := json.MarshalIndent(baseline, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(contents, '\n'), 0644)
}

// LoadBaseline reads a saved baseline. Reports whether one existed, so a first run
// with no baseline isn't treated as an error.
func LoadBaseline(path string) (Baseline, bool, error) {
	contents, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return Baseline{}, false, nil
	}
	if err != nil {
		return Baseline{}, false, err
	}

	var baseline Baseline
	if err := json.Unmarshal(contents, &baseline); err != nil {
		return Baseline{}, false, fmt.Errorf("parsing %s: %w", path, err)
	}
	return baseline, true, nil
}

// Write prints a human-readable report: the headline metrics, the single/multi-hop
// split, a delta against the baseline if there is one, and every miss.
func (report Report) Write(out io.Writer, baseline Baseline, hasBaseline bool) {
	fmt.Fprintf(out, "Retrieval eval — %d cases, %s\n\n",
		len(report.Results), report.GeneratedAt.Format("2006-01-02 15:04:05"))

	fmt.Fprintf(out, "%-12s %8s", "METRIC", "VALUE")
	if hasBaseline {
		fmt.Fprintf(out, " %10s %10s", "BASELINE", "DELTA")
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, strings.Repeat("-", 44))

	for _, k := range report.Ks {
		label := fmt.Sprintf("recall@%d", k)
		value := report.RecallAtK[k]
		fmt.Fprintf(out, "%-12s %8.3f", label, value)
		if hasBaseline {
			previous, found := baseline.RecallAtK[fmt.Sprintf("%d", k)]
			if found {
				fmt.Fprintf(out, " %10.3f %10s", previous, formatDelta(value-previous))
			} else {
				fmt.Fprintf(out, " %10s %10s", "-", "-")
			}
		}
		fmt.Fprintln(out)
	}

	fmt.Fprintf(out, "%-12s %8.3f", "MRR", report.MRR)
	if hasBaseline {
		fmt.Fprintf(out, " %10.3f %10s", baseline.MRR, formatDelta(report.MRR-baseline.MRR))
	}
	fmt.Fprintln(out)

	median := float64(report.LatencyPercentile(50).Microseconds()) / 1000
	p95 := float64(report.LatencyPercentile(95).Microseconds()) / 1000
	fmt.Fprintf(out, "%-12s %7.1fms", "latency p50", median)
	if hasBaseline {
		fmt.Fprintf(out, " %8.1fms %10s", baseline.MedianMS, formatDelta(median-baseline.MedianMS))
	}
	fmt.Fprintln(out)
	fmt.Fprintf(out, "%-12s %7.1fms", "latency p95", p95)
	if hasBaseline {
		fmt.Fprintf(out, " %8.1fms %10s", baseline.P95MS, formatDelta(p95-baseline.P95MS))
	}
	fmt.Fprintln(out)

	report.writeHopSplit(out)
	report.writeMisses(out)
}

// writeHopSplit reports single-hop and multi-hop cases separately. The gap between
// them is the evidence for or against multi-hop retrieval being worth its latency.
func (report Report) writeHopSplit(out io.Writer) {
	singleHop := report.Slice(func(testCase Case) bool { return testCase.Hops <= 1 })
	multiHop := report.Slice(func(testCase Case) bool { return testCase.Hops > 1 })
	if len(multiHop.Results) == 0 {
		return
	}

	fmt.Fprintf(out, "\n%-12s %6s %8s %8s\n", "SLICE", "CASES", "MRR", "recall@10")
	fmt.Fprintln(out, strings.Repeat("-", 38))
	fmt.Fprintf(out, "%-12s %6d %8.3f %8.3f\n",
		"single-hop", len(singleHop.Results), singleHop.MRR, singleHop.RecallAtK[10])
	fmt.Fprintf(out, "%-12s %6d %8.3f %8.3f\n",
		"multi-hop", len(multiHop.Results), multiHop.MRR, multiHop.RecallAtK[10])
}

// writeMisses lists the cases that retrieved nothing relevant. The aggregate says a
// change helped; this says which questions are still broken.
func (report Report) writeMisses(out io.Writer) {
	misses := report.Misses()
	if len(misses) == 0 {
		fmt.Fprintln(out, "\nNo complete misses.")
		return
	}

	fmt.Fprintf(out, "\nComplete misses (%d):\n", len(misses))
	for _, miss := range misses {
		fmt.Fprintf(out, "  %q\n", miss.Case.Question)
		fmt.Fprintf(out, "    want: %s\n", strings.Join(miss.Case.Relevant, ", "))
		fmt.Fprintf(out, "    got:  %s\n", summarize(miss.Retrieved, 3))
	}
}

// summarize renders the first few retrieved sources for a miss line.
func summarize(sources []string, limit int) string {
	if len(sources) == 0 {
		return "(nothing)"
	}
	shown := sources
	suffix := ""
	if len(shown) > limit {
		shown = shown[:limit]
		suffix = fmt.Sprintf(" (+%d more)", len(sources)-limit)
	}
	return strings.Join(shown, ", ") + suffix
}

// formatDelta renders a signed change, or a dash when it rounds to nothing.
func formatDelta(delta float64) string {
	if delta > -0.0005 && delta < 0.0005 {
		return "—"
	}
	return fmt.Sprintf("%+.3f", delta)
}
