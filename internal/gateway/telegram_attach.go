package gateway

import (
	"bytes"
	"fmt"
	"io"
	"mime/multipart"
	"path/filepath"
	"strconv"

	"github.com/jmylchreest/lobslaw/internal/logging"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// A turn that generates audio has a last mile the text path does not.
// Without this the model gets a file path back and the user gets a
// sentence describing where a file they cannot reach was saved.
//
// The bytes come through an opener rather than a storage dependency:
// the gateway should not learn about mounts, backends or config, only
// how to ask for a reference and get a reader.

// ArtifactOpener resolves an attachment reference ("mount:path") to
// its bytes. Returns a reader the caller closes.
type ArtifactOpener func(reference string) (io.ReadCloser, error)

// telegramMethodFor picks the Bot API method and form field for a
// kind. Voice is not a synonym for audio: a voice note plays inline
// with a waveform, which is what someone who asked to *hear*
// something wants, whereas audio arrives as a track to open.
func telegramMethodFor(kind types.AttachmentKind) (method, field string) {
	switch kind {
	case types.AttachmentVoice:
		return "sendVoice", "voice"
	case types.AttachmentAudio:
		return "sendAudio", "audio"
	case types.AttachmentImage:
		return "sendPhoto", "photo"
	case types.AttachmentVideo:
		return "sendVideo", "video"
	default:
		return "sendDocument", "document"
	}
}

// SendAttachments delivers the files a turn produced.
//
// Best-effort per attachment: one that fails is logged and the rest
// still go. The reply text has already been sent by this point, so a
// failure here costs the user the file rather than the whole answer.
func (h *TelegramHandler) SendAttachments(chatID int64, atts []types.Attachment, open ArtifactOpener) {
	if open == nil {
		if len(atts) > 0 {
			h.log.Warn("telegram: turn produced attachments but no artifact opener is wired; "+
				"the user will not receive them", "count", len(atts))
		}
		return
	}
	for _, a := range atts {
		if err := h.sendAttachment(chatID, a, open); err != nil {
			h.log.Error("telegram: attachment not delivered",
				"reference", a.Reference, "kind", a.Kind, "err", err)
		}
	}
}

func (h *TelegramHandler) sendAttachment(chatID int64, a types.Attachment, open ArtifactOpener) (retErr error) {
	defer func() { retErr = logging.SafeError(retErr) }()
	method, field := telegramMethodFor(a.Kind)

	rc, err := open(a.Reference)
	if err != nil {
		return fmt.Errorf("open %q: %w", a.Reference, err)
	}
	defer func() { _ = rc.Close() }()

	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err := mw.WriteField("chat_id", strconv.FormatInt(chatID, 10)); err != nil {
		return err
	}
	name := a.Filename
	if name == "" {
		name = filepath.Base(a.Reference)
	}
	part, err := mw.CreateFormFile(field, name)
	if err != nil {
		return err
	}
	if _, err := io.Copy(part, rc); err != nil {
		return fmt.Errorf("read artifact: %w", err)
	}
	if err := mw.Close(); err != nil {
		return err
	}

	url := fmt.Sprintf("%s/bot%s/%s", h.base, h.cfg.BotToken, method)
	resp, err := h.client.Post(url, mw.FormDataContentType(), &body)
	if err != nil {
		return fmt.Errorf("POST %s: %w", method, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, telegramAPIErrorMaxBytes))
		return fmt.Errorf("%s non-2xx (HTTP %d): %s", method, resp.StatusCode, string(raw))
	}
	h.log.Debug("telegram: attachment delivered",
		"method", method, "reference", a.Reference, "bytes", a.Size)
	return nil
}
