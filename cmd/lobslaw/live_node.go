package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net"
	"time"

	"google.golang.org/grpc"

	"github.com/jmylchreest/lobslaw/pkg/config"
	"github.com/jmylchreest/lobslaw/pkg/mtls"
)

// Talking to a RUNNING node.
//
// Every CLI subcommand before this one opened state.db directly, which
// takes bbolt's exclusive lock and therefore requires the node to be
// stopped. That is the right shape for forensics — you want to read a
// cluster that will not start — and the wrong one for anything
// routine. Approving a proposal is routine, and a workflow that begins
// "stop the node" is one nobody performs.
//
// So this is the counterpart to offlineStore, and it deliberately
// mirrors its flag conventions rather than inventing a second way to
// find a cluster: the same --config locates it, the same env var
// overrides, and the mTLS paths come from [cluster.mtls] exactly as
// the node reads them.

// liveNode holds the flags a subcommand needs to reach a running node.
type liveNode struct {
	configPath  string
	contextName string
	addr        string
	caCert      string
	nodeCert    string
	nodeKey     string
	timeout     time.Duration
}

// bind registers the shared flags on fs. Call before fs.Parse.
func (l *liveNode) bind(fs *flag.FlagSet) {
	fs.StringVar(&l.configPath, "config", envOr("LOBSLAW_CONFIG", ""),
		"path to config.toml; supplies [cluster] advertise_addr and [cluster.mtls] paths")
	fs.StringVar(&l.contextName, "context", envOr("LOBSLAW_CONTEXT", ""),
		"named cluster from contexts.toml; supplies addr and credentials")
	fs.StringVar(&l.addr, "addr", envOr("LOBSLAW_NODE_ADDR", ""),
		"host:port of a running node; overrides --config")
	fs.StringVar(&l.caCert, "ca-cert", "", "CA cert; overrides --config")
	fs.StringVar(&l.nodeCert, "node-cert", "", "client cert; overrides --config")
	fs.StringVar(&l.nodeKey, "node-key", "", "client key; overrides --config")
	fs.DurationVar(&l.timeout, "timeout", 10*time.Second, "per-call timeout")
}

// dial opens an mTLS connection to the node.
//
// mTLS is not optional and there is no plaintext fallback flag. Every
// cluster service already requires it, and a "just for local
// debugging" escape hatch is exactly the flag that ends up in a
// systemd unit — the CLI presents the same credential a peer node
// does, or it does not connect.
func (l *liveNode) dial() (*grpc.ClientConn, error) {
	addr, ca, cert, key, err := l.resolve()
	if err != nil {
		return nil, err
	}
	// Written back so every "this came from <where>" label reports the
	// address actually dialled. Without it a connection resolved from
	// a context or a config.toml left l.addr empty, and the source
	// line — the whole point of naming where an answer came from —
	// printed blank.
	l.addr = addr
	// The CLI only ever dials, so it loads a CLIENT credential. The
	// node loader would verify this certificate against the cluster
	// CA — right for a node, wrong for an OPERATOR, whose certificate
	// chains to the operator CA and would be rejected here before a
	// byte was sent.
	creds, err := mtls.LoadClientCreds(ca, cert, key)
	if err != nil {
		return nil, fmt.Errorf("load client credentials: %w", err)
	}
	conn, err := grpc.NewClient(addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	return conn, nil
}

// ctx returns a context bounded by the configured timeout.
func (l *liveNode) ctx() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), l.timeout)
}

// resolve works out where to connect and with what.
//
// Three sources, in this order: explicit flags, the named context, the
// node's config.toml. A missing piece is named individually rather
// than as "configuration error" — somebody running this on a machine
// that is not a cluster member will be missing exactly one of these,
// and which one is the whole answer.
//
// The context sits ABOVE config.toml deliberately. An operator who has
// gone to the trouble of naming a cluster means that cluster; a
// config.toml found on the same laptop is likelier to be a leftover
// from running a node locally than the thing they meant to reach.
func (l *liveNode) resolve() (addr, ca, cert, key string, err error) {
	addr, ca, cert, key = l.addr, l.caCert, l.nodeCert, l.nodeKey

	if addr == "" || ca == "" || cert == "" || key == "" {
		// Consulted even when no --context was passed, because the file
		// may name a default. An unknown name is a hard error rather
		// than a fallback: see resolveContext.
		named, cerr := resolveContext(l.contextName)
		if cerr != nil {
			return "", "", "", "", cerr
		}
		if addr == "" {
			addr = named.Addr
		}
		if ca == "" {
			ca = named.CACert
		}
		if cert == "" {
			cert = named.Cert
		}
		if key == "" {
			key = named.Key
		}
	}

	if addr == "" || ca == "" || cert == "" || key == "" {
		if l.configPath == "" {
			return "", "", "", "", errors.New(
				"no --context, no --addr / --ca-cert / --node-cert / --node-key, " +
					"and no --config to read them from")
		}
		cfg, cerr := config.Load(config.LoadOptions{Path: l.configPath})
		if cerr != nil {
			return "", "", "", "", fmt.Errorf("load config %q: %w", l.configPath, cerr)
		}
		if addr == "" {
			// AdvertiseAddr is what peers dial, so it is what a client
			// should dial too. ListenAddr can be 0.0.0.0:port, which
			// is a bind address and not somewhere to connect.
			addr = cfg.Cluster.AdvertiseAddr
			if addr == "" {
				addr = dialableListenAddr(cfg.Cluster.ListenAddr)
			}
		}
		if ca == "" {
			ca = cfg.Cluster.MTLS.CACert
		}
		if cert == "" {
			cert = cfg.Cluster.MTLS.NodeCert
		}
		if key == "" {
			key = cfg.Cluster.MTLS.NodeKey
		}
	}

	var missing []string
	for _, f := range []struct{ name, value string }{
		{"--addr", addr}, {"--ca-cert", ca}, {"--node-cert", cert}, {"--node-key", key},
	} {
		if f.value == "" {
			missing = append(missing, f.name)
		}
	}
	if len(missing) > 0 {
		return "", "", "", "", fmt.Errorf(
			"cannot reach a node: missing %v (pass them, or point --config at a config.toml that has them)",
			missing)
	}
	return addr, ca, cert, key, nil
}

// dialableListenAddr turns a bind address into one a client on this
// host can connect to.
//
// The fallback used ListenAddr verbatim, which is 0.0.0.0:7443 on
// every stock deployment — neither `lobslaw init` nor the shipped
// podman config sets advertise_addr. Dialling it failed the TLS
// handshake with "certificate is valid for 127.0.0.1, not 0.0.0.0",
// which reads as a certificate problem rather than as the client
// having aimed at a wildcard.
//
// Loopback is both correct and what the node's own certificate names:
// a CLI reaching a node bound to every interface is reaching the one
// on this machine.
func dialableListenAddr(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil {
		return listen
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]":
		return net.JoinHostPort("127.0.0.1", port)
	}
	return listen
}
