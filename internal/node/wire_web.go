//go:build !no_web

package node

import "github.com/jmylchreest/lobslaw/internal/gateway/ui"

// mountWebConsole attaches the embedded SPA when FunctionUIWeb is on.
// A missing Vite build is a warning, not a boot failure: Telegram and
// the API still serve.
func (n *Node) mountWebConsole(enabled bool) {
	if !enabled || n.gatewaySrv == nil {
		return
	}
	handler, err := ui.Handler()
	if err != nil {
		n.log.Warn("gateway: web console enabled but unavailable", "err", err)
		return
	}
	n.gatewaySrv.RegisterConsole(handler)
	n.log.Info("gateway: web console mounted", "path", "/")
}
