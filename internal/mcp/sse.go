package mcp

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
)

// Talking to an MCP server that is not a subprocess.
//
// The stdio transport owns its server: it spawns it, and the pipes
// exist because we made them. A remote server is somebody else's
// process on somebody else's host, which changes what confinement
// means. HTTPS_PROXY on a subprocess is a hint the subprocess may
// ignore; here lobslaw makes the request itself, through the egress
// client the loader hands in, so the role's ACL is enforced rather
// than suggested. Remote is the more confinable of the two.
//
// The wire is MCP's HTTP+SSE transport:
//
//  1. GET the SSE URL. The server replies with an `endpoint` event
//     naming the URL to POST to — usually carrying a session id.
//  2. Client→server frames are POSTed there. The response body is
//     empty; the reply arrives on the stream.
//  3. Server→client frames arrive as `message` events.
//
// Two connections for one conversation, which is the part worth
// knowing when it misbehaves: a POST that succeeds proves the server
// accepted the frame, not that it answered.

// ErrSessionGone means the server no longer knows the session this
// transport was POSTing to.
//
// A remote session is not forever: the server restarts, an idle
// stream is reaped, a proxy times the connection out. Distinguished
// from any other send failure because it is the one worth REBUILDING
// for — the endpoint is valid, the credentials are valid, and the
// only thing wrong is a session id that has been forgotten.
var ErrSessionGone = errors.New("mcp: sse session is gone")

// SSETransport implements Transport over MCP's HTTP+SSE binding.
type SSETransport struct {
	client *http.Client
	url    string
	header http.Header

	cancel context.CancelFunc
	frames chan []byte
	// streamErr carries the reason the stream ended, so a Recv that
	// finds a closed channel can say why rather than "EOF".
	streamErr chan error

	// endpointReady closes once the server has named its POST
	// endpoint. Send waits on it: a frame posted before then has
	// nowhere to go, and the handshake is the first thing sent.
	endpointReady chan struct{}

	mu       sync.Mutex
	endpoint string
	closed   bool
}

// SSEConfig is what the loader supplies.
type SSEConfig struct {
	// Client is the egress-scoped HTTP client. Required: an MCP
	// server reached with an unscoped client is one whose host
	// nothing constrains, which is the thing the role exists to
	// prevent.
	Client *http.Client
	// URL is the SSE endpoint to GET.
	URL string
	// Header carries authentication. Resolved from secret refs by
	// the caller — this type never reads config or the environment.
	Header http.Header
}

// NewSSETransport opens the event stream and waits for the server to
// name its message endpoint.
//
// Blocking on the endpoint here rather than on first Send keeps the
// failure legible: a server that never sends one has failed to
// start a session, and reporting that at construction names the
// server rather than the handshake frame that happened to be first.
func NewSSETransport(ctx context.Context, cfg SSEConfig) (*SSETransport, error) {
	if cfg.Client == nil {
		return nil, errors.New("mcp: SSE transport requires an egress-scoped HTTP client")
	}
	if strings.TrimSpace(cfg.URL) == "" {
		return nil, errors.New("mcp: SSE transport requires a url")
	}

	streamCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	t := &SSETransport{
		client:        cfg.Client,
		url:           cfg.URL,
		header:        cfg.Header,
		cancel:        cancel,
		frames:        make(chan []byte, 16),
		streamErr:     make(chan error, 1),
		endpointReady: make(chan struct{}),
	}

	req, err := http.NewRequestWithContext(streamCtx, http.MethodGet, cfg.URL, nil)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("mcp: sse request: %w", err)
	}
	t.applyHeader(req)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Cache-Control", "no-cache")

	resp, err := t.client.Do(req)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("mcp: sse connect: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		_ = resp.Body.Close()
		cancel()
		return nil, fmt.Errorf("mcp: sse connect: %s: %s",
			resp.Status, strings.TrimSpace(string(body)))
	}

	go t.readStream(resp)

	select {
	case <-t.endpointReady:
		return t, nil
	case err := <-t.streamErr:
		_ = t.Close()
		return nil, fmt.Errorf("mcp: sse stream ended before naming an endpoint: %w", err)
	case <-ctx.Done():
		_ = t.Close()
		return nil, fmt.Errorf("mcp: sse handshake: %w", ctx.Err())
	}
}

