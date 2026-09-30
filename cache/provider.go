package cache

import (
	"fmt"
	"io"
	"net/http"
)

// Provider translates between the cache's normalized view of a call and one vendor's
// wire format.
//
// The cache itself knows nothing about any vendor: it deals in a prompt, a namespace and
// a payload. Everything vendor-shaped — which field holds the prompt, whether a stream is
// NDJSON or server-sent events, what a finished response looks like — lives behind this.
type Provider interface {
	Name() string

	// Handles reports whether this provider owns a path.
	Handles(path string) bool

	// ParseRequest turns an inbound body into the normalized form the cache keys on.
	// Reporting false means the request is understood but must not be cached — a batch
	// embedding call, say — and should be passed straight through.
	ParseRequest(path string, body []byte) (Request, bool, error)

	// Forward sends the request upstream. When the caller asked for a stream, frames are
	// written to sink as they arrive. Either way the assembled payload comes back, so
	// the same code path caches a streamed and a non-streamed response.
	Forward(path string, body []byte, sink io.Writer) (Payload, error)

	// WriteHit renders a stored payload in the shape this caller asked for.
	WriteHit(writer http.ResponseWriter, request Request, payload Payload) error
}

// Registry routes a request to the provider that owns its path.
type Registry struct {
	providers []Provider
}

func NewRegistry(providers ...Provider) *Registry {
	return &Registry{providers: providers}
}

// For returns the provider handling a path.
func (registry *Registry) For(path string) (Provider, bool) {
	for _, provider := range registry.providers {
		if provider.Handles(path) {
			return provider, true
		}
	}
	return nil, false
}

// unsupportedProvider is the shape the OpenAI and Anthropic providers will take. They are
// not implemented: nothing in Engrex can exercise them, and shipping an untested code
// path that claims to talk to a paid API is worse than shipping nothing. The interface is
// what makes adding one a self-contained job.
type unsupportedProvider struct {
	name   string
	prefix string
}

// OpenAIProvider and AnthropicProvider are placeholders. Routing reaches them; they
// refuse rather than pretend.
func OpenAIProvider() Provider {
	return unsupportedProvider{name: "openai", prefix: "/v1/chat/completions"}
}

func AnthropicProvider() Provider {
	return unsupportedProvider{name: "anthropic", prefix: "/v1/messages"}
}

func (provider unsupportedProvider) Name() string { return provider.name }

func (provider unsupportedProvider) Handles(path string) bool { return path == provider.prefix }

func (provider unsupportedProvider) ParseRequest(string, []byte) (Request, bool, error) {
	return Request{}, false, provider.err()
}

func (provider unsupportedProvider) Forward(string, []byte, io.Writer) (Payload, error) {
	return Payload{}, provider.err()
}

func (provider unsupportedProvider) WriteHit(http.ResponseWriter, Request, Payload) error {
	return provider.err()
}

func (provider unsupportedProvider) err() error {
	return fmt.Errorf("cache: the %s provider is not implemented", provider.name)
}
