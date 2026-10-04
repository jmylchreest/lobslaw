package grpcinterceptors

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/types/known/emptypb"

	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

func TestPersistenceReconnectRefusesOlderServer(t *testing.T) {
	a, b := bufconn.Listen(1<<20), bufconn.Listen(1<<20)
	current := grpc.NewServer(grpc.UnaryInterceptor(DataFormat()))
	old := grpc.NewServer()
	pb.RegisterNodeServiceServer(current, formatPeer{})
	pb.RegisterNodeServiceServer(old, formatPeer{})
	var mutations atomic.Int32
	old.RegisterService(&grpc.ServiceDesc{ServiceName: "RaftTransport", HandlerType: (*interface{})(nil), Methods: []grpc.MethodDesc{{MethodName: "AppendEntries", Handler: func(_ any, _ context.Context, decode func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
		req := new(emptypb.Empty)
		if err := decode(req); err != nil {
			return nil, err
		}
		mutations.Add(1)
		return &emptypb.Empty{}, nil
	}}}}, new(struct{}))
	go func() { _ = current.Serve(a) }()
	go func() { _ = old.Serve(b) }()
	defer current.Stop()
	defer old.Stop()
	var swapped atomic.Bool
	// Replace the process after connecting but before the mutation dispatch.
	// The versioned endpoint must remain safe across ClientConn reconnects.
	replaceBeforeDispatch := func(ctx context.Context, method string, req, reply any, cc *grpc.ClientConn, invoke grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		if persistenceMethod(method) {
			swapped.Store(true)
			current.Stop()
			for cc.GetState() == connectivity.Ready {
				if !cc.WaitForStateChange(ctx, connectivity.Ready) {
					return ctx.Err()
				}
			}
			cc.Connect()
			for cc.GetState() != connectivity.Ready {
				s := cc.GetState()
				if !cc.WaitForStateChange(ctx, s) {
					t.Fatal("reconnect timeout")
				}
			}
		}
		return invoke(ctx, method, req, reply, cc, opts...)
	}
	cc, err := grpc.NewClient("passthrough:///reconnect", grpc.WithTransportCredentials(insecure.NewCredentials()), grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) {
		if swapped.Load() {
			return b.Dial()
		}
		return a.Dial()
	}), grpc.WithChainUnaryInterceptor(DataFormatClient(), replaceBeforeDispatch))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cc.Close() }()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := pb.NewNodeServiceClient(cc).GetPeers(ctx, &pb.GetPeersRequest{}); err != nil {
		t.Fatal(err)
	}
	err = cc.Invoke(ctx, "/RaftTransport/AppendEntries", &emptypb.Empty{}, &emptypb.Empty{})
	if mutations.Load() != 0 {
		t.Fatalf("old peer mutated after reconnect: count=%d err=%v", mutations.Load(), err)
	}
	if err == nil {
		t.Fatal("older server accepted persistence endpoint")
	}

}
