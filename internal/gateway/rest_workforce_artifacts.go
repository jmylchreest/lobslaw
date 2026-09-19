package gateway

import (
	"context"
	"io"
	"mime"
	"net/http"
	"strconv"

	"github.com/jmylchreest/lobslaw/internal/workforce"
)

func (s *Server) serveWorkforceArtifact(w http.ResponseWriter, r *http.Request, download *workforce.ArtifactDownload) {
	defer func() { _ = download.Reader.Close() }()
	w.Header().Set("Content-Type", workforce.ArtifactDownloadMime)
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": download.Artifact.Name}))
	w.Header().Set("Content-Length", strconv.FormatInt(download.Artifact.Size, 10))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(http.StatusOK)
	_, _ = io.CopyN(w, artifactContextReader{ctx: r.Context(), reader: download.Reader}, download.Artifact.Size)
}

type artifactContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r artifactContextReader) Read(p []byte) (int, error) {
	if e := r.ctx.Err(); e != nil {
		return 0, e
	}
	return r.reader.Read(p)
}
