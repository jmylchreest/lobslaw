package memory

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// sessionApplyTimeout bounds raft.Apply for a session append. Larger
// than channel state's because a turn's payload is bigger (every tool
// result from the turn rides along), but still short enough that a
// wedged raft doesn't hold a user's reply hostage — the gateway
// degrades to its in-memory buffer when this fails.
const sessionApplyTimeout = 5 * time.Second

// Session retention defaults. The cap is in MESSAGES, not turns: one
// user→assistant exchange that calls four tools produces ~10 messages
// (user + 5 assistant + 4 tool results), so 200 covers roughly 20
// multi-tool exchanges of replayable context.
//
// This is a STORAGE bound, not a context-window bound. What actually
// goes to the LLM is whatever the caller asks for via LoadTail, and
// eventually what compaction leaves behind; the cap only stops a
// long-lived chat grinding through disk forever.
const (
	DefaultSessionMaxMessages = 200
	// sessionSeqWidth zero-pads sequence numbers in message keys so
	// bbolt's lexical key order matches numeric sequence order. 20
	// digits covers the full uint64 range, so ordering never breaks.
	sessionSeqWidth = 20
)

// ErrNotLeader is returned by session writes attempted on a follower.
// Callers that can degrade (the gateway's in-memory buffer) check for
// it and continue; callers that can't surface it.
var ErrNotLeader = errors.New("memory: not the raft leader")

// TranscriptMessage is the package-local shape of one conversation
// message. Deliberately duplicated from compute.Message rather than
// imported: internal/compute already depends on internal/memory (the
// context engine reads the store), so the reverse import would cycle.
// A thin adapter wires the two at the gateway boundary, the same way
// EpisodicTurn does.
type TranscriptMessage struct {
	Role       string
	Content    string
	ToolCalls  []TranscriptToolCall
	ToolCallID string
}

// TranscriptToolCall mirrors compute.ToolCall.
type TranscriptToolCall struct {
	ID        string
	Name      string
	Arguments string
}

// SessionRef identifies one conversation.
type SessionRef struct {
	Channel   string
	ChannelID string
	// UserID is recorded on the index record so a later "show me my
	// conversations" can filter without decoding transcripts. Only
	// read on session creation; an established session keeps the
	// user it was opened with.
	UserID string
}

// SessionService is the raft-backed durable transcript store.
//
// Reads are local (straight off the FSM's bbolt), so any node can
// replay a conversation. Writes go through raft and are leader-only,
// matching every other write path in this package — there is no
// forwarding layer, so a follower-hosted turn gets ErrNotLeader and
// the caller decides whether that's fatal.
type SessionService struct {
	raft        *RaftNode
	store       *Store
	maxMessages int

	// writeMu serialises the read-modify-write of a session's index
	// record. Append and PutSummary both load the record, mutate it
	// and propose it; interleaved, a compaction that lands between
	// another writer's load and apply is silently discarded and the
	// summarised messages get replayed again. Writes are leader-only
	// and roughly one per turn, so a single lock is cheap enough.
	writeMu sync.Mutex
}

// SessionConfig tunes the service. Zero values take the defaults.
type SessionConfig struct {
	// MaxMessages caps retained messages per session. Trimming drops
	// the oldest first. <= 0 takes DefaultSessionMaxMessages.
	MaxMessages int
}

// NewSessionService wires the service against an existing Raft +
// Store. A nil raft leaves reads working and writes failing, matching
// ChannelStateService's asymmetry.
func NewSessionService(raft *RaftNode, store *Store, cfg SessionConfig) *SessionService {
	maxMessages := cfg.MaxMessages
	if maxMessages <= 0 {
		maxMessages = DefaultSessionMaxMessages
	}
	return &SessionService{raft: raft, store: store, maxMessages: maxMessages}
}

// sessionID composes the bucket key. Neither component may contain
// ':' — the separator has to stay unambiguous because message keys
// nest another one underneath it. Same rule as channelStateKey.
func sessionID(channel, channelID string) (string, error) {
	if channel == "" {
		return "", errors.New("session: channel required")
	}
	if channelID == "" {
		return "", errors.New("session: channel_id required")
	}
	for _, s := range []string{channel, channelID} {
		if strings.ContainsRune(s, ':') {
			return "", fmt.Errorf("session: %q must not contain ':'", s)
		}
	}
	return channel + ":" + channelID, nil
}

