package gateway

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/jmylchreest/lobslaw/internal/ids"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

const restMessageBodyLimit int64 = 1 << 20

func authenticatedUploadClaims(claims *types.Claims) bool {
	return claims != nil && claims.UserID != "" && claims.UserID != "anon"
}

func (s *Server) decodeMessageRequest(w http.ResponseWriter, r *http.Request, req *messageRequest) bool {
	r.Body = http.MaxBytesReader(w, r.Body, restMessageBodyLimit)
	if err := json.NewDecoder(r.Body).Decode(req); err != nil {
		s.jsonErr(w, http.StatusBadRequest, "bad JSON body: "+err.Error())
		return false
	}
	if len(req.UploadIDs) > restMessageMaxUploads {
		s.jsonErr(w, http.StatusBadRequest, fmt.Sprintf("at most %d uploads per message", restMessageMaxUploads))
		return false
	}
	if strings.TrimSpace(req.Message) == "" && len(req.UploadIDs) == 0 {
		s.jsonErr(w, http.StatusBadRequest, "message is required")
		return false
	}
	// Returned to the caller so even an unlabelled request has a usable trace.
	if req.TurnID == "" {
		req.TurnID = "rest-" + ids.New()
	}
	return true
}
