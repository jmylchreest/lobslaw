package compute

import (
	"context"
	"strings"
	"testing"

	"github.com/jmylchreest/lobslaw/internal/turn"
)

// fakeBrowser stands in for the transcript store. It honours the
// visibility predicate exactly where the real browser does — before
// the limit, inside Search and Recent — so tests exercise the scoping
// contract rather than an implementation that always says yes.
// ignoreVisibility flips that off, to prove the tools don't rely on
// the browser being well-behaved.
type fakeBrowser struct {
	hits             []SessionBrowseHit
	infos            []SessionBrowseInfo
	msgs             []Message
	perSession       map[turn.SessionKey][]Message
	ignoreVisibility bool

	lastQuery SessionBrowseQuery
	lastLimit int
	lastFrom  uint64
	lastKey   turn.SessionKey
	reads     []turn.SessionKey
}

func (f *fakeBrowser) Search(_ context.Context, q SessionBrowseQuery) ([]SessionBrowseHit, error) {
	f.lastQuery = q
	if q.Visible == nil || f.ignoreVisibility {
		return f.hits, nil
	}
	var out []SessionBrowseHit
	for _, h := range f.hits {
		if q.Visible(h.Info) {
			out = append(out, h)
		}
	}
	return out, nil
}

func (f *fakeBrowser) Recent(_ context.Context, limit int, visible SessionVisibleFunc) ([]SessionBrowseInfo, error) {
	f.lastLimit = limit
	if visible == nil || f.ignoreVisibility {
		return f.infos, nil
	}
	var out []SessionBrowseInfo
	for _, i := range f.infos {
		if visible(i) {
			out = append(out, i)
		}
	}
	return out, nil
}

func (f *fakeBrowser) Read(_ context.Context, key turn.SessionKey, fromSeq uint64, limit int) ([]Message, error) {
	f.lastFrom = fromSeq
	f.lastLimit = limit
	f.lastKey = key
	f.reads = append(f.reads, key)
	if msgs, ok := f.perSession[key]; ok {
		return msgs, nil
	}
	return f.msgs, nil
}

func (f *fakeBrowser) Info(_ context.Context, key turn.SessionKey) (SessionBrowseInfo, bool, error) {
	for _, i := range f.all() {
		if i.Channel == key.Channel && i.ChannelID == key.ChannelID {
			return i, true, nil
		}
	}
	return SessionBrowseInfo{}, false, nil
}

func (f *fakeBrowser) all() []SessionBrowseInfo {
	out := append([]SessionBrowseInfo(nil), f.infos...)
	for _, h := range f.hits {
		out = append(out, h.Info)
	}
	return out
}

func newTestSessionTools(t *testing.T, b *fakeBrowser, cfg SessionToolConfig) *Builtins {
	t.Helper()
	cfg.Browser = b
	builtins := NewBuiltins()
	if err := RegisterSessionBuiltins(builtins, cfg); err != nil {
		t.Fatal(err)
	}
	return builtins
}

func invokeToolCtx(ctx context.Context, t *testing.T, b *Builtins, name string, args map[string]string) ([]byte, int, error) {
	t.Helper()
	fn, ok := b.Get(name)
	if !ok {
		t.Fatalf("%s not registered", name)
	}
	return fn(ctx, args)
}

func invokeTool(t *testing.T, b *Builtins, name string, args map[string]string) ([]byte, int, error) {
	t.Helper()
	return invokeToolCtx(context.Background(), t, b, name, args)
}

func callToolCtx(ctx context.Context, t *testing.T, b *Builtins, name string, args map[string]string) string {
	t.Helper()
	out, code, err := invokeToolCtx(ctx, t, b, name, args)
	if err != nil {
		t.Fatalf("%s: %v (exit %d)", name, err, code)
	}
	return string(out)
}

func callTool(t *testing.T, b *Builtins, name string, args map[string]string) string {
	t.Helper()
	return callToolCtx(context.Background(), t, b, name, args)
}

// scopedCtx is a turn's context: user U talking in channel:chatID.
func scopedCtx(user, channel, chatID string) context.Context {
	return turn.WithIdentity(context.Background(), turn.Identity{
		UserID:    user,
		Channel:   channel,
		ChannelID: chatID,
	})
}

func TestSessionSearchRequiresQuery(t *testing.T) {
	t.Parallel()
	b := newTestSessionTools(t, &fakeBrowser{}, SessionToolConfig{})
	if _, code, err := invokeTool(t, b, "session_search", map[string]string{}); err == nil || code != 2 {
		t.Errorf("empty query should be a usage error; got code=%d err=%v", code, err)
	}
}

