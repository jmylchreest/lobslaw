package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/jmylchreest/lobslaw/internal/google/calendar"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

type CalendarRequest struct {
	Operation string                         `json:"operation"`
	ID        string                         `json:"id,omitempty"`
	Write     bool                           `json:"write,omitempty"`
	Email     string                         `json:"email,omitempty"`
	Calendars map[string]calendar.Permission `json:"calendars,omitempty"`
}
type CalendarManagement func(context.Context, *types.Claims, CalendarRequest) (any, error)

func (s *Server) handleCalendar(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	claims, err := s.authenticate(r, true)
	if err != nil {
		http.Error(w, "authentication required", http.StatusUnauthorized)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "POST required", http.StatusMethodNotAllowed)
		return
	}
	var req CalendarRequest
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		http.Error(w, "invalid request", http.StatusBadRequest)
		return
	}
	if decoder.Decode(new(any)) != io.EOF {
		http.Error(w, "one object required", http.StatusBadRequest)
		return
	}
	out, err := s.cfg.Calendar(r.Context(), claims, req)
	if err != nil {
		http.Error(w, err.Error(), http.StatusForbidden)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(out)
}
func (h *TelegramHandler) registerCalendarCommand() {
	if h.cfg.Calendar == nil {
		return
	}
	h.commands.Register(&Command{Name: "calendar", Summary: "connect and manage your Google Calendar in private", Handler: func(ctx context.Context, req CommandRequest) (string, error) {
		if req.Shared {
			return "", errors.New("calendar: use a private conversation")
		}
		fields := strings.Fields(req.Args)
		request := CalendarRequest{Operation: "list"}
		switch {
		case len(fields) == 0 || (len(fields) == 1 && fields[0] == "list"):
		case len(fields) == 2 && fields[0] == "connect" && (fields[1] == "read" || fields[1] == "write"):
			request.Operation = "connect"
			request.Write = fields[1] == "write"
		case len(fields) == 2 && (fields[0] == "pending" || fields[0] == "disconnect"):
			request.Operation = fields[0]
			request.ID = fields[1]
		case len(fields) == 5 && fields[0] == "allow" && (fields[4] == "read" || fields[4] == "write" || fields[4] == "read-write"):
			request.Operation = "activate"
			request.ID = fields[1]
			request.Email = fields[2]
			request.Calendars = map[string]calendar.Permission{fields[3]: {Read: fields[4] != "write", Write: fields[4] != "read"}}
		default:
			return "Use /calendar connect read|write, /calendar pending <flow-id>, /calendar allow <flow-id> <displayed-email> <calendar-id> read|write|read-write, /calendar list, or /calendar disconnect <connection-id>.", nil
		}
		out, err := h.cfg.Calendar(ctx, req.Claims, request)
		if err != nil {
			return "", err
		}
		b, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return "", err
		}
		// Full account/calendar selections must remain inspectable. The authenticated
		// REST endpoint supports larger lists; never truncate an approval target.
		if len(b) > 3500 {
			return "Calendar details exceed the chat limit. Use the authenticated /v1/calendar API to review the full response.", nil
		}
		return string(b), nil
	}})
}
