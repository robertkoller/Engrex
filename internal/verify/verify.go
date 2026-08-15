// Package verify checks whether an answer is actually supported by the passages it was
// generated from.
//
// A RAG prompt can ask for grounded answers, but nothing in the pipeline checks that it
// got one — and the failure mode this project has already hit is a model that produces
// fluent, confident, entirely invented content while correct passages sit in its
// context. Asking the model to behave is not a control. Checking the output is.
//
// Verification runs after generation, splits the answer into claims, and tests each one
// against the retrieved passages. It reports a groundedness rate and marks the claims
// that nothing supports.
package verify

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// Support is the verdict for one claim.
type Support int

const (
	// Unsupported means no passage backs the claim — the hallucination case.
	Unsupported Support = iota
	// Supported means at least one passage entails the claim.
	Supported
	// NotChecked marks claims verification deliberately skips, like questions or
	// hedges, which carry no factual assertion to verify.
	NotChecked
)

func (support Support) String() string {
	switch support {
	case Supported:
		return "supported"
	case Unsupported:
		return "unsupported"
	default:
		return "not-checked"
	}
}

// Claim is one sentence of the answer with its verdict.
type Claim struct {
	Text    string
	Support Support

	// SourceIndex is the 1-based passage that supports the claim, or 0 when none does.
	SourceIndex int
}

// Report is the outcome for a whole answer.
type Report struct {
	Claims []Claim

	// Groundedness is supported claims over checked claims. Claims that were not
	// checked are excluded from both, so an answer full of questions is not scored as
	// though it were full of facts.
	Groundedness float64

	Checked     int
	Supported   int
	Unsupported int
}

// Unsupported returns just the claims nothing backed — the part worth showing a user.
func (report Report) UnsupportedClaims() []Claim {
	var claims []Claim
	for _, claim := range report.Claims {
		if claim.Support == Unsupported {
			claims = append(claims, claim)
		}
	}
	return claims
}

// Verifier checks an answer against the passages it came from.
type Verifier interface {
	Verify(answer string, passages []string) (Report, error)
	Name() string
}

// LLMVerifier tests entailment with a local generation model, one claim at a time.
//
// Per-claim rather than whole-answer because entailment is a narrow judgment a small
// model can make reliably ("does any passage state this?"), whereas grading a whole
// answer at once collapses into a vague quality score. The cost is one call per claim,
// which is why this runs as an opt-in check rather than on every query.
type LLMVerifier struct {
	BaseURL string
	Model   string
	Timeout time.Duration

	// MaxClaims bounds the work. A long answer would otherwise issue dozens of calls.
	MaxClaims int
}

// NewLLM returns a verifier backed by an Ollama generation model.
func NewLLM(baseURL, model string) *LLMVerifier {
	return &LLMVerifier{
		BaseURL:   baseURL,
		Model:     model,
		Timeout:   30 * time.Second,
		MaxClaims: 20,
	}
}

func (verifier *LLMVerifier) Name() string { return "llm-entailment:" + verifier.Model }

// Verify splits the answer into claims and checks each against the passages.
func (verifier *LLMVerifier) Verify(answer string, passages []string) (Report, error) {
	sentences := SplitClaims(answer)
	if len(sentences) > verifier.MaxClaims {
		sentences = sentences[:verifier.MaxClaims]
	}

	report := Report{}
	for _, sentence := range sentences {
		claim := Claim{Text: sentence}

		if !IsFactualClaim(sentence) {
			claim.Support = NotChecked
			report.Claims = append(report.Claims, claim)
			continue
		}

		sourceIndex, err := verifier.checkClaim(sentence, passages)
		if err != nil {
			// An unreachable verifier must not turn into a false hallucination report.
			claim.Support = NotChecked
			report.Claims = append(report.Claims, claim)
			continue
		}

		report.Checked++
		if sourceIndex > 0 {
			claim.Support = Supported
			claim.SourceIndex = sourceIndex
			report.Supported++
		} else {
			claim.Support = Unsupported
			report.Unsupported++
		}
		report.Claims = append(report.Claims, claim)
	}

	if report.Checked > 0 {
		report.Groundedness = float64(report.Supported) / float64(report.Checked)
	}
	return report, nil
}

// checkClaim asks which passage supports a claim, returning the 1-based passage number
// or 0 for none.
func (verifier *LLMVerifier) checkClaim(claim string, passages []string) (int, error) {
	var builder strings.Builder

	builder.WriteString("Decide whether the STATEMENT is directly supported by any passage.\n\n")
	builder.WriteString("PASSAGES:\n")
	for index, passage := range passages {
		snippet := passage
		if len(snippet) > 600 {
			snippet = snippet[:600] + "..."
		}
		fmt.Fprintf(&builder, "[%d] %s\n\n", index+1, strings.ReplaceAll(snippet, "\n", " "))
	}
	fmt.Fprintf(&builder, "STATEMENT: %s\n\n", claim)
	builder.WriteString("If a passage states or directly implies the statement, reply with only that passage's number.\n")
	builder.WriteString("If no passage supports it, reply with only: NONE\n")
	builder.WriteString("Do not explain. Reply with a single number or NONE.\n\nANSWER:")

	response, err := verifier.generate(builder.String())
	if err != nil {
		return 0, err
	}
	return parseVerdict(response, len(passages)), nil
}

