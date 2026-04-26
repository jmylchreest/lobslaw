package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// IncomingDownloadDir is where channel handlers materialise inbound
// attachments. Inside /workspace so it lives in the operator's
// host-visible bind-mount + is reachable from MCP-spawned tools.
// Per-message subdir keeps unrelated turns isolated.
const IncomingDownloadDir = "/workspace/incoming"

// downloadAttachments fetches each attachment in im.Attachments
// from Telegram and stores it under IncomingDownloadDir/<turn>/.
// Mutates the slice in place to set LocalPath. Best-effort: a
// single failure logs + skips that attachment; the agent still
// gets the others. Returns nil unless the download dir itself
// is unwritable (a real config bug).
func (h *TelegramHandler) downloadAttachments(ctx context.Context, turnID string, im *IncomingMessage) error {
	if !im.HasMedia() {
		return nil
	}
	turnDir := filepath.Join(IncomingDownloadDir, turnID)
	if err := os.MkdirAll(turnDir, 0o755); err != nil {
		return fmt.Errorf("telegram: prep download dir %q: %w", turnDir, err)
	}
	for i := range im.Attachments {
		a := &im.Attachments[i]
		if a.Reference == "" {
			continue
		}
		path, err := h.downloadOne(ctx, turnDir, a)
		if err != nil {
			h.log.Warn("telegram: attachment download failed",
				"file_id", a.Reference,
				"kind", string(a.Kind),
				"err", err)
			continue
		}
		a.LocalPath = path
		h.log.Debug("telegram: attachment downloaded",
			"file_id", a.Reference,
			"kind", string(a.Kind),
			"path", path,
			"bytes", a.Size)
	}
	return nil
}

// downloadOne resolves a Telegram file_id to bytes-on-disk.
// getFile returns a server-side path; we then GET the file URL
// (/file/bot<TOKEN>/<path>) and write to turnDir/<file_id>.<ext>.
// Filename uses file_id (already unique) plus an extension picked
// from the upstream MimeType / Filename so MCP tools can sniff it.
func (h *TelegramHandler) downloadOne(ctx context.Context, turnDir string, a *types.Attachment) (string, error) {
	fileURL, err := h.resolveFileURL(ctx, a.Reference)
	if err != nil {
		return "", err
	}
	ext := pickExtension(a)
	dst := filepath.Join(turnDir, sanitiseRef(a.Reference)+ext)

	getCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(getCtx, http.MethodGet, fileURL, nil)
	if err != nil {
		return "", err
	}
	resp, err := h.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("fetch %q: %w", fileURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch %q: HTTP %d", fileURL, resp.StatusCode)
	}

	f, err := os.Create(dst)
	if err != nil {
		return "", fmt.Errorf("create %q: %w", dst, err)
	}
	defer f.Close()
	if _, err := io.Copy(f, resp.Body); err != nil {
		_ = os.Remove(dst)
		return "", fmt.Errorf("write %q: %w", dst, err)
	}
	return dst, nil
}

// resolveFileURL calls getFile to obtain the server-side path then
// constructs the full file URL. Telegram's API has a 20MB max for
// bot file downloads — we don't pre-check size; HTTP errors with
// "file is too big" are surfaced naturally.
func (h *TelegramHandler) resolveFileURL(ctx context.Context, fileID string) (string, error) {
	getCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	body, _ := json.Marshal(map[string]string{"file_id": fileID})
	url := fmt.Sprintf("%s/bot%s/getFile", h.base, h.cfg.BotToken)
	req, err := http.NewRequestWithContext(getCtx, http.MethodPost, url, strings.NewReader(string(body)))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := h.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("getFile: HTTP %d", resp.StatusCode)
	}
	var decoded struct {
		OK     bool `json:"ok"`
		Result struct {
			FilePath string `json:"file_path"`
		} `json:"result"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&decoded); err != nil {
		return "", fmt.Errorf("getFile decode: %w", err)
	}
	if !decoded.OK {
		return "", fmt.Errorf("getFile: %s", decoded.Description)
	}
	if decoded.Result.FilePath == "" {
		return "", fmt.Errorf("getFile: empty file_path")
	}
	// Telegram's file-download endpoint is at /file/<token>/<path>,
	// not /bot<token>/<path> — different URL space.
	return fmt.Sprintf("%s/file/bot%s/%s", h.base, h.cfg.BotToken, decoded.Result.FilePath), nil
}

func pickExtension(a *types.Attachment) string {
	if a.Filename != "" {
		if ext := filepath.Ext(a.Filename); ext != "" {
			return ext
		}
	}
	switch {
	case strings.HasPrefix(a.MimeType, "image/jpeg"):
		return ".jpg"
	case strings.HasPrefix(a.MimeType, "image/png"):
		return ".png"
	case strings.HasPrefix(a.MimeType, "image/webp"):
		return ".webp"
	case strings.HasPrefix(a.MimeType, "audio/ogg"), a.Kind == types.AttachmentVoice:
		return ".ogg"
	case strings.HasPrefix(a.MimeType, "audio/mpeg"):
		return ".mp3"
	case strings.HasPrefix(a.MimeType, "video/mp4"):
		return ".mp4"
	case a.Kind == types.AttachmentImage:
		return ".jpg"
	}
	return ""
}

// sanitiseRef strips path-traversal candidates from a Telegram
// file_id before using it as a filename component. file_ids are
// telegram-base64-ish but we defensively strip anything outside a
// safe alphabet.
func sanitiseRef(ref string) string {
	var b strings.Builder
	for _, r := range ref {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
	}
	if b.Len() == 0 {
		return "att"
	}
	return b.String()
}
