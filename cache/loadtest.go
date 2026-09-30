package cache

import (
	"bufio"
	"bytes"
	_ "embed"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

//go:embed workload.json
var workloadJSON []byte

// Workload is the query mix the load test replays.
type Workload struct {
	Questions []WorkloadQuestion `json:"questions"`
	Passages  []string           `json:"passages"`

	// Topics and UniqueTemplates compose the questions nobody has asked. They exist
	// because the obvious shortcut — take a base question and append "(variant 12)" —
	// produces text that is 99% identical to the original, so the supposedly unique
	// share embeds almost on top of it and the hit rate comes out flattered. Traffic a
	// cache genuinely cannot serve has to be genuinely different text.
	Topics          []string `json:"topics"`
	UniqueTemplates []string `json:"unique_templates"`
}

type WorkloadQuestion struct {
	Question    string   `json:"question"`
	Paraphrases []string `json:"paraphrases"`
}

// DefaultWorkload is built from the eval golden set plus hand-written paraphrases, so
// the load test exercises the same material the retrieval evaluation does.
func DefaultWorkload() (Workload, error) {
	var workload Workload
	err := json.Unmarshal(workloadJSON, &workload)
	return workload, err
}

// LoadTestOptions configure a run.
type LoadTestOptions struct {
	ProxyURL    string
	Model       string
	Requests    int
	Concurrency int

	// UniqueShare is the fraction of requests that are questions never asked before —
	// the part of any real workload a cache can do nothing about. The rest split between
	// verbatim repeats and paraphrases.
	UniqueShare float64

	// ParaphraseShare is the fraction that reword an earlier question. This is the part
	// that separates a semantic cache from a hash map.
	ParaphraseShare float64

	Workload Workload
	Seed     int64
	Progress io.Writer
}

func DefaultLoadTestOptions() LoadTestOptions {
	workload, _ := DefaultWorkload()
	return LoadTestOptions{
		ProxyURL:        "http://127.0.0.1:11435",
		Model:           "llama3.2",
		Requests:        2000,
		Concurrency:     4,
		UniqueShare:     0.35,
		ParaphraseShare: 0.25,
		Workload:        workload,
		Seed:            42,
	}
}

// LoadTestResult is what a run measured.
type LoadTestResult struct {
	Requests int
	Hits     int
	Misses   int
	Errors   int

	// WrongHits counts hits that served an answer written for a different question. It
	// is only measurable against the synthetic provider, which stamps every answer with
	// the question it was generated for — but it is the number that decides whether a
	// threshold is safe, so the benchmark measures it rather than assuming it.
	WrongHits   int
	CheckedHits int
	WrongByTier map[Tier]int

	Elapsed      time.Duration
	HitLatency   Percentiles
	MissLatency  Percentiles
	HitsByTier   map[Tier]int
	Convergence  []float64
	ProviderTime time.Duration
}

// Whether a served answer was the right one. Unchecked means the provider was real, so
// there is no stamp to compare against.
type answerCheck int

const (
	answerUnchecked answerCheck = iota
	answerRight
	answerWrong
)

// RunLoadTest drives requests through the proxy and measures what the cache did.
//
// Against a real local model this is slow — a couple of thousand generations is hours —
// so the useful way to run it at size is with `serve --synthetic-upstream`, which swaps
// the provider for one that returns canned text after a fixed delay. That measures the
// cache honestly; what it does not measure is the model. The report says which was used.
func RunLoadTest(options LoadTestOptions) (LoadTestResult, error) {
	if len(options.Workload.Questions) == 0 {
		return LoadTestResult{}, fmt.Errorf("cache: the workload has no questions")
	}

	plan := planRequests(options)
	result := LoadTestResult{Requests: len(plan), HitsByTier: map[Tier]int{}, WrongByTier: map[Tier]int{}}

	hitSamples := newSamples()
	missSamples := newSamples()

	var mutex sync.Mutex
	var waiting sync.WaitGroup
	work := make(chan plannedRequest)

	// Convergence is sampled in twentieths so the report can show the hit rate climbing
	// as the cache fills, which is the shape that makes the number believable.
	const buckets = 20
	perBucket := max(len(plan)/buckets, 1)
	bucketHits := make([]int, buckets)
	bucketTotal := make([]int, buckets)
	completed := 0

	client := &http.Client{}
	started := time.Now()

	for worker := 0; worker < max(options.Concurrency, 1); worker++ {
		waiting.Add(1)
		go func() {
			defer waiting.Done()
			for request := range work {
				status, tier, elapsed, correct, err := sendOne(client, options, request)

				mutex.Lock()
				bucket := min(completed/perBucket, buckets-1)
				bucketTotal[bucket]++
				switch {
				case err != nil:
					result.Errors++
				case status == StatusHit:
					result.Hits++
					result.HitsByTier[tier]++
					hitSamples.add(float64(elapsed.Microseconds()))
					bucketHits[bucket]++
					if correct == answerWrong {
						result.WrongHits++
						result.WrongByTier[tier]++
					}
					if correct != answerUnchecked {
						result.CheckedHits++
					}
				default:
					result.Misses++
					missSamples.add(float64(elapsed.Microseconds()))
					result.ProviderTime += elapsed
				}
				completed++
				if options.Progress != nil && completed%200 == 0 {
					fmt.Fprintf(options.Progress, "  %d/%d requests\n", completed, len(plan))
				}
				mutex.Unlock()
			}
		}()
	}

	for _, request := range plan {
		work <- request
	}
	close(work)
	waiting.Wait()

	result.Elapsed = time.Since(started)
	result.HitLatency = hitSamples.percentiles()
	result.MissLatency = missSamples.percentiles()
	for bucket := range bucketTotal {
		if bucketTotal[bucket] > 0 {
			result.Convergence = append(result.Convergence, float64(bucketHits[bucket])/float64(bucketTotal[bucket]))
		}
	}
	return result, nil
}

// planRequests builds the sequence of prompts up front, so a run is reproducible from
// its seed and two runs can be compared.
//
// The passages matter as much as the questions, because what a semantic cache can do
// depends on what retrieval hands it. Retrieval is itself a function of the question, so
// the model here is: the same question retrieves the same passages, a reworded question
// retrieves mostly the same ones — one swapped out of five — and a genuinely new question
// retrieves a different set. Giving paraphrases a disjoint set instead would make the
// tolerant tier look useless, and giving them an identical set would make it look
// unnecessary; neither is what retrieval does.
func planRequests(options LoadTestOptions) []plannedRequest {
	random := rand.New(rand.NewSource(options.Seed))
	passages := expandPassages(options.Workload)

	type asked struct {
		question   string
		passages   []string
		acceptable []string
	}

	var history []asked
	plan := make([]plannedRequest, 0, options.Requests)

	for number := 0; number < options.Requests; number++ {
		draw := random.Float64()
		index := random.Intn(len(options.Workload.Questions))
		base := options.Workload.Questions[index]

		var next asked
		switch {
		case draw < options.UniqueShare || len(history) == 0:
			// Traffic no cache can serve: a question nobody has asked, over passages
			// nobody has retrieved.
			question := uniqueQuestion(options.Workload, random)
			next = asked{
				question:   question,
				passages:   retrievedFor(passages, question),
				acceptable: []string{StampFor(question)},
			}
		case draw < options.UniqueShare+options.ParaphraseShare && len(base.Paraphrases) > 0:
			// The case that separates a semantic cache from a hash map: same question,
			// different words, and retrieval comes back with four of the same five.
			next = asked{
				question:   base.Paraphrases[random.Intn(len(base.Paraphrases))],
				passages:   swapOne(retrievedFor(passages, base.Question), passages, index+7),
				acceptable: stampsFor(base),
			}
		default:
			next = history[random.Intn(len(history))]
		}

		history = append(history, next)
		plan = append(plan, plannedRequest{
			prompt:     loadTestPrompt(next.question, next.passages),
			question:   next.question,
			acceptable: next.acceptable,
		})
	}
	return plan
}

// plannedRequest is one request, plus the identities of every wording of the question it
// is really asking.
//
// Every wording, not just the one sent, because serving the answer written for "what is
// the dedup threshold?" in response to "what dedup threshold does engrex use?" is the
// cache working, not failing. Checking against the literal string sent would score every
// successful paraphrase match as an error and make the cache look far worse than it is —
// which is exactly what it did until this was fixed.
type plannedRequest struct {
	prompt   string
	question string

	// acceptable is the stamp of each wording of the same underlying question.
	acceptable []string
}

// stampsFor is every wording of one question: the original and each paraphrase. An answer
// stamped with any of them is the right answer to what was asked.
func stampsFor(question WorkloadQuestion) []string {
	stamps := []string{StampFor(question.Question)}
	for _, paraphrase := range question.Paraphrases {
		stamps = append(stamps, StampFor(paraphrase))
	}
	return stamps
}

// uniqueQuestion composes a question about two unrelated topics, so that the share of
// traffic meant to be unservable really is.
func uniqueQuestion(workload Workload, random *rand.Rand) string {
	if len(workload.Topics) < 2 || len(workload.UniqueTemplates) == 0 {
		return fmt.Sprintf("an unrelated question %d", random.Int())
	}
	first := random.Intn(len(workload.Topics))
	second := (first + 1 + random.Intn(len(workload.Topics)-1)) % len(workload.Topics)
	template := workload.UniqueTemplates[random.Intn(len(workload.UniqueTemplates))]
	return fmt.Sprintf(template, workload.Topics[first], workload.Topics[second])
}

// retrievedFor is the set of passages a question comes back with: five of them, matching
// rag.DefaultSearchResults, and always the same five for the same question.
//
// Scattered across the corpus rather than taken as a consecutive run. A run makes two
// different questions share four of five passages whenever their seeds land next to each
// other, which sails through the context-overlap guard and turns the fixture into a
// generator of false hits that have nothing to do with the cache.
func retrievedFor(passages []string, question string) []string {
	const retrieved = 5
	chosen := make([]string, 0, retrieved)
	taken := make(map[int]bool, retrieved)

	seed := fnv64(question)
	for len(chosen) < retrieved {
		seed = seed*6364136223846793005 + 1442695040888963407
		index := int(seed % uint64(len(passages)))
		if taken[index] {
			continue
		}
		taken[index] = true
		chosen = append(chosen, passages[index])
	}
	return chosen
}

// expandPassages builds a corpus big enough, and varied enough, that distinct questions
// retrieve distinct passages.
//
// Both properties matter. Engrex indexes thousands of chunks, so a fixture with ten
// would collapse every question into a handful of namespaces. And the text has to
// actually differ: the overlap guard compares word shingles, so passages spun from one
// sentence template would all look alike to it however many there were, and every
// context would appear to overlap every other.
func expandPassages(workload Workload) []string {
	expanded := append([]string(nil), workload.Passages...)

	forms := []string{
		"%s is implemented in the daemon and its behaviour is recorded here for later reference.",
		"When %s changes, the surrounding pipeline has to be re-run before results can be trusted.",
		"An earlier revision of %s caused a regression that took two days to track down.",
		"Benchmarks covering %s were collected on an M-series laptop with nothing else running.",
		"The design note explaining %s argues that the simpler approach was rejected on latency grounds.",
		"Configuration affecting %s lives in the user's home directory rather than in the repository.",
		"Tests around %s deliberately avoid the network so they can run in a sandbox.",
		"Documentation for %s was rewritten after the first draft proved impossible to follow.",
		"A rejected proposal for %s would have introduced a second inference dependency.",
		"Failure handling in %s degrades to the previous behaviour instead of returning an error.",
	}
	for _, topic := range workload.Topics {
		for _, form := range forms {
			expanded = append(expanded, fmt.Sprintf(form, topic))
		}
	}
	return expanded
}

// swapOne replaces a single passage, which is what rewording a question typically does to
// the retrieved set. Four shared out of six distinct is a Jaccard of about 0.67, just
// above the overlap the tolerant tier requires.
func swapOne(chosen []string, all []string, seed int) []string {
	swapped := append([]string(nil), chosen...)
	swapped[len(swapped)-1] = all[seed%len(all)]
	return swapped
}

// loadTestPrompt builds a prompt in the shape internal/rag.buildPrompt emits, so the
// splitter treats it exactly as it treats a real one.
func loadTestPrompt(question string, passages []string) string {
	var builder strings.Builder
	builder.WriteString("You are a personal knowledge assistant with access to the user's OWN saved notes and documents. ")
	builder.WriteString("Everything in the CONTEXT below was saved by the user, so treat it as factual and authoritative.\n\n")
	builder.WriteString("RULES:\n1. Answer using ONLY the CONTEXT below.\n")
	builder.WriteString("\nCONTEXT:\n")
	for index, passage := range passages {
		fmt.Fprintf(&builder, "[%d] document: notes.md | saved: 2026-08-14\n%s\n\n", index+1, passage)
	}
	fmt.Fprintf(&builder, "QUESTION: %s\n\n", question)
	builder.WriteString("Answer from the CONTEXT above only. If it is not there, say so rather than guessing.\nANSWER:")
	return builder.String()
}

func sendOne(client *http.Client, options LoadTestOptions, request plannedRequest) (Status, Tier, time.Duration, answerCheck, error) {
	body, err := json.Marshal(map[string]any{
		"model": options.Model, "prompt": request.prompt, "stream": true,
		"options": map[string]any{"temperature": 0, "num_predict": 400},
	})
	if err != nil {
		return StatusMiss, "", 0, answerUnchecked, err
	}

	started := time.Now()
	response, err := client.Post(options.ProxyURL+generateEndpoint, "application/json", bytes.NewReader(body))
	if err != nil {
		return StatusMiss, "", time.Since(started), answerUnchecked, err
	}
	defer response.Body.Close() //nolint:errcheck

	// The body has to be read, not just closed: latency is not measured until the last
	// frame arrives, and a hit's whole point is that it arrives immediately.
	served, err := readAssembled(response.Body)
	elapsed := time.Since(started)
	if err != nil {
		return StatusMiss, "", elapsed, answerUnchecked, err
	}

	return Status(response.Header.Get(HeaderStatus)),
		Tier(response.Header.Get(HeaderTier)),
		elapsed,
		checkAnswer(request, served),
		nil
}

// checkAnswer asks whether the served answer was written for the question that was
// asked, in any of its wordings. A hit carrying some other question's stamp is a false
// hit: a confident answer to a question nobody asked, which is the failure a similarity
// threshold trades against and the one worth counting.
func checkAnswer(request plannedRequest, served string) answerCheck {
	stamp := StampIn(served)
	if stamp == "" {
		return answerUnchecked
	}
	for _, acceptable := range request.acceptable {
		if stamp == acceptable {
			return answerRight
		}
	}
	return answerWrong
}

// readAssembled reads an NDJSON stream back into the text it carried.
func readAssembled(body io.Reader) (string, error) {
	reader := bufio.NewReader(body)
	var assembled strings.Builder
	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			var frame struct {
				Response string `json:"response"`
			}
			if json.Unmarshal(line, &frame) == nil {
				assembled.WriteString(frame.Response)
			}
		}
		if err != nil {
			if err == io.EOF {
				return assembled.String(), nil
			}
			return assembled.String(), err
		}
	}
}

