// Package gemini streams answers from Google's Gemini API. It's the opt-in cloud
// alternative to Ollama for the answer step only, so retrieved passages leave the
// machine when it's used and nothing else does.
package gemini

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

const DefaultBaseURL = "https://generativelanguage.googleapis.com/v1beta"

// maxOutputTokens is higher than Ollama's 400 because Gemini counts its thinking
// against the same limit, and running out mid thought leaves an empty answer
const maxOutputTokens = 4096

// IsModel reports whether a model name should be sent to Gemini rather than Ollama
func IsModel(model string) bool {
	return strings.HasPrefix(strings.TrimSpace(model), "gemini-")
}

type Client struct {
	APIKey  string
	Model   string
	BaseURL string
	HTTP    *http.Client
}

func New(apiKey, model string) *Client {
	return &Client{
		APIKey:  apiKey,
		Model:   model,
		BaseURL: DefaultBaseURL,
		HTTP:    &http.Client{Timeout: 3 * time.Minute},
	}
}

type part struct {
	Text    string `json:"text"`
	Thought bool   `json:"thought,omitempty"`
}

type streamChunk struct {
	Candidates []struct {
		Content struct {
			Parts []part `json:"parts"`
		} `json:"content"`
		FinishReason string `json:"finishReason"`
	} `json:"candidates"`
	PromptFeedback struct {
		BlockReason string `json:"blockReason"`
	} `json:"promptFeedback"`
	Error *apiError `json:"error"`
}

type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Status  string `json:"status"`
}

// Stream sends the prompt and writes the answer to out as it arrives, returning the
// full text too. Thought parts are skipped so only the answer is shown
func (client *Client) Stream(prompt string, out io.Writer) (string, error) {
	if client.APIKey == "" {
		return "", fmt.Errorf("no Gemini API key, add GEMINI_API_KEY to .env")
	}

	body, err := json.Marshal(map[string]any{
		"contents": []map[string]any{
			{"role": "user", "parts": []map[string]string{{"text": prompt}}},
		},
		"generationConfig": map[string]any{
			"maxOutputTokens": maxOutputTokens,
			// Grounded answers from given passages don't need much reasoning, and
			// low keeps latency close to the local model
			"thinkingConfig": map[string]any{"thinkingLevel": "low"},
		},
	})
	if err != nil {
		return "", err
	}

	url := fmt.Sprintf("%s/models/%s:streamGenerateContent?alt=sse", client.BaseURL, client.Model)
	request, err := http.NewRequest(http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	request.Header.Set("Content-Type", "application/json")
	// Header rather than ?key= so the key never shows up in a logged URL
	request.Header.Set("x-goog-api-key", client.APIKey)

	response, err := client.HTTP.Do(request)
	if err != nil {
		return "", fmt.Errorf("reaching Gemini: %w", err)
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(io.LimitReader(response.Body, 64*1024))
		return "", fmt.Errorf("gemini returned %d: %s", response.StatusCode, errorMessage(raw))
	}

	var answer strings.Builder
	finishReason := ""
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		data, isData := strings.CutPrefix(scanner.Text(), "data: ")
		if !isData || strings.TrimSpace(data) == "" {
			continue
		}

		var chunk streamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return answer.String(), fmt.Errorf("bad Gemini stream frame: %w", err)
		}
		if chunk.Error != nil {
			return answer.String(), fmt.Errorf("gemini failed mid-answer: %s", chunk.Error.Message)
		}
		if chunk.PromptFeedback.BlockReason != "" {
			return answer.String(), fmt.Errorf("gemini blocked the prompt: %s", chunk.PromptFeedback.BlockReason)
		}

		for _, candidate := range chunk.Candidates {
			for _, piece := range candidate.Content.Parts {
				if piece.Thought || piece.Text == "" {
					continue
				}
				fmt.Fprint(out, piece.Text)
				answer.WriteString(piece.Text)
			}
			if candidate.FinishReason != "" {
				finishReason = candidate.FinishReason
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return answer.String(), fmt.Errorf("reading the Gemini stream: %w", err)
	}

	// STOP is a normal finish. Anything else (MAX_TOKENS, SAFETY...) is worth saying
	// since the answer is cut short or missing
	if finishReason != "" && finishReason != "STOP" {
		fmt.Fprintf(out, "\n[Gemini stopped early: %s]", finishReason)
	}
	return answer.String(), nil
}

// errorMessage pulls the message out of Gemini's JSON error body, falling back to the
// raw text
func errorMessage(raw []byte) string {
	var wrapped struct {
		Error apiError `json:"error"`
	}
	if err := json.Unmarshal(raw, &wrapped); err == nil && wrapped.Error.Message != "" {
		return wrapped.Error.Message
	}
	return strings.TrimSpace(string(raw))
}
