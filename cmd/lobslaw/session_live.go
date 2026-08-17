package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	lobslawv1 "github.com/jmylchreest/lobslaw/pkg/proto/lobslaw/v1"
)

// Reading transcripts from a laptop.
//
// `session list` printed "Total sessions: 0" on any machine that is
// not the node, because there was no state.db to open — and a count of
// zero reads as a quiet cluster rather than as the wrong store.
//
// The offline path stays, and stays read-only. Forgetting a
// conversation is a replicated mutation with its own path; browsing
// what was said does not need one that can also delete it.

func sessionClient(node *liveNode) (lobslawv1.SessionServiceClient, func(), error) {
	conn, err := node.dial()
	if err != nil {
		return nil, nil, err
	}
	return lobslawv1.NewSessionServiceClient(conn), func() { _ = conn.Close() }, nil
}

// --- list --------------------------------------------------------------

func sessionListLive(args []string) error {
	fs := flag.NewFlagSet("session list", flag.ExitOnError)
	var node liveNode
	node.bind(fs)
	channel := fs.String("channel", "", "only conversations on this channel kind (telegram, rest, ...)")
	user := fs.String("user", "", "only conversations opened by this canonical user id")
	asJSON := fs.Bool("json", false, "emit JSON instead of text")
	if err := fs.Parse(args); err != nil {
		return err
	}

	client, closeConn, err := sessionClient(&node)
	if err != nil {
		return err
	}
	defer closeConn()

	ctx, cancel := node.ctx()
	defer cancel()
	res, err := client.ListSessions(ctx, &lobslawv1.ListSessionsRequest{
		Channel: *channel, UserId: *user,
	})
	if err != nil {
		return err
	}
	return renderSessionList(os.Stdout, res.GetSessions(), node.addr, *asJSON)
}

// renderSessionList prints the index and SAYS WHERE IT CAME FROM. An
// empty list is indistinguishable from the wrong store otherwise, and
// "no conversations" is the answer somebody would believe.
func renderSessionList(w io.Writer, records []*lobslawv1.SessionRecord, source string, asJSON bool) error {
	if asJSON {
		out := make([]map[string]any, 0, len(records))
		for _, r := range records {
			out = append(out, sessionFields(r))
		}
		return emitJSON(map[string]any{"source": source, "sessions": out, "total": len(records)})
	}

	_, _ = fmt.Fprintln(w, source)
	for _, r := range records {
		_, _ = fmt.Fprintf(w, "\n  %s  channel=%s user=%s  retained=%d (seq %d..%d)  updated=%s\n",
			r.GetId(), orNone(r.GetChannel()), orNone(r.GetUserId()),
			retained(r), r.GetFirstSeq(), lastSeq(r), orNone(tsString(r.GetUpdatedAt())))
		if r.GetTitle() != "" {
			_, _ = fmt.Fprintf(w, "    title: %s\n", r.GetTitle())
		}
		if r.GetSummary() != "" {
			_, _ = fmt.Fprintf(w, "    summary (through seq %d): %s\n",
				r.GetSummaryThroughSeq(), collapse(r.GetSummary()))
		}
	}
	_, _ = fmt.Fprintf(w, "\nTotal sessions: %d\n", len(records))
	if len(records) > 0 {
		_, _ = fmt.Fprintln(w, "run `lobslaw session show <id>` for a full transcript")
	}
	return nil
}

// --- show --------------------------------------------------------------

func sessionShowLive(args []string) error {
	fs := flag.NewFlagSet("session show", flag.ExitOnError)
	var node liveNode
	node.bind(fs)
	trunc := fs.Int("truncate", 0, "cap each message at N characters (0 = full text)")
	asJSON := fs.Bool("json", false, "emit JSON instead of text")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("exactly one session id required (as printed by `lobslaw session list`)")
	}

	client, closeConn, err := sessionClient(&node)
	if err != nil {
		return err
	}
	defer closeConn()

	ctx, cancel := node.ctx()
	defer cancel()
	res, err := client.GetSession(ctx, &lobslawv1.GetSessionRequest{Id: fs.Arg(0)})
	if err != nil {
		return err
	}
	return renderTranscript(os.Stdout, res.GetSession(), res.GetMessages(), node.addr, *trunc, *asJSON)
}

