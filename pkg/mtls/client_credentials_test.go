package mtls

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// Exercise a retained credential, not a newly built tls.Config after Reload.
func clientHandshake(t *testing.T, client credentials.TransportCredentials, server *NodeCreds, authority string) (string, error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	type result struct {
		name string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		raw, err := ln.Accept()
		if err != nil {
			done <- result{err: err}
			return
		}
		defer func() { _ = raw.Close() }()
		_ = raw.SetDeadline(time.Now().Add(5 * time.Second))
		_, info, err := server.ServerCreds().ServerHandshake(raw)
		name := ""
		if err == nil {
			name = info.(credentials.TLSInfo).State.PeerCertificates[0].Subject.CommonName
		}
		done <- result{name, err}
	}()
	raw, err := net.DialTimeout("tcp", ln.Addr().String(), 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	_, _, err = client.ClientHandshake(ctx, authority, raw)
	other := <-done
	return other.name, errors.Join(err, other.err)
}

func TestRetainedClientCredentialsFollowCARotation(t *testing.T) {
	oldCA, _, oldCert, oldKey := setupCluster(t, "old-peer")
	newCA, _, newCert, newKey := setupCluster(t, "new-peer")
	client, err := LoadNodeCreds(oldCA, oldCert, oldKey)
	if err != nil {
		t.Fatal(err)
	}
	oldServer, err := LoadNodeCreds(oldCA, oldCert, oldKey)
	if err != nil {
		t.Fatal(err)
	}
	newServer, err := LoadNodeCreds(newCA, newCert, newKey)
	if err != nil {
		t.Fatal(err)
	}
	oldPEM, err := os.ReadFile(oldCA)
	if err != nil {
		t.Fatal(err)
	}
	newPEM, err := os.ReadFile(newCA)
	if err != nil {
		t.Fatal(err)
	}
	retained := client.ClientCreds()
	clients := []credentials.TransportCredentials{retained, retained.Clone()}
	for _, c := range clients {
		if _, err := clientHandshake(t, c, oldServer, "old-peer"); err != nil {
			t.Fatal(err)
		}
		if _, err := clientHandshake(t, c, newServer, "new-peer"); err == nil {
			t.Fatal("accepted untrusted new CA")
		}
	}
	// The new server must trust the client's old certificate during overlap.
	bundle := append(append([]byte{}, oldPEM...), newPEM...)
	for _, path := range []string{oldCA, newCA} {
		if err := os.WriteFile(path, bundle, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := client.Reload(); err != nil {
		t.Fatal(err)
	}
	if err := newServer.Reload(); err != nil {
		t.Fatal(err)
	}
	for _, c := range clients {
		if name, err := clientHandshake(t, c, newServer, "new-peer"); err != nil || name != "old-peer" {
			t.Fatalf("overlap handshake: %q %v", name, err)
		}
		if _, err := clientHandshake(t, c, oldServer, "old-peer"); err != nil {
			t.Fatal(err)
		}
	}
	// Rotate the local leaf too, then remove the old trust root.
	for dst, src := range map[string]string{oldCert: newCert, oldKey: newKey} {
		raw, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(oldCA, newPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := client.Reload(); err != nil {
		t.Fatal(err)
	}
	for _, c := range clients {
		if name, err := clientHandshake(t, c, newServer, "new-peer"); err != nil || name != "new-peer" {
			t.Fatalf("rotated handshake: %q %v", name, err)
		}
		if _, err := clientHandshake(t, c, oldServer, "old-peer"); err == nil {
			t.Fatal("accepted retired root")
		}
		if _, err := clientHandshake(t, c, newServer, "wrong-host"); err == nil {
			t.Fatal("accepted wrong hostname")
		}
	}
}

func TestClientCredentialsRejectOperatorSignedServers(t *testing.T) {
	client, clusterCA := nodeCredsWith(t)
	opCA, opKey := operatorCA(t)
	if err := client.TrustOperatorCA(pemOf(t, opCA)); err != nil {
		t.Fatal(err)
	}
	// ServerAuth is intentional: this must fail on trust, not just EKU.
	cert, key, err := SignNodeCert(opCA, opKey, SignOpts{NodeID: "impostor"})
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.X509KeyPair(cert, key)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{pair}, ClientCAs: poolOf(clusterCA), ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS13, NextProtos: []string{"h2"}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	done := make(chan struct{})
	go func() {
		defer close(done)
		c, e := ln.Accept()
		if e == nil {
			defer func() { _ = c.Close() }()
			_ = c.SetDeadline(time.Now().Add(5 * time.Second))
			_ = c.(*tls.Conn).Handshake()
		}
	}()
	raw, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, _, err := client.ClientCreds().ClientHandshake(ctx, "impostor", raw); err == nil {
		t.Fatal("operator CA entered outgoing trust")
	}
	<-done
}

func TestClientCredentialsConcurrentReloadAndTrust(t *testing.T) {
	creds, _ := nodeCredsWith(t)
	opCA, _ := operatorCA(t)
	c := creds.ClientCreds()
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 10 {
				if err := creds.Reload(); err != nil {
					t.Error(err)
				}
				if err := creds.TrustOperatorCA(pemOf(t, opCA)); err != nil {
					t.Error(err)
				}
				_ = creds.CAPool()
				_ = creds.Certificate()
				_ = creds.OperatorCA()
				clone := c.Clone()
				_ = clone.Info()
				if err := clone.(*clientCredentials).OverrideServerName("node-a"); err != nil {
					t.Error(err)
				}
			}
		})
	}
	wg.Go(func() {
		for range 10 {
			if _, err := clientHandshake(t, c, creds, "node-a"); err != nil {
				t.Error(err)
			}
		}
	})
	wg.Wait()
	if creds.OperatorCA() == nil {
		t.Fatal("operator anchor lost")
	}
}

func TestClientCredentialsCloneAndCancellation(t *testing.T) {
	creds, _ := nodeCredsWith(t)
	original := creds.ClientCreds()
	if err := original.(*clientCredentials).OverrideServerName("node-a"); err != nil {
		t.Fatal(err)
	}
	clone := original.Clone()
	if err := original.(*clientCredentials).OverrideServerName("changed"); err != nil {
		t.Fatal(err)
	}
	//nolint:staticcheck // Verify the legacy TransportCredentials interface still preserves clone settings.
	if clone.Info().ServerName != "node-a" || original.Info().ServerName != "changed" {
		t.Fatal("clone shares mutable override")
	}
	client, peer := net.Pipe()
	defer func() { _ = peer.Close() }()
	drained := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, peer); close(drained) }()
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, _, err := clone.ClientHandshake(ctx, "node-a", client); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation: %v", err)
	}
	select {
	case <-drained:
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled handshake did not close connection")
	}
}

