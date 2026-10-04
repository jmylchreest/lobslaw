package grpcinterceptors

import (
	"context"
	"slices"
	"strconv"
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
	return strings.HasPrefix(method, "/raft.") || strings.HasPrefix(method, "/RaftTransport/") || method == pb.NodeService_AddMember_FullMethodName || method == pb.NodeService_Propose_FullMethodName
}

type ContractProvider func() uint32

func requiredContract(providers []ContractProvider) uint32 {
	if len(providers) > 0 && providers[0] != nil {
		return providers[0]()
	}
	return 1
}

const requiredHeader = "lobslaw-data-required"
const supportedHeader = "lobslaw-data-supported"

func contractHeaders(required uint32) metadata.MD {
	values := make([]string, 0)
	for _, v := range dataformat.SupportedContracts() {
		values = append(values, strconv.FormatUint(uint64(v), 10))
	}
	return metadata.Pairs(dataProtocolHeader, dataformat.ControlProtocol, requiredHeader, strconv.FormatUint(uint64(required), 10), supportedHeader, strings.Join(values, ","))
}
func checkHeaders(md metadata.MD, required uint32) error {
	protocols := md.Get(dataProtocolHeader)
	needs := md.Get(requiredHeader)
	sets := md.Get(supportedHeader)
	if len(protocols) != 1 || protocols[0] != dataformat.ControlProtocol || len(needs) != 1 || len(sets) != 1 || required == 0 {
		return status.Error(codes.FailedPrecondition, "missing rolling upgrade protocol; coordinated initial upgrade required")
	}
	remoteRequired, err := strconv.ParseUint(needs[0], 10, 32)
	if err != nil || !slices.Contains(dataformat.SupportedContracts(), uint32(remoteRequired)) {
		return status.Error(codes.FailedPrecondition, "binary cannot read peer active/prepared contract")
	}
	for _, v := range strings.Split(sets[0], ",") {
		if v == strconv.FormatUint(uint64(required), 10) {
			return nil
		}
	}
	return status.Error(codes.FailedPrecondition, "peer cannot read local active/prepared contract")
}
func checkDataProtocol(ctx context.Context, required uint32) error {
	md, _ := metadata.FromIncomingContext(ctx)
	return checkHeaders(md, required)
}

// DataFormat is a compatibility gate, never an identity check. It follows the
// existing mTLS/operator guard. GetPeers provides a read-only preflight handshake.
func DataFormat(providers ...ContractProvider) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		if info.FullMethod == pb.NodeService_GetPeers_FullMethodName {
			if err := grpc.SetHeader(ctx, contractHeaders(requiredContract(providers))); err != nil {
				return nil, err
			}
		}
		if persistenceMethod(info.FullMethod) {
			if err := checkDataProtocol(ctx, requiredContract(providers)); err != nil {
				return nil, err
			}
		}
		return handler(ctx, req)
	}
}

func DataFormatStream(providers ...ContractProvider) grpc.StreamServerInterceptor {
	return func(srv any, stream grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		if persistenceMethod(info.FullMethod) {
			if err := checkDataProtocol(stream.Context(), requiredContract(providers)); err != nil {
				return err
			}
		}
		return handler(srv, stream)
	}
}

func dataContext(ctx context.Context, required uint32) context.Context {
	md, _ := metadata.FromOutgoingContext(ctx)
	md = md.Copy()
	for key, values := range contractHeaders(required) {
		md.Set(key, values...)
	}
	return metadata.NewOutgoingContext(ctx, md)
}

// VerifyDataPeer checks the remote reader before replication or membership changes.
func VerifyDataPeer(ctx context.Context, conn *grpc.ClientConn, providers ...ContractProvider) error {
	ctx, cancel := context.WithTimeout(ctx, compatibilityTimeout)
	defer cancel()
	var header metadata.MD
	_, err := pb.NewNodeServiceClient(conn).GetPeers(ctx, &pb.GetPeersRequest{}, grpc.Header(&header))
	if err != nil {
		return err
	}
	return checkHeaders(header, requiredContract(providers))
}

// DataFormatClient preflights BEFORE sending any log or snapshot to an old server. Merely
// attaching a header is insufficient: old servers would ignore it and mutate.
func DataFormatClient(providers ...ContractProvider) grpc.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req, reply any, conn *grpc.ClientConn, invoke grpc.UnaryInvoker, opts ...grpc.CallOption) error {
		if persistenceMethod(method) {
			if err := VerifyDataPeer(ctx, conn, providers...); err != nil {
				return err
			}
			ctx = dataContext(ctx, requiredContract(providers))
		}
		return invoke(ctx, method, req, reply, conn, opts...)
	}
}

func DataFormatStreamClient(providers ...ContractProvider) grpc.StreamClientInterceptor {
	return func(ctx context.Context, desc *grpc.StreamDesc, conn *grpc.ClientConn, method string, streamer grpc.Streamer, opts ...grpc.CallOption) (grpc.ClientStream, error) {
		if persistenceMethod(method) {
			if err := VerifyDataPeer(ctx, conn, providers...); err != nil {
				return nil, err
			}
			ctx = dataContext(ctx, requiredContract(providers))
		}
		return streamer(ctx, desc, conn, method, opts...)
	}
}
