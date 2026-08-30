package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"

	"google.golang.org/protobuf/proto"

	"github.com/jmylchreest/lobslaw/internal/memory"
	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
	"github.com/jmylchreest/lobslaw/pkg/textutil"
)

// sessionUsage carries the stopped-node constraint for the group, the
// same way memoryUsage does — both groups open state.db directly.
const sessionUsage = `lobslaw session — read stored conversation transcripts

All three talk to a RUNNING node over mTLS by default — use --context,
or --addr with the credential flags. Pass --offline to open state.db
directly instead; that path needs the node STOPPED, because bbolt takes
an exclusive lock, and it exists for reading a cluster that will not
start.

subcommands:
  list             one line per conversation, plus its running summary
  show <id>        the full transcript of one conversation
  search <text>    substring search across every transcript

Read-only: forgetting a conversation is a replicated operation and goes
through the running node's own path, not through here.`

// sessionForms pairs each subcommand's live and offline implementation.
//
// A table rather than a switch so the ROUTING is a value a test can
// assert. The bug worth catching is not a missing function — it is
// `list` quietly reading a laptop-local state.db and printing
// "Total sessions: 0" about a busy cluster.
var sessionForms = map[string]struct{ live, offline func([]string) error }{
	"list":   {live: sessionListLive, offline: sessionList},
	"show":   {live: sessionShowLive, offline: sessionShow},
	"search": {live: sessionSearchLive, offline: sessionSearch},
}

// sessionRoute returns the implementation for a subcommand, or nil if
// there is none. Live is the default; --offline is the opt-out.
func sessionRoute(sub string, offline bool) func([]string) error {
	form, ok := sessionForms[sub]
	if !ok {
		return nil
	}
	if offline {
		return form.offline
	}
	return form.live
}

// dispatchSession handles `lobslaw session <subcmd>`.
func dispatchSession(args []string) bool {
	idx := findSubcmd(args, "session")
	if idx < 0 {
		return false
	}
	sub := args[idx+1:]
	if len(sub) == 0 {
		fmt.Fprintln(os.Stderr, sessionUsage)
		os.Exit(2)
	}

	rest, offline := takeOffline(sub[1:])
	run := sessionRoute(sub[0], offline)
	if run == nil {
		fmt.Fprintf(os.Stderr, "lobslaw session: unknown subcommand %q\n\n", sub[0])
		fmt.Fprintln(os.Stderr, sessionUsage)
		os.Exit(2)
	}
	runOffline("session "+sub[0], run, rest)
	return true
}

func sessionList(args []string) error {
	fs := flag.NewFlagSet("session list", flag.ExitOnError)
	var opts offlineStore
	opts.bind(fs)
	channel := fs.String("channel", "", "only conversations on this channel kind (telegram, rest, ...)")
	user := fs.String("user", "", "only conversations opened by this canonical user id")
	asJSON := fs.Bool("json", false, "emit JSON instead of text")
	if err := fs.Parse(args); err != nil {
		return err
	}

	store, path, err := opts.open()
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	records, err := listSessions(store, *channel, *user)
	if err != nil {
		return err
	}
	return renderSessionList(os.Stdout, records, path, *asJSON)
}

func sessionShow(args []string) error {
	fs := flag.NewFlagSet("session show", flag.ExitOnError)
	var opts offlineStore
	opts.bind(fs)
	trunc := fs.Int("truncate", 0, "cap each message at N characters (0 = full text)")
	asJSON := fs.Bool("json", false, "emit JSON instead of text")
	positional, err := parseFlagsAndPositionals(fs, args)
	if err != nil {
		return err
	}
	if len(positional) != 1 {
		return errors.New("exactly one session id required (as printed by `lobslaw session list`)")
	}
	id := positional[0]

	store, path, err := opts.open()
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	raw, err := store.Get(memory.BucketSessions, id)
	if err != nil {
		if memory.IsNotFound(err) {
			return fmt.Errorf("no session with id %q — `lobslaw session list` prints the valid ids", id)
		}
		return err
	}
	var rec lobslawv1.SessionRecord
	if err := proto.Unmarshal(raw, &rec); err != nil {
		return fmt.Errorf("unmarshal session %q: %w", id, err)
	}

	msgs, err := loadMessages(store, id)
	if err != nil {
		return err
	}

	return renderTranscript(os.Stdout, &rec, msgs, path, *trunc, *asJSON)
}

