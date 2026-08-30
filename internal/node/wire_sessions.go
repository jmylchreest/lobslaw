package node

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	"github.com/jmylchreest/lobslaw/internal/compute"
	"github.com/jmylchreest/lobslaw/internal/gateway"
	"github.com/jmylchreest/lobslaw/internal/identity"
	"github.com/jmylchreest/lobslaw/internal/memory"
	"github.com/jmylchreest/lobslaw/internal/turn"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// sessionStoreAdapter adapts memory.SessionService to the
// gateway.SessionStore interface. The two speak the same shape but
// can't import each other without a package cycle, so the adapter
// sits here at the wiring layer — same pattern as
// episodicIngesterAdapter.
//
// It also translates memory.ErrNotLeader into
// gateway.ErrSessionUnavailable, which is what lets the gateway log a
// follower's failed write at debug rather than warn without importing
// the memory package to recognise it.
type sessionStoreAdapter struct {
	inner *memory.SessionService
	// grants lets Forget clear a conversation's approvals alongside its
	// transcript. Nil on a node with no replicated grant store, where
	// the process-local map dies with the process anyway.
	grants *memory.SessionGrantStore
	log    *slog.Logger
}

// newSessionStore returns a gateway.SessionStore backed by raft, or
// nil when this node has no local memory state to write to. A nil
// return leaves the channel on its in-memory buffer.
func (n *Node) newSessionStore() gateway.SessionStore {
	if n.raft == nil || n.store == nil {
		return nil
	}
	return &sessionStoreAdapter{
		inner: memory.NewSessionService(n.raft, n.store, memory.SessionConfig{
			MaxMessages: n.cfg.Gateway.SessionMaxMessages,
		}),
		grants: n.sessionGrants,
		log:    n.log,
	}
}

// newSessionCompactor builds the compaction hook. Returns nil when
// anything it needs is missing — no raft/store (nothing to compact),
// or no summariser role resolved (nothing to compact with). A nil
// compactor means long conversations lose their head to the context
// budget instead of being summarised, which is the pre-compaction
// behaviour rather than a failure.
func (n *Node) newSessionCompactor() gateway.SessionCompactor {
	if n.raft == nil || n.store == nil || n.roleMap == nil {
		return nil
	}
	provider := n.roleMap.For(compute.RoleSummariser)
	if provider == nil {
		return nil
	}
	var titler compute.Titler
	if te := n.cfg.Compute.Context.TitlesEnabled; te == nil || *te {
		// 0 takes the titler's own default. A title's length does not
		// vary by deployment, so it is a constant rather than a setting.
		titler = compute.NewLLMTitler(provider, "", 0)
	}
	svc := memory.NewSessionService(n.raft, n.store, memory.SessionConfig{
		MaxMessages: n.cfg.Gateway.SessionMaxMessages,
	})
	cfg := n.cfg.Compute.Context
	if cfg.CompactEnabled != nil && !*cfg.CompactEnabled {
		return nil
	}
	inner := compute.NewCompactor(
		&sessionSummaryAdapter{inner: svc},
		compute.NewLLMSummarizer(provider, "", compute.SummarizerConfig{
			MaxCompletionTokens: completionCapFor(derefInt(cfg.CompactMaxSummaryTokens)),
			ToolResultBytes:     derefInt(cfg.CompactToolResultBytes),
			ExtraInstructions:   cfg.CompactInstructions,
		}),
		compute.CompactorConfig{
			KeepMessages:     derefInt(cfg.CompactKeepMessages),
			TriggerTokens:    derefInt(cfg.CompactTriggerTokens),
			MaxSummaryTokens: derefInt(cfg.CompactMaxSummaryTokens),
			Titler:           titler,
			Logger:           n.log,
		})
	if inner == nil {
		return nil
	}
	return &compactorAdapter{inner: inner}
}

