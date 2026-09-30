package cache

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

// Ollama speaks the subset of the Ollama API that Engrex uses: generation and
// embeddings. Everything else the proxy passes through untouched.
type Ollama struct {
	BaseURL string
	Client  *http.Client
}

// NewOllama returns a provider pointed at an Ollama server.
//
// No client timeout. Engrex's own calls set their own — 60s for reranking, 45s for
// rewriting, 30s for verification — and a proxy that timed out sooner than its caller
// would turn a slow generation into a failure that looks like the cache's fault.
func NewOllama(baseURL string) *Ollama {
	return &Ollama{
		BaseURL: strings.TrimRight(baseURL, "/"),
		Client:  &http.Client{},
	}
}

func (provider *Ollama) Name() string { return "ollama" }

const (
	generateEndpoint = "/api/generate"
	chatEndpoint     = "/api/chat"
	tagsEndpoint     = "/api/tags"
)

func (provider *Ollama) Handles(path string) bool {
	return path == generateEndpoint || path == chatEndpoint || path == embedEndpoint
}

// generateBody is the request shape Engrex sends. Kept as json.RawMessage where the
// content does not matter, so forwarding never re-encodes a field it does not understand.
type generateBody struct {
	Model   string         `json:"model"`
	Prompt  string         `json:"prompt"`
	Stream  *bool          `json:"stream"`
	Options map[string]any `json:"options"`
}

type embedBody struct {
	Model string          `json:"model"`
	Input json.RawMessage `json:"input"`
}

// chatBody is the /api/chat request shape. Engrex itself only ever calls /api/generate,
// but the Ollama desktop app and most third-party clients use this one, and a proxy that
// silently stopped caching for them would not be much of a drop-in.
type chatBody struct {
	Model    string         `json:"model"`
	Messages []chatMessage  `json:"messages"`
	Stream   *bool          `json:"stream"`
	Options  map[string]any `json:"options"`
}

type chatMessage struct {
	Role     string `json:"role"`
	Content  string `json:"content"`
	Thinking string `json:"thinking,omitempty"`

	// Images and ToolCalls are only read to notice them. The cache keys on text, so a
	// conversation carrying either cannot be matched safely and is passed through
	Images    []json.RawMessage `json:"images,omitempty"`
	ToolCalls []json.RawMessage `json:"tool_calls,omitempty"`
}

// renderChat flattens a conversation into the one string the splitter works on.
//
// The layout is deliberate: the last message ends up as its own trailing paragraph, so
// TailSplitter treats it as the question and everything above it — the system prompt and
// the conversation so far — as context that has to match exactly. That is the same rule
// the guide asks for when it says two identical user prompts under different system
// prompts must not share an entry, arrived at without a special case.
func renderChat(messages []chatMessage) string {
	var builder strings.Builder
	for index, message := range messages {
		if index > 0 {
			builder.WriteString("\n\n")
		}
		builder.WriteString(message.Role)
		builder.WriteString(": ")
		if message.Thinking != "" {
			builder.WriteString("[thinking] ")
			builder.WriteString(message.Thinking)
			builder.WriteString(" [/thinking] ")
		}
		builder.WriteString(message.Content)
	}
	return builder.String()
}

// Fields each endpoint's parser reads itself. Anything else in the body is either keyed
// through Request.Variant or, if it is in unreplayableFields, stops the request being
// cached. keep_alive is here because it only decides how long Ollama keeps the model
// loaded and never changes the output
var (
	generateFields = map[string]bool{"model": true, "prompt": true, "stream": true, "options": true, "keep_alive": true}
	chatFields     = map[string]bool{"model": true, "messages": true, "stream": true, "options": true, "keep_alive": true}
	embedFields    = map[string]bool{"model": true, "input": true, "keep_alive": true}
)

// unreplayableFields make a request impossible to serve from cache when they carry a
// value. Images are not part of the text the cache keys on, so two different pictures
// with one caption would collide. Tools can make the model answer with a tool call, which
// the payload has no field for. And context is Ollama's token state from an earlier
// generate call, far too large and opaque to key on usefully
var unreplayableFields = map[string]bool{"images": true, "tools": true, "context": true}

