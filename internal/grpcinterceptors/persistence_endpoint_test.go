package grpcinterceptors

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/emptypb"

	"github.com/jmylchreest/lobslaw/pkg/mtls"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Use real verified TLS chains: a synthetic peer context would miss credential
// classification errors at the versioned stream's actual dispatch boundary.
func persistenceTestCredentials(t *testing.T, operator bool) (credentials.TransportCredentials, credentials.TransportCredentials) {
	t.Helper()
	caPEM, keyPEM, err := mtls.GenerateCA(mtls.CAOpts{})
	if err != nil {
		t.Fatal(err)
	}
	caPair, err := tls.X509KeyPair(caPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}
	ca, err := x509.ParseCertificate(caPair.Certificate[0])
	if err != nil {
		t.Fatal(err)
	}
	caKey := caPair.PrivateKey.(ed25519.PrivateKey)
	serverPEM, serverKey, err := mtls.SignNodeCert(ca, caKey, mtls.SignOpts{NodeID: "persistence-server"})
	if err != nil {
		t.Fatal(err)
	}
	serverPair, err := tls.X509KeyPair(serverPEM, serverKey)
	if err != nil {
		t.Fatal(err)
	}
	sign := mtls.SignNodeCert
	if operator {
		sign = mtls.SignOperatorCert
	}
	clientPEM, clientKey, err := sign(ca, caKey, mtls.SignOpts{NodeID: "persistence-caller"})
	if err != nil {
		t.Fatal(err)
	}
	clientPair, err := tls.X509KeyPair(clientPEM, clientKey)
	if err != nil {
		t.Fatal(err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		t.Fatal("invalid CA")
	}
	return credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{serverPair}, ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: roots}),
		credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{clientPair}, RootCAs: roots, ServerName: "persistence-server"})
}

func TestPersistenceStreamDispatchAndGuards(t *testing.T) {
	for _, tc := range []struct {
		name                 string
		operator, clientGate bool
		badHeader            bool
		want                 codes.Code
	}{
		{name: "compatible-peer", clientGate: true, want: codes.OK},
		{name: "missing-protocol", want: codes.FailedPrecondition},
		{name: "wrong-protocol", badHeader: true, want: codes.FailedPrecondition},
		{name: "operator", operator: true, clientGate: true, want: codes.PermissionDenied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			serverCreds, clientCreds := persistenceTestCredentials(t, tc.operator)
			var methodSeen atomic.Value
			observe := func(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, next grpc.StreamHandler) error {
				methodSeen.Store(info.FullMethod)
				return next(srv, stream)
			}
			server := grpc.NewServer(grpc.Creds(serverCreds), grpc.ChainStreamInterceptor(observe, OperatorNotAPeerStream(), DataFormatStream()))
			var mutations atomic.Int32
			PersistenceRegistrar{ServiceRegistrar: server}.RegisterService(&grpc.ServiceDesc{ServiceName: "RaftTransport", HandlerType: (*interface{})(nil), Streams: []grpc.StreamDesc{{StreamName: "InstallSnapshot", ClientStreams: true, Handler: func(_ any, stream grpc.ServerStream) error {
				if err := stream.RecvMsg(new(emptypb.Empty)); err != nil {
					return err
				}
				mutations.Add(1)
				return stream.SendMsg(new(emptypb.Empty))
			}}}}, new(struct{}))
			listener := bufconn.Listen(1 << 20)
			go func() { _ = server.Serve(listener) }()
			defer server.Stop()
			opts := []grpc.DialOption{grpc.WithTransportCredentials(clientCreds), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() })}
			method := "/RaftTransport/InstallSnapshot"
			if tc.clientGate {
				opts = append(opts, grpc.WithStreamInterceptor(DataFormatStreamClient()))
			} else {
				method = persistenceEndpoint(method)
			}
			conn, err := grpc.NewClient("passthrough:///persistence-server", opts...)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if tc.badHeader {
				ctx = metadata.AppendToOutgoingContext(ctx, dataProtocolHeader, "unsupported")
			}
			stream, err := conn.NewStream(ctx, &grpc.StreamDesc{ClientStreams: true}, method)
			if err == nil {
				// Rejections can arrive before or after SendMsg. RecvMsg carries the
				// server status in both cases.
				_ = stream.SendMsg(new(emptypb.Empty))
				_ = stream.CloseSend()
				err = stream.RecvMsg(new(emptypb.Empty))
			}
			if status.Code(err) != tc.want {
				t.Fatalf("status=%v want=%v err=%v", status.Code(err), tc.want, err)
			}
			wantMutations := int32(0)
			if tc.want == codes.OK {
				wantMutations = 1
			}
			if mutations.Load() != wantMutations {
				t.Fatalf("mutations=%d want=%d", mutations.Load(), wantMutations)
			}
			if methodSeen.Load() != persistenceEndpoint("/RaftTransport/InstallSnapshot") {
				t.Fatalf("dispatched method=%v", methodSeen.Load())
			}
		})
	}
}

type persistenceNodePeer struct {
	pb.UnimplementedNodeServiceServer
	mutations atomic.Int32
}

func (p *persistenceNodePeer) Propose(context.Context, *pb.ProposeRequest) (*pb.ProposeResponse, error) {
	p.mutations.Add(1)
	return &pb.ProposeResponse{}, nil
}
func (p *persistenceNodePeer) AddMember(context.Context, *pb.AddMemberRequest) (*pb.AddMemberResponse, error) {
	p.mutations.Add(1)
	return &pb.AddMemberResponse{}, nil
}

func TestPersistenceNodeAliases(t *testing.T) {
	serverCreds, clientCreds := persistenceTestCredentials(t, false)
	server := grpc.NewServer(grpc.Creds(serverCreds), grpc.ChainUnaryInterceptor(OperatorNotAPeer(), DataFormat()))
	impl := new(persistenceNodePeer)
	pb.RegisterNodeServiceServer(PersistenceRegistrar{ServiceRegistrar: server}, impl)
	listener := bufconn.Listen(1 << 20)
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()
	opts := []grpc.DialOption{grpc.WithTransportCredentials(clientCreds), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() })}
	conn, err := grpc.NewClient("passthrough:///persistence-server", append(opts, grpc.WithUnaryInterceptor(DataFormatClient()))...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	client := pb.NewNodeServiceClient(conn)
	if _, err := client.Propose(ctx, &pb.ProposeRequest{}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.AddMember(ctx, &pb.AddMemberRequest{}); err != nil {
		t.Fatal(err)
	}
	if impl.mutations.Load() != 2 {
		t.Fatalf("mutation count=%d", impl.mutations.Load())
	}
	raw, err := grpc.NewClient("passthrough:///persistence-server", opts...)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = raw.Close() }()
	if err := raw.Invoke(ctx, persistenceEndpoint(pb.NodeService_Propose_FullMethodName), &pb.ProposeRequest{}, &pb.ProposeResponse{}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("unguarded proposal: %v", err)
	}
	if err := raw.Invoke(ctx, persistenceEndpoint(pb.NodeService_AddMember_FullMethodName), &pb.AddMemberRequest{}, &pb.AddMemberResponse{}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("unguarded member addition: %v", err)
	}
	if impl.mutations.Load() != 2 {
		t.Fatalf("rejected calls mutated count=%d", impl.mutations.Load())
	}
	if err := raw.Invoke(ctx, persistenceEndpoint(pb.NodeService_GetPeers_FullMethodName), &pb.GetPeersRequest{}, &pb.GetPeersResponse{}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("aliased unrelated discovery method: %v", err)
	}
}
