package node

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func listening(t *testing.T, addr string) bool {
	t.Helper()
	c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// pprof serves a memory dump to anyone who can reach it, so "off unless
// asked" is a security property and not a default.
func TestPprofStaysOffUnlessConfigured(t *testing.T) {
	t.Setenv("LOBSLAW_PPROF_ADDR", "")

	n, _ := approvalGateNode(t, false, false)
	n.startPprof(t.Context())

	// Nothing configured, so nothing should be listening anywhere.
	if listening(t, pprofDefaultAddr) {
		t.Fatalf("pprof is serving on %s with no pprof_addr set", pprofDefaultAddr)
	}
}

// Loopback detection decides whether the operator gets warned that they
// have published a memory dump, so it fails towards warning.
func TestPprofLoopbackDetection(t *testing.T) {
	t.Parallel()
	for addr, want := range map[string]bool{
		"127.0.0.1:6060": true,
		"localhost:6060": true,
		"[::1]:6060":     true,
		"0.0.0.0:6060":   false,
		"192.168.1.5:60": false,
		"[::]:6060":      false,
		"6060":           false, // unparseable — warn rather than assume
		"":               false,
	} {
		if got := pprofAddrIsLoopback(addr); got != want {
			t.Errorf("pprofAddrIsLoopback(%q) = %v, want %v", addr, got, want)
		}
	}
}

// And it does serve when asked. Port 0 so the test cannot collide with
// a developer's own pprof or another test.
func TestPprofServesWhenConfigured(t *testing.T) {
	t.Setenv("LOBSLAW_PPROF_ADDR", "127.0.0.1:0")

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // take the port number, give the port back

	t.Setenv("LOBSLAW_PPROF_ADDR", addr)
	n, _ := approvalGateNode(t, false, false)
	n.startPprof(t.Context())

	deadline := time.Now().Add(2 * time.Second)
	for !listening(t, addr) {
		if time.Now().After(deadline) {
			t.Fatalf("pprof did not come up on %s", addr)
		}
		time.Sleep(10 * time.Millisecond)
	}

	resp, err := http.Get("http://" + addr + "/debug/pprof/goroutine?debug=1")
	if err != nil {
		t.Fatalf("GET goroutine profile: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("goroutine profile returned %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	if !bytes.Contains(body, []byte("goroutine profile")) {
		t.Errorf("response is not a goroutine profile: %.80q", body)
	}
}