// variantOf renders every field of the body its parser does not read, so a request
// carrying a system prompt, a format, a template or a thinking switch never shares an
// entry with one that does not. It reports false when the request cannot be cached at all
func variantOf(body []byte, handled map[string]bool) (string, bool, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return "", false, err
	}

	extra := make(map[string]any)
	for name, raw := range fields {
		if handled[name] {
			continue
		}
		var value any
		if err := json.Unmarshal(raw, &value); err != nil {
			return "", false, err
		}
		if isEmptyValue(value) {
			continue
		}
		if unreplayableFields[name] {
			return "", false, nil
		}
		extra[name] = value
	}
	if len(extra) == 0 {
		return "", true, nil
	}

	rendered, err := json.Marshal(extra)
	if err != nil {
		return "", false, err
	}
	return string(rendered), true, nil
}

// isEmptyValue treats null, "", [] and {} as absent, which is how Ollama treats them, so a
// client that always sends "images": [] still gets cached
func isEmptyValue(value any) bool {
	switch typed := value.(type) {
	case nil:
		return true
	case string:
		return typed == ""
	case []any:
		return len(typed) == 0
	case map[string]any:
		return len(typed) == 0
	}
	return false
}

func (provider *Ollama) ParseRequest(path string, body []byte) (Request, bool, error) {
	switch path {
	case generateEndpoint:
		var decoded generateBody
		if err := json.Unmarshal(body, &decoded); err != nil {
			return Request{}, false, err
		}
		variant, cacheable, err := variantOf(body, generateFields)
		if err != nil || !cacheable {
			return Request{}, false, err
		}
		// Ollama streams unless told otherwise, so an absent field means true.
		stream := true
		if decoded.Stream != nil {
			stream = *decoded.Stream
		}
		return Request{
			Endpoint: path,
			Model:    decoded.Model,
			Text:     decoded.Prompt,
			Options:  decoded.Options,
			Variant:  variant,
			Stream:   stream,
		}, true, nil

	case chatEndpoint:
		var decoded chatBody
		if err := json.Unmarshal(body, &decoded); err != nil {
			return Request{}, false, err
		}
		if len(decoded.Messages) == 0 {
			return Request{}, false, nil
		}
		for _, message := range decoded.Messages {
			if len(message.Images) > 0 || len(message.ToolCalls) > 0 {
				return Request{}, false, nil
			}
		}
		variant, cacheable, err := variantOf(body, chatFields)
		if err != nil || !cacheable {
			return Request{}, false, err
		}
		stream := true
		if decoded.Stream != nil {
			stream = *decoded.Stream
		}
		return Request{
			Endpoint: path,
			Model:    decoded.Model,
			Text:     renderChat(decoded.Messages),
			Options:  decoded.Options,
			Variant:  variant,
			Stream:   stream,
		}, true, nil

	case embedEndpoint:
		var decoded embedBody
		if err := json.Unmarshal(body, &decoded); err != nil {
			return Request{}, false, err
		}
		// Ollama accepts a batch here. Engrex always sends one string, and caching a
		// batch would need a key per element, so batches are passed straight through
		// rather than half-handled.
		var input string
		if err := json.Unmarshal(decoded.Input, &input); err != nil {
			return Request{}, false, nil
		}
		// truncate, dimensions and options all change the vector that comes back
		variant, cacheable, err := variantOf(body, embedFields)
		if err != nil || !cacheable {
			return Request{}, false, err
		}
		return Request{Endpoint: path, Model: decoded.Model, Text: input, Variant: variant}, true, nil
	}
	return Request{}, false, fmt.Errorf("cache: ollama provider does not handle %s", path)
}

// generateFrame is one NDJSON line of a streamed generation, and also the whole body of a
// non-streamed one.
type generateFrame struct {
	Model     string `json:"model,omitempty"`
	CreatedAt string `json:"created_at,omitempty"`
	Response  string `json:"response"`
	Done      bool   `json:"done"`

	// Thinking is where a reasoning model such as qwen3 puts its reasoning, separate from
	// Response. Measured on Ollama 0.30.8: a qwen3 generation cut off by num_predict can
	// come back with an empty response and everything in here, so dropping this field
	// would cache and replay an empty answer
	Thinking string `json:"thinking,omitempty"`

	// Message is where /api/chat puts the text, instead of Response.
	Message *chatMessage `json:"message,omitempty"`

	DoneReason      string `json:"done_reason,omitempty"`
	TotalDuration   int64  `json:"total_duration,omitempty"`
	PromptEvalCount int    `json:"prompt_eval_count,omitempty"`
	EvalCount       int    `json:"eval_count,omitempty"`

	// Error is how Ollama reports a failure mid-stream. A response carrying one is never
	// cached.
	Error string `json:"error,omitempty"`
}

