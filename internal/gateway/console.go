package gateway

import (
	"fmt"
	"net"
	"net/http"
	"strings"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

const consoleMountPath = "/"

// RegisterConsole mounts the embedded SPA on the REST mux at Start.
// Nil is a no-op. Call before Start.
func (s *Server) RegisterConsole(h http.Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.console = h
}

func (s *Server) mountConsole(mux *http.ServeMux) {
	s.mu.Lock()
	h := s.console
	s.mu.Unlock()
	if h == nil {
		return
	}
	mux.Handle(consoleMountPath, h)
}

func (s *Server) consoleEnabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.console != nil
}

// checkConsoleBind refuses an unauthenticated console on a non-loopback
// bind. An empty address is every interface, which is the case this
// exists for.
func checkConsoleBind(addr string, requireAuth bool) error {
	if requireAuth {
		return nil
	}
	if isLoopbackBind(addr) {
		return nil
	}
	return fmt.Errorf("%w: ui-web enabled on %q needs [auth] require_auth = true — "+
		"the console must not be reachable without a token. Bind to localhost instead if this is a single-machine setup",
		types.ErrInvalidConfig, describeBind(addr))
}

func isLoopbackBind(addr string) bool {
	host := strings.TrimSpace(addr)
	if host == "" {
		return false
	}
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func describeBind(addr string) string {
	if strings.TrimSpace(addr) == "" {
		return "every interface"
	}
	return addr
}
