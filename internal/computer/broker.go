package computer

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
)

const brokerRole = "computer"
const brokerConnections int = 64
const brokerHeaderBytes int = 16 << 10
const brokerHeaderTimeout time.Duration = 5 * time.Second
const brokerTunnelTimeout time.Duration = 2 * time.Minute
const brokerHTTPPort int = 80
const brokerHTTPSPort int = 443
const maxTCPPort int = 65535

// The browser is an untrusted native subprocess. Role assignment lives here in
// the host, not in its Node bridge. This broker can dial only the existing egress
// UDS, never a destination host; smokescreen remains the outbound policy engine.
type roleBroker struct {
	dir, upstream string
	proxyPort     int
	listener      net.Listener
	server        *http.Server
	done          chan struct{}
	slots         chan struct{}
	mu            sync.Mutex
	closed        bool
	connections   map[net.Conn]struct{}
	once          sync.Once
}

func newRoleBroker(upstream, proxyAddress string) (*roleBroker, error) {
	_, rawPort, err := net.SplitHostPort(proxyAddress)
	proxyPort, portErr := strconv.Atoi(rawPort)
	if err != nil || portErr != nil || proxyPort <= 0 || proxyPort > maxTCPPort {
		return nil, fmt.Errorf("%w: shared proxy listener address is required", ErrUnavailable)
	}
	dir, err := os.MkdirTemp(brokerTempParent(upstream), "lcb-")
	if err != nil {
		return nil, fmt.Errorf("computer broker directory: %w", err)
	}
	b := &roleBroker{dir: dir, upstream: upstream, proxyPort: proxyPort, done: make(chan struct{}), slots: make(chan struct{}, brokerConnections), connections: make(map[net.Conn]struct{})}
	listener, err := net.Listen("unix", b.Path())
	if err != nil {
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("computer broker listen: %w", err)
	}
	if err := os.Chmod(b.Path(), privateFileMode); err != nil {
		_ = listener.Close()
		_ = os.RemoveAll(dir)
		return nil, fmt.Errorf("computer broker permissions: %w", err)
	}
	b.listener = listener
	b.server = &http.Server{Handler: b, ReadHeaderTimeout: brokerHeaderTimeout, MaxHeaderBytes: brokerHeaderBytes, IdleTimeout: brokerHeaderTimeout}
	go func() { defer close(b.done); _ = b.server.Serve(listener) }()
	return b, nil
}

func pathInside(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}
func brokerTempParent(upstream string) string {
	if pathInside(filepath.Dir(upstream), "/tmp") {
		return "/var/tmp"
	}
	return "/tmp"
}

func (b *roleBroker) Path() string { return filepath.Join(b.dir, "proxy.sock") }

func (b *roleBroker) track(conn net.Conn) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		_ = conn.Close()
		return false
	}
	b.connections[conn] = struct{}{}
	return true
}
func (b *roleBroker) untrack(conn net.Conn) {
	_ = conn.Close()
	b.mu.Lock()
	delete(b.connections, conn)
	b.mu.Unlock()
}

func (b *roleBroker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	select {
	case b.slots <- struct{}{}:
		defer func() { <-b.slots }()
	default:
		http.Error(w, "browser proxy busy", http.StatusServiceUnavailable)
		return
	}
	if r.URL.Host == "" || (r.Method != http.MethodConnect && r.URL.Scheme != "http" && r.URL.Scheme != "https") {
		http.Error(w, "absolute http(s) target required", http.StatusBadRequest)
		return
	}
	// Even an operator-allowed loopback range must not permit a CONNECT back
	// into the shared proxy's TCP listener, where role headers regain authority.
	// Reserve that port for every hostname, including aliases and DNS rebinding.
	port := brokerHTTPPort
	if r.Method == http.MethodConnect || r.URL.Scheme == "https" {
		port = brokerHTTPSPort
	}
	if raw := r.URL.Port(); raw != "" {
		var err error
		port, err = strconv.Atoi(raw)
		if err != nil || port <= 0 || port > maxTCPPort {
			http.Error(w, "invalid destination port", http.StatusBadRequest)
			return
		}
	}
	if port == b.proxyPort {
		http.Error(w, "shared proxy self-tunneling is forbidden", http.StatusForbidden)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), ActionTimeout)
	defer cancel()
	upstream, err := (&net.Dialer{}).DialContext(ctx, "unix", b.upstream)
	if err != nil {
		http.Error(w, "egress unavailable", http.StatusBadGateway)
		return
	}
	if !b.track(upstream) {
		http.Error(w, "broker closed", http.StatusServiceUnavailable)
		return
	}
	defer b.untrack(upstream)
	_ = upstream.SetDeadline(time.Now().Add(brokerTunnelTimeout))
	req := r.Clone(ctx)
	req.Host = req.URL.Host
	req.RequestURI = ""
	req.Header.Del("X-Lobslaw-Role")
	req.Header.Del("Proxy-Authorization")
	req.Header.Set("X-Lobslaw-Role", brokerRole)
	req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(brokerRole+":_")))
	if err := req.WriteProxy(upstream); err != nil {
		http.Error(w, "egress unavailable", http.StatusBadGateway)
		return
	}
	reader := bufio.NewReader(upstream)
	response, err := http.ReadResponse(reader, req)
	if err != nil {
		http.Error(w, "egress unavailable", http.StatusBadGateway)
		return
	}
	if r.Method == http.MethodConnect && response.StatusCode == http.StatusOK {
		b.tunnel(w, upstream, reader)
		return
	}
	defer func() { _ = response.Body.Close() }()
	for key, values := range response.Header {
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.WriteHeader(response.StatusCode)
	_, _ = io.Copy(w, response.Body)
}

func (b *roleBroker) tunnel(w http.ResponseWriter, upstream net.Conn, reader *bufio.Reader) {
	client, buffer, err := http.NewResponseController(w).Hijack()
	if err != nil {
		return
	}
	if !b.track(client) {
		return
	}
	defer b.untrack(client)
	_ = client.SetDeadline(time.Now().Add(brokerTunnelTimeout))
	if _, err := buffer.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n"); err != nil {
		return
	}
	if err := buffer.Flush(); err != nil {
		return
	}
	var group errgroup.Group
	group.Go(func() error {
		_, err := io.Copy(upstream, buffer)
		_ = upstream.Close()
		_ = client.Close()
		return err
	})
	group.Go(func() error { _, err := io.Copy(client, reader); _ = upstream.Close(); _ = client.Close(); return err })
	_ = group.Wait()
}

func (b *roleBroker) Close() error {
	b.once.Do(func() {
		b.mu.Lock()
		b.closed = true
		for conn := range b.connections {
			_ = conn.Close()
		}
		b.mu.Unlock()
		_ = b.server.Close()
		<-b.done
		_ = os.RemoveAll(b.dir)
	})
	return nil
}
