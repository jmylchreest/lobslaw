package mtls

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"sync"

	"google.golang.org/grpc/credentials"
)

// Clones retain the live source, never a tls.Config with frozen trust roots.
type clientCredentials struct {
	node       *NodeCreds
	mu         sync.RWMutex
	serverName string
}

func (c *clientCredentials) config() *tls.Config {
	state := c.node.state()
	c.mu.RLock()
	name := c.serverName
	c.mu.RUnlock()
	return &tls.Config{
		Certificates: []tls.Certificate{*state.certificate},
		RootCAs:      state.clusterRoots,
		MinVersion:   tls.VersionTLS13,
		ServerName:   name,
	}
}

func (c *clientCredentials) ClientHandshake(ctx context.Context, authority string, raw net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return credentials.NewTLS(c.config()).ClientHandshake(ctx, authority, raw)
}

// Client credentials must not accidentally become a server without client authentication.
func (c *clientCredentials) ServerHandshake(raw net.Conn) (net.Conn, credentials.AuthInfo, error) {
	return nil, nil, errors.New("client-only credentials: use NodeCreds.ServerCreds for servers")
}

func (c *clientCredentials) Info() credentials.ProtocolInfo {
	return credentials.NewTLS(c.config()).Info()
}

func (c *clientCredentials) Clone() credentials.TransportCredentials {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return &clientCredentials{node: c.node, serverName: c.serverName}
}

func (c *clientCredentials) OverrideServerName(name string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.serverName = name
	return nil
}