// sessionMessagePrefix is the key range holding one session's
// transcript. The trailing ':' matters: without it session "rest:1"
// would also match "rest:10"'s messages.
func sessionMessagePrefix(id string) string {
	return id + ":"
}

// sessionMessageKey is prefix + zero-padded seq.
func sessionMessageKey(id string, seq uint64) string {
	return fmt.Sprintf("%s%0*d", sessionMessagePrefix(id), sessionSeqWidth, seq)
}

// Load returns the retained transcript in order, oldest first. A
// session that has never been written returns (nil, nil) — an absent
// conversation is not an error, it's a new one.
func (s *SessionService) Load(ctx context.Context, ref SessionRef) ([]TranscriptMessage, error) {
	return s.LoadTail(ctx, ref, 0)
}

// LoadTail returns at most the last n messages of the transcript,
// oldest first. n <= 0 means everything retained.
//
// Tail rather than head: when a caller can only afford part of a
// conversation, the recent part is the part that matters. Callers
// wanting a specific window can read the whole thing and slice.
func (s *SessionService) LoadTail(_ context.Context, ref SessionRef, n int) ([]TranscriptMessage, error) {
	if s.store == nil {
		return nil, errors.New("session: store not wired")
	}
	id, err := sessionID(ref.Channel, ref.ChannelID)
	if err != nil {
		return nil, err
	}
	var out []TranscriptMessage
	err = s.store.ForEachPrefix(BucketSessionMessages, sessionMessagePrefix(id),
		func(key string, raw []byte) error {
			var msg lobslawv1.SessionMessage
			if err := proto.Unmarshal(raw, &msg); err != nil {
				return fmt.Errorf("session: unmarshal %s: %w", key, err)
			}
			out = append(out, fromProtoMessage(&msg))
			return nil
		})
	if err != nil {
		return nil, err
	}
	if n > 0 && len(out) > n {
		out = out[len(out)-n:]
	}
	return out, nil
}

// Transcript is a conversation prepared for a turn: the running
// summary of everything already compacted, plus the messages that
// are still replayed verbatim.
type Transcript struct {
	// Summary covers messages up to SummaryThroughSeq. Empty when
	// the conversation has never needed compacting.
	Summary string
	// SummaryThroughSeq is the highest sequence the summary covers.
	SummaryThroughSeq uint64
	// Messages are the verbatim tail — everything after
	// SummaryThroughSeq, oldest first.
	Messages []TranscriptMessage
	// NextSeq is the sequence the next appended message will take.
	// Callers use it to decide what is eligible for compaction.
	NextSeq uint64
	// Title is the conversation's generated label, empty until one
	// has been produced.
	Title string
}