func TestSessionSearchReportsNoMatchesPlainly(t *testing.T) {
	t.Parallel()
	b := newTestSessionTools(t, &fakeBrowser{}, SessionToolConfig{})
	out := callTool(t, b, "session_search", map[string]string{"query": "nothing"})
	if !strings.Contains(out, "No past conversation") {
		t.Errorf("output = %q", out)
	}
}

func TestSessionSearchRendersTitleAndAddress(t *testing.T) {
	t.Parallel()
	browser := &fakeBrowser{hits: []SessionBrowseHit{{
		Info: SessionBrowseInfo{
			Channel: "telegram", ChannelID: "555",
			Title: "Raft snapshot corruption", UpdatedAt: "2026-08-12 10:00 UTC",
		},
		Matches:  3,
		Snippets: []SessionBrowseSnippet{{Seq: 7, Role: "user", Text: "the\nsnapshot   was corrupt"}},
	}}}
	b := newTestSessionTools(t, browser, SessionToolConfig{})
	out := callTool(t, b, "session_search", map[string]string{"query": "snapshot"})

	for _, want := range []string{"Raft snapshot corruption", "telegram", "555", "#7", "3 match"} {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q:\n%s", want, out)
		}
	}
	// Snippets are flattened so a multi-line match doesn't wreck the list.
	if strings.Contains(out, "the\nsnapshot") || strings.Contains(out, "snapshot   was") {
		t.Errorf("snippet whitespace not collapsed:\n%s", out)
	}
}

// The model must not be able to widen its own limits and pull an
// unbounded amount of transcript into context.
func TestSessionToolsClampModelSuppliedLimits(t *testing.T) {
	t.Parallel()
	browser := &fakeBrowser{}
	b := newTestSessionTools(t, browser, SessionToolConfig{MaxSearchResults: 5, MaxReadMessages: 10})

	callTool(t, b, "session_search", map[string]string{"query": "x", "limit": "9999"})
	if browser.lastQuery.Limit > 5 {
		t.Errorf("search limit = %d, want clamped to 5", browser.lastQuery.Limit)
	}
	callTool(t, b, "session_read", map[string]string{"channel": "rest", "channel_id": "1", "limit": "9999"})
	if browser.lastLimit > 10 {
		t.Errorf("read limit = %d, want clamped to 10", browser.lastLimit)
	}
}

func TestSessionReadRequiresAddress(t *testing.T) {
	t.Parallel()
	b := newTestSessionTools(t, &fakeBrowser{}, SessionToolConfig{})
	if _, code, err := invokeTool(t, b, "session_read", map[string]string{"channel": "rest"}); err == nil || code != 2 {
		t.Errorf("missing channel_id should be a usage error; got code=%d err=%v", code, err)
	}
}

func TestSessionReadRejectsNonNumericFromSeq(t *testing.T) {
	t.Parallel()
	b := newTestSessionTools(t, &fakeBrowser{}, SessionToolConfig{})
	_, code, err := invokeTool(t, b, "session_read",
		map[string]string{"channel": "rest", "channel_id": "1", "from_seq": "banana"})
	if err == nil || code != 2 {
		t.Errorf("non-numeric from_seq should be a usage error; got code=%d err=%v", code, err)
	}
}

func TestSessionListShowsUntitledSessions(t *testing.T) {
	t.Parallel()
	browser := &fakeBrowser{infos: []SessionBrowseInfo{
		{Channel: "rest", ChannelID: "1", Messages: 4, UpdatedAt: "2026-08-12 10:00 UTC"},
	}}
	b := newTestSessionTools(t, browser, SessionToolConfig{})
	out := callTool(t, b, "session_list", map[string]string{})
	if !strings.Contains(out, "(untitled)") {
		t.Errorf("an untitled session should still be listed and addressable:\n%s", out)
	}
	if !strings.Contains(out, "rest") || !strings.Contains(out, "4 messages") {
		t.Errorf("output = %q", out)
	}
}

func TestSessionToolDefsAreWellFormed(t *testing.T) {
	t.Parallel()
	defs := SessionToolDefs()
	if len(defs) != 3 {
		t.Fatalf("got %d tool defs, want 3", len(defs))
	}
	for _, d := range defs {
		if d.Name == "" || d.Path == "" || len(d.ParametersSchema) == 0 {
			t.Errorf("incomplete tool def: %+v", d)
		}
		if !strings.HasPrefix(d.Path, BuiltinScheme) {
			t.Errorf("%s path = %q, want the builtin scheme", d.Name, d.Path)
		}
	}
}
