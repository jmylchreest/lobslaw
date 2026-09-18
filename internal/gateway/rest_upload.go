package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jmylchreest/lobslaw/internal/ids"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

const (
	restUploadMaxBytes    int64 = 32 << 20
	restUploadTotalBytes  int64 = 256 << 20
	restUploadMaxFiles          = 128
	restUploadTTL               = time.Hour
	restMessageMaxUploads       = 16
)

var errUploadUnavailable = errors.New("upload unavailable or expired")
var errUploadCapacity = errors.New("upload staging capacity reached; retry later")

// restUploads holds owner-bound, node-local staging files. References are
// process-local, and active turns pin their files against expiry cleanup.
type restUploads struct {
	mu        sync.Mutex
	root, dir string
	entries   map[string]*restUpload
	used      int64 // includes reservations for uploads in progress
	pending   int
	closed    bool
}
type restUpload struct {
	owner      string
	attachment types.Attachment
	expires    time.Time
	active     int
}
type uploadResponse struct {
	UploadID  string    `json:"upload_id"`
	MimeType  string    `json:"mime_type"`
	Size      int64     `json:"size"`
	ExpiresAt time.Time `json:"expires_at"`
}

func newRESTUploads(root string) *restUploads {
	if root == "" {
		root = DefaultIncomingDownloadDir
	}
	return &restUploads{root: root, entries: make(map[string]*restUpload)}
}

// reserve budgets the entire possible upload before reading any bytes, so
// simultaneous unknown-length requests cannot oversubscribe the disk budget.
func (u *restUploads) reserve() (string, error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	if u.closed {
		return "", errUploadUnavailable
	}
	if u.used+restUploadMaxBytes > restUploadTotalBytes || len(u.entries)+u.pending >= restUploadMaxFiles {
		return "", errUploadCapacity
	}
	if u.dir == "" {
		if err := os.MkdirAll(u.root, 0o700); err != nil {
			return "", err
		}
		dir, err := os.MkdirTemp(u.root, "rest-uploads-")
		if err != nil {
			return "", err
		}
		u.dir = dir
	}
	u.used += restUploadMaxBytes
	u.pending++
	return u.dir, nil
}

// finish publishes only a completely written, closed file. Failed requests
// release their reservation and remove the partial file before returning.
func (u *restUploads) finish(id string, entry *restUpload, path string) error {
	u.mu.Lock()
	defer u.mu.Unlock()
	u.pending--
	u.used -= restUploadMaxBytes
	if entry == nil || u.closed {
		if path != "" {
			if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
				// Retain the reservation as an expired entry for cleanup retry.
				// Failed deletion must not make occupied disk look available.
				u.used += restUploadMaxBytes
				u.entries[id] = &restUpload{attachment: types.Attachment{LocalPath: path, Size: int(restUploadMaxBytes)}}
				return err
			}
		}
		if u.closed && len(u.entries) == 0 && u.pending == 0 && u.dir != "" {
			_ = os.Remove(u.dir)
		}
		if u.closed {
			return errUploadUnavailable
		}
		return nil
	}
	u.used += int64(entry.attachment.Size)
	u.entries[id] = entry
	return nil
}

func (u *restUploads) acquire(owner string, ids []string, now time.Time) ([]types.Attachment, func(), error) {
	u.mu.Lock()
	defer u.mu.Unlock()
	entries := make([]*restUpload, 0, len(ids))
	atts := make([]types.Attachment, 0, len(ids))
	seen := make(map[string]bool)
	for _, id := range ids {
		e := u.entries[id]
		if u.closed || e == nil || e.owner != owner || !now.Before(e.expires) || seen[id] {
			return nil, nil, errUploadUnavailable
		}
		seen[id] = true
		entries = append(entries, e)
		atts = append(atts, e.attachment)
	}
	for _, e := range entries {
		e.active++
	}
	var once sync.Once
	release := func() {
		once.Do(func() {
			u.mu.Lock()
			for _, e := range entries {
				e.active--
			}
			u.mu.Unlock()
			u.sweep(time.Now())
		})
	}
	return atts, release, nil
}