// WriteLoadTestReport prints the run in the house style — see internal/hnsw/bench.go.
func WriteLoadTestReport(out io.Writer, result LoadTestResult, synthetic bool) {
	fmt.Fprintf(out, "\n%-22s %14s\n", "MEASURE", "VALUE")
	fmt.Fprintln(out, strings.Repeat("-", 40))

	hitRate := 0.0
	if served := result.Hits + result.Misses; served > 0 {
		hitRate = float64(result.Hits) / float64(served)
	}

	fmt.Fprintf(out, "%-22s %14d\n", "requests", result.Requests)
	fmt.Fprintf(out, "%-22s %14d\n", "hits", result.Hits)
	fmt.Fprintf(out, "%-22s %14d\n", "misses", result.Misses)
	if result.Errors > 0 {
		fmt.Fprintf(out, "%-22s %14d\n", "errors", result.Errors)
	}
	fmt.Fprintf(out, "%-22s %13.1f%%\n", "hit rate", hitRate*100)
	if result.CheckedHits > 0 {
		fmt.Fprintf(out, "%-22s %14d\n", "hits checked", result.CheckedHits)
		fmt.Fprintf(out, "%-22s %14d\n", "wrong hits", result.WrongHits)
		fmt.Fprintf(out, "%-22s %13.2f%%\n", "false hit rate",
			100*float64(result.WrongHits)/float64(result.CheckedHits))
	}
	fmt.Fprintf(out, "%-22s %14s\n", "wall clock", result.Elapsed.Round(time.Millisecond))

	fmt.Fprintf(out, "\n%-22s %12s %12s %12s\n", "LATENCY", "P50", "P95", "P99")
	fmt.Fprintln(out, strings.Repeat("-", 62))
	fmt.Fprintf(out, "%-22s %12s %12s %12s\n", "hit",
		formatLatency(result.HitLatency.P50), formatLatency(result.HitLatency.P95), formatLatency(result.HitLatency.P99))
	fmt.Fprintf(out, "%-22s %12s %12s %12s\n", "miss",
		formatLatency(result.MissLatency.P50), formatLatency(result.MissLatency.P95), formatLatency(result.MissLatency.P99))

	if len(result.HitsByTier) > 0 {
		fmt.Fprintf(out, "\n%-22s %14s\n", "HITS BY TIER", "COUNT")
		fmt.Fprintln(out, strings.Repeat("-", 40))
		tiers := make([]string, 0, len(result.HitsByTier))
		for tier := range result.HitsByTier {
			tiers = append(tiers, string(tier))
		}
		sort.Strings(tiers)
		for _, tier := range tiers {
			wrong := result.WrongByTier[Tier(tier)]
			if result.CheckedHits > 0 {
				fmt.Fprintf(out, "%-22s %14d   %d wrong\n", tier, result.HitsByTier[Tier(tier)], wrong)
				continue
			}
			fmt.Fprintf(out, "%-22s %14d\n", tier, result.HitsByTier[Tier(tier)])
		}
	}

	if len(result.Convergence) > 1 {
		fmt.Fprintf(out, "\nHIT RATE AS THE CACHE FILLS\n")
		fmt.Fprintln(out, strings.Repeat("-", 40))
		for index, rate := range result.Convergence {
			fmt.Fprintf(out, "%3d%%  %s %5.1f%%\n",
				(index+1)*100/len(result.Convergence), bar(rate), rate*100)
		}
	}

	writeLoadTestVerdict(out, result, hitRate, synthetic)
}

