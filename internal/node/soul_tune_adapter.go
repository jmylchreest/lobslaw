package node

import (
	"context"
	"errors"
	"time"

	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/soul"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// raftSoulTuneStore adapts memory.SoulTuneService to the
// soul.TuneStore interface. Lives in the node package so the soul
// package stays free of proto/raft imports.
type raftSoulTuneStore struct {
	svc *memory.SoulTuneService
}

func newRaftSoulTuneStore(svc *memory.SoulTuneService) *raftSoulTuneStore {
	return &raftSoulTuneStore{svc: svc}
}

func (s *raftSoulTuneStore) Get(ctx context.Context) (*soul.TuneState, error) {
	rec, err := s.svc.Get(ctx)
	if err != nil {
		return nil, err
	}
	if rec == nil || rec.Current == nil {
		return nil, nil
	}
	return tuneStateFromProto(rec.Current), nil
}

func (s *raftSoulTuneStore) Put(ctx context.Context, state *soul.TuneState) error {
	if state == nil {
		return errors.New("soul tune: state nil")
	}
	return s.svc.Put(ctx, tuneStateToProto(state))
}

func (s *raftSoulTuneStore) Rollback(ctx context.Context, steps int) (*soul.TuneState, error) {
	picked, err := s.svc.Rollback(ctx, steps)
	if err != nil {
		return nil, err
	}
	return tuneStateFromProto(picked), nil
}

func tuneStateFromProto(p *lobslawv1.SoulTuneState) *soul.TuneState {
	if p == nil {
		return nil
	}
	out := &soul.TuneState{UpdatedBy: p.GetUpdatedBy()}
	if p.UpdatedAt != nil {
		out.UpdatedAt = p.UpdatedAt.AsTime()
	}
	if p.Name != nil {
		v := *p.Name
		out.Name = &v
	}
	if e := p.EmotiveStyle; e != nil {
		if e.Excitement != nil {
			v := int(*e.Excitement)
			out.Excitement = &v
		}
		if e.Formality != nil {
			v := int(*e.Formality)
			out.Formality = &v
		}
		if e.Directness != nil {
			v := int(*e.Directness)
			out.Directness = &v
		}
		if e.Sarcasm != nil {
			v := int(*e.Sarcasm)
			out.Sarcasm = &v
		}
		if e.Humor != nil {
			v := int(*e.Humor)
			out.Humor = &v
		}
		if e.EmojiUsage != nil {
			v := *e.EmojiUsage
			out.EmojiUsage = &v
		}
	}
	if p.GetFragmentsSet() {
		frags := append([]string(nil), p.GetFragments()...)
		out.Fragments = &frags
	}
	return out
}

func tuneStateToProto(s *soul.TuneState) *lobslawv1.SoulTuneState {
	if s == nil {
		return nil
	}
	out := &lobslawv1.SoulTuneState{UpdatedBy: s.UpdatedBy}
	if !s.UpdatedAt.IsZero() {
		out.UpdatedAt = timestamppb.New(s.UpdatedAt)
	} else {
		out.UpdatedAt = timestamppb.New(time.Now())
	}
	if s.Name != nil {
		v := *s.Name
		out.Name = &v
	}
	emotive := &lobslawv1.EmotiveStyleTune{}
	hasEmotive := false
	if s.Excitement != nil {
		v := int32(*s.Excitement)
		emotive.Excitement = &v
		hasEmotive = true
	}
	if s.Formality != nil {
		v := int32(*s.Formality)
		emotive.Formality = &v
		hasEmotive = true
	}
	if s.Directness != nil {
		v := int32(*s.Directness)
		emotive.Directness = &v
		hasEmotive = true
	}
	if s.Sarcasm != nil {
		v := int32(*s.Sarcasm)
		emotive.Sarcasm = &v
		hasEmotive = true
	}
	if s.Humor != nil {
		v := int32(*s.Humor)
		emotive.Humor = &v
		hasEmotive = true
	}
	if s.EmojiUsage != nil {
		v := *s.EmojiUsage
		emotive.EmojiUsage = &v
		hasEmotive = true
	}
	if hasEmotive {
		out.EmotiveStyle = emotive
	}
	if s.Fragments != nil {
		out.FragmentsSet = true
		out.Fragments = append([]string(nil), (*s.Fragments)...)
	}
	return out
}
