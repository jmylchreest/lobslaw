package node_test

import (
	"context"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jmylchreest/lobslaw/internal/gateway/ui"
	"github.com/jmylchreest/lobslaw/internal/node"
	"github.com/jmylchreest/lobslaw/pkg/config"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// A booted node with FunctionUIWeb actually serves the console on the
// same listener as the API. Skips when `make web` has not run so a
// fresh clone's Go suite stays green.
func TestNodeServesTheWebConsoleAndAPIOnOneListener(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping console boot integration in short mode")
	}
	if !ui.Built() {
		t.Skip("no web assets in this binary; run `make web` to exercise the console")
	}

	tmp := t.TempDir()
	nodeID := "ui-web-boot-node"
	creds := signNodeCert(t, filepath.Join(tmp, "certs"), nodeID)

	cfg := node.Config{
		NodeID:         nodeID,
		Functions:      []types.NodeFunction{types.FunctionUIWeb},
		ListenAddr:     "127.0.0.1:0",
		Creds:          creds,
		APIKeyResolver: func(string) (string, error) { return "ui-web-test-hs256-secret", nil },
		Auth:           config.AuthConfig{RequireAuth: true, AllowHS256: true, JWTSecretRef: "env:UI_WEB_TEST_JWT"},
		Gateway: config.GatewayConfig{
			Enabled:          true,
			HTTPPort:         0,
			UnknownUserScope: "public",
		},
		UIWeb: config.UIWebConfig{Backend: "127.0.0.1:9"},
	}

	n, err := node.New(cfg)
	if err != nil {
		t.Fatalf("node.New: %v", err)
	}
	if n.Gateway() == nil {
		t.Fatal("Gateway() nil — ui-web must wire the HTTP surface without compute")
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- n.Start(ctx) }()
	t.Cleanup(func() {
		cancel()
		<-done
	})

	deadline := time.Now().Add(3 * time.Second)
	for n.Gateway().Addr() == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if n.Gateway().Addr() == "" {
		t.Fatal("gateway didn't bind within 3s")
	}

	base := "http://" + n.Gateway().Addr()
	resp, err := http.Get(base + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET / = %d body=%s", resp.StatusCode, body)
	}
	if !strings.Contains(string(body), `id="root"`) {
		t.Errorf("console body is not the app shell")
	}

	health, err := http.Get(base + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer func() { _ = health.Body.Close() }()
	if health.StatusCode != http.StatusOK {
		t.Errorf("/healthz = %d", health.StatusCode)
	}
}

func TestNodeBootsUIWebWithoutAssets(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping console boot integration in short mode")
	}
	if ui.Built() {
		t.Skip("this binary has web assets; the missing-build path is the one under test")
	}

	tmp := t.TempDir()
	nodeID := "ui-web-empty-node"
	creds := signNodeCert(t, filepath.Join(tmp, "certs"), nodeID)

	cfg := node.Config{
		NodeID:         nodeID,
		Functions:      []types.NodeFunction{types.FunctionUIWeb},
		ListenAddr:     "127.0.0.1:0",
		Creds:          creds,
		APIKeyResolver: func(string) (string, error) { return "ui-web-test-hs256-secret", nil },
		Auth:           config.AuthConfig{RequireAuth: true, AllowHS256: true, JWTSecretRef: "env:UI_WEB_TEST_JWT"},
		Gateway: config.GatewayConfig{
			Enabled:          true,
			HTTPPort:         0,
			UnknownUserScope: "public",
		},
		UIWeb: config.UIWebConfig{Backend: "127.0.0.1:9"},
	}

	n, err := node.New(cfg)
	if err != nil {
		t.Fatalf("node.New with no assets: %v — missing console must not take the node down", err)
	}
	if n.Gateway() == nil {
		t.Fatal("Gateway() nil — ui-web still wires REST when assets are absent")
	}
}
