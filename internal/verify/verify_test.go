package verify

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func fakeOllama(t *testing.T, completion string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		json.NewEncoder(writer).Encode(map[string]string{"response": completion}) //nolint:errcheck
	}))
}

func TestSplitClaimsKeepsNumbersIntact(t *testing.T) {
	claims := SplitClaims("The error was 3.57 percent. Version 1.2.3 shipped.")

	if len(claims) != 2 {
		t.Fatalf("got %d claims, want 2: %q", len(claims), claims)
	}
	if !strings.Contains(claims[0], "3.57") {
		t.Errorf("decimal was split: %q", claims[0])
	}
	if !strings.Contains(claims[1], "1.2.3") {
		t.Errorf("version was split: %q", claims[1])
	}
}

func TestIsFactualClaim(t *testing.T) {
	factual := []string{
		"ResNet achieved a top-5 error rate of 3.57 percent on ImageNet.",
		"The model uses residual connections between layers.",
	}
	for _, sentence := range factual {
		if !IsFactualClaim(sentence) {
			t.Errorf("expected factual: %q", sentence)
		}
	}

	// Hedges report the absence of information. Marking them unsupported would punish
	// the honest behavior the answer prompt asks for.
	notFactual := []string{
		"Which one would you like?",
		"I couldn't find any specific research paper in the context.",
		"The context does not mention the error rate.",
		"Unfortunately, that is not in your notes.",
		"Sure.",
	}
	for _, sentence := range notFactual {
		if IsFactualClaim(sentence) {
			t.Errorf("expected not factual: %q", sentence)
		}
	}
}

func TestParseVerdict(t *testing.T) {
	cases := map[string]int{
		"2":                  2,
		"  3  ":              3,
		"NONE":               0,
		"none":               0,
		"Passage 2 supports": 2,
		"99":                 0, // out of range
		"":                   0,
		"I think maybe":      0,
	}
	for response, want := range cases {
		if got := parseVerdict(response, 5); got != want {
			t.Errorf("parseVerdict(%q) = %d, want %d", response, got, want)
		}
	}
}

func TestVerifyMarksSupportedClaims(t *testing.T) {
	server := fakeOllama(t, "1")
	defer server.Close()

	verifier := NewLLM(server.URL, "test-model")
	report, err := verifier.Verify(
		"ResNet achieved a top-5 error rate of 3.57 percent.",
		[]string{"ResNet reached 3.57% top-5 error on ImageNet."})
	if err != nil {
		t.Fatal(err)
	}

	if report.Supported != 1 || report.Unsupported != 0 {
		t.Errorf("got %d supported / %d unsupported", report.Supported, report.Unsupported)
	}
	if report.Groundedness != 1.0 {
		t.Errorf("groundedness = %v, want 1.0", report.Groundedness)
	}
	if report.Claims[0].SourceIndex != 1 {
		t.Errorf("SourceIndex = %d, want 1", report.Claims[0].SourceIndex)
	}
}

// The case this package exists for: fluent invented content with real passages present.
func TestVerifyCatchesHallucination(t *testing.T) {
	server := fakeOllama(t, "NONE")
	defer server.Close()

	verifier := NewLLM(server.URL, "test-model")
	report, err := verifier.Verify(
		"The paper was written by Gonzalo Rupprecht and published in IJCAI 2016.",
		[]string{"ResNet reached 3.57% top-5 error on ImageNet."})
	if err != nil {
		t.Fatal(err)
	}

	if report.Unsupported != 1 {
		t.Errorf("hallucinated claim not flagged: %+v", report)
	}
	if report.Groundedness != 0.0 {
		t.Errorf("groundedness = %v, want 0.0", report.Groundedness)
	}
	if len(report.UnsupportedClaims()) != 1 {
		t.Errorf("UnsupportedClaims returned %d", len(report.UnsupportedClaims()))
	}
}

// Hedges are excluded from both numerator and denominator, so an honest "I don't know"
// answer is not scored as ungrounded.
func TestVerifyExcludesHedgesFromScoring(t *testing.T) {
	server := fakeOllama(t, "NONE")
	defer server.Close()

	verifier := NewLLM(server.URL, "test-model")
	report, err := verifier.Verify(
		"I couldn't find any specific research paper in the provided context.",
		[]string{"Some passage."})
	if err != nil {
		t.Fatal(err)
	}

	if report.Checked != 0 {
		t.Errorf("hedge was checked: %+v", report)
	}
	if report.Unsupported != 0 {
		t.Errorf("hedge counted as unsupported: %+v", report)
	}
}

// An unreachable verifier must not manufacture a hallucination report.
func TestVerifyFallsBackWhenModelUnreachable(t *testing.T) {
	verifier := NewLLM("http://127.0.0.1:1", "test-model")

	report, err := verifier.Verify(
		"ResNet achieved a top-5 error rate of 3.57 percent.",
		[]string{"Some passage."})
	if err != nil {
		t.Fatalf("unreachable verifier should not error: %v", err)
	}
	if report.Unsupported != 0 {
		t.Errorf("unreachable verifier reported %d unsupported claims", report.Unsupported)
	}
	if report.Claims[0].Support != NotChecked {
		t.Errorf("claim marked %v, want not-checked", report.Claims[0].Support)
	}
}

func TestVerifyBoundsClaimCount(t *testing.T) {
	server := fakeOllama(t, "1")
	defer server.Close()

	verifier := NewLLM(server.URL, "test-model")
	verifier.MaxClaims = 3

	var answer strings.Builder
	for range 10 {
		answer.WriteString("ResNet achieved a top-5 error rate of 3.57 percent. ")
	}

	report, err := verifier.Verify(answer.String(), []string{"passage"})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Claims) > 3 {
		t.Errorf("got %d claims, cap is 3", len(report.Claims))
	}
}

func TestWriteReportListsUnsupportedClaims(t *testing.T) {
	report := Report{
		Claims: []Claim{
			{Text: "Supported thing.", Support: Supported, SourceIndex: 1},
			{Text: "Invented thing.", Support: Unsupported},
		},
		Checked: 2, Supported: 1, Unsupported: 1, Groundedness: 0.5,
	}

	var builder strings.Builder
	WriteReport(report, &builder)
	output := builder.String()

	if !strings.Contains(output, "50%") {
		t.Errorf("groundedness rate missing: %q", output)
	}
	if !strings.Contains(output, "Invented thing.") {
		t.Errorf("unsupported claim not listed: %q", output)
	}
	if strings.Contains(output, "Supported thing.") {
		t.Errorf("supported claim should not be listed: %q", output)
	}
}
