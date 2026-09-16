package memory

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/jmylchreest/lobslaw/internal/archive"
	"github.com/jmylchreest/lobslaw/internal/ids"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

const ArchiveChunkBytes = 1 << 20

type ArchiveRPC struct {
	lobslawv1.UnimplementedArchiveServiceServer
	service   *Service
	authorize func(context.Context, string) error
	validate  func(context.Context, []archive.Record) error
}

// NewArchiveRPC requires a data authorizer. A machine certificate by itself
// does not grant access to the deployment's personal data.
func NewArchiveRPC(service *Service, authorize func(context.Context, string) error, validate func(context.Context, []archive.Record) error) *ArchiveRPC {
	return &ArchiveRPC{service: service, authorize: authorize, validate: validate}
}

func (s *ArchiveRPC) authorized(ctx context.Context, action string) error {
	if s.authorize == nil {
		return status.Error(codes.PermissionDenied, "archive data authority is not configured")
	}
	if err := s.authorize(ctx, action); err != nil {
		return err
	}
	if s.service == nil || s.service.store == nil || s.service.raft == nil {
		return status.Error(codes.FailedPrecondition, "archive service requires a memory node")
	}
	if !s.service.raft.IsLeader() {
		return status.Errorf(codes.FailedPrecondition, "retry archive operation at leader %s", s.service.raft.LeaderAddress())
	}
	return nil
}

func (s *ArchiveRPC) ExportArchive(_ *lobslawv1.ExportArchiveRequest, stream grpc.ServerStreamingServer[lobslawv1.ExportArchiveResponse]) error {
	if err := s.authorized(stream.Context(), "archive:export"); err != nil {
		return err
	}
	records, err := s.service.store.ArchiveRecords(stream.Context())
	if err != nil {
		return status.Error(codes.Internal, "cannot read complete archive snapshot")
	}
	snapshot := archive.Snapshot{
		Records: records,
		Manifest: archive.Manifest{
			SnapshotID: ids.New(), CreatedAt: time.Now().UTC(),
			Omissions: []string{"embeddings", "credentials and certificates", "policy grants", "Raft metadata", "filesystem attachments", "execution audit history"},
		},
	}
	var payload bytes.Buffer
	if err := archive.Write(&payload, snapshot); err != nil {
		return status.Error(codes.Internal, "cannot encode archive")
	}
	for payload.Len() > 0 {
		if err := stream.Send(&lobslawv1.ExportArchiveResponse{Data: payload.Next(ArchiveChunkBytes)}); err != nil {
			return err
		}
	}
	return nil
}

func (s *ArchiveRPC) ImportArchive(stream grpc.ClientStreamingServer[lobslawv1.ImportArchiveRequest, lobslawv1.ImportArchiveResponse]) error {
	ctx := stream.Context()
	if err := s.authorized(ctx, "archive:import"); err != nil {
		return err
	}
	header, err := stream.Recv()
	if err != nil {
		return err
	}
	if len(header.Data) != 0 || len(header.OptionsJson) > ArchiveChunkBytes {
		return status.Error(codes.InvalidArgument, "first chunk must contain options only")
	}
	var opts ArchiveImportOptions
	decoder := json.NewDecoder(bytes.NewReader(header.OptionsJson))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&opts); err != nil {
		return status.Error(codes.InvalidArgument, "invalid archive import options")
	}
	var payload bytes.Buffer
	for {
		chunk, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return err
		}
		if len(chunk.Data) > ArchiveChunkBytes || len(chunk.OptionsJson) > 0 || chunk.Apply || chunk.RequireEmpty {
			return status.Error(codes.InvalidArgument, "invalid archive data chunk")
		}
		if int64(payload.Len()+len(chunk.Data)) > archive.MaxArchiveBytes {
			return status.Error(codes.ResourceExhausted, "archive upload exceeds size limit")
		}
		payload.Write(chunk.Data)
	}
	snapshot, err := archive.Read(&payload)
	if err != nil {
		return status.Error(codes.InvalidArgument, "archive verification failed")
	}
	if err := bindArchiveSource(snapshot.Manifest, &opts, header.RequireEmpty); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	existing, err := s.service.store.ArchiveRecords(ctx)
	if err != nil {
		return status.Error(codes.Internal, "cannot read destination snapshot")
	}
	progress, pending, err := archiveImportProgress(s.service.store, snapshot.Records, opts)
	if err != nil {
		return status.Error(codes.Internal, "cannot read import progress")
	}
	if header.RequireEmpty {
		if err := archiveRestoreTarget(s.service.store, existing, snapshot.Records, opts); err != nil {
			return status.Error(codes.FailedPrecondition, err.Error())
		}
	}
	plan, err := PlanArchiveImport(existing, pending, opts)
	if err != nil {
		return status.Errorf(codes.InvalidArgument, "plan import: %v", err)
	}
	if s.validate != nil {
		if err := s.validate(ctx, append(existing, plan.Additions...)); err != nil {
			return status.Errorf(codes.InvalidArgument, "validate imported skills: %v", err)
		}
	}
	planJSON, err := json.Marshal(plan)
	if err != nil {
		return status.Error(codes.Internal, "cannot encode import plan")
	}
	progressJSON, err := json.Marshal(progress)
	if err != nil {
		return status.Error(codes.Internal, "cannot encode import progress")
	}
	response := &lobslawv1.ImportArchiveResponse{PlanJson: planJSON, ResultJson: progressJSON}
	if header.Apply {
		result, applyErr := ApplyArchiveImport(ctx, s.service.raft, s.service.store, snapshot.Records, opts, s.service.embedder)
		response.ResultJson, err = json.Marshal(result)
		if err != nil {
			return status.Error(codes.Internal, "cannot encode import progress")
		}
		if applyErr != nil {
			response.Error = applyErr.Error()
		}
	}
	return stream.SendAndClose(response)
}

func archiveRestoreTarget(store *Store, existing, incoming []archive.Record, opts ArchiveImportOptions) error {
	if len(existing) == 0 {
		return nil
	}
	id, err := archiveImportID(incoming, opts)
	if err != nil {
		return err
	}
	completed, err := archiveCompleted(store, id)
	if err != nil {
		return err
	}
	sources, err := archiveMappedSources(incoming, opts)
	if err != nil {
		return err
	}
	for _, record := range existing {
		source, ok := sources[archiveRecordKey{record.Kind, record.ID}]
		if !ok || !completed[archiveRecordKey{source.Kind, source.ID}] {
			return errors.New("backup restore requires an empty knowledge store or its own partial restore; use archive import to merge")
		}
	}
	return nil
}

func bindArchiveSource(manifest archive.Manifest, opts *ArchiveImportOptions, restore bool) error {
	source, err := archive.SourceIdentity(manifest, opts.SourceID)
	if err != nil {
		return err
	}
	opts.SourceID = source
	if restore {
		if len(opts.Alongside) > 0 || len(opts.Skip) > 0 || len(opts.Replace) > 0 || len(opts.ReplaceOriginal) > 0 {
			return errors.New("backup restore does not support conflict resolutions; use archive import")
		}
		// Restore existing provenance rather than adopting the backup as a new source.
		opts.SourceID = ""
	}
	return nil
}
