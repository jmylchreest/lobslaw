package node

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"runtime"
	"runtime/pprof"
	"sync"
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
//
// Checked on the decision rather than by probing a port: the default
// address is a fixed one, so a probe answers "is anything on :6060",
// which on a developer's machine is usually their own running node.
func TestPprofStaysOffUnlessConfigured(t *testing.T) {
	t.Setenv("LOBSLAW_PPROF_ADDR", "")

	n, _ := approvalGateNode(t, false, false)
	if addr, ok := n.pprofListenAddr(); ok {
		t.Fatalf("pprof would listen on %q with nothing configured", addr)
	}

	n.cfg.Debug.PprofAddr = "on"
	if addr, ok := n.pprofListenAddr(); !ok || addr != pprofDefaultAddr {
		t.Errorf(`pprof_addr "on" resolved to (%q, %v), want the loopback default`, addr, ok)
	}

	n.cfg.Debug.PprofAddr = "0.0.0.0:9999"
	if addr, _ := n.pprofListenAddr(); addr != "0.0.0.0:9999" {
		t.Errorf("explicit address resolved to %q", addr)
	}

	// The env var is the escape hatch for a node already going wrong,
	// so it has to win over the file.
	t.Setenv("LOBSLAW_PPROF_ADDR", "127.0.0.1:1234")
	if addr, _ := n.pprofListenAddr(); addr != "127.0.0.1:1234" {
		t.Errorf("LOBSLAW_PPROF_ADDR did not override config: %q", addr)
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

// The block and mutex profiles record nothing unless the runtime is
// told to, and a stock build serves a well-formed EMPTY profile rather
// than an error — which reads as "no contention" instead of "not
// recording". This asserts the difference.
func TestContentionProfilesRecordOnlyWhenEnabled(t *testing.T) {
	// Process-global runtime state, so no t.Parallel, and put it back.
	t.Cleanup(func() {
		runtime.SetBlockProfileRate(0)
		runtime.SetMutexProfileFraction(0)
	})

	contend := func() {
		var mu sync.Mutex
		var wg sync.WaitGroup
		ch := make(chan struct{})
		for range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				mu.Lock()
				time.Sleep(time.Millisecond)
				mu.Unlock()
				<-ch
			}()
		}
		time.Sleep(20 * time.Millisecond)
		close(ch)
		wg.Wait()
	}

	// Off: the profiles exist and stay empty however hard we contend.
	n, _ := approvalGateNode(t, false, false)
	n.startPprof(t.Context()) // no pprof_addr, so this only sets rates
	contend()
	if got := pprof.Lookup("block").Count(); got != 0 {
		t.Errorf("block profile recorded %d events with block_profile_rate unset", got)
	}

	// On: the same contention is now visible.
	n.cfg.Debug.BlockProfileRate = 1
	n.cfg.Debug.MutexProfileFraction = 1
	n.startPprof(t.Context())
	contend()

	if got := pprof.Lookup("block").Count(); got == 0 {
		t.Error("block profile is still empty after block_profile_rate was set; the endpoint would report no contention")
	}
	if got := pprof.Lookup("mutex").Count(); got == 0 {
		t.Error("mutex profile is still empty after mutex_profile_fraction was set")
	}
	// SetMutexProfileFraction(-1) reads without changing.
	if got := runtime.SetMutexProfileFraction(-1); got != 1 {
		t.Errorf("mutex profile fraction = %d, want 1", got)
	}
}
