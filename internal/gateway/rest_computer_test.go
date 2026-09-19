package gateway

import (
	"context"
	"net/http"
	"path/filepath"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/computer"
)

type computerOwner struct{}

func (computerOwner) AuthorizeProject(_ context.Context, owner, project string) error {
	if owner != "user:alice" || project != "project" {
		return computer.ErrForbidden
	}
	return nil
}

func TestComputerOwnerRoutesAndRemoteAllowlist(t *testing.T) {
	t.Parallel()
	service := computer.New(computer.Config{Root: filepath.Join(t.TempDir(), "private")}, computerOwner{})
	defer func() { _ = service.Close() }()
	srv := startWebREST(t, nil, func(c *RESTConfig) { c.Computer = service })
	front := startWebREST(t, nil, func(c *RESTConfig) { c.RemoteConsole = testConsoleClient(t, srv) })
	for _, tc := range []struct {
		method, path, body, subject string
		status                      int
	}{
		{http.MethodGet, "/v1/computers/project", "", "alice@idp", http.StatusOK},
		{http.MethodPost, "/v1/computers/project", `{"action":"takeover"}`, "alice@idp", http.StatusOK},
		{http.MethodPost, "/v1/computers/project", `{"action":"start"}`, "alice@idp", http.StatusServiceUnavailable},
		{http.MethodGet, "/v1/computers/project", "", "bob", http.StatusForbidden},
		{http.MethodGet, "/v1/computers/project/screenshot", "", "bob", http.StatusForbidden},
		{http.MethodPost, "/v1/computers/project", `{"action":"release"}`, "bob", http.StatusForbidden},
		{http.MethodPost, "/v1/computers/project", `{"action":"navigate","url":"https://example.com"}`, "bob", http.StatusForbidden},
	} {
		t.Run(tc.method+tc.path+tc.subject+tc.body, func(t *testing.T) {
			for _, host := range []*Server{srv, front} {
				res := doJSON(t, tc.method, webBaseURL(host)+tc.path, tc.body, http.Header{"Authorization": {"Bearer " + mintJWTWith(t, tc.subject, nil)}})
				if res.StatusCode != tc.status {
					t.Fatalf("status %d want %d", res.StatusCode, tc.status)
				}
			}
			if srv.backendConsoleHandler(tc.path) == nil {
				t.Fatal("browser path missing from ConsoleService forwarding")
			}
		})
	}
}
