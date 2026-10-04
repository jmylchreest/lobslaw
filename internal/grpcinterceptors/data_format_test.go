package grpcinterceptors

import (
	"context"
	"net"
	"sync/atomic"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/emptypb"

	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

type formatPeer struct {
	pb.UnimplementedNodeServiceServer
	preflights *atomic.Int32
}

func (p formatPeer) GetPeers(context.Context, *pb.GetPeersRequest) (*pb.GetPeersResponse, error) {
	if p.preflights != nil {
		p.preflights.Add(1)
	}
	return &pb.GetPeersResponse{}, nil
}

func TestDataProtocolPreflightBeforeMutation(t *testing.T) {
	for _, compatible := range []bool{false, true} {
		t.Run(map[bool]string{false: "old-peer", true: "compatible-peer"}[compatible], func(t *testing.T) {
			listener := bufconn.Listen(1 << 20)
			var serverOptions []grpc.ServerOption
			if compatible {
				serverOptions = []grpc.ServerOption{grpc.UnaryInterceptor(DataFormat()), grpc.StreamInterceptor(DataFormatStream())}
			}
			server := grpc.NewServer(serverOptions...)
			var preflights atomic.Int32
			pb.RegisterNodeServiceServer(server, formatPeer{preflights: &preflights})
			var mutations atomic.Int32
			registrar := grpc.ServiceRegistrar(server)
			if compatible {
				registrar = PersistenceRegistrar{ServiceRegistrar: server}
			}
			registrar.RegisterService(&grpc.ServiceDesc{ServiceName: "RaftTransport", HandlerType: (*interface{})(nil), Methods: []grpc.MethodDesc{{MethodName: "AppendEntries", Handler: func(srv any, ctx context.Context, decode func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
				request := new(emptypb.Empty)
				if err := decode(request); err != nil {
					return nil, err
				}
				handler := func(context.Context, any) (any, error) { mutations.Add(1); return &emptypb.Empty{}, nil }
				if interceptor != nil {
					return interceptor(ctx, request, &grpc.UnaryServerInfo{Server: srv, FullMethod: "/RaftTransport/AppendEntries"}, handler)
				}
				return handler(ctx, request)
			}}}}, new(struct{}))
			go func() { _ = server.Serve(listener) }()
			t.Cleanup(server.Stop)
			conn, err := grpc.NewClient("passthrough:///test", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }), grpc.WithUnaryInterceptor(DataFormatClient()), grpc.WithStreamInterceptor(DataFormatStreamClient()))
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = conn.Close() }()
			err = conn.Invoke(context.Background(), "/RaftTransport/AppendEntries", &emptypb.Empty{}, &emptypb.Empty{})
			if preflights.Load() != 0 {
				t.Fatal("per-mutation preflight returned to consensus path")
			}
			if compatible {
				if err != nil || mutations.Load() != 1 {
					t.Fatal(err, mutations.Load())
				}
			} else {
				if status.Code(err) != codes.FailedPrecondition || mutations.Load() != 0 {
					t.Fatal("sent mutation to old peer", err, mutations.Load())
				}
				stream, err := conn.NewStream(context.Background(), &grpc.StreamDesc{ClientStreams: true}, "/RaftTransport/InstallSnapshot")
				if err == nil {
					err = stream.RecvMsg(new(emptypb.Empty))
				}
				if status.Code(err) != codes.Unimplemented {
					t.Fatal("old peer accepted snapshot endpoint", err)
				}
			}
		})
	}
}

func TestDataProtocolServerRejectsUnversionedPeer(t *testing.T) {
	called := false
	_, err := DataFormat()(context.Background(), nil, &grpc.UnaryServerInfo{FullMethod: pb.NodeService_AddMember_FullMethodName}, func(context.Context, any) (any, error) { called = true; return nil, nil })
	if status.Code(err) != codes.FailedPrecondition || called {
		t.Fatal("accepted incompatible join", err)
	}
}

func TestForwardedRaftProposalRequiresProtocol(t *testing.T) {
	called := false
	_, err := DataFormat()(context.Background(), &pb.ProposeRequest{}, &grpc.UnaryServerInfo{FullMethod: pb.NodeService_Propose_FullMethodName}, func(context.Context, any) (any, error) { called = true; return nil, nil })
	if status.Code(err) != codes.FailedPrecondition || called {
		t.Fatal("accepted old serialized proposal", err)
	}
}

func TestVersionedPersistenceRetainsOperatorGuard(t *testing.T) {
	for _, method := range []string{"/RaftTransport/AppendEntries", "/RaftTransport/InstallSnapshot"} {
		if !isPeerOnly(persistenceEndpoint(method)) {
			t.Fatalf("versioned endpoint bypasses peer guard: %s", method)
		}
		if !persistenceMethod(persistenceEndpoint(method)) {
			t.Fatalf("versioned endpoint bypasses data guard: %s", method)
		}
	}
}
