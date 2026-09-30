package cache

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// LookupRecord is one line of the lookup log: what was asked, and how close the cache
// came to already having it.
type LookupRecord struct {
	At    time.Time `json:"at"`
	Class Class     `json:"class"`
	Model string    `json:"model"`

	// Semantic is the question as it was matched. Kept so the tuner can show which
	// questions a looser threshold would start serving from cache, rather than only how
	// many.
	Semantic string `json:"semantic"`

	Status Status `json:"status"`
	Tier   Tier   `json:"tier,omitempty"`

	// Similarity is the best score the lookup saw. On a miss this is the near-miss
	// number: the whole point of the log is that a miss at 0.96 against a 0.97 threshold
	// is a hit that a slightly looser setting would have caught.
	Similarity float64 `json:"similarity"`
	Threshold  float64 `json:"threshold"`
}

// LookupLog appends a record for every lookup, so thresholds can be chosen from what
// actually happened rather than from taste.
type LookupLog struct {
	mutex  sync.Mutex
	file   *os.File
	writer *bufio.Writer
}

func OpenLookupLog(path string) (*LookupLog, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		return nil, err
	}
	return &LookupLog{file: file, writer: bufio.NewWriter(file)}, nil
}

func (log *LookupLog) RecordLookup(result Result) {
	if log == nil || result.Status == StatusBypass {
		return
	}
	record := LookupRecord{
		At:         time.Now().UTC(),
		Class:      result.Class,
		Model:      result.Request.Model,
		Semantic:   result.Parts.Semantic,
		Status:     result.Status,
		Tier:       result.Tier,
		Similarity: result.Similarity,
		Threshold:  result.Threshold,
	}

	log.mutex.Lock()
	defer log.mutex.Unlock()
	line, err := json.Marshal(record)
	if err != nil {
		return
	}
	log.writer.Write(append(line, '\n')) //nolint:errcheck — logging must never fail a request
	log.writer.Flush()                   //nolint:errcheck
}

func (log *LookupLog) RecordStore(*Entry) {}

func (log *LookupLog) RecordEviction(int) {}

func (log *LookupLog) Close() error {
	if log == nil {
		return nil
	}
	log.mutex.Lock()
	defer log.mutex.Unlock()
	log.writer.Flush() //nolint:errcheck
	return log.file.Close()
}

// ReadLookupLog replays a lookup log.
func ReadLookupLog(path string) ([]LookupRecord, error) {
	file, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer file.Close() //nolint:errcheck

	var records []LookupRecord
	reader := bufio.NewReader(file)
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 && err == nil {
			var record LookupRecord
			if json.Unmarshal(line, &record) == nil {
				records = append(records, record)
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, err
		}
	}
	return records, nil
}

// ThresholdRow is what one candidate threshold would have done.
type ThresholdRow struct {
	Threshold float64

	// Hits counts lookups that would have been served from cache at this threshold —
	// the ones that really hit, plus the near-misses this setting would have admitted.
	Hits int

	// Total is every lookup that could have hit, so the rate is comparable across rows.
	Total int

	// Additional is how many of the hits are ones the current threshold refused. These
	// are the entries whose correctness is in question, and the only ones worth paying a
	// model call to check.
	Additional int

	HitRate float64
}

// SweepThresholds replays the log at a range of thresholds and reports what each would
// have done.
//
// The hit rate side of the guide's trade-off curve comes free: the log already records
// how close every lookup came, so what a different threshold would have done is
// arithmetic, not another run. The accuracy side is not free, which is what Divergence
// below is for.
func SweepThresholds(records []LookupRecord, class Class, thresholds []float64) []ThresholdRow {
	relevant := make([]LookupRecord, 0, len(records))
	for _, record := range records {
		if class == "" || record.Class == class {
			relevant = append(relevant, record)
		}
	}

	rows := make([]ThresholdRow, 0, len(thresholds))
	for _, threshold := range thresholds {
		row := ThresholdRow{Threshold: threshold, Total: len(relevant)}
		for _, record := range relevant {
			switch {
			case record.Status == StatusHit && record.Tier == TierExact:
				// An exact repeat hits at any threshold; it never needed similarity.
				row.Hits++
			case record.Similarity >= threshold:
				row.Hits++
				if record.Status != StatusHit {
					row.Additional++
				}
			}
		}
		if row.Total > 0 {
			row.HitRate = float64(row.Hits) / float64(row.Total)
		}
		rows = append(rows, row)
	}
	return rows
}