// registerSessionTools exposes session_search / session_list /
// session_read to the agent. They seed default-allow like other
// builtins: read-only, and scoped to the turn's own user by
// compute.SessionScope, which the agent loop attaches per turn. That
// scoping is what makes default-allow defensible on a shared node —
// "read-only over data the operator already stores" is only true when
// the operator is the only user, and Telegram's UserIDScopes exists
// precisely because they often aren't.
func (n *Node) registerSessionTools() error {
	if n.raft == nil || n.store == nil || n.builtinsRegistry == nil || n.toolRegistry == nil {
		return nil
	}
	svc := memory.NewSessionService(n.raft, n.store, memory.SessionConfig{
		MaxMessages: n.cfg.Gateway.SessionMaxMessages,
	})
	cfg := n.cfg.Compute.Context
	if err := compute.RegisterSessionBuiltins(n.builtinsRegistry, compute.SessionToolConfig{
		Browser:          &sessionBrowserAdapter{inner: svc, ids: n.identityResolver()},
		MaxSearchResults: derefInt(cfg.SessionSearchResults),
		MaxSnippets:      derefInt(cfg.SessionSearchSnippets),
		MaxReadMessages:  derefInt(cfg.SessionReadMessages),
	}); err != nil {
		return fmt.Errorf("register session builtins: %w", err)
	}
	for _, td := range compute.SessionToolDefs() {
		if err := n.toolRegistry.Register(td); err != nil {
			return fmt.Errorf("register session tool %q: %w", td.Name, err)
		}
	}
	n.log.Debug("compute: session_search/list/read registered")
	return nil
}

// conversationConfig maps the tunables a channel needs for replay
// depth and its degraded-mode cache.
func (n *Node) conversationConfig() gateway.ConversationConfig {
	return gateway.ConversationConfig{
		TailMessages:  tailMessagesFor(derefInt(n.cfg.Compute.Context.TailTokens)),
		CacheMessages: n.cfg.Gateway.SessionCacheMessages,
		CacheTTL:      n.cfg.Gateway.SessionCacheTTL,
	}
}