func sessionSearch(args []string) error {
	fs := flag.NewFlagSet("session search", flag.ExitOnError)
	var opts offlineStore
	opts.bind(fs)
	channel := fs.String("channel", "", "restrict to one channel kind")
	user := fs.String("user", "", "restrict to conversations opened by this user id")
	limit := fs.Int("limit", 0, "cap conversations returned (0 = service default)")
	snippets := fs.Int("snippets", 0, "cap matching messages shown per conversation (0 = service default)")
	asJSON := fs.Bool("json", false, "emit JSON instead of text")
	words, err := parseFlagsAndPositionals(fs, args)
	if err != nil {
		return err
	}
	if len(words) == 0 {
		return errors.New("search text required")
	}
	// Parsed rather than joined from fs.Args(): a flag written after
	// the search text became PART OF THE QUERY, so `session search
	// lobster --context prod` searched for "lobster --context prod"
	// and found nothing, on a cluster it never contacted.
	query := strings.Join(words, " ")

	store, path, err := opts.open()
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()

	// Same code path the agent's session_search tool drives, so what
	// an operator sees here is what the model would have found. A nil
	// raft is fine: search only reads.
	svc := memory.NewSessionService(nil, store, memory.SessionConfig{})
	hits, err := svc.SearchTranscripts(context.Background(), memory.SessionSearchQuery{
		Text:               query,
		Channel:            *channel,
		UserID:             *user,
		Limit:              *limit,
		SnippetsPerSession: *snippets,
	})
	if err != nil {
		return err
	}

	// Converted to the wire shape so the two forms render through one
	// function. A second renderer would drift, and the operator would
	// be the one noticing that live and offline disagree about what a
	// match looks like.
	out := make([]*lobslawv1.SessionSearchHitProto, 0, len(hits))
	for _, h := range hits {
		sn := make([]*lobslawv1.SessionSnippetProto, 0, len(h.Snippets))
		for _, s := range h.Snippets {
			sn = append(sn, &lobslawv1.SessionSnippetProto{Seq: s.Seq, Role: s.Role, Text: s.Text})
		}
		out = append(out, &lobslawv1.SessionSearchHitProto{
			Session: h.Session,
			//nolint:gosec // a match count is bounded by the transcript
			Matches:  int32(h.Matches),
			Snippets: sn,
		})
	}
	return renderSessionHits(os.Stdout, out, query, path, *asJSON)
}

// listSessions reads the session index, applying the CLI filters and
// sorting most-recently-updated first.
//
// The filtering lives in internal/memory so this and SessionService
// answer with one definition of what "--channel telegram" selects.
func listSessions(store *memory.Store, channel, user string) ([]*lobslawv1.SessionRecord, error) {
	svc := memory.NewSessionService(nil, store, memory.SessionConfig{})
	return svc.ListFiltered(context.Background(), channel, user)
}

// loadMessages reads one conversation's transcript in sequence order.
func loadMessages(store *memory.Store, id string) ([]*lobslawv1.SessionMessage, error) {
	svc := memory.NewSessionService(nil, store, memory.SessionConfig{})
	return svc.LoadMessages(context.Background(), id)
}

func messageLine(m *lobslawv1.SessionMessage, trunc int) string {
	text := collapse(m.Content)
	if trunc > 0 {
		text = textutil.Truncate(text, "…", trunc)
	}
	line := fmt.Sprintf("  [%03d] %-9s %s", m.Seq, m.Role, text)
	for _, tc := range m.ToolCalls {
		line += " tool_call=" + tc.Name
	}
	if m.ToolCallId != "" {
		line += " tool_result_for=" + m.ToolCallId
	}
	return line
}

func messageFields(m *lobslawv1.SessionMessage, trunc int) map[string]any {
	content := m.Content
	if trunc > 0 {
		content = textutil.Truncate(content, "…", trunc)
	}
	calls := make([]map[string]any, 0, len(m.ToolCalls))
	for _, tc := range m.ToolCalls {
		calls = append(calls, map[string]any{"id": tc.Id, "name": tc.Name, "arguments": tc.Arguments})
	}
	return map[string]any{
		"seq":          m.Seq,
		"role":         m.Role,
		"content":      content,
		"tool_calls":   calls,
		"tool_call_id": m.ToolCallId,
		"turn_id":      m.TurnId,
		"timestamp":    tsString(m.Timestamp),
	}
}

func sessionFields(r *lobslawv1.SessionRecord) map[string]any {
	return map[string]any{
		"id":                  r.Id,
		"channel":             r.Channel,
		"channel_id":          r.ChannelId,
		"user_id":             r.UserId,
		"title":               r.Title,
		"first_seq":           r.FirstSeq,
		"next_seq":            r.NextSeq,
		"retained":            retained(r),
		"created_at":          tsString(r.CreatedAt),
		"updated_at":          tsString(r.UpdatedAt),
		"summary":             r.Summary,
		"summary_through_seq": r.SummaryThroughSeq,
		"summary_updated_at":  tsString(r.SummaryUpdatedAt),
	}
}

// retained is the live message count. Trimming advances FirstSeq, so
// the difference is what is still on disk rather than what was ever
// written.
func retained(r *lobslawv1.SessionRecord) uint64 {
	if r.NextSeq <= r.FirstSeq {
		return 0
	}
	return r.NextSeq - r.FirstSeq
}

func lastSeq(r *lobslawv1.SessionRecord) uint64 {
	if r.NextSeq == 0 {
		return 0
	}
	return r.NextSeq - 1
}
