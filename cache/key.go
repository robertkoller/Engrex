package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

// Request is an incoming provider call, reduced to the parts that decide what comes
// back.
type Request struct {
	// Endpoint is the provider path, "/api/generate" or "/api/embed".
	Endpoint string

	Model string

	// Text is the prompt for a generation call, or the input for an embedding call.
	Text string

	// Options are the provider's sampling parameters.
	Options map[string]any

	// Variant is every other request field that changes what comes back — a system
	// prompt, a response format, a thinking switch — rendered canonically. Without it two
	// requests differing only in format:"json" would share an entry and one of them would
	// get the wrong shape of answer
	Variant string

	// Stream is how the caller wants the reply framed. Deliberately not part of the key
	// — see Entry.Payload.
	Stream bool
}

// Namespace is the exact-match bucket a request falls into: same model, same endpoint,
// same sampling parameters, same context. Similarity is only ever compared inside one
// bucket, so a stored answer can never be served for a prompt built from different
// passages.
func Namespace(request Request, context string) string {
	hash := sha256.New()
	for _, field := range []string{
		request.Endpoint,
		request.Model,
		canonicalOptions(request.Options),
		request.Variant,
		context,
	} {
		hash.Write([]byte(field))
		hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// ExactKey identifies a request that must match to the byte. Used for embeddings, which
// are never matched approximately, and as the fast path for a repeat of a prompt the
// cache has already seen verbatim.
func ExactKey(request Request) string {
	hash := sha256.New()
	for _, field := range []string{
		request.Endpoint,
		request.Model,
		canonicalOptions(request.Options),
		request.Variant,
		request.Text,
	} {
		hash.Write([]byte(field))
		hash.Write([]byte{0})
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// unkeyedOptions are the parameters left out of the key.
//
// num_ctx is the only one, and it is left out because Engrex derives it from the prompt's
// own length — len(prompt)/3 plus a per-caller headroom, clamped to [4096, 32768]. Two
// wordings of the same question can land on either side of a boundary and would then
// never match, which is exactly the case a semantic cache exists to catch. It is safe to
// drop because the context is already in the key byte for byte, so the only length that
// varies is the question's, and a window that fits one wording fits the other.
var unkeyedOptions = map[string]bool{"num_ctx": true}

// canonicalOptions renders the sampling parameters as a stable string. JSON rather than
// fmt's %v because encoding/json sorts map keys at every depth and quotes strings, where
// %v prints stop:["a","b"] and stop:["a b"] identically
func canonicalOptions(options map[string]any) string {
	if len(options) == 0 {
		return ""
	}

	keyed := make(map[string]any, len(options))
	for name, value := range options {
		if unkeyedOptions[name] {
			continue
		}
		keyed[name] = value
	}
	if len(keyed) == 0 {
		return ""
	}

	rendered, err := json.Marshal(keyed)
	if err != nil {
		// Only reachable with a value JSON cannot hold, which a decoded request never
		// contains. Fall back to something still deterministic
		return fmt.Sprintf("%v", keyed)
	}
	return string(rendered)
}
