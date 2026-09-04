package node

import (
	"context"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"runtime"
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
	n.enableContentionProfiles()

	addr, ok := n.pprofListenAddr()
	if !ok {
		return
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

// pprofListenAddr resolves where pprof should listen, and whether it
// should listen at all.
//
// Split from startPprof so that "off unless asked" is a decision a test
// can check directly. It was previously asserted by dialling the
// default port and expecting nothing there, which is only true on a
// machine that is not already running a node — it passed in CI and
// failed on a developer's box for the most confusing possible reason,
// that the software worked.
func (n *Node) pprofListenAddr() (string, bool) {
	addr := n.cfg.Debug.PprofAddr
	if env := os.Getenv("LOBSLAW_PPROF_ADDR"); env != "" {
		addr = env
	}
	switch addr {
	case "":
		return "", false
	case "on", "true":
		return pprofDefaultAddr, true
	}
	return addr, true
}

// enableContentionProfiles turns on the two profiles the runtime keeps
// switched off.
//
// Block and mutex are the profiles worth opening a profiler for on a
// node like this one — raft, a gateway and a dozen background loops all
// contending for the same stores — and they are the two that record
// nothing unless asked. The failure is quiet in the wrong direction:
// /debug/pprof/block on a stock build returns a well-formed EMPTY
// profile, which reads as "no contention" rather than "not recording",
// so the question looks answered when it was never asked.
//
// Deliberately not defaulted on. Both cost time on every block or
// contended lock, and a profiler left running is overhead nobody
// remembers adding. Logged when enabled for the same reason.
//
// Separate from the HTTP endpoint, and before its early return: a
// profile can be collected without serving it — a test, or
// runtime/pprof writing to a file — and tying the recording to the
// listener would make that impossible.
func (n *Node) enableContentionProfiles() {
	if rate := n.cfg.Debug.BlockProfileRate; rate > 0 {
		runtime.SetBlockProfileRate(rate)
		n.log.Info("pprof: block profiling on; this samples every blocking operation and is not free",
			"rate_ns", rate)
	}
	if frac := n.cfg.Debug.MutexProfileFraction; frac > 0 {
		runtime.SetMutexProfileFraction(frac)
		n.log.Info("pprof: mutex contention profiling on; this samples contended unlocks and is not free",
			"fraction", frac)
	}
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