func (u *restUploads) sweep(now time.Time) {
	u.mu.Lock()
	defer u.mu.Unlock()
	for id, e := range u.entries {
		if e.active != 0 || (!u.closed && now.Before(e.expires)) {
			continue
		}
		if err := os.Remove(e.attachment.LocalPath); err != nil && !os.IsNotExist(err) {
			continue
		}
		u.used -= int64(e.attachment.Size)
		delete(u.entries, id)
	}
	if u.closed && len(u.entries) == 0 && u.pending == 0 && u.dir != "" {
		_ = os.Remove(u.dir)
	}
}
func (u *restUploads) close() {
	u.mu.Lock()
	u.closed = true
	u.mu.Unlock()
	u.sweep(time.Now())
}
func (u *restUploads) run(ctx context.Context) {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			u.sweep(now)
		}
	}
}

// uploadMediaType selects a canonical suffix; client filenames and paths are
// never used. Content-Type describes the media, not a guarantee of validity;
// the configured vision/transcription tool still validates the file itself.
func uploadMediaType(header string) (string, string, types.AttachmentKind) {
	typ, _, err := mime.ParseMediaType(header)
	if err != nil {
		return "", "", ""
	}
	switch typ {
	case "image/png":
		return typ, ".png", types.AttachmentImage
	case "image/jpeg":
		return typ, ".jpg", types.AttachmentImage
	case "image/gif":
		return typ, ".gif", types.AttachmentImage
	case "image/webp":
		return typ, ".webp", types.AttachmentImage
	case "audio/ogg":
		return typ, ".ogg", types.AttachmentAudio
	case "audio/webm":
		return typ, ".webm", types.AttachmentAudio
	case "audio/mpeg":
		return typ, ".mp3", types.AttachmentAudio
	case "audio/mp4":
		return typ, ".m4a", types.AttachmentAudio
	case "audio/wav", "audio/x-wav":
		return "audio/wav", ".wav", types.AttachmentAudio
	case "audio/flac":
		return typ, ".flac", types.AttachmentAudio
	default:
		return "", "", ""
	}
}

func (s *Server) handleUpload(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	claims, err := s.authenticate(r)
	if err != nil || claims.UserID == "" {
		s.jsonErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if s.agent == nil {
		s.jsonErr(w, http.StatusServiceUnavailable, "agent not configured on this node")
		return
	}
	media, ext, kind := uploadMediaType(r.Header.Get("Content-Type"))
	if media == "" {
		s.jsonErr(w, http.StatusUnsupportedMediaType, "expected a supported image or audio Content-Type with a raw file body")
		return
	}
	if r.ContentLength > restUploadMaxBytes {
		s.jsonErr(w, http.StatusRequestEntityTooLarge, "upload exceeds 32 MiB")
		return
	}
	s.uploads.sweep(time.Now())
	dir, err := s.uploads.reserve()
	if err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, errUploadCapacity) {
			code = http.StatusTooManyRequests
		}
		s.jsonErr(w, code, "upload storage unavailable")
		return
	}
	id := "upload-" + ids.New()
	path := filepath.Join(dir, id+ext)
	settled := false
	cleanupPath := ""
	defer func() {
		if settled {
			return
		}
		if err := s.uploads.finish(id, nil, cleanupPath); err != nil {
			s.log.Warn("rest: upload cleanup failed", "err", err)
		}
	}()
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		s.jsonErr(w, http.StatusInternalServerError, "cannot create upload")
		return
	}
	cleanupPath = path
	r.Body = http.MaxBytesReader(w, r.Body, restUploadMaxBytes)
	n, copyErr := io.Copy(f, r.Body)
	closeErr := f.Close()
	if copyErr != nil {
		code := http.StatusBadRequest
		var tooLarge *http.MaxBytesError
		if errors.As(copyErr, &tooLarge) {
			code = http.StatusRequestEntityTooLarge
		}
		s.jsonErr(w, code, "upload incomplete or exceeds 32 MiB")
		return
	}
	if closeErr != nil {
		s.jsonErr(w, http.StatusInternalServerError, "cannot store upload")
		return
	}
	if n == 0 {
		s.jsonErr(w, http.StatusBadRequest, "empty upload")
		return
	}
	expires := time.Now().Add(restUploadTTL)
	entry := &restUpload{owner: claims.UserID, expires: expires, attachment: types.Attachment{Kind: kind, MimeType: media, Size: int(n), Reference: id, Filename: id + ext, LocalPath: path}}
	// Publish before the response, so a client can immediately reference the id.
	err = s.uploads.finish(id, entry, path)
	settled = true
	if err != nil {
		s.jsonErr(w, http.StatusServiceUnavailable, "upload storage closed")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(uploadResponse{UploadID: id, MimeType: media, Size: n, ExpiresAt: expires})
}
