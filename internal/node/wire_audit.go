package node

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/jmylchreest/lobslaw/internal/audit"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// wireAudit constructs the AuditLog coordinator and registers the
// AuditService on the gRPC server. Silently no-ops when both sinks
// are disabled in config — the log object is still created (so
// callers can Append to a nil-sink log without special-casing) but
// no gRPC service is registered to avoid confusing clients with a
// service that will never produce results.
func (n *Node) wireAudit(ctx context.Context) error {
	var sinks []audit.AuditSink

	if n.cfg.Audit.Local.Enabled {
		path := n.cfg.Audit.Local.Path
		if path == "" {
			path = filepath.Join(n.cfg.DataDir, "audit", "audit.jsonl")
		}
		ls, err := audit.NewLocalSink(audit.LocalConfig{
			Path:      path,
			MaxSizeMB: n.cfg.Audit.Local.MaxSizeMB,
			MaxFiles:  n.cfg.Audit.Local.MaxFiles,
		})
		if err != nil {
			return fmt.Errorf("local sink: %w", err)
		}
		sinks = append(sinks, ls)
	}

	if n.cfg.Audit.Raft.Enabled {
		if n.raft == nil || n.store == nil {
			n.log.Warn("audit: raft sink requested but node doesn't host Raft; skipping")
		} else {
			rs, err := audit.NewRaftSink(audit.RaftConfig{
				Raft:  n.raft,
				Store: n.store,
			})
			if err != nil {
				return fmt.Errorf("raft sink: %w", err)
			}
			sinks = append(sinks, rs)
		}
	}

	log, err := audit.NewAuditLog(ctx, audit.Config{
		Sinks:  sinks,
		Logger: n.log,
	})
	if err != nil {
		return fmt.Errorf("coordinator: %w", err)
	}
	n.auditLog = log

	if len(sinks) == 0 {
		n.log.Info("audit: no sinks configured — log is a no-op")
		return nil
	}

	svc, err := audit.NewService(log)
	if err != nil {
		return fmt.Errorf("service: %w", err)
	}
	n.auditSvc = svc
	lobslawv1.RegisterAuditServiceServer(n.server, svc)

	sinkNames := make([]string, len(sinks))
	for i, s := range sinks {
		sinkNames[i] = s.Name()
	}
	n.log.Info("audit wired", "sinks", sinkNames)
	return nil
}

// seedStorageMountsFromConfig propagates [[storage.mounts]]
// config entries into the Raft-backed storage bucket so they
// show up in the live Manager + debug_storage + skill resolver.
// Idempotent: AddMount on an existing label is a no-op Replace,
// and we skip labels already in the store via Get.
//
// Without this, operators who declare mounts in config.toml see
// debug_storage return [] until they manually gRPC-call
// StorageService.AddMount, which is silly for local dev.
