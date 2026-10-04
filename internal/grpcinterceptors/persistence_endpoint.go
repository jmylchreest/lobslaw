package grpcinterceptors

import (
	"strings"

	"google.golang.org/grpc"
)

// PersistenceServicePrefix is a wire-level mutation fence. Pre-control binaries
// have no service at this name, even if ClientConn reconnects after negotiation.
// New implementations must enforce compatibility headers on these endpoints.
const PersistenceServicePrefix = "lobslaw.persistence.v1."

func persistenceEndpoint(method string) string {
	return "/" + PersistenceServicePrefix + strings.TrimPrefix(method, "/")
}

func originalPersistenceMethod(method string) string {
	return "/" + strings.TrimPrefix(strings.TrimPrefix(method, "/"), PersistenceServicePrefix)
}

// PersistenceRegistrar keeps the ordinary service for discovery and registers
// typed persistence methods under a versioned service name. It preserves the
// generated request/response handlers; there is no envelope or JSON tunnel.
type PersistenceRegistrar struct{ grpc.ServiceRegistrar }

func (r PersistenceRegistrar) RegisterService(desc *grpc.ServiceDesc, impl any) {
	r.ServiceRegistrar.RegisterService(desc, impl)
	alias := *desc
	alias.ServiceName = PersistenceServicePrefix + desc.ServiceName
	alias.Methods = nil
	alias.Streams = nil
	for _, method := range desc.Methods {
		if persistenceMethod("/" + desc.ServiceName + "/" + method.MethodName) {
			alias.Methods = append(alias.Methods, method)
		}
	}
	for _, stream := range desc.Streams {
		if persistenceMethod("/" + desc.ServiceName + "/" + stream.StreamName) {
			alias.Streams = append(alias.Streams, stream)
		}
	}
	if len(alias.Methods)+len(alias.Streams) > 0 {
		r.ServiceRegistrar.RegisterService(&alias, impl)
	}
}
