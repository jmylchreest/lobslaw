package node

import "github.com/jmylchreest/lobslaw/internal/computer"

// Called after the workforce service exists. Missing runtime components remain
// a lazy, actionable 503 so a broken optional browser does not stop ordinary chat.
func (n *Node) wireComputer(authorizer computer.ProjectAuthorizer) {
	if !n.cfg.Computer.Enabled || authorizer == nil {
		return
	}
	cfg := n.cfg.Computer
	var socket string
	var address string
	if n.egressProvider != nil {
		socket = n.egressProvider.UDSPath()
		address = n.egressProvider.ProxyURL().Host
	}
	n.computer = computer.New(computer.Config{Root: cfg.Root, Node: cfg.Node, Chromium: cfg.Chromium,
		Playwright: cfg.Playwright, IP: cfg.IP, ReadPaths: cfg.ReadPaths, ProxySocket: socket, ProxyAddress: address}, authorizer)
}