func renderTranscript(w io.Writer, rec *lobslawv1.SessionRecord,
	msgs []*lobslawv1.SessionMessage, source string, trunc int, asJSON bool) error {
	if asJSON {
		out := make([]map[string]any, 0, len(msgs))
		for _, m := range msgs {
			out = append(out, messageFields(m, trunc))
		}
		return emitJSON(map[string]any{
			"source": source, "session": sessionFields(rec), "messages": out,
		})
	}

	_, _ = fmt.Fprintln(w, source)
	_, _ = fmt.Fprintf(w, "%s  channel=%s user=%s  retained=%d (seq %d..%d)\n",
		rec.GetId(), orNone(rec.GetChannel()), orNone(rec.GetUserId()),
		retained(rec), rec.GetFirstSeq(), lastSeq(rec))
	_, _ = fmt.Fprintf(w, "  title:   %s\n", orNone(rec.GetTitle()))
	_, _ = fmt.Fprintf(w, "  created: %s\n", orNone(tsString(rec.GetCreatedAt())))
	_, _ = fmt.Fprintf(w, "  updated: %s\n", orNone(tsString(rec.GetUpdatedAt())))
	if rec.GetSummary() != "" {
		_, _ = fmt.Fprintf(w, "  summary (through seq %d, updated %s):\n    %s\n",
			rec.GetSummaryThroughSeq(), orNone(tsString(rec.GetSummaryUpdatedAt())), collapse(rec.GetSummary()))
	}
	_, _ = fmt.Fprintln(w)
	for _, m := range msgs {
		_, _ = fmt.Fprintln(w, messageLine(m, trunc))
	}
	_, _ = fmt.Fprintf(w, "\n%d message(s).\n", len(msgs))
	return nil
}

// --- search ------------------------------------------------------------

func sessionSearchLive(args []string) error {
	fs := flag.NewFlagSet("session search", flag.ExitOnError)
	var node liveNode
	node.bind(fs)
	channel := fs.String("channel", "", "restrict to one channel kind")
	user := fs.String("user", "", "restrict to conversations opened by this user id")
	limit := fs.Int("limit", 0, "cap conversations returned (0 = service default)")
	snippets := fs.Int("snippets", 0, "cap matching messages shown per conversation (0 = service default)")
	asJSON := fs.Bool("json", false, "emit JSON instead of text")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() == 0 {
		// Enumeration is `session list`. An empty search matching every
		// conversation would read as "they all mention it".
		return errors.New("search text required")
	}
	query := strings.Join(fs.Args(), " ")

	client, closeConn, err := sessionClient(&node)
	if err != nil {
		return err
	}
	defer closeConn()

	ctx, cancel := node.ctx()
	defer cancel()
	res, err := client.SearchSessions(ctx, &lobslawv1.SearchSessionsRequest{
		Text:    query,
		Channel: *channel,
		UserId:  *user,
		//nolint:gosec // CLI caps are not attacker-controlled
		Limit: int32(*limit),
		//nolint:gosec // CLI caps are not attacker-controlled
		SnippetsPerSession: int32(*snippets),
	})
	if err != nil {
		return err
	}
	return renderSessionHits(os.Stdout, res.GetHits(), query, node.addr, *asJSON)
}

func renderSessionHits(w io.Writer, hits []*lobslawv1.SessionSearchHitProto,
	query, source string, asJSON bool) error {
	if asJSON {
		out := make([]map[string]any, 0, len(hits))
		for _, h := range hits {
			sn := make([]map[string]any, 0, len(h.GetSnippets()))
			for _, s := range h.GetSnippets() {
				sn = append(sn, map[string]any{"seq": s.GetSeq(), "role": s.GetRole(), "text": s.GetText()})
			}
			out = append(out, map[string]any{
				"session":  sessionFields(h.GetSession()),
				"matches":  h.GetMatches(),
				"snippets": sn,
			})
		}
		return emitJSON(map[string]any{"source": source, "query": query, "hits": out})
	}

	_, _ = fmt.Fprintln(w, source)
	_, _ = fmt.Fprintf(w, "=== TRANSCRIPT SEARCH: %q ===\n", query)
	if len(hits) == 0 {
		_, _ = fmt.Fprintln(w, "no matches")
		return nil
	}
	for _, h := range hits {
		title := h.GetSession().GetTitle()
		if title == "" {
			title = "(untitled)"
		}
		// The total match count, not the snippet count: it is what tells
		// a passing mention from a thread about the thing.
		_, _ = fmt.Fprintf(w, "\n  %s [%s]  %d match(es)\n",
			title, h.GetSession().GetId(), h.GetMatches())
		for _, sn := range h.GetSnippets() {
			_, _ = fmt.Fprintf(w, "    [#%d %s] %s\n", sn.GetSeq(), sn.GetRole(), collapse(sn.GetText()))
		}
	}
	return nil
}
