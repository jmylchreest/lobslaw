package memory

import (
	"context"
	"fmt"
	"sort"

	"google.golang.org/protobuf/proto"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Browsing transcripts from either side of the wire.
//
// `lobslaw session list` and `session show` could only read a state.db
// on the same filesystem, so on an operator's laptop they printed
// "Total sessions: 0" — which reads as a quiet cluster rather than as
// the wrong store.
//
// The filtering and the message scan lived in cmd/lobslaw. They are
// here now so the CLI and SessionService answer with one definition of
// what "--channel telegram" selects.

// ListFiltered returns session records narrowed by channel and user,
// most recently updated first.
//
// Newest first because somebody scanning conversations is nearly
// always looking for a recent one, and the list has no other natural
// order — session ids are addresses, not sequence.
func (s *SessionService) ListFiltered(ctx context.Context, channel, userID string) (
	[]*lobslawv1.SessionRecord, error) {
	all, err := s.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]*lobslawv1.SessionRecord, 0, len(all))
	for _, r := range all {
		if channel != "" && r.GetChannel() != channel {
			continue
		}
		if userID != "" && r.GetUserId() != userID {
			continue
		}
		out = append(out, r)
	}
	sort.SliceStable(out, func(i, j int) bool {
		return LaterThan(out[i].GetUpdatedAt(), out[j].GetUpdatedAt())
	})
	return out, nil
}

// LoadMessages reads one conversation's transcript in sequence order.
//
// Message keys are "<session id>:<zero-padded seq>", so the thread is
// an ordered prefix scan rather than a decrypt of every message in the
// cluster — which is what makes reading one conversation cheap on a
// store holding thousands.
func (s *SessionService) LoadMessages(_ context.Context, id string) ([]*lobslawv1.SessionMessage, error) {
	if s.store == nil {
		return nil, fmt.Errorf("session: store not wired")
	}
	var out []*lobslawv1.SessionMessage
	err := s.store.ForEachPrefix(BucketSessionMessages, id+":", func(key string, raw []byte) error {
		var m lobslawv1.SessionMessage
		if uerr := proto.Unmarshal(raw, &m); uerr != nil {
			return fmt.Errorf("unmarshal message %q: %w", key, uerr)
		}
		out = append(out, &m)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}
