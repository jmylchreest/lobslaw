package turn

import (
	"github.com/jmylchreest/lobslaw/internal/commandrisk"
	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// Request is the channel-facing description of one turn. Channels
// never hold a live budget object — Caps and Spent are enough for a
// local or remote runner to reconstruct one.
type Request struct {
	Message             string
	Claims              *types.Claims
	Principal           identity.Principal
	TurnID              string
	Channel             string
	ChannelID           string
	SharedConversation  bool
	Hint                string
	UserTimezone        string
	SystemPrompt        string
	Model               string
	ConversationHistory []Message
	ConversationSummary string
	RecalledContext     string
	Attachments         []types.Attachment
	Caps                BudgetCaps
	Spent               BudgetState
	BotID               string
	Stream              StreamObserver
}

// Response is the per-turn output a channel renders.
type Response struct {
	Reply                 string
	ToolCalls             []ToolInvocation
	Attachments           []types.Attachment
	Messages              []Message
	TurnStartIndex        int
	BudgetState           BudgetState
	NeedsConfirmation     bool
	ConfirmationAction    string
	ConfirmationResource  string
	ConfirmationGrantable bool
	ConfirmationLabels    []commandrisk.RiskLabel
	ConfirmationReason    string
}

// StreamObserver receives incremental output when a channel asked
// to stream. Nil on Request means the runner may ignore it.
type StreamObserver interface {
	OnStart(turnID string)
	OnDelta(text string)
	OnDone(*Response)
}