func TestRetainedGRPCConnectionReconnectsAfterCAReload(t *testing.T) {
	oldCA, _, oldCert, oldKey := setupCluster(t, "node-a")
	newCA, _, newCert, newKey := setupCluster(t, "node-a")
	client, err := LoadNodeCreds(oldCA, oldCert, oldKey)
	if err != nil {
		t.Fatal(err)
	}
	server, err := LoadNodeCreds(oldCA, oldCert, oldKey)
	if err != nil {
		t.Fatal(err)
	}
	start := func(address string) (*grpc.Server, string) {
		t.Helper()
		ln, err := net.Listen("tcp", address)
		if err != nil {
			t.Fatal(err)
		}
		srv := grpc.NewServer(grpc.Creds(server.ServerCreds()))
		healthpb.RegisterHealthServer(srv, health.NewServer())
		go func() { _ = srv.Serve(ln) }()
		t.Cleanup(srv.Stop)
		return srv, ln.Addr().String()
	}
	srv, address := start("127.0.0.1:0")
	conn, err := grpc.NewClient(address, grpc.WithTransportCredentials(client.ClientCreds()), grpc.WithAuthority("node-a"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	check := func() {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		if _, err := healthpb.NewHealthClient(conn).Check(ctx, &healthpb.HealthCheckRequest{}, grpc.WaitForReady(true)); err != nil {
			t.Fatal(err)
		}
	}
	check()
	for dst, src := range map[string]string{oldCA: newCA, oldCert: newCert, oldKey: newKey} {
		raw, err := os.ReadFile(src)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, raw, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := client.Reload(); err != nil {
		t.Fatal(err)
	}
	if err := server.Reload(); err != nil {
		t.Fatal(err)
	}
	srv.Stop()
	// Wait until the client observes the stopped transport before making another
	// RPC. WaitForReady does not retry a call sent on the dying connection.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	for conn.GetState() == connectivity.Ready {
		if !conn.WaitForStateChange(ctx, connectivity.Ready) {
			t.Fatal("client did not observe the stopped transport:", ctx.Err())
		}
	}
	_, _ = start(address)
	check()
}

func TestFailedReloadPreservesCredentialGeneration(t *testing.T) {
	for _, which := range []string{"CA PEM", "mismatched key", "untrusted certificate"} {
		t.Run(which, func(t *testing.T) {
			ca, _, cert, key := setupCluster(t, "node-a")
			otherCA, _, _, otherKey := setupCluster(t, "other")
			creds, err := LoadNodeCreds(ca, cert, key)
			if err != nil {
				t.Fatal(err)
			}
			retained, previous := creds.ClientCreds(), creds.state()
			path, raw := ca, []byte("invalid PEM")
			if which != "CA PEM" {
				src := otherCA
				if which == "mismatched key" {
					path, src = key, otherKey
				}
				raw, err = os.ReadFile(src)
				if err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(path, raw, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := creds.Reload(); err == nil {
				t.Fatal("invalid reload succeeded")
			}
			if creds.state() != previous {
				t.Fatal("failed reload published state")
			}
			if _, err := clientHandshake(t, retained, creds, "node-a"); err != nil {
				t.Fatal(err)
			}
		})
	}
}
