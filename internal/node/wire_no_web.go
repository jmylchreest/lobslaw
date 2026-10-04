//go:build no_web

package node

func (n *Node) mountWebConsole(enabled bool) {
	if enabled {
		n.log.Warn("gateway: ui-web requested but web support was excluded by the no_web build tag; APIs and other functions remain available")
	}
}
