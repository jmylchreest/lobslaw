package discovery

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"time"

	"github.com/jmylchreest/lobslaw/pkg/types"
)

// BroadcastConfig controls the UDP auto-discovery Broadcaster.
//
// The broadcast packet is a hint, not a trust boundary — the actual
// cluster-join handshake still requires mTLS with our CA, so an
// attacker spoofing a packet can only trick us into dialling a bogus
// address where the TLS handshake then fails.
type BroadcastConfig struct {
	// Address is the destination for outbound announces. Typically the
	// limited broadcast address ("255.255.255.255:<port>"). Tests use
	// loopback-scoped addresses.
	Address string

	// ListenAddr is where we bind the inbound socket. ":<port>" listens
	// on all interfaces; operators can restrict via
	// [discovery] broadcast_interface.
	ListenAddr string

	// Interval between self-announcements. Defaults to 30s.
	Interval time.Duration

	// Local is what we announce.
	Local types.NodeInfo

	// Registry receives inbound peers. Folds them in on each recv.
	Registry *Registry

	// Logger receives lifecycle + parse events.
	Logger *slog.Logger
}

// packet is the on-wire shape announced over UDP. Minimal by design —
// peers dial the announced address to get the full handshake.
type packet struct {
	Type    string `json:"type"` // "announce" only for MVP
	NodeID  string `json:"node_id"`
	Address string `json:"address"`
	Cluster string `json:"cluster,omitempty"` // reserved: cluster id filter
}

// Broadcaster runs one announce goroutine + one listen goroutine.
// Stop via context cancellation. Close idempotently releases the socket.
type Broadcaster struct {
	cfg  BroadcastConfig
	conn *net.UDPConn
	log  *slog.Logger
}

// NewBroadcaster binds the listen socket and enables SO_BROADCAST so
// outbound writes to the limited broadcast address aren't rejected by
// the kernel.
func NewBroadcaster(cfg BroadcastConfig) (*Broadcaster, error) {
	if cfg.Registry == nil {
		return nil, fmt.Errorf("BroadcastConfig: Registry required")
	}
	if cfg.Local.ID == "" {
		return nil, fmt.Errorf("BroadcastConfig: Local.ID required")
	}
	if cfg.Interval <= 0 {
		cfg.Interval = 30 * time.Second
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}

	addr, err := net.ResolveUDPAddr("udp4", cfg.ListenAddr)
	if err != nil {
		return nil, fmt.Errorf("resolve listen addr %q: %w", cfg.ListenAddr, err)
	}
	conn, err := net.ListenUDP("udp4", addr)
	if err != nil {
		return nil, fmt.Errorf("listen udp %q: %w", cfg.ListenAddr, err)
	}
	if err := enableBroadcast(conn); err != nil {
		_ = conn.Close()
		return nil, fmt.Errorf("enable SO_BROADCAST: %w", err)
	}

	return &Broadcaster{cfg: cfg, conn: conn, log: cfg.Logger}, nil
}

// Start spawns the announce + listen goroutines and blocks until ctx
// is cancelled. The caller is expected to run this in its own
// goroutine from node.Start.
func (b *Broadcaster) Start(ctx context.Context) error {
	announceDone := make(chan struct{})
	listenDone := make(chan struct{})

	go func() {
		defer close(announceDone)
		b.runAnnouncer(ctx)
	}()
	go func() {
		defer close(listenDone)
		b.runListener(ctx)
	}()

	<-ctx.Done()
	// Closing the conn wakes the Read in runListener.
	_ = b.conn.Close()
	<-announceDone
	<-listenDone
	return nil
}

// Close releases the socket. Safe to call more than once.
func (b *Broadcaster) Close() error {
	return b.conn.Close()
}

// LocalAddr exposes the bound address — useful for tests that listen
// on a loopback ephemeral port.
func (b *Broadcaster) LocalAddr() net.Addr {
	return b.conn.LocalAddr()
}

// runAnnouncer sends one announce immediately then on each Interval.
func (b *Broadcaster) runAnnouncer(ctx context.Context) {
	target, err := net.ResolveUDPAddr("udp4", b.cfg.Address)
	if err != nil {
		b.log.Warn("broadcast: resolve target", "addr", b.cfg.Address, "err", err)
		return
	}

	announce := func() {
		p := packet{
			Type:    "announce",
			NodeID:  string(b.cfg.Local.ID),
			Address: b.cfg.Local.Address,
		}
		data, _ := json.Marshal(&p)
		if _, err := b.conn.WriteToUDP(data, target); err != nil {
			b.log.Debug("broadcast write", "err", err)
		}
	}

	announce() // don't wait a full Interval for the first ping

	t := time.NewTicker(b.cfg.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			announce()
		}
	}
}

// runListener receives packets in a tight loop until the conn closes.
// Each well-formed announce from a peer folds into the registry;
// packets from ourselves are ignored.
func (b *Broadcaster) runListener(ctx context.Context) {
	buf := make([]byte, 4096)
	for {
		if ctx.Err() != nil {
			return
		}
		_ = b.conn.SetReadDeadline(time.Now().Add(1 * time.Second))
		n, src, err := b.conn.ReadFromUDP(buf)
		if err != nil {
			// Closed socket or read deadline. Loop checks ctx.
			if ne, ok := err.(net.Error); ok && ne.Timeout() {
				continue
			}
			return
		}
		var p packet
		if err := json.Unmarshal(buf[:n], &p); err != nil {
			b.log.Debug("broadcast: bad packet", "src", src, "err", err)
			continue
		}
		if p.Type != "announce" || p.NodeID == "" || p.Address == "" {
			continue
		}
		if p.NodeID == string(b.cfg.Local.ID) {
			continue
		}
		b.cfg.Registry.Register(types.NodeInfo{
			ID:      types.NodeID(p.NodeID),
			Address: p.Address,
		})
	}
}

// enableBroadcast sets SO_BROADCAST on the underlying socket. Without
// this, writes to 255.255.255.255 are rejected by the kernel on Linux.
// The actual setsockopt call is platform-specific (fd type differs
// between Unix and Windows); see broadcast_unix.go / broadcast_windows.go.
func enableBroadcast(conn *net.UDPConn) error {
	raw, err := conn.SyscallConn()
	if err != nil {
		return err
	}
	var inner error
	err = raw.Control(func(fd uintptr) {
		inner = setSoBroadcast(fd)
	})
	if err != nil {
		return err
	}
	return inner
}
