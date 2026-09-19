//go:build linux

package computer

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/egress"
)

func TestBrokerPinsRoleDespiteForgedHeaders(t *testing.T) {
	t.Parallel()
	fixture := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = fmt.Fprint(w, "allowed fixture") }))
	defer fixture.Close()
	socket := filepath.Join(t.TempDir(), "upstream.sock")
	proxy, err := egress.NewSmokescreenProvider(egress.SmokescreenConfig{UDSPath: socket, ACL: egress.Rules{Roles: map[string][]string{"computer": {"127.0.0.1"}, "fetch_url": {"localhost"}}}, AllowRanges: []string{"127.0.0.0/8"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = proxy.Stop(context.Background()) }()
	// Prove the forged role genuinely could reach this target without the
	// broker, so a DNS/private-range failure cannot make a broken test pass.
	probe, err := proxy.For("fetch_url").HTTPClient().Get(strings.Replace(fixture.URL, "127.0.0.1", "localhost", 1))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = probe.Body.Close() }()
	if probe.StatusCode != http.StatusOK {
		t.Fatalf("spoofing positive control failed: %d", probe.StatusCode)
	}
	broker, err := newRoleBroker(socket, proxy.ProxyURL().Host)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = broker.Close() }()
	_, proxyPort, _ := net.SplitHostPort(proxy.ProxyURL().Host)
	for _, target := range []string{proxy.ProxyURL().String(), "http://localhost:0" + proxyPort} {
		for _, method := range []string{http.MethodGet, http.MethodConnect} {
			req, err := http.NewRequest(method, target, nil)
			if err != nil {
				t.Fatal(err)
			}
			if method == http.MethodConnect {
				req.URL.Scheme = ""
				req.URL.Path = ""
			}
			conn, err := net.Dial("unix", broker.Path())
			if err != nil {
				t.Fatal(err)
			}
			if err := req.WriteProxy(conn); err != nil {
				_ = conn.Close()
				t.Fatal(err)
			}
			res, err := http.ReadResponse(bufio.NewReader(conn), req)
			if err != nil {
				_ = conn.Close()
				t.Fatal(err)
			}
			_ = res.Body.Close()
			_ = conn.Close()
			if res.StatusCode != http.StatusForbidden {
				t.Fatalf("proxy self-tunnel %s status %d", method, res.StatusCode)
			}
		}
	}
	for _, method := range []string{http.MethodGet, http.MethodConnect} {
		for _, forged := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/forged=%v", method, forged), func(t *testing.T) {
				target := fixture.URL
				if forged {
					target = strings.Replace(target, "127.0.0.1", "localhost", 1)
				}
				req, err := http.NewRequest(method, target, nil)
				if err != nil {
					t.Fatal(err)
				}
				if method == http.MethodConnect {
					req.URL.Scheme = ""
					req.URL.Path = ""
				}
				req.Header.Set("X-Lobslaw-Role", "fetch_url")
				req.Header.Set("Proxy-Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte("fetch_url:_")))
				conn, err := net.Dial("unix", broker.Path())
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = conn.Close() }()
				if err := req.WriteProxy(conn); err != nil {
					t.Fatal(err)
				}
				res, err := http.ReadResponse(bufio.NewReader(conn), req)
				if err != nil {
					t.Fatal(err)
				}
				defer func() { _ = res.Body.Close() }()
				if forged && res.StatusCode == http.StatusOK {
					t.Fatal("forged role escaped computer ACL")
				}
				if !forged && res.StatusCode != http.StatusOK {
					t.Fatalf("computer ACL request = %d", res.StatusCode)
				}
			})
		}
	}
}