// derefInt reads an optional config int; nil means "take the default",
// which the compute-side constructor applies.
func derefInt(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

func (a *sessionStoreAdapter) LoadTranscript(ctx context.Context, ref gateway.SessionRef, n int) (gateway.Transcript, error) {
	t, err := a.inner.LoadTranscript(ctx, toMemoryRef(ref))
	if err != nil {
		return gateway.Transcript{}, err
	}
	msgs := t.Messages
	if n > 0 && len(msgs) > n {
		msgs = msgs[len(msgs)-n:]
	}
	return gateway.Transcript{
		Summary:  t.Summary,
		Messages: toComputeMessages(msgs),
	}, nil
}

// compactorAdapter bridges the gateway's compaction hook to the
// compute-side Compactor, which owns the summariser.
type compactorAdapter struct {
	inner *compute.Compactor
}

func (a *compactorAdapter) MaybeCompact(ctx context.Context, ref gateway.SessionRef) (bool, error) {
	ok, err := a.inner.MaybeCompact(ctx, turn.SessionKey{
		Channel:   ref.Channel,
		ChannelID: ref.ChannelID,
	})
	return ok, translateSessionErr(err)
}

// sessionSummaryAdapter exposes the session store to the compute-side
// compactor. compute can't take memory's types directly without the
// import cycle the rest of this file works around.
type sessionSummaryAdapter struct {
	inner *memory.SessionService
}

func toMemoryKey(k turn.SessionKey) memory.SessionRef {
	return memory.SessionRef{Channel: k.Channel, ChannelID: k.ChannelID}
}

func (a *sessionSummaryAdapter) Pending(ctx context.Context, k turn.SessionKey) (string, uint64, uint64, error) {
	t, err := a.inner.LoadTranscript(ctx, toMemoryKey(k))
	if err != nil {
		return "", 0, 0, err
	}
	return t.Summary, t.SummaryThroughSeq, t.NextSeq, nil
}

func (a *sessionSummaryAdapter) Range(ctx context.Context, k turn.SessionKey, after, through uint64) ([]compute.Message, error) {
	msgs, err := a.inner.LoadRange(ctx, toMemoryKey(k), after, through)
	if err != nil {
		return nil, err
	}
	return toComputeMessages(msgs), nil
}

func (a *sessionSummaryAdapter) PutSummary(ctx context.Context, k turn.SessionKey, summary string, through uint64) error {
	return a.inner.PutSummary(ctx, toMemoryKey(k), summary, through)
}

func (a *sessionSummaryAdapter) Title(ctx context.Context, k turn.SessionKey) (string, error) {
	t, err := a.inner.LoadTranscript(ctx, toMemoryKey(k))
	if err != nil {
		return "", err
	}
	return t.Title, nil
}

func (a *sessionSummaryAdapter) PutTitle(ctx context.Context, k turn.SessionKey, title string) error {
	return a.inner.PutTitle(ctx, toMemoryKey(k), title)
}

// sessionBrowserAdapter exposes the read side of the transcript store
// to the agent's session_search / session_list / session_read tools.
type sessionBrowserAdapter struct {
	inner *memory.SessionService
	// ids resolves the channel user id stored on a record to a
	// canonical principal, so visibility compares people rather than
	// per-channel handles. Nil resolves every id to itself.
	ids *identity.Resolver
}

func (a *sessionBrowserAdapter) Search(ctx context.Context, q compute.SessionBrowseQuery) ([]compute.SessionBrowseHit, error) {
	hits, err := a.inner.SearchTranscripts(ctx, memory.SessionSearchQuery{
		Text:               q.Text,
		Channel:            q.Channel,
		UserID:             q.UserID,
		Limit:              q.Limit,
		SnippetsPerSession: q.SnippetsPerSession,
		Visible:            toRecordPredicate(a.ids, q.Visible),
	})
	if err != nil {
		return nil, err
	}
	out := make([]compute.SessionBrowseHit, 0, len(hits))
	for _, h := range hits {
		hit := compute.SessionBrowseHit{Info: toBrowseInfo(a.ids, h.Session), Matches: h.Matches}
		for _, s := range h.Snippets {
			hit.Snippets = append(hit.Snippets, compute.SessionBrowseSnippet{
				Seq: s.Seq, Role: s.Role, Text: s.Text,
			})
		}
		out = append(out, hit)
	}
	return out, nil
}

func (a *sessionBrowserAdapter) Recent(ctx context.Context, limit int, visible compute.SessionVisibleFunc) ([]compute.SessionBrowseInfo, error) {
	recs, err := a.inner.List(ctx)
	if err != nil {
		return nil, err
	}
	sort.Slice(recs, func(i, j int) bool {
		return recs[i].UpdatedAt.AsTime().After(recs[j].UpdatedAt.AsTime())
	})
	out := make([]compute.SessionBrowseInfo, 0, len(recs))
	for _, r := range recs {
		info := toBrowseInfo(a.ids, r)
		// Filtered before the limit, not after: otherwise a busy
		// shared node fills the caller's whole window with other
		// people's threads and then discards them.
		if visible != nil && !visible(info) {
			continue
		}
		out = append(out, info)
		if limit > 0 && len(out) == limit {
			break
		}
	}
	return out, nil
}

func (a *sessionBrowserAdapter) Info(ctx context.Context, k turn.SessionKey) (compute.SessionBrowseInfo, bool, error) {
	rec, err := a.inner.Describe(ctx, toMemoryKey(k))
	if err != nil || rec == nil {
		return compute.SessionBrowseInfo{}, false, err
	}
	return toBrowseInfo(a.ids, rec), true, nil
}

// toRecordPredicate lifts the compute-side visibility rule onto the
// stored record so memory can apply it while it still has the full
// candidate set. The rule itself stays in compute — this only changes
// what it's handed.
func toRecordPredicate(res *identity.Resolver, visible compute.SessionVisibleFunc) func(*lobslawv1.SessionRecord) bool {
	if visible == nil {
		return nil
	}
	return func(r *lobslawv1.SessionRecord) bool { return visible(toBrowseInfo(res, r)) }
}

func (a *sessionBrowserAdapter) Read(ctx context.Context, k turn.SessionKey, fromSeq uint64, limit int) ([]compute.Message, error) {
	// LoadRange is exclusive of its lower bound, so reading "from
	// seq N" means asking for everything after N-1.
	after := uint64(0)
	if fromSeq > 0 {
		after = fromSeq - 1
	}
	msgs, err := a.inner.LoadRange(ctx, toMemoryKey(k), after, after+uint64(limit))
	if err != nil {
		return nil, err
	}
	return toComputeMessages(msgs), nil
}

func toBrowseInfo(res *identity.Resolver, r *lobslawv1.SessionRecord) compute.SessionBrowseInfo {
	var updated string
	if r.UpdatedAt != nil {
		updated = r.UpdatedAt.AsTime().Format("2006-01-02 15:04 UTC")
	}
	var count uint64
	if r.NextSeq > r.FirstSeq {
		count = r.NextSeq - r.FirstSeq
	}
	return compute.SessionBrowseInfo{
		Channel:   r.Channel,
		ChannelID: r.ChannelId,
		Title:     r.Title,
		UserID:    r.UserId,
		Owner:     res.Resolve(r.UserId).String(),
		Messages:  count,
		UpdatedAt: updated,
		Summary:   r.Summary,
	}
}

func toComputeMessages(in []memory.TranscriptMessage) []compute.Message {
	out := make([]compute.Message, 0, len(in))
	for _, m := range in {
		out = append(out, compute.Message{
			Role:       m.Role,
			Content:    m.Content,
			ToolCalls:  toComputeToolCalls(m.ToolCalls),
			ToolCallID: m.ToolCallID,
		})
	}
	return out
}

func (a *sessionStoreAdapter) Append(ctx context.Context, ref gateway.SessionRef, turnID string, msgs []compute.Message) error {
	out := make([]memory.TranscriptMessage, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, memory.TranscriptMessage{
			Role:       m.Role,
			Content:    m.Content,
			ToolCalls:  toTranscriptToolCalls(m.ToolCalls),
			ToolCallID: m.ToolCallID,
		})
	}
	_, err := a.inner.Append(ctx, toMemoryRef(ref), turnID, out)
	return translateSessionErr(err)
}

