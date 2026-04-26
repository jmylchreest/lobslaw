package gateway

import (
	"time"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// IncomingMessage is the channel-agnostic shape every gateway
// channel converts its native payload into before handing to the
// agent. Attachments use the shared types.Attachment so compute
// can consume them without a cycle.
type IncomingMessage struct {
	Text        string
	Caption     string
	Attachments []types.Attachment
	Channel     string
	UserID      string
	ChatID      string
	Timestamp   time.Time
}

// Re-exports so existing channel-side call sites don't have to
// import pkg/types directly. New code should prefer the types.
// references — these aliases stay for one cycle of cleanup.
type (
	Attachment     = types.Attachment
	AttachmentKind = types.AttachmentKind
)

const (
	AttachmentImage    = types.AttachmentImage
	AttachmentVoice    = types.AttachmentVoice
	AttachmentAudio    = types.AttachmentAudio
	AttachmentVideo    = types.AttachmentVideo
	AttachmentDocument = types.AttachmentDocument
	AttachmentSticker  = types.AttachmentSticker
)

// HasMedia reports whether at least one attachment is present.
func (m IncomingMessage) HasMedia() bool { return len(m.Attachments) > 0 }