// readStream parses events until the connection ends.
func (t *SSETransport) readStream(resp *http.Response) {
	defer func() { _ = resp.Body.Close() }()
	defer close(t.frames)

	scanner := bufio.NewScanner(resp.Body)
	// An MCP frame carrying a tool result is routinely larger than
	// bufio's 64 KiB default, and the failure mode of the default is
	// a truncated line reported as a parse error.
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)

	var (
		event string
		data  bytes.Buffer
	)
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			t.dispatch(event, strings.TrimSuffix(data.String(), "\n"))
			event, data = "", bytes.Buffer{}
		case strings.HasPrefix(line, ":"):
			// A comment, which servers send as a keep-alive.
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimPrefix(strings.TrimPrefix(line, "data:"), " "))
			data.WriteString("\n")
		}
	}
	err := scanner.Err()
	if err == nil {
		err = io.EOF
	}
	select {
	case t.streamErr <- err:
	default:
	}
}

// dispatch routes one complete event.
func (t *SSETransport) dispatch(event, data string) {
	if data == "" {
		return
	}
	switch event {
	case "endpoint":
		t.setEndpoint(data)
	case "message", "":
		// An unnamed event is a message: the SSE default is
		// "message", and servers routinely omit the field.
		select {
		case t.frames <- []byte(data):
		default:
			// Dropping is wrong, so block instead — a full buffer
			// means the reader is behind, and losing a JSON-RPC
			// response strands the call that is waiting for it.
			t.frames <- []byte(data)
		}
	}
}

func (t *SSETransport) setEndpoint(raw string) {
	resolved := raw
	if base, err := url.Parse(t.url); err == nil {
		if ref, err := url.Parse(raw); err == nil {
			resolved = base.ResolveReference(ref).String()
		}
	}
	t.mu.Lock()
	first := t.endpoint == ""
	t.endpoint = resolved
	t.mu.Unlock()
	if first {
		close(t.endpointReady)
	}
}

// Send POSTs one frame to the endpoint the server named.
func (t *SSETransport) Send(ctx context.Context, frame []byte) error {
	select {
	case <-t.endpointReady:
	case <-ctx.Done():
		return fmt.Errorf("mcp: waiting for sse endpoint: %w", ctx.Err())
	}

	t.mu.Lock()
	endpoint, closed := t.endpoint, t.closed
	t.mu.Unlock()
	if closed {
		return errors.New("mcp: transport closed")
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(frame))
	if err != nil {
		return fmt.Errorf("mcp: sse send: %w", err)
	}
	t.applyHeader(req)
	req.Header.Set("Content-Type", "application/json")

	resp, err := t.client.Do(req)
	if err != nil {
		return fmt.Errorf("mcp: sse send: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	// 202 is the documented answer and 200 is common; both mean
	// accepted, and neither carries the reply — that arrives on the
	// stream.
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted &&
		resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		text := strings.TrimSpace(string(body))
		// 404 and 410 on a POST to an endpoint we were given mean the
		// SESSION is gone, not the server: the caller can rebuild and
		// carry on rather than reporting the integration broken.
		// KitchenOwl answers 404 {"error":"Unknown or expired
		// session"}; the status alone is enough to act on.
		if resp.StatusCode == http.StatusNotFound || resp.StatusCode == http.StatusGone {
			return fmt.Errorf("%w: %s: %s", ErrSessionGone, resp.Status, text)
		}
		return fmt.Errorf("mcp: sse send: %s: %s", resp.Status, text)
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return nil
}

// Recv returns the next frame from the stream.
func (t *SSETransport) Recv(ctx context.Context) ([]byte, error) {
	select {
	case frame, ok := <-t.frames:
		if !ok {
			return nil, t.endedWhy()
		}
		return frame, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// endedWhy reports why the stream stopped, so a closed channel does
// not surface as a bare EOF with nothing to act on.
//
// Wrapped in ErrSessionGone: a stream that ended has taken its
// session with it, and the caller's recovery is the same one a 404 on
// send calls for.
func (t *SSETransport) endedWhy() error {
	select {
	case err := <-t.streamErr:
		return fmt.Errorf("%w: stream ended: %w", ErrSessionGone, err)
	default:
		return fmt.Errorf("%w: stream ended", ErrSessionGone)
	}
}

// Close ends the stream. Idempotent.
func (t *SSETransport) Close() error {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return nil
	}
	t.closed = true
	t.mu.Unlock()

	t.cancel()
	return nil
}

func (t *SSETransport) applyHeader(req *http.Request) {
	for k, vs := range t.header {
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
}