// Forget drops the transcript AND the conversation's approval grants.
//
// Both, because "forget this conversation" is a statement about what
// the deployment still holds, and a cleared conversation that keeps
// privileges the user believes they revoked is the worse half to get
// wrong. GATEWAY.md has described this behaviour since session grants
// landed; RevokeSession existed and was tested, and nothing called it.
//
// Grants go FIRST. If the transcript delete then fails the user is
// told the forget failed, having lost no privileges they still expect
// to hold — whereas deleting the transcript and failing to revoke
// leaves a conversation the user believes is gone still able to act.
func (a *sessionStoreAdapter) Forget(ctx context.Context, ref gateway.SessionRef) error {
	if a.grants != nil {
		// Same "<channel>:<channel_id>" key the transcript uses, which
		// is what lets one conversation's grants be found from its ref.
		n, err := a.grants.RevokeSession(ctx, ref.Channel+":"+ref.ChannelID)
		if err != nil {
			return translateSessionErr(err)
		}
		if n > 0 {
			a.log.Info("session: revoked conversation approvals on forget",
				"channel", ref.Channel, "channel_id", ref.ChannelID, "grants", n)
		}
	}
	return translateSessionErr(a.inner.Forget(ctx, toMemoryRef(ref)))
}

func toMemoryRef(ref gateway.SessionRef) memory.SessionRef {
	return memory.SessionRef{
		Channel:   ref.Channel,
		ChannelID: ref.ChannelID,
		UserID:    ref.UserID,
	}
}

func toComputeToolCalls(in []memory.TranscriptToolCall) []compute.ToolCall {
	if len(in) == 0 {
		return nil
	}
	out := make([]compute.ToolCall, 0, len(in))
	for _, tc := range in {
		out = append(out, compute.ToolCall{ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments})
	}
	return out
}

func toTranscriptToolCalls(in []compute.ToolCall) []memory.TranscriptToolCall {
	if len(in) == 0 {
		return nil
	}
	out := make([]memory.TranscriptToolCall, 0, len(in))
	for _, tc := range in {
		out = append(out, memory.TranscriptToolCall{ID: tc.ID, Name: tc.Name, Arguments: tc.Arguments})
	}
	return out
}

// translateSessionErr maps the leader-only write failure onto the
// gateway's degradable sentinel, preserving the original text so the
// operator still sees which node to retry against.
func translateSessionErr(err error) error {
	if err == nil {
		return nil
	}
	// ErrNotLeader survives for the paths that are still leader-only.
	// The forwarding ones fail differently now: ErrNoLeader during an
	// election, ErrForwardUnavailable when the leader is unreachable.
	// All three mean "this write cannot land right now" and the
	// gateway's answer is the same — degrade to the in-memory buffer
	// and log quietly rather than fail the user's turn.
	if errors.Is(err, memory.ErrNotLeader) ||
		errors.Is(err, memory.ErrNoLeader) ||
		errors.Is(err, memory.ErrForwardUnavailable) {
		return fmt.Errorf("%w: %s", gateway.ErrSessionUnavailable, err)
	}
	return err
}

