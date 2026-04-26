package types

import (
	"fmt"
	"strings"
)

// AttachmentKind classifies media at a level downstream code can
// switch on without parsing MimeType. Stable + small.
type AttachmentKind string

const (
	AttachmentImage    AttachmentKind = "image"
	AttachmentVoice    AttachmentKind = "voice"
	AttachmentAudio    AttachmentKind = "audio"
	AttachmentVideo    AttachmentKind = "video"
	AttachmentDocument AttachmentKind = "document"
	AttachmentSticker  AttachmentKind = "sticker"
)

// Attachment is one media item flowing in or out through a channel.
// Reference is the channel-specific handle used to fetch the bytes
// (Telegram file_id, REST upload id, S3 key); LocalPath is filled
// after a downloader has materialised the bytes to disk so the
// agent's tools can read them.
type Attachment struct {
	Kind      AttachmentKind
	MimeType  string
	Size      int
	Width     int
	Height    int
	Duration  int
	Reference string
	Filename  string
	LocalPath string
}

// Describe renders an attachment as a short string for use in agent
// prompt decoration ("[user attached: image jpeg 12kb]") and in
// the friendly fallback reply when no preprocessor or tool is
// available for the modality.
func (a Attachment) Describe() string {
	parts := []string{string(a.Kind)}
	if a.MimeType != "" {
		parts = append(parts, a.MimeType)
	}
	if a.Filename != "" {
		parts = append(parts, "name="+a.Filename)
	}
	if a.Width > 0 && a.Height > 0 {
		parts = append(parts, fmt.Sprintf("%dx%d", a.Width, a.Height))
	} else if a.Duration > 0 {
		parts = append(parts, fmt.Sprintf("%ds", a.Duration))
	}
	if a.Size > 0 {
		parts = append(parts, fmt.Sprintf("%dB", a.Size))
	}
	if a.LocalPath != "" {
		parts = append(parts, "path="+a.LocalPath)
	}
	return strings.Join(parts, " ")
}
