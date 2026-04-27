//go:build debug

package node

import (
	"context"
	"net/http"
	"net/http/pprof"
	"os"
	"time"
)

// Loopback default — pprof has no auth so binding on a routable
// interface would expose goroutine + heap dumps to the world.
const pprofDefaultAddr = "127.0.0.1:6060"

// startPprof exposes /debug/pprof/* under the debug build tag.
// Override the bind address via LOBSLAW_PPROF_ADDR.
//
// To dump goroutines on a hung process:
//
//	curl -s http://127.0.0.1:6060/debug/pprof/goroutine?debug=2
func (n *Node) startPprof(ctx context.Context) {
	addr := os.Getenv("LOBSLAW_PPROF_ADDR")
	if addr == "" {
		addr = pprofDefaultAddr
	}

	mux := http.NewServeMux()
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

	n.log.Info("pprof debug server starting (build tag: debug)", "addr", addr)
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
