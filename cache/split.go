package cache

import "strings"

// Parts is a prompt cut into the piece that has to match exactly and the piece that is
// matched by meaning.
type Parts struct {
	// Context must be identical for two prompts to be comparable at all. For an answer
	// prompt this is the instruction preamble plus the retrieved passages.
	Context string

	// Semantic is what actually gets embedded — the question, the claim, the thing the
	// caller is really asking about.
	Semantic string

	Class Class
}

// Splitter separates a prompt into those two parts.
//
// Split reports false when it does not recognize the prompt, so the caller can fall
// through to something more general rather than guessing at a format it has never seen.
type Splitter interface {
	Split(prompt string) (Parts, bool)
	Name() string
}

// marker describes one known prompt format.
//
// The prefix is what identifies it, and it has to be a prefix rather than just a search:
// a saved note can perfectly well contain the text "QUESTION: ", so matching on the
// inner markers alone would let stored content steer how its own prompt gets split.
// Position 0 is the one place the user's material can never reach.
type marker struct {
	name   string
	class  Class
	prefix string
	start  string

	// end empty means the semantic part runs to the end of the prompt.
	end string
}

// markers are the prompt formats Engrex itself emits. The prefixes and delimiters are
// copied from the builders named in each comment — if one of those changes, the splitter
// stops recognizing it and falls back to tailSplitter, which is a loss of hit rate but
// never a wrong answer. splitter_test.go builds real prompts to catch the drift.
var markers = []marker{
	{
		// internal/rag.buildPrompt
		name:   "engrex-answer",
		class:  ClassAnswer,
		prefix: "You are a personal knowledge assistant with access to the user's OWN saved notes",
		start:  "\nQUESTION: ",
		end:    "\n\nAnswer from the CONTEXT above only.",
	},
	{
		// internal/rag.buildNoContextPrompt — the question is the whole tail here.
		name:   "engrex-no-context",
		class:  ClassAnswer,
		prefix: "You are a personal knowledge assistant. The user has no saved notes relevant",
		start:  "\n\nQuestion: ",
		end:    "",
	},
	{
		// internal/rerank.buildPrompt — note the question comes before the passages.
		name:   "engrex-rerank",
		class:  ClassRerank,
		prefix: "You rank search results by relevance.",
		start:  "\nQUESTION: ",
		end:    "\n\nPASSAGES:\n",
	},
	{
		// internal/verify.checkClaim — the passage block is identical for every claim in
		// one answer, which is what makes this the best target in the pipeline.
		name:   "engrex-verify",
		class:  ClassVerify,
		prefix: "Decide whether the STATEMENT is directly supported by any passage.",
		start:  "\nSTATEMENT: ",
		end:    "\n\nIf a passage states",
	},
	{
		// internal/rewrite.buildPrompt — carries a few-shot example that itself starts
		// with "QUESTION: ", so this one genuinely needs the last-occurrence search.
		name:   "engrex-rewrite",
		class:  ClassRewrite,
		prefix: "Break a search question into the separate things that must be looked up.",
		start:  "\nQUESTION: ",
		end:    "\nLOOKUPS:",
	},
}

// EngrexSplitter recognizes the prompt formats this project builds.
type EngrexSplitter struct{}

func (EngrexSplitter) Name() string { return "engrex" }

func (EngrexSplitter) Split(prompt string) (Parts, bool) {
	for _, candidate := range markers {
		if !strings.HasPrefix(prompt, candidate.prefix) {
			continue
		}
		parts, found := candidate.apply(prompt)
		if found {
			return parts, true
		}
	}
	return Parts{}, false
}

// apply cuts the prompt at the marker's delimiters, searching from the end.
//
// From the end because the delimiters are not unique: the rewrite prompt's own example
// contains both of its, and any retrieved passage may contain either. The real ones are
// always the last, because they wrap the caller's input at the tail of the prompt.
func (candidate marker) apply(prompt string) (Parts, bool) {
	endIndex := len(prompt)
	if candidate.end != "" {
		endIndex = strings.LastIndex(prompt, candidate.end)
		if endIndex < 0 {
			return Parts{}, false
		}
	}

	startIndex := strings.LastIndex(prompt[:endIndex], candidate.start)
	if startIndex < 0 {
		return Parts{}, false
	}
	semanticStart := startIndex + len(candidate.start)

	return Parts{
		Context:  joinContext(prompt[:semanticStart], prompt[endIndex:]),
		Semantic: prompt[semanticStart:endIndex],
		Class:    candidate.class,
	}, true
}

// TailSplitter is the fallback for prompts nobody has taught the cache about: the last
// paragraph is treated as the question and everything above it as context. That holds
// for essentially every RAG template, which is the point — the proxy is useful in front
// of Ollama generally, not only for Engrex.
type TailSplitter struct {
	// MinContext is how much text has to sit above the tail before splitting is worth
	// doing. Below it the prompt is all question and no context, so splitting would just
	// shorten what gets embedded.
	MinContext int
}

func (TailSplitter) Name() string { return "tail" }

func (splitter TailSplitter) Split(prompt string) (Parts, bool) {
	minimum := splitter.MinContext
	if minimum <= 0 {
		minimum = defaultMinContext
	}

	boundary := strings.LastIndex(strings.TrimRight(prompt, "\n"), "\n\n")
	if boundary < 0 || boundary < minimum {
		return Parts{}, false
	}

	return Parts{
		Context:  joinContext(prompt[:boundary], ""),
		Semantic: strings.TrimSpace(prompt[boundary:]),
		Class:    ClassOther,
	}, true
}

// defaultMinContext is roughly a paragraph. Chosen to be obviously smaller than any
// prompt with real retrieved context in it and obviously larger than a bare question.
const defaultMinContext = 200

// WholeSplitter treats the entire prompt as the semantic part. Correct for short prompts
// that are nothing but a question, and the last resort for everything else.
type WholeSplitter struct{}

func (WholeSplitter) Name() string { return "whole" }

func (WholeSplitter) Split(prompt string) (Parts, bool) {
	return Parts{Context: "", Semantic: prompt, Class: ClassOther}, true
}

// Chain tries each splitter in turn and takes the first that recognizes the prompt.
type Chain []Splitter

func (chain Chain) Name() string { return "chain" }

func (chain Chain) Split(prompt string) (Parts, bool) {
	for _, splitter := range chain {
		if parts, found := splitter.Split(prompt); found {
			return parts, true
		}
	}
	return Parts{}, false
}

// DefaultSplitter is what the proxy runs: the formats we know, then the general rule,
// then give up and embed the lot.
func DefaultSplitter() Splitter {
	return Chain{EngrexSplitter{}, TailSplitter{}, WholeSplitter{}}
}

// contextSeparator joins the two halves of a context around the hole the semantic part
// was cut from. A byte that cannot occur in a prompt, so no arrangement of text can hash
// to the same context as a different arrangement.
const contextSeparator = "\x00"

func joinContext(before, after string) string {
	return before + contextSeparator + after
}
