package cache

import (
	"strings"
	"time"
)

// Policy decides three things about a request: whether it may be cached at all, how
// close a stored entry has to be before it can be served, and how long that entry stays
// good.
type Policy struct {
	// Thresholds is the minimum cosine similarity for a hit, per class. Higher is
	// stricter.
	Thresholds map[Class]float64

	// TTLs is how long an entry of each class stays valid.
	TTLs map[Class]time.Duration

	// VolatileTTL applies to prompts that ask about now. Zero, the default, means they
	// are not cached at all.
	VolatileTTL time.Duration

	// MaxEntries caps the cache. Past it, least-recently-used entries are evicted.
	MaxEntries int
}

// DefaultPolicy is set from measurement, not from taste. Run `engrex-cache calibrate`
// to reproduce the numbers below.
//
// The similarity a threshold has to clear is a property of the embedding model and
// nothing else. The guide suggests starting at 0.95, which is reasonable for OpenAI's
// embeddings, where even unrelated text scores high. Engrex embeds with
// nomic-embed-text, where the geometry is completely different: over the workload's
// hand-written paraphrases, two wordings of the same question score 0.73 at the median
// and two different questions score 0.48. At 0.95 only 5.6% of genuine rewordings clear
// the bar, which is a cache that does not work.
//
// 0.70 is the highest score seen between two different questions (0.65) plus a margin.
// It serves 56% of rewordings and, on that sample, nothing wrong. The distributions do
// overlap — the weakest paraphrase scores 0.50, below the closest unrelated pair — so no
// threshold catches everything, and this is deliberately set on the safe side of the
// overlap: a miss only costs time, while a false hit is a confident answer to a question
// nobody asked.
func DefaultPolicy() Policy {
	return Policy{
		Thresholds: map[Class]float64{
			ClassAnswer:  0.70,
			ClassRerank:  0.70,
			ClassRewrite: 0.70,

			// Held higher than the rest. The others compare questions; this compares
			// claims pulled out of one answer, which are short, similarly worded, and
			// about the same passages — the case where two different statements look
			// most alike. Verification earns most of its keep from exact repeats anyway.
			ClassVerify: 0.85,

			ClassEmbed: exactOnly,
			ClassOther: 0.75,
		},
		TTLs: map[Class]time.Duration{
			// A stored vector is what the model returns for that exact input, and the
			// model is part of the key, so this only ever expires to bound disk use.
			ClassEmbed: 30 * 24 * time.Hour,

			// Deterministic given their inputs, so they stay valid until the prompt
			// builders themselves change — which invalidation handles, not the clock.
			ClassRerank:  7 * 24 * time.Hour,
			ClassRewrite: 7 * 24 * time.Hour,
			ClassVerify:  7 * 24 * time.Hour,

			ClassAnswer: 24 * time.Hour,
			ClassOther:  24 * time.Hour,
		},
		// Not cached at all rather than cached briefly. The guide offers either, but
		// this corpus changes underneath the cache continuously — the watcher ingests
		// whatever lands in ~/Engrex — and "what did I save today" is precisely the
		// question a stale answer is most visibly wrong about. These queries are rare,
		// so bypassing them costs almost no hit rate. Set it to an hour if you would
		// rather have the speed.
		VolatileTTL: 0,
		MaxEntries:  10000,
	}
}

// exactOnly is the threshold for classes that must never match approximately. No two
// distinct vectors reach it, so it can only be satisfied by the exact-key path.
const exactOnly = 1.1

// Threshold is the similarity a stored entry must reach to be served for this class.
func (policy Policy) Threshold(class Class) float64 {
	if threshold, set := policy.Thresholds[class]; set {
		return threshold
	}
	return policy.Thresholds[ClassOther]
}

// ApproximateAllowed reports whether this class may be served on similarity at all.
//
// Embeddings may not. Serving a near neighbour's vector would put a subtly wrong
// direction into the index, and every distance threshold in Engrex is calibrated against
// vectors the model actually produced. It would not fail loudly; retrieval would just
// get quietly worse.
func (policy Policy) ApproximateAllowed(class Class) bool {
	return policy.Threshold(class) <= 1
}

// TTL is how long a response to this prompt stays good.
func (policy Policy) TTL(class Class, prompt string) time.Duration {
	if IsVolatile(prompt) {
		return policy.VolatileTTL
	}
	if ttl, set := policy.TTLs[class]; set {
		return ttl
	}
	return policy.TTLs[ClassOther]
}

// Cacheable reports whether a response to this prompt may be stored at all.
func (policy Policy) Cacheable(class Class, prompt string) bool {
	return policy.TTL(class, prompt) > 0
}

// volatileTerms mark a question whose right answer changes with the clock. Matching on
// words rather than asking a model, because a classifier call per request would cost
// exactly what the cache is here to save.
var volatileTerms = []string{
	"today", "yesterday", "tomorrow", "right now", "just now",
	"this week", "this month", "this year", "so far",
	"latest", "most recent", "currently", "current time", "up to date",
}

// IsVolatile reports whether a prompt asks about the present.
//
// Only the semantic part should be passed in. Running this over a whole answer prompt
// would match on the retrieved passages, and one saved note containing the word "latest"
// would make every question about that document uncacheable.
func IsVolatile(prompt string) bool {
	lowered := strings.ToLower(prompt)
	for _, term := range volatileTerms {
		if strings.Contains(lowered, term) {
			return true
		}
	}
	return false
}
