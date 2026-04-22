---
topic: lobslaw-lang-detection-library
decision: "pemistahl/lingua-go. Pure Go, accurate on short text and mixed-language text. Sample 1-2 sentences from inbound messages, detect language, reply in same language unless [soul.language.detect] = false. Rejected: franco/franc-go (less accurate on short text per lingua's benchmarks); google cld3 (CGO); running the detection through an LLM call (overkill, adds latency per turn)"
decided_by: johnm
date: 2026-04-22
---

# lobslaw-lang-detection-library

**Decision:** pemistahl/lingua-go. Pure Go, accurate on short text and mixed-language text. Sample 1-2 sentences from inbound messages, detect language, reply in same language unless [soul.language.detect] = false. Rejected: franco/franc-go (less accurate on short text per lingua's benchmarks); google cld3 (CGO); running the detection through an LLM call (overkill, adds latency per turn)

## Rationale

Reply-language-matching is a small-but-visible personality feature (Chinese user → Chinese reply through a British-culture soul). It needs to be cheap and local - running it through the LLM on every turn is wasted latency and cost. lingua-go is the only pure-Go library with accuracy on short text

