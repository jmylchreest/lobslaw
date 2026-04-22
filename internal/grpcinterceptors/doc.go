// Package grpcinterceptors supplies the lobslaw-standard gRPC server
// interceptor chain: per-RPC request-ID propagation into the context
// logger, and panic recovery that turns crashes into Internal errors
// instead of killing the process.
//
// Additional interceptors (OTel spans, audit emit) slot in later —
// see Phase 5 (observability) and Phase 11 (audit log).
package grpcinterceptors
