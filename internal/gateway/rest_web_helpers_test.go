package gateway

import (
	"context"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	jwtlib "github.com/golang-jwt/jwt/v5"

	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/internal/turn"
	"github.com/jmylchreest/lobslaw/pkg/auth"
	"github.com/jmylchreest/lobslaw/pkg/config"
)

// captureRunner records the last turn.Request so tests can assert
// identity came from credentials, not the JSON body.
type captureRunner struct {
	mu   sync.Mutex
	last turn.Request
	hold chan struct{}
}

func (c *captureRunner) Run(ctx context.Context, req turn.Request) (*turn.Response, error) {
	c.mu.Lock()
	c.last = req
	hold := c.hold
	c.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return &turn.Response{Reply: "ok"}, nil
}

func (c *captureRunner) Resume(context.Context, turn.Request, []turn.Message) (*turn.Response, error) {
	return &turn.Response{Reply: "ok"}, nil
}

func (c *captureRunner) lastRequest() turn.Request {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.last
}

func enrolledAlice() []config.UserConfig {
	return []config.UserConfig{{
		ID:          "alice",
		DisplayName: "Alice",
		Channels: []config.UserChannelAddrConfig{{
			Type:    ChannelREST,
			Address: "alice@idp",
		}},
	}}
}

func webAuthValidator(t *testing.T) *auth.Validator {
	t.Helper()
	v, err := auth.NewValidator(auth.Config{
		AllowHS256:  true,
		HS256Secret: restTestSecret,
	})
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func mintJWTWith(t *testing.T, sub string, extra jwtlib.MapClaims) string {
	t.Helper()
	claims := jwtlib.MapClaims{
		"sub":   sub,
		"scope": "household",
		"exp":   time.Now().Add(time.Hour).Unix(),
	}
	for k, v := range extra {
		claims[k] = v
	}
	tok := jwtlib.NewWithClaims(jwtlib.SigningMethodHS256, claims)
	s, err := tok.SignedString([]byte(restTestSecret))
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func startWebREST(t *testing.T, runner turn.Runner, extra func(*RESTConfig)) *Server {
	t.Helper()
	cfg := RESTConfig{
		Addr:         "127.0.0.1:0",
		JWTValidator: webAuthValidator(t),
		RequireAuth:  true,
		Identity:     identity.NewResolver(nil),
		Users:        enrolledAlice(),
		Plan:         &fakePlanService{},
		Prompts:      NewPromptRegistry(),
		DefaultScope: "public",
	}
	if extra != nil {
		extra(&cfg)
	}
	srv := NewServer(cfg, runner)
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Go(func() {
		_ = srv.Start(ctx)
	})
	deadline := time.Now().Add(time.Second)
	for srv.Addr() == "" && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if srv.Addr() == "" {
		cancel()
		wg.Wait()
		t.Fatal("server didn't bind within 1s")
	}
	t.Cleanup(func() {
		cancel()
		wg.Wait()
	})
	return srv
}

func webBaseURL(s *Server) string { return "http://" + s.Addr() }

func doJSON(t *testing.T, method, url, body string, header http.Header) *http.Response {
	t.Helper()
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req, err := http.NewRequest(method, url, rdr)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, vs := range header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = resp.Body.Close() })
	return resp
}
