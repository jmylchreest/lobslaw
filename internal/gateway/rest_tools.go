package gateway

import "net/http"

// ToolCatalogue lists the tools a bot may be granted. An interface so
// the gateway does not import the tool registry, matching the other
// consumer-side contracts here.
type ToolCatalogue interface {
	List() []ToolInfo
}

// ToolInfo is the wire shape for one selectable tool.
type ToolInfo struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

// handleTools serves the node's tool catalogue so the console can offer
// the real set instead of asking somebody to type names from memory.
//
// Read-only and authenticated: the names are not secret, but they do
// describe what this deployment can do, which is not the internet's
// business.
func (s *Server) handleTools(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if _, err := s.authenticateRequest(r); err != nil {
		s.jsonErr(w, http.StatusUnauthorized, err.Error())
		return
	}
	if s.cfg.Tools == nil {
		s.jsonErr(w, http.StatusServiceUnavailable, "this node does not publish its tool catalogue")
		return
	}
	tools := s.cfg.Tools.List()
	if tools == nil {
		tools = []ToolInfo{}
	}
	respondJSON(w, http.StatusOK, map[string]any{"tools": tools})
}
