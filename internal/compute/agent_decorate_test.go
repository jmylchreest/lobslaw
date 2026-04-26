package compute

import (
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

func TestDecorateWithAttachmentsAddsImageHint(t *testing.T) {
	t.Parallel()
	out := decorateWithAttachments("does this mean anything to you?", []types.Attachment{
		{Kind: types.AttachmentImage, MimeType: "image/jpeg", LocalPath: "/workspace/incoming/abc/x.jpg"},
	})
	if !strings.Contains(out, "[user attached") {
		t.Errorf("missing attachment block: %q", out)
	}
	if !strings.Contains(out, "read_image(path=") {
		t.Errorf("missing read_image hint for image attachment: %q", out)
	}
}

func TestDecorateWithAttachmentsNoHintForNonImage(t *testing.T) {
	t.Parallel()
	out := decorateWithAttachments("here's audio", []types.Attachment{
		{Kind: types.AttachmentVoice, MimeType: "audio/ogg", LocalPath: "/workspace/incoming/abc/v.ogg"},
	})
	if strings.Contains(out, "read_image") {
		t.Errorf("should not nudge read_image for voice: %q", out)
	}
}

func TestDecorateWithAttachmentsNoOpWhenEmpty(t *testing.T) {
	t.Parallel()
	out := decorateWithAttachments("hello", nil)
	if out != "hello" {
		t.Errorf("expected pass-through for empty attachments; got %q", out)
	}
}

func TestDecorateWithAttachmentsAddsAudioHint(t *testing.T) {
	t.Parallel()
	out := decorateWithAttachments("listen to this", []types.Attachment{
		{Kind: types.AttachmentVoice, MimeType: "audio/ogg", LocalPath: "/workspace/incoming/abc/v.ogg"},
	})
	if !strings.Contains(out, "read_audio(path=") {
		t.Errorf("missing read_audio hint for voice attachment: %q", out)
	}
}

func TestDecorateWithAttachmentsAddsPDFHint(t *testing.T) {
	t.Parallel()
	out := decorateWithAttachments("see attached", []types.Attachment{
		{Kind: types.AttachmentDocument, MimeType: "application/pdf", Filename: "report.pdf", LocalPath: "/workspace/incoming/abc/r.pdf"},
	})
	if !strings.Contains(out, "read_pdf(path=") {
		t.Errorf("missing read_pdf hint for PDF attachment: %q", out)
	}
}