// text is the completion carried by a frame, from whichever field this endpoint uses.
func (frame generateFrame) text() string {
	if frame.Message != nil {
		return frame.Message.Content
	}
	return frame.Response
}

// thinking is the reasoning carried by a frame, from whichever field this endpoint uses
func (frame generateFrame) thinking() string {
	if frame.Message != nil {
		return frame.Message.Thinking
	}
	return frame.Thinking
}

// UpstreamError is a non-200 reply from the provider. The proxy relays it with its own
// status and body, so a caller asking for a model that is not pulled still hears Ollama's
// 404 "model not found" rather than a generic 502 from the cache
type UpstreamError struct {
	StatusCode  int
	ContentType string
	Body        []byte
}

func (upstreamError *UpstreamError) Error() string {
	return fmt.Sprintf("cache: upstream returned %d: %s", upstreamError.StatusCode, string(bytes.TrimSpace(upstreamError.Body)))
}

func (provider *Ollama) Forward(path string, body []byte, sink io.Writer) (Payload, error) {
	response, err := provider.Client.Post(provider.BaseURL+path, "application/json", bytes.NewReader(body))
	if err != nil {
		return Payload{}, err
	}
	defer response.Body.Close() //nolint:errcheck

	if response.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(response.Body)
		return Payload{}, &UpstreamError{
			StatusCode:  response.StatusCode,
			ContentType: response.Header.Get("Content-Type"),
			Body:        raw,
		}
	}

	if path == embedEndpoint {
		return provider.forwardEmbed(response.Body, sink)
	}
	return provider.forwardGenerate(response.Body, sink)
}

