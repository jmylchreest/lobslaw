package node

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/internal/scheduler"
	"github.com/jmylchreest/lobslaw/pkg/config"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/types"
)

// The generation poll handler had no tests at all.
//
// R22 asks that "a poll handler that has not run is not silently
// dropped", and that is exactly the kind of property that holds until
// the day it does not: every branch here decides between RETRY (the
// job is still out there), GIVE UP LOUDLY (say so and stop), and
// CLOSE (nothing can ever poll this again). Getting one wrong loses
// work that is already running and already being billed, or polls a
// dead handle until the heat death of the cluster.

// scriptedJobDriver returns a fixed state, or a fixed error.
type scriptedJobDriver struct {
	state compute.JobState
	err   error
	polls int
}

func (d *scriptedJobDriver) Submit(context.Context, compute.JobRequest) (compute.JobHandle, error) {
	return compute.JobHandle{Driver: "scripted", Raw: "job-1"}, nil
}

func (d *scriptedJobDriver) Poll(context.Context, compute.JobHandle) (compute.JobState, error) {
	d.polls++
	if d.err != nil {
		return compute.JobState{}, d.err
	}
	return d.state, nil
}

func (d *scriptedJobDriver) PollInterval() time.Duration { return 7 * time.Second }

func pollNode(t *testing.T, d compute.JobDriver) *Node {
	t.Helper()
	n := &Node{log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if d != nil {
		n.RegisterJobDriver("scripted", d)
	}
	return n
}

func pollCommitment(t *testing.T, deadline time.Time) *lobslawv1.AgentCommitment {
	t.Helper()
	raw, err := compute.JobHandle{Driver: "scripted", Raw: "job-1"}.Encode()
	if err != nil {
		t.Fatal(err)
	}
	params := map[string]string{paramJobHandle: raw, paramArtifactName: "clip"}
	if !deadline.IsZero() {
		params[paramJobDeadline] = deadline.Format(time.RFC3339)
	}
	return &lobslawv1.AgentCommitment{Id: "gen-1", Params: params}
}

// --- the job is still out there: RETRY ---------------------------------

// A running job must ask to be polled again, at the DRIVER's cadence.
// Returning nil would close the commitment and abandon work that is
// running and being billed.
func TestARunningJobAsksToBePolledAgain(t *testing.T) {
	t.Parallel()
	for _, status := range []compute.JobStatus{compute.JobPending, compute.JobRunning} {
		d := &scriptedJobDriver{state: compute.JobState{Status: status}}
		n := pollNode(t, d)

		err := n.runGenerationPoll(context.Background(), pollCommitment(t, time.Time{}))
		retry, ok := scheduler.AsRetryAfter(err)
		if !ok {
			t.Fatalf("%s: err = %v; the commitment would be closed on a job still running", status, err)
		}
		// The driver's own interval, not a global one: a single cadence
		// is either wasteful against one vendor or rate-limited by
		// another.
		if until := time.Until(retry.At); until > 8*time.Second || until < 5*time.Second {
			t.Errorf("%s: retry in %v, want the driver's 7s", status, until)
		}
	}
}

// A poll that FAILS is not a job that failed. A 503 from the status
// endpoint says nothing about the job.
func TestATransientPollFailureRetriesRatherThanDropping(t *testing.T) {
	t.Parallel()
	d := &scriptedJobDriver{err: compute.Transient(errors.New("503 from the status endpoint"))}
	n := pollNode(t, d)

	err := n.runGenerationPoll(context.Background(), pollCommitment(t, time.Time{}))
	if _, ok := scheduler.AsRetryAfter(err); !ok {
		t.Errorf("err = %v; a transient poll failure dropped a running job", err)
	}
}

// --- nothing can ever poll this again: CLOSE ---------------------------

// A permanent failure says this handle will never be pollable.
// Retrying it forever would poll a dead job until the deadline.
func TestAPermanentPollFailureClosesTheCommitment(t *testing.T) {
	t.Parallel()
	d := &scriptedJobDriver{err: compute.Permanent(errors.New("400 unknown operation"))}
	n := pollNode(t, d)

	err := n.runGenerationPoll(context.Background(), pollCommitment(t, time.Time{}))
	if err == nil {
		t.Fatal("a permanently broken poll was treated as success")
	}
	if _, ok := scheduler.AsRetryAfter(err); ok {
		t.Errorf("err = %v; a dead handle asked to be polled again", err)
	}
}

// A handle that will never decode cannot be polled by anyone. Leaving
// the commitment pending would re-poll it forever.
func TestAnUndecodableHandleIsNotRetried(t *testing.T) {
	t.Parallel()
	n := pollNode(t, &scriptedJobDriver{})
	c := &lobslawv1.AgentCommitment{Id: "gen-1", Params: map[string]string{
		paramJobHandle: "this is not a handle",
	}}
	err := n.runGenerationPoll(context.Background(), c)
	if err == nil {
		t.Fatal("an undecodable handle was accepted")
	}
	if _, ok := scheduler.AsRetryAfter(err); ok {
		t.Errorf("err = %v; a handle that can never decode asked to be polled again", err)
	}
}

// A commitment with no handle at all is the same case.
func TestACommitmentWithNoHandleIsRefused(t *testing.T) {
	t.Parallel()
	n := pollNode(t, &scriptedJobDriver{})
	err := n.runGenerationPoll(context.Background(),
		&lobslawv1.AgentCommitment{Id: "gen-1", Params: map[string]string{}})
	if err == nil {
		t.Fatal("a commitment with no job handle was accepted")
	}
}

// A handle minted by a driver this node does not have must say so.
// This is the restart case: a node that lost a driver from its config
// still holds commitments naming it.
func TestAMissingDriverIsNamedInTheError(t *testing.T) {
	t.Parallel()
	n := pollNode(t, nil)
	err := n.runGenerationPoll(context.Background(), pollCommitment(t, time.Time{}))
	if err == nil {
		t.Fatal("a commitment for an unregistered driver was accepted")
	}
	if !strings.Contains(err.Error(), "scripted") {
		t.Errorf("err = %q; it does not name the missing driver", err)
	}
}

// --- give up, but LOUDLY -----------------------------------------------

// A provider that loses a task would otherwise leave this being polled
// forever. Past the deadline it stops — and says so, because a job
// that silently stops being polled is exactly the "silently dropped"
// the acceptance criterion forbids.
func TestAJobPastItsDeadlineGivesUpAndSaysSo(t *testing.T) {
	t.Parallel()
	d := &scriptedJobDriver{state: compute.JobState{Status: compute.JobRunning}}
	n := pollNode(t, d)

	err := n.runGenerationPoll(context.Background(),
		pollCommitment(t, time.Now().Add(-time.Minute)))
	if err == nil {
		t.Fatal("an expired job was treated as finished")
	}
	if _, ok := scheduler.AsRetryAfter(err); ok {
		t.Errorf("err = %v; an expired job asked to be polled again", err)
	}
	// It must not have bothered the provider: the deadline is checked
	// before the poll, so a dead job costs nothing to abandon.
	if d.polls != 0 {
		t.Errorf("polled %d times past the deadline", d.polls)
	}
}

// A deadline that will not parse must not be read as "expired now".
// A malformed timestamp would otherwise abandon every job it touched.
func TestAnUnparseableDeadlineDoesNotAbandonTheJob(t *testing.T) {
	t.Parallel()
	d := &scriptedJobDriver{state: compute.JobState{Status: compute.JobRunning}}
	n := pollNode(t, d)
	c := pollCommitment(t, time.Time{})
	c.Params[paramJobDeadline] = "not a timestamp"

	err := n.runGenerationPoll(context.Background(), c)
	if _, ok := scheduler.AsRetryAfter(err); !ok {
		t.Errorf("err = %v; a malformed deadline abandoned a running job", err)
	}
}

// --- the job finished --------------------------------------------------

// A failed job closes the commitment: there is nothing left to poll.
// It is reported rather than swallowed, because the user asked for
// something and is owed an answer either way.
func TestAFailedJobClosesTheCommitment(t *testing.T) {
	t.Parallel()
	d := &scriptedJobDriver{state: compute.JobState{
		Status: compute.JobFailed, Err: "the model refused the prompt",
	}}
	n := pollNode(t, d)

	if err := n.runGenerationPoll(context.Background(), pollCommitment(t, time.Time{})); err != nil {
		t.Errorf("err = %v; a failed job should close the commitment, not retry it", err)
	}
}

// A success with no artifact is a driver bug, and closing the
// commitment quietly would lose the only evidence of it.
func TestASuccessWithNoArtifactIsAnError(t *testing.T) {
	t.Parallel()
	d := &scriptedJobDriver{state: compute.JobState{Status: compute.JobSucceeded}}
	n := pollNode(t, d)

	if err := n.runGenerationPoll(context.Background(), pollCommitment(t, time.Time{})); err == nil {
		t.Error("a job that succeeded with no artifact was accepted")
	}
}

// A status nobody defined must not be read as success or as "keep
// going" — both guess, and one of them delivers nothing while
// reporting delivery.
func TestAnUnknownStatusIsAnError(t *testing.T) {
	t.Parallel()
	d := &scriptedJobDriver{state: compute.JobState{Status: compute.JobStatus("sideways")}}
	n := pollNode(t, d)

	err := n.runGenerationPoll(context.Background(), pollCommitment(t, time.Time{}))
	if err == nil {
		t.Fatal("an unknown status was accepted")
	}
	if _, ok := scheduler.AsRetryAfter(err); ok {
		t.Errorf("err = %v; an unknown status asked to be polled again", err)
	}
}

// --- pricing at completion ---------------------------------------------

// The rate card is found by the label carried on the commitment,
// because by completion the turn is long over.
func TestThePriceIsFoundByTheCommitmentsProviderLabel(t *testing.T) {
	t.Parallel()
	n := pollNode(t, nil)
	n.cfg.Compute.Providers = []config.ProviderConfig{{
		Label: "video-vendor",
		Pricing: types.ProviderPricing{
			UnitUSD: map[string]float64{string(compute.UnitVideoSeconds): 0.5},
		},
	}}
	got := n.pricingForLabel("video-vendor")
	if got.UnitUSD[string(compute.UnitVideoSeconds)] != 0.5 {
		t.Errorf("pricing = %+v; the rate card was not found", got)
	}
}

// A commitment can outlive the config that created it. An operator who
// renames a provider mid-job should still get their video — unpriced,
// with the quantity intact — rather than a delivery failure over the
// accounting.
func TestAnUnknownLabelYieldsNoRatesRatherThanFailing(t *testing.T) {
	t.Parallel()
	n := pollNode(t, nil)
	if got := n.pricingForLabel("renamed-since"); got.IsSet() {
		t.Errorf("pricing = %+v, want empty", got)
	}
	if got := n.pricingForLabel(""); got.IsSet() {
		t.Errorf("pricing = %+v for no label, want empty", got)
	}
}

// Owner has to be the PRINCIPAL, because that is what ownedByCaller
// compares against. Stamping turn.UserID instead meant no caller ever
// matched their own generation work: an unauthenticated REST turn is
// UserID "anon" and principal "user:anon", so commitment_list answered
// "count: 0" while the scheduler was polling the job every 15s.
func TestGenerationOwnerIsThePrincipal(t *testing.T) {
	ctx := compute.WithTurnIdentity(context.Background(), compute.TurnIdentity{
		UserID:    "anon",
		Principal: identity.User("anon"),
		Channel:   "rest",
		ChannelID: "c1",
	})
	turn, _ := compute.TurnIdentityFrom(ctx)

	c, err := NewGenerationCommitment("gen-1", compute.JobHandle{Driver: "dashscope", Raw: "t"},
		0, turn.Principal.String(), turn.Channel, turn.ChannelID, "a cube", "qwen-video")
	if err != nil {
		t.Fatal(err)
	}
	if c.Owner != "user:anon" {
		t.Errorf("Owner = %q, want the principal form %q", c.Owner, "user:anon")
	}
	if c.Owner == turn.UserID {
		t.Error("Owner must not be the raw channel id — ownedByCaller compares against the principal")
	}
}

// A generation commitment with no DueAt is skipped by the scheduler on
// every scan, so the job runs at the provider, is billed, and is
// collected by nobody. That is what shipped: generate_video submitted
// successfully, returned a commitment id, and never delivered.
func TestGenerationCommitmentIsDue(t *testing.T) {
	h := compute.JobHandle{Driver: "dashscope", Raw: "task-1"}
	before := time.Now()
	c, err := NewGenerationCommitment("gen-1", h, 0, "alice", "rest", "chat-1", "a red circle", "qwen-video")
	if err != nil {
		t.Fatal(err)
	}
	if c.DueAt == nil {
		t.Fatal("DueAt is nil; the scheduler will never fire this commitment")
	}
	if got := c.DueAt.AsTime(); got.After(time.Now()) && got.Sub(before) > time.Second {
		t.Errorf("DueAt = %v, want due immediately for iv=0", got)
	}
}

// iv is the delay before the FIRST poll. A driver that wants to wait
// before asking must be able to say so.
func TestGenerationCommitmentHonoursInterval(t *testing.T) {
	h := compute.JobHandle{Driver: "dashscope", Raw: "task-2"}
	c, err := NewGenerationCommitment("gen-2", h, time.Minute, "alice", "rest", "chat-1", "x", "qwen-video")
	if err != nil {
		t.Fatal(err)
	}
	if c.DueAt == nil {
		t.Fatal("DueAt is nil")
	}
	if d := time.Until(c.DueAt.AsTime()); d < 30*time.Second {
		t.Errorf("DueAt is %v away, want ~1m", d)
	}
}
