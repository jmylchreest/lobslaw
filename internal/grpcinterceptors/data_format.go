package grpcinterceptors

import (
	"context"
	"strings"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/jmylchreest/lobslaw/internal/dataformat"
	pb "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

const dataProtocolHeader = "lobslaw-data-protocol"
const compatibilityTimeout = 5 * time.Second

func persistenceMethod(method string) bool {
	return strings.HasPrefix(method, "/raft.") || strings.HasPrefix(method, "/RaftTransport/") || method == pb.NodeService_AddMember_FullMethodName
}

func checkDataProtocol(ctx context.Context) error {
	md, _ := metadata.FromIncomingContext(ctx)
	values := md.Get(dataProtocolHeader)
	if len(values) != 1 || values[0] != dataformat.ClusterProtocol {
		return status.Error(codes.FailedPrecondition, "incompatible persistence protocol; coordinated cluster upgrade required")
	}
	return nil
}

// DataFormat is a compatibility gate, never an identity check. It follows the
// existing mTLS/operator guard. GetPeers provides a read-only preflight handshake.
func DataFormat() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if info.FullMethod == pb.NodeService_GetPeers_FullMethodName {
			if err := grpc.SetHeader(ctx, metadata.Pairs(dataProtocolHeader, dataformat.ClusterProtocol)); err != nil {
				return nil, err
			}
		}
		if persistenceMethod(info.FullMethod) {
			if err := checkDataProtocol(ctx); err != nil {
				return nil, err
			}
		}
		return handler(ctx, req)
	}
}

func DataFormatStream() grpc.StreamServerInterceptor {
	return func(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if persistenceMethod(info.FullMethod) {
			if err := checkDataProtocol(stream.Context()); err != nil {
				return err
			}
		}
		return handler(srv, stream)
	}
}

func dataContext(ctx context.Context) context.Context {
	md, _ := metadata.FromOutgoingContext(ctx)
	md = md.Copy()
	md.Set(dataProtocolHeader, dataformat.ClusterProtocol)
	return metadata.NewOutgoingContext(ctx, md)
}

// VerifyDataPeer checks the remote reader before replication or membership changes.
func VerifyDataPeer(ctx context.Context, conn *grpc.ClientConn) error {
	ctx, cancel := context.WithTimeout(ctx, compatibilityTimeout)
	defer cancel()
	var header metadata.MD
	_, err := pb.NewNodeServiceClient(conn).GetPeers(ctx, &pb.GetPeersRequest{}, grpc.Header(&header))
	if err != nil {
		return err
	}
	values := header.Get(dataProtocolHeader)
	if len(values) != 1 || values[0] != dataformat.ClusterProtocol {
		return status.Error(codes.FailedPrecondition, "peer lacks compatible persistence protocol; upgrade all cluster nodes before replication")
	}
	return nil
}

// DataFormatClient preflights BEFORE sending any log or snapshot to an old server. Merely
// attaching a header is insufficient: old servers would ignore it and mutate.
func DataFormatClient() grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, conn *grpc.ClientConn, invoke grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		if persistenceMethod(method) {
			if err := VerifyDataPeer(ctx, conn); err != nil {
				return err
			}
			ctx = dataContext(ctx)
		}
		return invoke(ctx, method, req, reply, conn, opts...)
	}
}

func DataFormatStreamClient() grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, conn *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		if persistenceMethod(method) {
			if err := VerifyDataPeer(ctx, conn); err != nil {
				return nil, err
			}
			ctx = dataContext(ctx)
		}
		return streamer(ctx, desc, conn, method, opts...)
	}
}
