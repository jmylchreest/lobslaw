package gateway

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// slackFile is one file shared into a conversation.
//
// The download url is url_private, which is NOT public despite living
// on files.slack.com: it needs the bot token as a bearer, and fetching
// it without one returns an HTML sign-in page with HTTP 200. That is
// the trap this file exists to not fall into — the bytes would land on
// disk, the vision builtin would be handed a login page, and the only
// symptom is the model describing a screenshot it never saw.
type slackFile struct {
	ID         string `json:"id"`
	Name       string `json:"name,omitempty"`
	Mimetype   string `json:"mimetype,omitempty"`
	Filetype   string `json:"filetype,omitempty"`
	Size       int    `json:"size,omitempty"`
	URLPrivate string `json:"url_private,omitempty"`
}

// slackIncomingDir is where inbound files are materialised. Shared with
// Telegram so a file lands somewhere the vision/audio/pdf builtins
// already look, whichever channel it arrived on.
func (h *SlackHandler) incomingDir() string {
	if d := strings.TrimSpace(h.cfg.IncomingDir); d != "" {
		return d
	}
	return DefaultIncomingDownloadDir
}

// slackFilesToAttachments maps Slack's file metadata onto the
// channel-agnostic shape the agent consumes.
func slackFilesToAttachments(files []slackFile) []types.Attachment {
	if len(files) == 0 {
		return nil
	}
	out := make([]types.Attachment, 0, len(files))
	for _, f := range files {
		if f.URLPrivate == "" {
			continue
		}
		out = append(out, types.Attachment{
			Kind:      slackAttachmentKind(f),
			MimeType:  f.Mimetype,
			Size:      f.Size,
			Reference: f.URLPrivate,
			Filename:  f.Name,
		})
	}
	return out
}

// slackAttachmentKind classifies a file for the agent.
//
// Mimetype first because it is what the modality builtins dispatch on;
// filetype is Slack's own short tag and only consulted when the
// mimetype is missing or uselessly generic.
func slackAttachmentKind(f slackFile) types.AttachmentKind {
	mt := strings.ToLower(f.Mimetype)
	switch {
	case strings.HasPrefix(mt, "image/"):
		return types.AttachmentImage
	case strings.HasPrefix(mt, "audio/"):
		return types.AttachmentAudio
	case strings.HasPrefix(mt, "video/"):
		return types.AttachmentVideo
	}
	switch strings.ToLower(f.Filetype) {
	case "png", "jpg", "jpeg", "gif", "webp", "heic":
		return types.AttachmentImage
	case "mp3", "m4a", "wav", "ogg", "flac":
		return types.AttachmentAudio
	case "mp4", "mov", "webm", "mkv":
		return types.AttachmentVideo
	}
	return types.AttachmentDocument
}

// downloadAttachments fetches each file to <incoming dir>/<turn>/ and
// records the local path.
//
// Best-effort per file, matching the Telegram path: one failure logs
// and skips, and the agent still gets the rest. Returns an error only
// when the directory itself is unwritable, which is a config problem
// rather than a bad attachment.
func (h *SlackHandler) downloadAttachments(ctx context.Context, turnID string, im *IncomingMessage) error {
	if !im.HasMedia() {
		return nil
	}
	turnDir := filepath.Join(h.incomingDir(), turnID)
	if err := os.MkdirAll(turnDir, 0o755); err != nil {
		return fmt.Errorf("slack: prep download dir %q: %w", turnDir, err)
	}
	for i := range im.Attachments {
		a := &im.Attachments[i]
		if a.Reference == "" {
			continue
		}
		path, err := h.downloadOne(ctx, turnDir, a)
		if err != nil {
			h.log.Warn("slack: attachment download failed",
				"file", a.Filename, "kind", string(a.Kind), "err", err)
			continue
		}
		a.LocalPath = path
		h.log.Debug("slack: attachment downloaded",
			"file", a.Filename, "kind", string(a.Kind), "path", path, "bytes", a.Size)
	}
	return nil
}

// slackMaxUploadBytes caps an artifact this handler will send.
//
// A ceiling exists because the upload has to be buffered: Slack wants
// the exact byte count before it will hand out an upload url, and an
// ArtifactOpener is a reader with no length. 64MiB is far above a
// generated image or a TTS clip and far below anything that should
// arrive in a chat message.
const slackMaxUploadBytes = 64 << 20

// SendAttachments delivers the files a turn produced.
//
// Best-effort per attachment, matching Telegram: the reply text has
// already gone by this point, so a failure here costs the user the
// file rather than the whole answer.
func (h *SlackHandler) SendAttachments(ctx context.Context, channel, thread string, atts []types.Attachment, open ArtifactOpener) {
	if len(atts) == 0 {
		return
	}
	if open == nil {
		// Said out loud: a turn that generated audio and delivered
		// nothing is indistinguishable from one that did not generate
		// anything, and the model will have described a file the user
		// never received.
		h.log.Warn("slack: turn produced attachments but no artifact opener is wired; "+
			"the user will not receive them", "count", len(atts))
		return
	}
	for _, a := range atts {
		if err := h.sendAttachment(ctx, channel, thread, a, open); err != nil {
			h.log.Error("slack: attachment not delivered",
				"reference", a.Reference, "kind", a.Kind, "err", err)
		}
	}
}

