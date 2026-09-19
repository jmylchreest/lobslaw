package gateway

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/jmylchreest/lobslaw/internal/computer"
)

const computerBodyLimit int64 = 64 << 10

func (s *Server) handleComputer(w http.ResponseWriter, r *http.Request) {
	authn, err := s.authenticateRequest(r)
	if err != nil || s.principalOf(r) == "" {
		s.jsonErr(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if err := s.checkCookieCSRF(r, authn); err != nil {
		s.jsonErr(w, http.StatusForbidden, err.Error())
		return
	}
	if s.cfg.Computer == nil {
		s.jsonErr(w, http.StatusServiceUnavailable, computer.ErrUnavailable.Error())
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/computers/"), "/")
	if len(parts) > 2 || parts[0] == "" {
		s.jsonErr(w, http.StatusNotFound, "computer route not found")
		return
	}
	principal := s.principalOf(r)
	if err := s.cfg.Computer.AuthorizeProject(r.Context(), principal, parts[0]); err != nil {
		code := http.StatusForbidden
		if errors.Is(err, computer.ErrNotFound) || status.Code(err) == codes.NotFound {
			code = http.StatusNotFound
		}
		if errors.Is(err, computer.ErrUnavailable) || status.Code(err) == codes.Unavailable {
			code = http.StatusServiceUnavailable
		}
		s.jsonErr(w, code, "project is unavailable or access was denied")
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if len(parts) == 2 {
		if parts[1] != "screenshot" || r.Method != http.MethodGet {
			s.jsonErr(w, http.StatusNotFound, "computer route not found")
			return
		}
		result, err := s.cfg.Computer.Action(r.Context(), principal, parts[0], computer.RoutineStep{Action: "capture"})
		if err != nil {
			s.computerError(w, err)
			return
		}
		frame, err := base64.StdEncoding.DecodeString(result.Screenshot)
		if err != nil || len(frame) == 0 {
			s.jsonErr(w, http.StatusServiceUnavailable, "browser did not return a frame")
			return
		}
		w.Header().Set("Content-Type", "image/png")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(frame)
		return
	}
	switch r.Method {
	case http.MethodGet:
		state, err := s.cfg.Computer.State(r.Context(), principal, parts[0])
		if err != nil {
			s.computerError(w, err)
			return
		}
		respondJSON(w, http.StatusOK, state)
	case http.MethodPost:
		var step computer.RoutineStep
		decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, computerBodyLimit))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&step); err != nil {
			s.jsonErr(w, http.StatusBadRequest, "invalid computer action")
			return
		}
		if _, err := s.cfg.Computer.Action(r.Context(), principal, parts[0], step); err != nil {
			s.computerError(w, err)
			return
		}
		state, err := s.cfg.Computer.State(r.Context(), principal, parts[0])
		if err != nil {
			s.computerError(w, err)
			return
		}
		respondJSON(w, http.StatusOK, state)
	default:
		s.jsonErr(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) computerError(w http.ResponseWriter, err error) {
	code := http.StatusServiceUnavailable
	switch {
	case errors.Is(err, computer.ErrForbidden):
		code = http.StatusForbidden
	case errors.Is(err, computer.ErrNotFound):
		code = http.StatusNotFound
	case errors.Is(err, computer.ErrTakeover), errors.Is(err, computer.ErrConflict), errors.Is(err, computer.ErrManual):
		code = http.StatusConflict
	case errors.Is(err, computer.ErrInvalid):
		code = http.StatusBadRequest
	}
	s.jsonErr(w, code, err.Error())
}
