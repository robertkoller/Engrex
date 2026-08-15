package eval

import (
	"math"
	"os"
	"testing"
)

func writeFile(path, contents string) error {
	return os.WriteFile(path, []byte(contents), 0644)
}

// staticRetriever returns a fixed result list per question, so scoring can be tested
// without Ollama or a database.
func staticRetriever(byQuestion map[string][]string) Retriever {
	return func(question string, topK int) ([]string, error) {
		results := byQuestion[question]
		if len(results) > topK {
			results = results[:topK]
		}
		return results, nil
	}
}

func closeEnough(got, want float64) bool {
	return math.Abs(got-want) < 1e-9
}

func TestRunScoresRecallAndMRR(t *testing.T) {
	set := Set{Cases: []Case{
		{Question: "first", Relevant: []string{"alpha.md"}},
		{Question: "second", Relevant: []string{"beta.md"}},
	}}

	// "first" hits at rank 1, "second" at rank 3.
	retrieve := staticRetriever(map[string][]string{
		"first":  {"/notes/alpha.md", "/notes/other.md", "/notes/more.md"},
		"second": {"/notes/wrong.md", "/notes/nope.md", "/notes/beta.md"},
	})

	report, err := Run(set, retrieve, []int{1, 3})
	if err != nil {
		t.Fatal(err)
	}

	// Only "first" is found within the top 1.
	if !closeEnough(report.RecallAtK[1], 0.5) {
		t.Errorf("recall@1 = %v, want 0.5", report.RecallAtK[1])
	}
	if !closeEnough(report.RecallAtK[3], 1.0) {
		t.Errorf("recall@3 = %v, want 1.0", report.RecallAtK[3])
	}

	// MRR = (1/1 + 1/3) / 2
	want := (1.0 + 1.0/3.0) / 2
	if !closeEnough(report.MRR, want) {
		t.Errorf("MRR = %v, want %v", report.MRR, want)
	}
}

// Retrieving the same relevant document repeatedly must not score as if several
// distinct expected sources were found.
func TestRecallCountsDistinctSources(t *testing.T) {
	set := Set{Cases: []Case{
		{Question: "q", Relevant: []string{"alpha.md", "beta.md"}},
	}}
	retrieve := staticRetriever(map[string][]string{
		"q": {"/notes/alpha.md", "/notes/alpha.md", "/notes/alpha.md"},
	})

	report, err := Run(set, retrieve, []int{3})
	if err != nil {
		t.Fatal(err)
	}
	if !closeEnough(report.RecallAtK[3], 0.5) {
		t.Errorf("recall@3 = %v, want 0.5 (one of two distinct sources)", report.RecallAtK[3])
	}
}

func TestMissesListsUnretrievedCases(t *testing.T) {
	set := Set{Cases: []Case{
		{Question: "found", Relevant: []string{"alpha.md"}},
		{Question: "lost", Relevant: []string{"missing.md"}},
	}}
	retrieve := staticRetriever(map[string][]string{
		"found": {"/notes/alpha.md"},
		"lost":  {"/notes/irrelevant.md"},
	})

	report, err := Run(set, retrieve, []int{1})
	if err != nil {
		t.Fatal(err)
	}

	misses := report.Misses()
	if len(misses) != 1 {
		t.Fatalf("got %d misses, want 1", len(misses))
	}
	if misses[0].Case.Question != "lost" {
		t.Errorf("wrong case reported as a miss: %q", misses[0].Case.Question)
	}
}

func TestSliceSeparatesHops(t *testing.T) {
	set := Set{Cases: []Case{
		{Question: "single", Relevant: []string{"alpha.md"}},
		{Question: "multi", Relevant: []string{"beta.md"}, Hops: 2},
	}}
	retrieve := staticRetriever(map[string][]string{
		"single": {"/notes/alpha.md"},
		"multi":  {"/notes/wrong.md"},
	})

	report, err := Run(set, retrieve, []int{1})
	if err != nil {
		t.Fatal(err)
	}

	multiHop := report.Slice(func(testCase Case) bool { return testCase.Hops > 1 })
	if len(multiHop.Results) != 1 {
		t.Fatalf("multi-hop slice has %d cases, want 1", len(multiHop.Results))
	}
	if !closeEnough(multiHop.RecallAtK[1], 0.0) {
		t.Errorf("multi-hop recall@1 = %v, want 0", multiHop.RecallAtK[1])
	}

	singleHop := report.Slice(func(testCase Case) bool { return testCase.Hops <= 1 })
	if !closeEnough(singleHop.RecallAtK[1], 1.0) {
		t.Errorf("single-hop recall@1 = %v, want 1", singleHop.RecallAtK[1])
	}
}

func TestLoadRejectsIncompleteCases(t *testing.T) {
	path := t.TempDir() + "/golden.json"

	writeSet := func(contents string) {
		t.Helper()
		if err := writeFile(path, contents); err != nil {
			t.Fatal(err)
		}
	}

	writeSet(`{"cases": []}`)
	if _, err := Load(path); err == nil {
		t.Error("expected an error for an empty set")
	}

	writeSet(`{"cases": [{"question": "", "relevant": ["a.md"]}]}`)
	if _, err := Load(path); err == nil {
		t.Error("expected an error for a blank question")
	}

	writeSet(`{"cases": [{"question": "q", "relevant": []}]}`)
	if _, err := Load(path); err == nil {
		t.Error("expected an error for a case with no relevant sources")
	}

	writeSet(`{"cases": [{"question": "q", "relevant": ["a.md"]}]}`)
	if _, err := Load(path); err != nil {
		t.Errorf("valid set rejected: %v", err)
	}
}
