package node

import (
	"context"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"time"
)

// pprofDefaultAddr is used when the operator asks for pprof without
// saying where. Loopback, because pprof has no authentication.
const pprofDefaultAddr = "127.0.0.1:6060"

// startPprof serves /debug/pprof/* when [debug] pprof_addr is set, and
// otherwise does nothing.
//
// This used to be a `debug` build tag, so that net/http/pprof's
// transitive imports only landed in the binary when explicitly opted
// in. The tag bought less than it cost: profiling a node meant
// rebuilding and redeploying it, which is precisely what you cannot do
// to the process that is currently misbehaving — and a heisenbug does
// not survive the restart. Off-by-default at runtime keeps the property
// that mattered (no listener unless asked) without that.
//
// Linking it is safe here because nothing in this tree serves on
// http.DefaultServeMux, which is the usual way importing net/http/pprof
// exposes profiles by accident: its init() registers handlers there,
// and any server built with a nil Handler would then serve them. Every
// server in lobslaw passes an explicit mux. If that ever changes, this
// import becomes a hole.
//
// To dump goroutines on a hung process:
//
//	curl -s http://127.0.0.1:6060/debug/pprof/goroutine?debug=2
func (n *Node) startPprof(ctx context.Context) {
	addr := n.cfg.Debug.PprofAddr
	if env := os.Getenv("LOBSLAW_PPROF_ADDR"); env != "" {
		addr = env
	}
	if addr == "" {
		return
	}
	if addr == "on" || addr == "true" {
		addr = pprofDefaultAddr
	}

	mux := http.NewServeMux()
	// Index also serves the named runtime profiles — heap, goroutine,
	// allocs, block, mutex — by looking them up at request time, so
	// profiles added by a newer Go appear here without a code change.
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}

	if !pprofAddrIsLoopback(addr) {
		// Not refused: a container's loopback is unreachable from the
		// host, so profiling one legitimately needs 0.0.0.0. Said out
		// loud every time, because a profile is a memory dump and the
		// only thing between it and a network is a firewall rule
		// nobody re-reads.
		n.log.Warn("pprof: listening on a non-loopback address with no authentication; anyone who can reach it can read this process's memory, including secrets",
			"addr", addr)
	}
	n.log.Info("pprof: diagnostics server starting", "addr", addr)

	go func() {
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			n.log.Error("pprof: serve failed", "err", err)
		}
	}()
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(shutdownCtx)
	}()
}

// pprofAddrIsLoopback reports whether the address reaches only this
// machine. An unparseable or hostname-based address counts as NOT
// loopback: the warning is cheap and being wrong the other way is not.
func pprofAddrIsLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