// DefaultThresholds is the range worth sweeping.
//
// It starts at 0.55 rather than somewhere comfortable because that is where the numbers
// actually live. On nomic-embed-text two wordings of one question score 0.73 at the
// median and two different questions 0.48, so a sweep that began at 0.85 would show a
// flat line and explain nothing. See `engrex-cache calibrate`.
var DefaultThresholds = []float64{
	0.55, 0.60, 0.65, 0.70, 0.75, 0.80, 0.85, 0.90, 0.95, 0.97,
}

// WriteThresholdReport prints the sweep in the same shape as the rest of this project's
// reports — see internal/hnsw/bench.go and internal/eval/report.go.
func WriteThresholdReport(out io.Writer, rows []ThresholdRow, current float64) {
	rows = withCurrent(rows, current)

	fmt.Fprintf(out, "\n%-11s %8s %8s %11s %10s   %s\n",
		"THRESHOLD", "HITS", "TOTAL", "HIT RATE", "NEW HITS", "")
	fmt.Fprintln(out, strings.Repeat("-", 74))

	for _, row := range rows {
		marker := "  "
		if row.Threshold == current {
			marker = "->"
		}
		fmt.Fprintf(out, "%s %-8.2f %8d %8d %10.1f%% %10d   %s\n",
			marker, row.Threshold, row.Hits, row.Total, row.HitRate*100, row.Additional,
			bar(row.HitRate))
	}
	writeThresholdVerdict(out, rows, current)
}

// withCurrent makes sure the threshold actually in force appears in the table. Without
// it, a configured value that is not one of the swept constants shows up nowhere, the
// arrow marking "you are here" never prints, and the verdict has no row to compare
// against — which reads as "no data" when there is plenty.
func withCurrent(rows []ThresholdRow, current float64) []ThresholdRow {
	for _, row := range rows {
		if row.Threshold == current {
			return rows
		}
	}
	if len(rows) == 0 {
		return rows
	}

	// Recomputed the same way SweepThresholds did, from the row totals it already has.
	added := append([]ThresholdRow(nil), rows...)
	nearest := added[0]
	for _, row := range added {
		if row.Threshold <= current && row.Threshold > nearest.Threshold {
			nearest = row
		}
	}
	nearest.Threshold = current
	added = append(added, nearest)
	sort.Slice(added, func(first, second int) bool {
		return added[first].Threshold < added[second].Threshold
	})
	return added
}

// bar draws a proportion, the same way internal/hnsw/bench.go does.
func bar(fraction float64) string {
	const width = 20
	filled := int(fraction*width + 0.5)
	if filled > width {
		filled = width
	}
	return strings.Repeat("█", filled) + strings.Repeat("░", width-filled)
}

// writeThresholdVerdict says what the numbers mean, because a table of rates does not
// answer the question the person running it actually has.
func writeThresholdVerdict(out io.Writer, rows []ThresholdRow, current float64) { //nolint:gocritic
	if len(rows) == 0 {
		return
	}

	sorted := append([]ThresholdRow(nil), rows...)
	sort.Slice(sorted, func(first, second int) bool { return sorted[first].Threshold < sorted[second].Threshold })

	var currentRow ThresholdRow
	for _, row := range sorted {
		if row.Threshold == current {
			currentRow = row
		}
	}
	loosest := sorted[0]

	fmt.Fprintln(out)
	if currentRow.Total == 0 {
		// The configured threshold was not one of the swept values, so fall back to the
		// tightest row rather than claiming there is no data.
		currentRow = sorted[len(sorted)-1]
		current = currentRow.Threshold
	}
	if currentRow.Total == 0 {
		fmt.Fprintf(out, "No lookups recorded for this class yet. Run some queries first.\n")
		return
	}

	gained := loosest.Hits - currentRow.Hits
	fmt.Fprintf(out, "At the current %.2f, %.1f%% of lookups are served from cache.\n",
		current, currentRow.HitRate*100)
	if gained <= 0 {
		fmt.Fprintf(out, "Loosening to %.2f would gain nothing — every miss is a genuinely different question.\n",
			loosest.Threshold)
		return
	}
	fmt.Fprintf(out, "Loosening to %.2f would serve %d more (%.1f%%), each of them an answer\n",
		loosest.Threshold, gained, loosest.HitRate*100)
	fmt.Fprintf(out, "written for a differently worded question.\n\n")
	fmt.Fprintf(out, "Whether those would have been right is a separate question, and this log\n")
	fmt.Fprintf(out, "cannot answer it — it records how close things were, not what was correct.\n")
	fmt.Fprintf(out, "`engrex-cache calibrate` measures that against known paraphrases, and\n")
	fmt.Fprintf(out, "`loadtest --synthetic` counts wrong answers directly.\n")
}