// LoadTranscript returns the summary plus the messages after it.
//
// Summarised messages are deliberately NOT replayed — the summary
// stands in for them. Replaying both would pay for the same content
// twice and invite the model to treat one as a correction of the
// other.
func (s *SessionService) LoadTranscript(_ context.Context, ref SessionRef) (*Transcript, error) {
	if s.store == nil {
		return nil, errors.New("session: store not wired")
	}
	id, err := sessionID(ref.Channel, ref.ChannelID)
	if err != nil {
		return nil, err
	}
	rec, err := s.loadRecord(id)
	if err != nil {
		return nil, err
	}
	out := &Transcript{}
	if rec != nil {
		out.Summary = rec.Summary
		out.SummaryThroughSeq = rec.SummaryThroughSeq
		out.NextSeq = rec.NextSeq
		out.Title = rec.Title
	}
	err = s.store.ForEachPrefix(BucketSessionMessages, sessionMessagePrefix(id),
		func(key string, raw []byte) error {
			var msg lobslawv1.SessionMessage
			if err := proto.Unmarshal(raw, &msg); err != nil {
				return fmt.Errorf("session: unmarshal %s: %w", key, err)
			}
			if msg.Seq <= out.SummaryThroughSeq {
				return nil
			}
			out.Messages = append(out.Messages, fromProtoMessage(&msg))
			return nil
		})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// LoadRange returns the messages in (afterSeq, throughSeq], oldest
// first. Used by compaction to read exactly the span that is aging
// out of verbatim replay.
func (s *SessionService) LoadRange(_ context.Context, ref SessionRef, afterSeq, throughSeq uint64) ([]TranscriptMessage, error) {
	if s.store == nil {
		return nil, errors.New("session: store not wired")
	}
	id, err := sessionID(ref.Channel, ref.ChannelID)
	if err != nil {
		return nil, err
	}
	var out []TranscriptMessage
	err = s.store.ForEachPrefix(BucketSessionMessages, sessionMessagePrefix(id),
		func(key string, raw []byte) error {
			var msg lobslawv1.SessionMessage
			if err := proto.Unmarshal(raw, &msg); err != nil {
				return fmt.Errorf("session: unmarshal %s: %w", key, err)
			}
			if msg.Seq <= afterSeq || msg.Seq > throughSeq {
				return nil
			}
			out = append(out, fromProtoMessage(&msg))
			return nil
		})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// PutSummary stores a compaction result: the new running summary and
// the sequence it covers through.
//
// throughSeq must not go backwards — a stale compaction landing after
// a newer one would resurrect messages the newer summary already
// folded in, and the transcript would replay them a second time.
func (s *SessionService) PutSummary(ctx context.Context, ref SessionRef, summary string, throughSeq uint64) error {
	if s.store == nil {
		return errors.New("session: store not wired")
	}
	id, err := sessionID(ref.Channel, ref.ChannelID)
	if err != nil {
		return err
	}
	if s.raft == nil {
		return fmt.Errorf("%w: raft not wired", ErrNotLeader)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rec, err := s.loadRecord(id)
	if err != nil {
		return err
	}
	if rec == nil {
		return fmt.Errorf("session: %s does not exist", id)
	}
	if throughSeq <= rec.SummaryThroughSeq {
		return nil
	}
	now := timestamppb.Now()
	rec.Summary = summary
	rec.SummaryThroughSeq = throughSeq
	rec.SummaryUpdatedAt = now
	rec.UpdatedAt = now

	entry := &lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_PUT,
		Id: id,
		Payload: &lobslawv1.LogEntry_SessionAppend{
			SessionAppend: &lobslawv1.SessionAppendRecord{Session: rec},
		},
	}
	data, err := proto.Marshal(entry)
	if err != nil {
		return fmt.Errorf("session: marshal: %w", err)
	}
	if _, err := s.raft.ApplyOrForward(ctx, data, sessionApplyTimeout); err != nil {
		return fmt.Errorf("session: raft apply: %w", err)
	}
	return nil
}

// Append records the messages a turn produced and returns the
// resulting index record.
//
// Trimming happens here, on the leader, and the evictions ride along
// inside the raft entry — see SessionAppendRecord's comment for why
// the FSM must not recompute them.
//
// System messages are dropped: promptgen rebuilds the system prompt
// every turn from live state, so a persisted copy would be stale the
// moment SOUL or the tool list changed.
func (s *SessionService) Append(ctx context.Context, ref SessionRef, turnID string, msgs []TranscriptMessage) (*lobslawv1.SessionRecord, error) {
	if s.store == nil {
		return nil, errors.New("session: store not wired")
	}
	id, err := sessionID(ref.Channel, ref.ChannelID)
	if err != nil {
		return nil, err
	}
	if s.raft == nil {
		return nil, fmt.Errorf("%w: raft not wired", ErrNotLeader)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	rec, err := s.loadRecord(id)
	if err != nil {
		return nil, err
	}
	now := timestamppb.Now()
	if rec == nil {
		rec = &lobslawv1.SessionRecord{
			Id:        id,
			Channel:   ref.Channel,
			ChannelId: ref.ChannelID,
			UserId:    ref.UserID,
			NextSeq:   1,
			FirstSeq:  1,
			CreatedAt: now,
		}
	}
	rec.UpdatedAt = now

	out := make([]*lobslawv1.SessionMessage, 0, len(msgs))
	for _, m := range msgs {
		if m.Role == "system" {
			continue
		}
		out = append(out, toProtoMessage(id, rec.NextSeq, turnID, now, m))
		rec.NextSeq++
	}
	if len(out) == 0 {
		return rec, nil
	}

	// Retained range is [FirstSeq, NextSeq). Advance FirstSeq until
	// it fits the cap, collecting the keys the FSM should drop.
	var evict []string
	for rec.NextSeq-rec.FirstSeq > uint64(s.maxMessages) {
		evict = append(evict, sessionMessageKey(id, rec.FirstSeq))
		rec.FirstSeq++
	}

	entry := &lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_PUT,
		Id: id,
		Payload: &lobslawv1.LogEntry_SessionAppend{
			SessionAppend: &lobslawv1.SessionAppendRecord{
				Session:   rec,
				Messages:  out,
				EvictKeys: evict,
			},
		},
	}
	data, err := proto.Marshal(entry)
	if err != nil {
		return nil, fmt.Errorf("session: marshal: %w", err)
	}
	if _, err := s.raft.ApplyOrForward(ctx, data, sessionApplyTimeout); err != nil {
		return nil, fmt.Errorf("session: raft apply: %w", err)
	}
	return rec, nil
}

// Forget drops a conversation and its whole transcript. Used by
// /reset and by the user asking the agent to forget a thread.
//
// Deliberately a hard delete, not a retention downgrade: a user
// saying "forget this conversation" means the bytes go away, and
// leaving them recoverable in a lower tier would betray that.
func (s *SessionService) Forget(ctx context.Context, ref SessionRef) error {
	id, err := sessionID(ref.Channel, ref.ChannelID)
	if err != nil {
		return err
	}
	if s.raft == nil {
		return fmt.Errorf("%w: raft not wired", ErrNotLeader)
	}
	entry := &lobslawv1.LogEntry{
		Op:      lobslawv1.LogOp_LOG_OP_DELETE,
		Id:      id,
		Payload: &lobslawv1.LogEntry_Session{Session: &lobslawv1.SessionRecord{Id: id}},
	}
	data, err := proto.Marshal(entry)
	if err != nil {
		return fmt.Errorf("session: marshal: %w", err)
	}
	if _, err := s.raft.ApplyOrForward(ctx, data, sessionApplyTimeout); err != nil {
		return fmt.Errorf("session: raft apply: %w", err)
	}
	return nil
}

// List returns every session index record, unordered. Callers that
// need "the user's recent conversations" filter and sort — the set is
// small (one per live chat) so this stays cheap.
func (s *SessionService) List(_ context.Context) ([]*lobslawv1.SessionRecord, error) {
	if s.store == nil {
		return nil, errors.New("session: store not wired")
	}
	var out []*lobslawv1.SessionRecord
	err := s.store.ForEach(BucketSessions, func(key string, raw []byte) error {
		var rec lobslawv1.SessionRecord
		if err := proto.Unmarshal(raw, &rec); err != nil {
			return fmt.Errorf("session: unmarshal %s: %w", key, err)
		}
		out = append(out, &rec)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Describe returns one session's index record — its owner, title and
// counters — without decoding the transcript. (nil, nil) for a
// conversation that has never been written; callers that need to know
// who owns an address treat that as "no such conversation", not as an
// error.
func (s *SessionService) Describe(_ context.Context, ref SessionRef) (*lobslawv1.SessionRecord, error) {
	if s.store == nil {
		return nil, errors.New("session: store not wired")
	}
	id, err := sessionID(ref.Channel, ref.ChannelID)
	if err != nil {
		return nil, err
	}
	return s.loadRecord(id)
}

// loadRecord fetches the index record, returning (nil, nil) when the
// session doesn't exist yet.
func (s *SessionService) loadRecord(id string) (*lobslawv1.SessionRecord, error) {
	raw, err := s.store.Get(BucketSessions, id)
	if err != nil {
		if IsNotFound(err) {
			return nil, nil
		}
		return nil, err
	}
	var rec lobslawv1.SessionRecord
	if err := proto.Unmarshal(raw, &rec); err != nil {
		return nil, fmt.Errorf("session: unmarshal %s: %w", id, err)
	}
	return &rec, nil
}

func toProtoMessage(id string, seq uint64, turnID string, now *timestamppb.Timestamp, m TranscriptMessage) *lobslawv1.SessionMessage {
	msg := &lobslawv1.SessionMessage{
		SessionId:  id,
		Seq:        seq,
		Role:       m.Role,
		Content:    m.Content,
		ToolCallId: m.ToolCallID,
		TurnId:     turnID,
		Timestamp:  now,
	}
	for _, tc := range m.ToolCalls {
		msg.ToolCalls = append(msg.ToolCalls, &lobslawv1.SessionToolCall{
			Id:        tc.ID,
			Name:      tc.Name,
			Arguments: tc.Arguments,
		})
	}
	return msg
}

func fromProtoMessage(msg *lobslawv1.SessionMessage) TranscriptMessage {
	out := TranscriptMessage{
		Role:       msg.Role,
		Content:    msg.Content,
		ToolCallID: msg.ToolCallId,
	}
	for _, tc := range msg.ToolCalls {
		out.ToolCalls = append(out.ToolCalls, TranscriptToolCall{
			ID:        tc.Id,
			Name:      tc.Name,
			Arguments: tc.Arguments,
		})
	}
	return out
}

// SessionSearchQuery filters a transcript search.
type SessionSearchQuery struct {
	// Text is matched case-insensitively against message content.
	// Required — this is search, not enumeration; List covers that.
	Text string
	// Channel, when set, restricts to one channel kind.
	Channel string
	// UserID, when set, restricts to sessions opened by that user.
	UserID string
	// Visible, when non-nil, gates which sessions are searched at
	// all. Broader than UserID: the agent's scoping rule also lets a
	// caller see the conversation they're currently in, whoever
	// opened it. Applied before Limit truncates, so a caller's own
	// results can't be displaced by hits they may not see.
	Visible func(*lobslawv1.SessionRecord) bool
	// Limit caps the number of SESSIONS returned (not messages).
	// <= 0 takes DefaultSessionSearchLimit.
	Limit int
	// SnippetsPerSession caps matching messages returned per session.
	// <= 0 takes DefaultSnippetsPerSession.
	SnippetsPerSession int
}

// SessionSearchHit is one matching conversation.
type SessionSearchHit struct {
	Session  *lobslawv1.SessionRecord
	Snippets []SessionSnippet
	// Matches is the total number of matching messages, which may
	// exceed len(Snippets).
	Matches int
}

// SessionSnippet locates one match inside a transcript.
type SessionSnippet struct {
	Seq  uint64
	Role string
	Text string
}

// Session search defaults.
const (
	DefaultSessionSearchLimit = 5
	DefaultSnippetsPerSession = 3
	// snippetContextBytes is how much of a matching message is
	// returned around the hit. Enough to judge relevance without
	// pulling a whole tool result into the agent's context.
	snippetContextBytes = 240
)

// SearchTranscripts finds conversations containing text.
//
// Substring, not semantic: episodic memory already embeds every turn
// and answers "what do I know about X" through memory_search. What
// that cannot do is find the exact words in the exact thread — quoting
// a command, an error string, a name — which is what this is for.
// Building a second embedding pipeline over the same content would
// duplicate cost for a worse version of a capability we already have.
func (s *SessionService) SearchTranscripts(_ context.Context, q SessionSearchQuery) ([]SessionSearchHit, error) {
	if s.store == nil {
		return nil, errors.New("session: store not wired")
	}
	needle := strings.ToLower(strings.TrimSpace(q.Text))
	if needle == "" {
		return nil, errors.New("session: search text required")
	}
	limit := q.Limit
	if limit <= 0 {
		limit = DefaultSessionSearchLimit
	}
	perSession := q.SnippetsPerSession
	if perSession <= 0 {
		perSession = DefaultSnippetsPerSession
	}

	records, err := s.List(context.Background())
	if err != nil {
		return nil, err
	}
	var hits []SessionSearchHit
	for _, rec := range records {
		if q.Channel != "" && rec.Channel != q.Channel {
			continue
		}
		if q.UserID != "" && rec.UserId != q.UserID {
			continue
		}
		if q.Visible != nil && !q.Visible(rec) {
			continue
		}
		hit := SessionSearchHit{Session: rec}
		err := s.store.ForEachPrefix(BucketSessionMessages, sessionMessagePrefix(rec.Id),
			func(key string, raw []byte) error {
				var msg lobslawv1.SessionMessage
				if err := proto.Unmarshal(raw, &msg); err != nil {
					return fmt.Errorf("session: unmarshal %s: %w", key, err)
				}
				idx := strings.Index(strings.ToLower(msg.Content), needle)
				if idx < 0 {
					return nil
				}
				hit.Matches++
				if len(hit.Snippets) < perSession {
					hit.Snippets = append(hit.Snippets, SessionSnippet{
						Seq:  msg.Seq,
						Role: msg.Role,
						Text: snippetAround(msg.Content, idx, len(needle)),
					})
				}
				return nil
			})
		if err != nil {
			return nil, err
		}
		if hit.Matches > 0 {
			hits = append(hits, hit)
		}
	}
	// Most recently updated first: when several threads mention the
	// same thing, the live one is nearly always the one meant.
	sort.Slice(hits, func(i, j int) bool {
		return hits[i].Session.UpdatedAt.AsTime().After(hits[j].Session.UpdatedAt.AsTime())
	})
	if len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

// snippetAround returns a window of text centred on a match, with
// ellipses where it was cut.
//
// idx is an offset into the LOWERCASED copy of content that the search
// matched against, so for text where lowercasing changes a character's
// encoded width it addresses a slightly different place in the
// original. The window is 240 bytes wide and only has to be roughly
// centred, so a few bytes of drift is invisible — but the offset is
// clamped rather than trusted, because "no lowercase mapping is ever
// wider than the character it replaces" is an assumption about the
// whole of Unicode, and being wrong about it here is a panic on a
// slice bound in the middle of a search.
//
// Boundaries are then aligned to whole characters: cutting a
// multi-byte character in half yields U+FFFD in a snippet that goes
// straight into the agent's context.
func snippetAround(content string, idx, matchLen int) string {
	if idx > len(content) {
		idx = len(content)
	}
	if idx < 0 {
		idx = 0
	}
	start := idx - snippetContextBytes/2
	if start < 0 {
		start = 0
	}
	end := idx + matchLen + snippetContextBytes/2
	if end > len(content) {
		end = len(content)
	}
	start = alignRuneStart(content, start)
	end = alignRuneStart(content, end)
	out := content[start:end]
	if start > 0 {
		out = "…" + out
	}
	if end < len(content) {
		out += "…"
	}
	return out
}

// alignRuneStart moves an offset back to the nearest character
// boundary. Used for both ends of a snippet: backing a start offset up
// keeps the character it landed inside, and backing an end offset up
// drops the partial character it would otherwise have cut.
func alignRuneStart(s string, i int) int {
	if i <= 0 {
		return 0
	}
	if i >= len(s) {
		return len(s)
	}
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return i
}

// PutTitle sets a conversation's human-readable label. Titles are
// advisory — nothing keys off them — so an empty or duplicate title
// is not an error.
func (s *SessionService) PutTitle(ctx context.Context, ref SessionRef, title string) error {
	if s.store == nil {
		return errors.New("session: store not wired")
	}
	id, err := sessionID(ref.Channel, ref.ChannelID)
	if err != nil {
		return err
	}
	if s.raft == nil {
		return fmt.Errorf("%w: raft not wired", ErrNotLeader)
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	rec, err := s.loadRecord(id)
	if err != nil {
		return err
	}
	if rec == nil {
		return fmt.Errorf("session: %s does not exist", id)
	}
	rec.Title = strings.TrimSpace(title)
	rec.UpdatedAt = timestamppb.Now()
	entry := &lobslawv1.LogEntry{
		Op: lobslawv1.LogOp_LOG_OP_PUT,
		Id: id,
		Payload: &lobslawv1.LogEntry_SessionAppend{
			SessionAppend: &lobslawv1.SessionAppendRecord{Session: rec},
		},
	}
	data, err := proto.Marshal(entry)
	if err != nil {
		return fmt.Errorf("session: marshal: %w", err)
	}
	if _, err := s.raft.ApplyOrForward(ctx, data, sessionApplyTimeout); err != nil {
		return fmt.Errorf("session: raft apply: %w", err)
	}
	return nil
}