// identityResolver builds the principal resolver from
// [identity.aliases], backed by the channel bindings an operator
// declared under [[user]].
//
// Rebuilt per call rather than cached: it is constructed at wiring
// time, the map is a handful of entries, and a cached copy would
// silently outlive a config reload.
//
// The bindings half is what lets an operator bind a channel's stable
// address — a Telegram numeric id rather than its renameable @handle —
// so identity survives a rename and does not transfer with the handle.
// Nil on a node without user prefs, which keeps the alias-map-only
// behaviour.
func (n *Node) identityResolver() *identity.Resolver {
	r := identity.NewResolver(n.cfg.Identity.Aliases)
	if n.userPrefsSvc != nil {
		r = r.WithBindings(n.userPrefsSvc)
	}
	return r
}

// sessionLeaserAdapter adapts memory.LeaseService to
// gateway.SessionLeaser. Same reason as sessionStoreAdapter: the two
// packages cannot import each other, so the translation sits here.
//
// It also maps memory.ErrLeaseHeld onto gateway.ErrLeaseUnavailable,
// which is what lets the gateway recognise "another node has this
// conversation" without importing the memory package.
type sessionLeaserAdapter struct {
	inner *memory.LeaseService
}

func (a sessionLeaserAdapter) AcquireLease(ctx context.Context, key, turnID string) (gateway.LeaseHandle, error) {
	lease, err := a.inner.Acquire(ctx, key, turnID)
	if err != nil {
		if errors.Is(err, memory.ErrLeaseHeld) {
			return nil, fmt.Errorf("%w: %s", gateway.ErrLeaseUnavailable, err)
		}
		return nil, err
	}
	// A nil *memory.SessionLease is the no-raft path. Returning it
	// straight through would produce a non-nil interface wrapping a
	// nil pointer — which works here only because every method on
	// SessionLease tolerates a nil receiver. Return an explicit nil
	// instead, so the gate's `handle != nil` checks mean what they say.
	if lease == nil {
		return nil, nil
	}
	return lease, nil
}

// newSessionLeaser returns the cluster half of turn serialisation, or
// nil when this node has no raft — a single-node gateway has no second
// process to race with, and refusing turns would be worse than the
// hazard it cannot have.
func (n *Node) newSessionLeaser() gateway.SessionLeaser {
	if n.raft == nil || n.store == nil {
		return nil
	}
	return sessionLeaserAdapter{inner: memory.NewLeaseService(n.raft, n.store, memory.LeaseConfig{
		NodeID: n.cfg.NodeID,
		TTL:    n.cfg.Gateway.HardTimeout,
		Logger: n.log,
	})}
}

// Two derived caps, where there used to be two more settings.
//
// A second cap on a thing another setting already caps can contradict
// it, and the tighter of the two wins SILENTLY: a tail_messages of 5
// beside a generous tail_tokens truncates history for a reason nobody
// can see in the config. Deriving them means an operator cannot set
// the pair to disagree — better than a validation error, which only
// tells them after they already have.

// tailMessagesFor bounds how many stored messages are read per turn.
//
// This was never a preference; it is an I/O guard, so that reading ten
// thousand messages to then discard all but forty is not paid for on
// every turn. What an operator actually means is tail_tokens, so the
// guard follows from it — generously, so the TOKEN cap is always the
// one that binds and the message cap only stops absurd reads.
func tailMessagesFor(tailTokens int) int {
	if tailTokens <= 0 {
		// Explicit 0 is "unbounded tokens". The read still needs a
		// ceiling or a long conversation loads entirely into memory
		// before anything trims it.
		return maxTailMessages
	}
	n := tailTokens / minTokensPerMessage

	if n < minTailMessages {
		return minTailMessages
	}

	if n > maxTailMessages {
		return maxTailMessages
	}
	return n
}

// completionCapFor bounds what the summariser may generate.
//
// Distinct from the summary cap only in being a safety margin on it:
// the output is truncated to the summary cap regardless, so this exists
// to stop a model that ignores its instructions generating megabytes.
// Double is ample for that and cannot be set to contradict the cap it
// protects.
func completionCapFor(maxSummaryTokens int) int {
	if maxSummaryTokens <= 0 {
		return 0 // take the summariser's own default
	}
	return maxSummaryTokens * 2
}

const (
	// minTokensPerMessage is the assumed floor for a stored message.
	// Deliberately small: it makes the derived message cap generous,
	// which is what keeps the token budget the binding constraint.
	// At the defaults (4000 tail tokens) it reproduces the 100 the
	// message cap used to default to.
	minTokensPerMessage = 40
	minTailMessages     = 20
	maxTailMessages     = 500
)
