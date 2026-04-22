package types

// TrustTier classifies an LLM provider's data-handling posture.
type TrustTier string

const (
	TrustLocal   TrustTier = "local"
	TrustPrivate TrustTier = "private"
	TrustPublic  TrustTier = "public"
)

func (t TrustTier) IsValid() bool {
	switch t {
	case TrustLocal, TrustPrivate, TrustPublic:
		return true
	}
	return false
}

// AtLeast reports whether t satisfies a floor set by other.
// Ordering: local > private > public.
func (t TrustTier) AtLeast(other TrustTier) bool {
	return t.rank() >= other.rank()
}

func (t TrustTier) rank() int {
	switch t {
	case TrustLocal:
		return 3
	case TrustPrivate:
		return 2
	case TrustPublic:
		return 1
	}
	return 0
}
