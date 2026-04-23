package storage

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/memory"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// BackendFactory constructs a Mount for a given backend type +
// StorageMount config. The storage Service hands registered
// factories the replicated config; factories live in sibling
// packages (local/, nfs/, rclone/) and register themselves at boot
// via the SecretResolver-aware adapter in node.go.
type BackendFactory func(cfg *lobslawv1.StorageMount) (Mount, error)

// Service implements lobslawv1.StorageServiceServer. Writes go
// through Raft; reads hit the local FSM-backed store. An observer
// goroutine watches the replicated storage_mounts bucket and
// synchronises the local Manager on every change.
type Service struct {
	lobslawv1.UnimplementedStorageServiceServer

	raft      raftApplier
	store     *memory.Store
	fsm       *memory.FSM
	manager   *Manager
	factories map[string]BackendFactory
	log       *slog.Logger

	applyTimeout time.Duration
}

// raftApplier is the subset of *memory.RaftNode the service uses.
// Kept as an interface so unit tests can substitute a fake.
type raftApplier interface {
	Apply(data []byte, timeout time.Duration) (any, error)
}

// ServiceConfig bundles the dependencies Service needs. Kept
// explicit so node.go's wiring reads cleanly.
type ServiceConfig struct {
	Raft         raftApplier
	Store        *memory.Store
	FSM          *memory.FSM
	Manager      *Manager
	Factories    map[string]BackendFactory
	Logger       *slog.Logger
	ApplyTimeout time.Duration
}

// NewService wires the service. Invariants checked at construction
// so a misconfigured node fails loud at boot rather than when the
// first RPC lands.
func NewService(cfg ServiceConfig) (*Service, error) {
	if cfg.Raft == nil {
		return nil, errors.New("storage.Service: Raft required")
	}
	if cfg.Store == nil {
		return nil, errors.New("storage.Service: Store required")
	}
	if cfg.Manager == nil {
		return nil, errors.New("storage.Service: Manager required")
	}
	if cfg.Factories == nil {
		cfg.Factories = make(map[string]BackendFactory)
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.ApplyTimeout <= 0 {
		cfg.ApplyTimeout = 5 * time.Second
	}
	return &Service{
		raft:         cfg.Raft,
		store:        cfg.Store,
		fsm:          cfg.FSM,
		manager:      cfg.Manager,
		factories:    cfg.Factories,
		log:          cfg.Logger,
		applyTimeout: cfg.ApplyTimeout,
	}, nil
}

// Reconcile brings the local Manager in line with the replicated
// storage_mounts bucket. Called once at boot + on every FSM change
// that touches the storage bucket. Safe to call concurrently: the
// Manager itself is thread-safe; we only need to ensure we compute
// the desired set under a single bbolt view.
func (s *Service) Reconcile(ctx context.Context) error {
	desired := make(map[string]*lobslawv1.StorageMount)
	err := s.store.ForEach(memory.BucketStorageMounts, func(_ string, raw []byte) error {
		var m lobslawv1.StorageMount
		if err := proto.Unmarshal(raw, &m); err != nil {
			return err
		}
		desired[m.Label] = &m
		return nil
	})
	if err != nil {
		return fmt.Errorf("storage: reconcile read: %w", err)
	}

	// Drop locally-registered mounts the cluster no longer wants.
	for _, info := range s.manager.List() {
		if _, keep := desired[info.Label]; !keep {
			if err := s.manager.Unregister(ctx, info.Label); err != nil {
				s.log.Warn("storage: reconcile unregister", "label", info.Label, "err", err)
			}
		}
	}

	// Add/replace mounts. Register is atomic-replace so config
	// updates don't orphan an old backing.
	for label, cfg := range desired {
		factory, ok := s.factories[cfg.Type]
		if !ok {
			s.log.Warn("storage: no factory for backend type; skipping",
				"label", label, "type", cfg.Type)
			continue
		}
		mount, err := factory(cfg)
		if err != nil {
			s.log.Warn("storage: factory rejected config",
				"label", label, "type", cfg.Type, "err", err)
			continue
		}
		if err := s.manager.Register(ctx, mount); err != nil {
			s.log.Warn("storage: register failed",
				"label", label, "type", cfg.Type, "err", err)
		}
	}
	return nil
}

// AddMount replicates a StorageMount via Raft PUT. Label + type
// required; backend-specific fields validated by the factory.
func (s *Service) AddMount(_ context.Context, req *lobslawv1.AddMountRequest) (*lobslawv1.AddMountResponse, error) {
	if req == nil || req.Mount == nil {
		return nil, status.Error(codes.InvalidArgument, "mount required")
	}
	m := req.Mount
	if m.Label == "" {
		return nil, status.Error(codes.InvalidArgument, "label required")
	}
	if m.Type == "" {
		return nil, status.Error(codes.InvalidArgument, "type required")
	}
	if _, ok := s.factories[m.Type]; !ok {
		return nil, status.Errorf(codes.InvalidArgument,
			"no backend registered for type %q", m.Type)
	}

	entry := &lobslawv1.LogEntry{
		Op:      lobslawv1.LogOp_LOG_OP_PUT,
		Id:      m.Label,
		Payload: &lobslawv1.LogEntry_StorageMount{StorageMount: m},
	}
	if err := s.apply(entry); err != nil {
		return nil, status.Errorf(codes.Internal, "apply: %v", err)
	}
	return &lobslawv1.AddMountResponse{}, nil
}

// RemoveMount deletes a mount from the replicated config. The
// FSM's DELETE apply triggers every node's reconcile loop (via the
// storage-mounts change hook) so every node unmounts locally.
func (s *Service) RemoveMount(_ context.Context, req *lobslawv1.RemoveMountRequest) (*lobslawv1.RemoveMountResponse, error) {
	if req == nil || req.Label == "" {
		return nil, status.Error(codes.InvalidArgument, "label required")
	}
	if _, err := s.store.Get(memory.BucketStorageMounts, req.Label); err != nil {
		return nil, status.Errorf(codes.NotFound, "mount %q not found", req.Label)
	}
	entry := &lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_DELETE,
		Id: req.Label,
		Payload: &lobslawv1.LogEntry_StorageMount{
			StorageMount: &lobslawv1.StorageMount{Label: req.Label},
		},
	}
	if err := s.apply(entry); err != nil {
		return nil, status.Errorf(codes.Internal, "apply: %v", err)
	}
	return &lobslawv1.RemoveMountResponse{}, nil
}

// ListMounts returns the current cluster-wide config. Reads the
// replicated bucket directly, so returns the authoritative view
// even on nodes where reconcile hasn't run yet.
func (s *Service) ListMounts(_ context.Context, _ *lobslawv1.ListMountsRequest) (*lobslawv1.ListMountsResponse, error) {
	out := &lobslawv1.ListMountsResponse{}
	err := s.store.ForEach(memory.BucketStorageMounts, func(_ string, raw []byte) error {
		var m lobslawv1.StorageMount
		if err := proto.Unmarshal(raw, &m); err != nil {
			return err
		}
		out.Mounts = append(out.Mounts, &m)
		return nil
	})
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list: %v", err)
	}
	return out, nil
}

// apply marshals + submits an entry to Raft. Translates an FSM
// error response into a Go error.
func (s *Service) apply(entry *lobslawv1.LogEntry) error {
	data, err := proto.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}
	res, err := s.raft.Apply(data, s.applyTimeout)
	if err != nil {
		return err
	}
	if ferr, ok := res.(error); ok && ferr != nil {
		return ferr
	}
	return nil
}