// sendAttachment runs Slack's three-step external upload.
//
// Reserve a url, POST the bytes, then publish into the conversation.
// The file does not exist for the user until the third step: between
// two and three it is uploaded but unattached, so a failure there
// leaves a file nobody can see and the workspace still stores.
func (h *SlackHandler) sendAttachment(ctx context.Context, channel, thread string, a types.Attachment, open ArtifactOpener) error {
	rc, err := open(a.Reference)
	if err != nil {
		return fmt.Errorf("open %q: %w", a.Reference, err)
	}
	defer func() { _ = rc.Close() }()

	// Buffered because step one needs the exact length. The limit is
	// one byte over the cap so a file AT the cap still reads whole and
	// only a genuinely oversized one trips it.
	body, err := io.ReadAll(io.LimitReader(rc, slackMaxUploadBytes+1))
	if err != nil {
		return fmt.Errorf("read artifact %q: %w", a.Reference, err)
	}
	if len(body) > slackMaxUploadBytes {
		return fmt.Errorf("artifact %q exceeds the %d MiB upload cap", a.Reference, slackMaxUploadBytes>>20)
	}
	if len(body) == 0 {
		// Slack rejects a zero-length upload with an error naming the
		// filename, which sends the reader looking in the wrong place.
		return fmt.Errorf("artifact %q is empty", a.Reference)
	}

	name := a.Filename
	if name == "" {
		name = filepath.Base(a.Reference)
	}

	uploadURL, fileID, err := h.api.getUploadURL(ctx, name, len(body))
	if err != nil {
		return err
	}
	if err := h.api.uploadBytes(ctx, uploadURL, body); err != nil {
		return err
	}
	if err := h.api.completeUpload(ctx, fileID, name, channel, thread); err != nil {
		// Distinct from the two above: the bytes are in the workspace
		// and only the sharing failed, so this leaks a file rather than
		// losing one.
		return fmt.Errorf("uploaded but not shared (file %s): %w", fileID, err)
	}
	h.log.Debug("slack: attachment delivered",
		"reference", a.Reference, "kind", a.Kind, "bytes", len(body), "thread", thread)
	return nil
}

// slackFilePrefix returns a "F0123ABC-" prefix taken from a
// url_private, or "" when the reference does not carry one.
//
// Read from the URL rather than plumbed through types.Attachment: the
// id is already in the reference every inbound Slack file has, and one
// channel's identifier does not belong in the shared attachment type.
func slackFilePrefix(ref string) string {
	segs := strings.Split(ref, "/")
	// The last segment is the filename, and a file genuinely called
	// "FOO-bar.png" would otherwise donate its own prefix.
	if len(segs) > 1 {
		segs = segs[:len(segs)-1]
	}
	for _, seg := range segs {
		for part := range strings.SplitSeq(seg, "-") {
			if len(part) > 1 && part[0] == 'F' && isSlackIDBody(part[1:]) {
				return part + "-"
			}
		}
	}
	return ""
}

func isSlackIDBody(s string) bool {
	for _, r := range s {
		if !(r >= '0' && r <= '9') && !(r >= 'A' && r <= 'Z') {
			return false
		}
	}
	return true
}

func (h *SlackHandler) downloadOne(ctx context.Context, turnDir string, a *types.Attachment) (string, error) {
	getCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(getCtx, http.MethodGet, a.Reference, nil)
	if err != nil {
		return "", err
	}
	// The bearer is what makes this a download rather than a login
	// page. See slackFile.
	req.Header.Set("Authorization", "Bearer "+h.cfg.BotToken)

	resp, err := h.api.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch %q: %w", a.Filename, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch %q: HTTP %d", a.Filename, resp.StatusCode)
	}
	// Slack answers an unauthenticated or expired fetch with the
	// sign-in page and a 200, so status alone does not mean bytes.
	// Checking the content type is what turns that into a failure the
	// operator can see rather than a corrupt file the model describes.
	if ct := resp.Header.Get("Content-Type"); strings.HasPrefix(ct, "text/html") && !strings.HasPrefix(a.MimeType, "text/html") {
		return "", fmt.Errorf("fetch %q: got an HTML page, not the file — the bot token may lack files:read", a.Filename)
	}

	name := a.Filename
	if name == "" {
		name = sanitiseRef(a.Reference) + pickExtension(a)
	}
	// Prefixed with the Slack file id, because two files sharing a name
	// in one turn is ordinary — "screenshot.png" twice — and without it
	// the second silently overwrote the first, so the agent read one
	// image and was told about two.
	dst := filepath.Join(turnDir, slackFilePrefix(a.Reference)+filepath.Base(name))

	f, err := os.Create(dst)
	if err != nil {
		return "", fmt.Errorf("create %q: %w", dst, err)
	}
	defer func() { _ = f.Close() }()
	// Capped at the same ceiling as an outbound artifact, and for a
	// stronger reason: the size here is chosen by whoever dropped the
	// file into the channel, Slack's own per-file limit is 1GB, and the
	// only bound before this was a request timeout.
	n, err := io.Copy(f, io.LimitReader(resp.Body, slackMaxUploadBytes+1))
	if err != nil {
		_ = os.Remove(dst)
		return "", fmt.Errorf("write %q: %w", dst, err)
	}
	if n > slackMaxUploadBytes {
		_ = os.Remove(dst)
		return "", fmt.Errorf("fetch %q: exceeds the %d MiB cap", a.Filename, slackMaxUploadBytes>>20)
	}
	// Closed explicitly: the final flush can fail after io.Copy has
	// reported success, and swallowing that hands the caller a path to
	// a truncated file. Same reasoning as the Telegram downloader.
	if err := f.Close(); err != nil {
		_ = os.Remove(dst)
		return "", fmt.Errorf("write %q: %w", dst, err)
	}
	return dst, nil
}
