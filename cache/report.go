package cache

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"
)

// WriteStatsReport prints a snapshot in the same shape as the rest of this project's
// reports — fixed-width columns, a rule, then a plain-English verdict, following
// internal/hnsw/bench.go and internal/eval/report.go.
func WriteStatsReport(out io.Writer, snapshot Snapshot) {
	lookups := snapshot.Hits + snapshot.Misses

	fmt.Fprintf(out, "\n%-24s %14s\n", "MEASURE", "VALUE")
	fmt.Fprintln(out, strings.Repeat("-", 42))
	fmt.Fprintf(out, "%-24s %14d\n", "lookups", lookups)
	fmt.Fprintf(out, "%-24s %14d\n", "hits", snapshot.Hits)
	fmt.Fprintf(out, "%-24s %14d\n", "misses", snapshot.Misses)
	fmt.Fprintf(out, "%-24s %14d\n", "bypassed", snapshot.Bypasses)
	if snapshot.Errors > 0 {
		fmt.Fprintf(out, "%-24s %14d\n", "errors", snapshot.Errors)
	}
	fmt.Fprintf(out, "%-24s %13.1f%%\n", "hit rate", snapshot.HitRate*100)
	fmt.Fprintf(out, "%-24s %14d\n", "entries held", snapshot.Entries)
	fmt.Fprintf(out, "%-24s %14d\n", "evictions", snapshot.Evictions)
	fmt.Fprintf(out, "%-24s %14s\n", "generation time saved",
		time.Duration(snapshot.TimeSavedSeconds*float64(time.Second)).Round(time.Second))

	fmt.Fprintf(out, "\n%-24s %12s %12s %12s\n", "LATENCY", "P50", "P95", "P99")
	fmt.Fprintln(out, strings.Repeat("-", 64))
	fmt.Fprintf(out, "%-24s %12s %12s %12s\n", "hit",
		formatLatency(snapshot.HitLatencyMicros.P50),
		formatLatency(snapshot.HitLatencyMicros.P95),
		formatLatency(snapshot.HitLatencyMicros.P99))
	fmt.Fprintf(out, "%-24s %12s %12s %12s\n", "miss (provider included)",
		formatLatency(snapshot.MissLatencyMicros.P50),
		formatLatency(snapshot.MissLatencyMicros.P95),
		formatLatency(snapshot.MissLatencyMicros.P99))

	writeClassTable(out, snapshot)
	writeTierTable(out, snapshot)
	writeStatsVerdict(out, snapshot)
}

func writeClassTable(out io.Writer, snapshot Snapshot) {
	names := map[Class]bool{}
	for class := range snapshot.HitsByClass {
		names[class] = true
	}
	for class := range snapshot.MissesByClass {
		names[class] = true
	}
	if len(names) == 0 {
		return
	}

	ordered := make([]string, 0, len(names))
	for class := range names {
		ordered = append(ordered, string(class))
	}
	sort.Strings(ordered)

	fmt.Fprintf(out, "\n%-24s %10s %10s %11s   %s\n", "CLASS", "HITS", "MISSES", "HIT RATE", "")
	fmt.Fprintln(out, strings.Repeat("-", 78))
	for _, name := range ordered {
		hits := snapshot.HitsByClass[Class(name)]
		misses := snapshot.MissesByClass[Class(name)]
		rate := 0.0
		if hits+misses > 0 {
			rate = float64(hits) / float64(hits+misses)
		}
		fmt.Fprintf(out, "%-24s %10d %10d %10.1f%%   %s\n", name, hits, misses, rate*100, bar(rate))
	}
}

func writeTierTable(out io.Writer, snapshot Snapshot) {
	if len(snapshot.HitsByTier) == 0 {
		return
	}

	ordered := make([]string, 0, len(snapshot.HitsByTier))
	for tier := range snapshot.HitsByTier {
		ordered = append(ordered, string(tier))
	}
	sort.Strings(ordered)

	fmt.Fprintf(out, "\n%-24s %10s   %s\n", "HITS BY TIER", "COUNT", "")
	fmt.Fprintln(out, strings.Repeat("-", 60))
	for _, tier := range ordered {
		fmt.Fprintf(out, "%-24s %10d   %s\n", tier, snapshot.HitsByTier[Tier(tier)], tierMeaning(Tier(tier)))
	}
}

// tierMeaning says what a tier is, because the counts only matter if you know which of
// them could ever have been wrong.
func tierMeaning(tier Tier) string {
	switch tier {
	case TierExact:
		return "the same prompt, byte for byte"
	case TierNamespace:
		return "a reworded question over identical passages"
	case TierTolerant:
		return "a reworded question over mostly the same passages"
	default:
		return ""
	}
}

func writeStatsVerdict(out io.Writer, snapshot Snapshot) {
	fmt.Fprintln(out)
	lookups := snapshot.Hits + snapshot.Misses
	if lookups == 0 {
		fmt.Fprintln(out, "Nothing has come through yet. Run `engrex-cache enable`, restart the daemon,")
		fmt.Fprintln(out, "and ask a few questions.")
		return
	}

	speedup := 0.0
	if snapshot.HitLatencyMicros.P50 > 0 {
		speedup = snapshot.MissLatencyMicros.P50 / snapshot.HitLatencyMicros.P50
	}
	saved := time.Duration(snapshot.TimeSavedSeconds * float64(time.Second))

	fmt.Fprintf(out, "%.1f%% of %d lookups were served from cache, saving about %s of\n",
		snapshot.HitRate*100, lookups, saved.Round(time.Second))
	fmt.Fprintf(out, "generation time.")
	if speedup > 1 {
		fmt.Fprintf(out, " A hit comes back about %.0fx faster than a miss.", speedup)
	}
	fmt.Fprintln(out)

	if snapshot.NearMissSimilarity.Count > 0 {
		fmt.Fprintf(out, "\nMisses came within %.3f of a stored entry at the median. Run\n",
			snapshot.NearMissSimilarity.P50)
		fmt.Fprintln(out, "`engrex-cache tune` to see what a different threshold would have served.")
	}
}