func (provider *Ollama) forwardEmbed(body io.Reader, sink io.Writer) (Payload, error) {
	raw, err := io.ReadAll(body)
	if err != nil {
		return Payload{}, err
	}
	if sink != nil {
		if _, err := sink.Write(raw); err != nil {
			return Payload{}, err
		}
	}

	var decoded struct {
		Embeddings [][]float32 `json:"embeddings"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		return Payload{}, err
	}
	if len(decoded.Embeddings) == 0 {
		return Payload{}, fmt.Errorf("cache: upstream returned no embeddings")
	}
	return Payload{Embedding: decoded.Embeddings[0]}, nil
}

// forwardGenerate relays the response to the caller while assembling it for the cache.
//
// The frames go out as they arrive — streaming is what makes a slow local model bearable,
// and buffering the whole answer to cache it would take that away. Nothing is stored
// unless a frame arrives with done set and no error, so a stream that dies halfway is
// served to the caller as far as it got and then forgotten.
func (provider *Ollama) forwardGenerate(body io.Reader, sink io.Writer) (Payload, error) {
	reader := bufio.NewReader(body)
	var assembled, reasoning strings.Builder
	var final generateFrame
	completed := false
	replayable := true

	for {
		line, err := reader.ReadBytes('\n')
		if len(line) > 0 {
			if sink != nil {
				if _, writeErr := sink.Write(line); writeErr != nil {
					return Payload{}, writeErr
				}
			}
			var frame generateFrame
			if json.Unmarshal(bytes.TrimSpace(line), &frame) == nil {
				if frame.Error != "" {
					return Payload{}, fmt.Errorf("cache: upstream error: %s", frame.Error)
				}
				assembled.WriteString(frame.text())
				reasoning.WriteString(frame.thinking())
				if frame.Message != nil && len(frame.Message.ToolCalls) > 0 {
					replayable = false
				}
				if frame.Done {
					final = frame
					completed = true
				}
			}
		}
		if err != nil {
			if err == io.EOF {
				break
			}
			return Payload{}, err
		}
	}

	if !completed {
		return Payload{}, fmt.Errorf("cache: the response ended before it was done")
	}
	return Payload{
		Text:        assembled.String(),
		Thinking:    reasoning.String(),
		Meta:        metaFrom(final),
		Uncacheable: !replayable,
	}, nil
}

// metaFrom keeps the provider's trailing numbers so a replayed response still reports
// them. Ollama's context array is deliberately dropped: nothing in Engrex reads it, and
// it is large enough on its own to push a replayed frame past the 64KB line limit that
// bufio.Scanner applies by default — which the caller in internal/rag uses.
func metaFrom(frame generateFrame) map[string]any {
	meta := map[string]any{}
	if frame.Model != "" {
		meta["model"] = frame.Model
	}
	if frame.DoneReason != "" {
		meta["done_reason"] = frame.DoneReason
	}
	if frame.TotalDuration != 0 {
		meta["total_duration"] = frame.TotalDuration
	}
	if frame.PromptEvalCount != 0 {
		meta["prompt_eval_count"] = frame.PromptEvalCount
	}
	if frame.EvalCount != 0 {
		meta["eval_count"] = frame.EvalCount
	}
	return meta
}

// replayChunk is how much cached text goes into one NDJSON frame on a hit.
//
// Chunked rather than emitted whole because the caller in internal/rag reads the stream
// with a default bufio.Scanner, which gives up silently on any line over 64KB. A cached
// answer replayed as a single line would truncate without an error for exactly the long
// answers the cache is most worth having.
const replayChunk = 2048

func (provider *Ollama) WriteHit(writer http.ResponseWriter, request Request, payload Payload) error {
	if request.Endpoint == embedEndpoint {
		writer.Header().Set("Content-Type", "application/json")
		return json.NewEncoder(writer).Encode(map[string]any{
			"model":      request.Model,
			"embeddings": [][]float32{payload.Embedding},
		})
	}

	chat := request.Endpoint == chatEndpoint

	if !request.Stream {
		writer.Header().Set("Content-Type", "application/json")
		frame := hitFrame(request, payload)
		setText(frame, chat, payload.Text, payload.Thinking)
		frame["done"] = true
		return json.NewEncoder(writer).Encode(frame)
	}

	writer.Header().Set("Content-Type", "application/x-ndjson")
	encoder := json.NewEncoder(writer)
	flusher, flushable := writer.(http.Flusher)

	// Reasoning goes out before the answer, the order the model produced them in
	emit := func(text string, isThinking bool) error {
		for offset := 0; offset < len(text); offset += replayChunk {
			end := min(offset+replayChunk, len(text))
			frame := map[string]any{
				"model":      request.Model,
				"created_at": time.Now().UTC().Format(time.RFC3339Nano),
				"done":       false,
			}
			if isThinking {
				setText(frame, chat, "", text[offset:end])
			} else {
				setText(frame, chat, text[offset:end], "")
			}
			if err := encoder.Encode(frame); err != nil {
				return err
			}
			if flushable {
				flusher.Flush()
			}
		}
		return nil
	}
	if err := emit(payload.Thinking, true); err != nil {
		return err
	}
	if err := emit(payload.Text, false); err != nil {
		return err
	}

	frame := hitFrame(request, payload)
	setText(frame, chat, "", "")
	frame["done"] = true
	if err := encoder.Encode(frame); err != nil {
		return err
	}
	if flushable {
		flusher.Flush()
	}
	return nil
}

// setText puts the completion and any reasoning in whichever fields the endpoint the
// caller used expects.
func setText(frame map[string]any, chat bool, text, thinking string) {
	if chat {
		message := map[string]any{"role": "assistant", "content": text}
		if thinking != "" {
			message["thinking"] = thinking
		}
		frame["message"] = message
		return
	}
	frame["response"] = text
	if thinking != "" {
		frame["thinking"] = thinking
	}
}

// hitFrame builds the final frame of a replay, carrying the metadata from the one time
// this really was generated.
func hitFrame(request Request, payload Payload) map[string]any {
	frame := map[string]any{
		"model":      request.Model,
		"created_at": time.Now().UTC().Format(time.RFC3339Nano),
	}
	for name, value := range payload.Meta {
		frame[name] = value
	}
	if _, stated := frame["done_reason"]; !stated {
		frame["done_reason"] = "stop"
	}
	return frame
}