func (verifier *LLMVerifier) generate(prompt string) (string, error) {
	body, err := json.Marshal(map[string]any{
		"model":  verifier.Model,
		"prompt": prompt,
		"stream": false,
		"options": map[string]any{
			"temperature": 0,
			"num_ctx":     contextWindowFor(prompt),
		},
	})
	if err != nil {
		return "", err
	}

	client := &http.Client{Timeout: verifier.Timeout}
	response, err := client.Post(verifier.BaseURL+"/api/generate", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer response.Body.Close()

	var decoded struct {
		Response string `json:"response"`
	}
	if err := json.NewDecoder(response.Body).Decode(&decoded); err != nil {
		return "", err
	}
	return decoded.Response, nil
}

// parseVerdict reads a passage number out of the reply, or 0 for unsupported.
//
// Anything that isn't a valid passage number counts as unsupported. That direction is
// deliberate: treating an unparseable verdict as support would let exactly the failure
// this package exists to catch slip through silently.
func parseVerdict(response string, passageCount int) int {
	trimmed := strings.TrimSpace(response)
	if trimmed == "" {
		return 0
	}
	upper := strings.ToUpper(trimmed)
	if strings.HasPrefix(upper, "NONE") {
		return 0
	}

	digits := strings.Builder{}
	for _, character := range trimmed {
		if character >= '0' && character <= '9' {
			digits.WriteRune(character)
			continue
		}
		if digits.Len() > 0 {
			break // take only the first run of digits
		}
	}
	if digits.Len() == 0 {
		return 0
	}

	value := 0
	for _, character := range digits.String() {
		value = value*10 + int(character-'0')
	}
	if value < 1 || value > passageCount {
		return 0
	}
	return value
}

// SplitClaims breaks an answer into sentence-level claims.
//
// Reuses the same rule the chunker settled on — a terminator only ends a sentence when
// whitespace follows — so decimals, versions, and identifiers in an answer survive
// intact rather than being split into fragments that could never be verified.
func SplitClaims(answer string) []string {
	var claims []string
	start := 0

	for index := 0; index < len(answer); index++ {
		character := answer[index]
		if character != '.' && character != '!' && character != '?' && character != '\n' {
			continue
		}

		end := index
		for end+1 < len(answer) && isTerminator(answer[end+1]) {
			end++
		}
		if character != '\n' && end+1 < len(answer) && !isSpace(answer[end+1]) {
			continue // mid-token punctuation, not a sentence end
		}

		if trimmed := strings.TrimSpace(answer[start : end+1]); len(trimmed) > 2 {
			claims = append(claims, trimmed)
		}
		start = end + 1
		index = end
	}

	if trimmed := strings.TrimSpace(answer[start:]); len(trimmed) > 2 {
		claims = append(claims, trimmed)
	}
	return claims
}

func isTerminator(character byte) bool {
	return character == '.' || character == '!' || character == '?'
}

func isSpace(character byte) bool {
	return character == ' ' || character == '\t' || character == '\n' || character == '\r'
}

// hedges open sentences that report the absence of information rather than assert a
// fact. Verifying them is meaningless — "the notes don't say" is not entailed by any
// passage, and marking it unsupported would penalise exactly the honest behavior the
// answer prompt asks for.
var hedges = []string{
	"i couldn't find", "i could not find", "i don't have", "i do not have",
	"the context does not", "the context doesn't", "there is no mention",
	"not mentioned", "the notes do not", "the notes don't", "no information",
	"is not in", "unfortunately",
}

// IsFactualClaim reports whether a sentence asserts something checkable.
func IsFactualClaim(sentence string) bool {
	trimmed := strings.TrimSpace(sentence)
	if len(trimmed) < 15 {
		return false // too short to carry a verifiable assertion
	}
	if strings.HasSuffix(trimmed, "?") {
		return false // a question asserts nothing
	}

	lowered := strings.ToLower(trimmed)
	for _, hedge := range hedges {
		if strings.Contains(lowered, hedge) {
			return false
		}
	}
	return true
}

func contextWindowFor(prompt string) int {
	const (
		minimum  = 4096
		maximum  = 32768
		perToken = 3
		headroom = 256
	)
	needed := len(prompt)/perToken + headroom
	if needed < minimum {
		return minimum
	}
	if needed > maximum {
		return maximum
	}
	return needed
}

// WriteReport renders a verification result for the user.
func WriteReport(report Report, builder *strings.Builder) {
	if report.Checked == 0 {
		return
	}

	fmt.Fprintf(builder, "\n─── grounding: %.0f%% (%d/%d claims supported)",
		report.Groundedness*100, report.Supported, report.Checked)

	unsupported := report.UnsupportedClaims()
	if len(unsupported) == 0 {
		builder.WriteString(" ───\n")
		return
	}

	fmt.Fprintf(builder, ", %d unsupported ───\n", len(unsupported))
	for _, claim := range unsupported {
		text := claim.Text
		if len(text) > 120 {
			text = text[:120] + "..."
		}
		fmt.Fprintf(builder, "  ⚠ not in your notes: %s\n", text)
	}
}
