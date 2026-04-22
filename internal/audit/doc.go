// Package audit implements the dual-sink audit log: Raft-backed
// (cluster-authoritative) and local JSONL (defence-in-depth +
// single-node primary). Entries are SHA-256 hash-chained; chain
// is preserved across lumberjack rotation boundaries on the local
// sink.
package audit