func writeLoadTestVerdict(out io.Writer, result LoadTestResult, hitRate float64, synthetic bool) {
	fmt.Fprintln(out)
	if result.Hits == 0 {
		fmt.Fprintln(out, "Nothing hit. Either the cache is not in the path or every request was unique.")
		return
	}

	speedup := 0.0
	if result.HitLatency.P50 > 0 {
		speedup = result.MissLatency.P50 / result.HitLatency.P50
	}
	avoided := time.Duration(float64(result.ProviderTime) / float64(max(result.Misses, 1)) * float64(result.Hits))

	fmt.Fprintf(out, "%.1f%% of requests were served from cache, at %.0fx the speed of a miss\n", hitRate*100, speedup)
	fmt.Fprintf(out, "(%s against %s at the median). That avoided about %s of provider time.\n",
		formatLatency(result.HitLatency.P50), formatLatency(result.MissLatency.P50), avoided.Round(time.Second))

	if result.CheckedHits > 0 {
		wrongRate := 100 * float64(result.WrongHits) / float64(result.CheckedHits)
		fmt.Fprintf(out, "\n%.2f%% of hits served an answer written for a different question.\n", wrongRate)
		if result.WrongHits > 0 {
			fmt.Fprintln(out, "That is the cost of the current threshold. Raise it to trade hit rate for")
			fmt.Fprintln(out, "correctness; `engrex-cache calibrate` shows the curve.")
		}
	}

	if synthetic {
		fmt.Fprintln(out, "\nThe provider was synthetic, so the hit rate, the cache-hit latency and the")
		fmt.Fprintln(out, "false-hit rate are real measurements; the avoided time is the simulated")
		fmt.Fprintln(out, "per-call cost times the number of hits.")
	}
}

// formatLatency picks a unit, the same way internal/hnsw/bench.go does.
func formatLatency(microseconds float64) string {
	switch {
	case microseconds == 0:
		return "-"
	case microseconds < 1000:
		return fmt.Sprintf("%.0fµs", microseconds)
	case microseconds < 1_000_000:
		return fmt.Sprintf("%.1fms", microseconds/1000)
	default:
		return fmt.Sprintf("%.2fs", microseconds/1_000_000)
	}
}
