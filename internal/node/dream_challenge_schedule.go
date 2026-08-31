package node

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/jmylchreest/lobslaw/internal/ids"
	"github.com/jmylchreest/lobslaw/internal/memory"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Getting a challenge in front of somebody who does not start a
// conversation.
//
// The notice path covers the user who says something: the question
// rides out on the reply. It covers nobody else. A contradiction
// found on Friday night, for a user who says nothing all weekend,
// waits — and dream keeps re-raising a question that never reaches
// anyone.
//
// So after each pass, anybody with a live contradiction gets a
// message scheduled. Not a new delivery mechanism: an ordinary
// commitment, the same thing "remind me on Tuesday" produces, which
// already knows how to reach a user through the channels they are
// subscribed to.

const (
	// challengeHour is when a scheduled challenge fires, in the
	// user's own timezone. Morning, because being asked to adjudicate
	// your own memory at 03:00 is not improved by the asking being
	// scheduled.
	challengeHour = 9

	// challengeQuietWindow is how far ahead an existing commitment
	// counts as "they will hear from us anyway".
	//
	// A day. Anything already due inside it will start a conversation
	// the question can ride on, and a second message would be the
	// agent contacting somebody twice about things it could have said
	// once.
	challengeQuietWindow = 24 * time.Hour
)

// scheduleDreamChallenges makes sure everyone with a live
// contradiction will be asked about it.
//
// Runs after a dream pass, on the node that ran it. Errors are
// reported but never fail the pass: a message that could not be
// scheduled is a question asked later, and the pass itself did real
// work worth keeping.
func (n *Node) scheduleDreamChallenges(ctx context.Context) error {
	if n.store == nil || n.raft == nil {
		return nil
	}
	owners, err := memory.ChallengeOwners(n.store)
	if err != nil {
		return fmt.Errorf("challenge owners: %w", err)
	}
	now := time.Now()
	for _, owner := range owners {
		pending, err := memory.HasPendingCommitment(n.store, owner, now.Add(challengeQuietWindow))
		if err != nil {
			return fmt.Errorf("pending commitments for %s: %w", owner, err)
		}
		if pending {
			// Either something else is already scheduled, or this is
			// the challenge message from a previous pass that has not
			// fired yet. Both mean the same thing: they are going to
			// hear from us, and once is enough.
			n.log.Debug("dream: challenge rides on a message already scheduled", "owner", owner)
			continue
		}
		if err := n.scheduleChallengeFor(ctx, owner, now); err != nil {
			n.log.Warn("dream: could not schedule a challenge", "owner", owner, "err", err)
		}
	}
	return nil
}

func (n *Node) scheduleChallengeFor(_ context.Context, owner string, now time.Time) error {
	challenges, err := memory.UnresolvedChallenges(n.store, owner, 1)
	if err != nil || len(challenges) == 0 {
		return err
	}
	question := challenges[0].Question
	if question == "" {
		return nil
	}

	userID := strings.TrimPrefix(owner, "user:")
	due := nextChallengeTime(now, n.resolveUserTimezone(userID))

	// The question is carried in the prompt rather than looked up at
	// fire time, so the message stands on its own. The cost is a
	// question that may have been settled in the meantime, which is a
	// redundant sentence; the alternative is a scheduled message whose
	// content depends on a lookup that can come back empty, and an
	// agent woken to say nothing.
	prompt := fmt.Sprintf(
		"Two memories about %s disagree, and dream could not tell which is right: %s\n\n"+
			"Ask them, in your own words and briefly. The firing turn has no chat to reply into — "+
			"call notify(text=\"...\") to reach them. If they answer, settle it with memory_correct "+
			"or memory_forget; if they do not, leave it alone and it will come up again.",
		userID, question)

	id := ids.New()
	c := &lobslawv1.AgentCommitment{
		Id:         id,
		DueAt:      timestamppb.New(due),
		Trigger:    "time",
		Reason:     "a contradiction in memory needs settling",
		Status:     "pending",
		HandlerRef: AgentTurnHandlerRef,
		Params:     map[string]string{"prompt": prompt, "user_id": userID},
		CreatedFor: userID,
		Owner:      owner,
	}
	entry := &lobslawv1.LogEntry{
		Op:      lobslawv1.LogOp_LOG_OP_PUT,
		Id:      id,
		Payload: &lobslawv1.LogEntry_Commitment{Commitment: c},
	}
	data, err := proto.Marshal(entry)
	if err != nil {
		return fmt.Errorf("marshal commitment: %w", err)
	}
	if _, err := n.raft.Apply(data, 5*time.Second); err != nil {
		return fmt.Errorf("raft apply: %w", err)
	}
	n.log.Info("dream: challenge scheduled", "owner", owner, "due", due)
	return nil
}

// nextChallengeTime is the next challengeHour in the user's zone.
//
// Never "in six hours": a fixed offset from a 02:00 dream lands at
// 08:00 for one user and in the middle of the night for another, and
// the whole point is that the hour is a fact about the person.
func nextChallengeTime(now time.Time, tz string) time.Time {
	loc := time.UTC
	if l, err := time.LoadLocation(tz); err == nil {
		loc = l
	}
	local := now.In(loc)
	due := time.Date(local.Year(), local.Month(), local.Day(), challengeHour, 0, 0, 0, loc)
	if !due.After(local) {
		due = due.AddDate(0, 0, 1)
	}
	return due
}
