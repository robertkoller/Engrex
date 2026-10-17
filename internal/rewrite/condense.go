package rewrite

import (
	"strings"
	"time"

	"github.com/robertkoller/engrex/internal/protocol"
)

// condenseTurns is how many of the latest messages the condenser sees. A follow-up
// almost always points at the last exchange, and a shorter prompt keeps this step fast
const condenseTurns = 4

// condenseTurnChars caps each message in the condense prompt. Only the subject of an
// answer matters here, which is nearly always in its opening lines
const condenseTurnChars = 600

// maxStandaloneChars rejects a condensed question that is really an answer. Small
// models sometimes answer instead of rewriting, and searching for an answer is worse
// than searching for the raw follow-up
const maxStandaloneChars = 300

// Condenser turns a follow-up like "what about the second one?" into a question that
// can be searched on its own
type Condenser struct {
	BaseURL string
	Model   string
	Timeout time.Duration
}

// NewCondenser returns a condenser backed by an Ollama generation model.
func NewCondenser(baseURL, model string) *Condenser {
	return &Condenser{BaseURL: baseURL, Model: model, Timeout: 45 * time.Second}
}

// Condense returns a standalone version of question. Any failure returns the question
// unchanged, so the worst case is searching the way a one-off query would
func (condenser *Condenser) Condense(history []protocol.Turn, question string) string {
	trimmed := strings.TrimSpace(question)
	if trimmed == "" || len(history) == 0 {
		return trimmed
	}

	response, err := complete(condenser.BaseURL, condenser.Model, condenser.Timeout, buildCondensePrompt(history, trimmed))
	if err != nil {
		return trimmed
	}
	standalone := parseStandalone(response)
	if standalone == "" || len(standalone) > maxStandaloneChars {
		return trimmed
	}
	return standalone
}

func buildCondensePrompt(history []protocol.Turn, question string) string {
	if len(history) > condenseTurns {
		history = history[len(history)-condenseTurns:]
	}

	var builder strings.Builder
	builder.WriteString("Rewrite the user's latest message as a standalone search question.\n\n")
	builder.WriteString("Rules:\n")
	builder.WriteString("- Replace pronouns and vague references (it, that, the second one) with what they refer to in the conversation.\n")
	builder.WriteString("- If the latest message names something the conversation never mentions, keep that name exactly as written. Never swap it for something else from the conversation.\n")
	builder.WriteString("- Keep everything the latest message asks about.\n")
	builder.WriteString("- If the latest message already stands alone, repeat it unchanged.\n")
	builder.WriteString("- Output only the question on one line. Do not answer it.\n\n")

	builder.WriteString("CONVERSATION:\n")
	for _, turn := range history {
		content := strings.TrimSpace(turn.Content)
		if len(content) > condenseTurnChars {
			content = strings.ToValidUTF8(content[:condenseTurnChars], "") + "..."
		}
		speaker := "User"
		if turn.Role == protocol.RoleAssistant {
			speaker = "Assistant"
		}
		builder.WriteString(speaker + ": " + content + "\n")
	}

	builder.WriteString("\nLATEST MESSAGE: " + question + "\nSTANDALONE QUESTION:")
	return builder.String()
}

// parseStandalone takes the first real line of the completion, dropping the label and
// quotes that small models add anyway
func parseStandalone(response string) string {
	for _, line := range strings.Split(response, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "STANDALONE QUESTION:")
		line = strings.TrimSpace(line)
		line = strings.Trim(line, "\"'`")
		line = strings.TrimSpace(line)
		if line != "" {
			return line
		}
	}
	return ""
}
