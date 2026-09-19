package workforce

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/memory"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

const schemaVersion uint32 = 1
const applyTimeout time.Duration = 5 * time.Second

type RaftRepository struct {
	Raft  *memory.RaftNode
	Store *memory.Store
}

func decode(raw []byte) (*State, error) {
	var rec lobslawv1.WorkforceRecord
	if e := proto.Unmarshal(raw, &rec); e != nil {
		return nil, e
	}
	if rec.SchemaVersion != schemaVersion {
		return nil, fmt.Errorf("unsupported workforce schema %d", rec.SchemaVersion)
	}
	var st State
	if e := json.Unmarshal(rec.StateJson, &st); e != nil {
		return nil, e
	}
	if st.Project.ID != rec.Id || st.Project.Owner != rec.Owner {
		return nil, ErrInvalid
	}
	st.Revision = rec.Revision
	return &st, nil
}
func (r *RaftRepository) Get(_ context.Context, id string) (*State, error) {
	if r.Store == nil {
		return nil, ErrUnavailable
	}
	raw, e := r.Store.Get(memory.BucketWorkforce, id)
	if errors.Is(e, types.ErrNotFound) {
		return nil, ErrNotFound
	}
	if e != nil {
		return nil, e
	}
	return decode(raw)
}
func (r *RaftRepository) List(context.Context) ([]*State, error) {
	if r.Store == nil {
		return nil, ErrUnavailable
	}
	out := []*State{}
	e := r.Store.ForEach(memory.BucketWorkforce, func(_ string, raw []byte) error {
		st, e := decode(raw)
		if e != nil {
			return e
		}
		out = append(out, st)
		return nil
	})
	return out, e
}
func (r *RaftRepository) Put(ctx context.Context, st *State, revision uint64) error {
	if r.Raft == nil {
		return ErrUnavailable
	}
	raw, e := json.Marshal(st)
	if e != nil {
		return e
	}
	if len(raw) > MaxStateBytes {
		return ErrInvalid
	}
	data, e := proto.Marshal(&lobslawv1.LogEntry{Op: lobslawv1.LogOp_LOG_OP_CLAIM, Id: st.Project.ID, ExpectedRevision: &revision, Payload: &lobslawv1.LogEntry_Workforce{Workforce: &lobslawv1.WorkforceRecord{Id: st.Project.ID, Owner: st.Project.Owner, StateJson: raw, SchemaVersion: schemaVersion}}})
	if e != nil {
		return e
	}
	res, e := r.Raft.ApplyOrForward(ctx, data, applyTimeout)
	if e != nil {
		return e
	}
	if e, ok := res.(error); ok {
		return e
	}
	st.Revision = revision + 1
	return nil
}
