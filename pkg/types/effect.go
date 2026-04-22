package types

// Effect is what the policy engine returns. EffectRequireConfirmation
// blocks until a human answers via ChannelService.Prompt; timeout
// and denial both fail closed.
type Effect string

const (
	EffectAllow               Effect = "allow"
	EffectDeny                Effect = "deny"
	EffectRequireConfirmation Effect = "require_confirmation"
)

func (e Effect) IsValid() bool {
	switch e {
	case EffectAllow, EffectDeny, EffectRequireConfirmation:
		return true
	}
	return false
}
